"""Workspace lifecycle endpoints for branch workspace actors."""

import json
import uuid
from datetime import UTC, datetime
from typing import Annotated

from fastapi import APIRouter, Header, HTTPException, Response
from fastapi.responses import JSONResponse
from mainloop.config import settings
from mainloop.db import db
from mainloop.runtime import workspace_adapter
from mainloop.runtime.actor_provisioner import get_actor_provisioner
from mainloop.runtime.contracts import ContractError
from mainloop.runtime.credential_broker import CredentialBroker, CredentialBrokerError
from mainloop.runtime.credential_reauth import (
    CredentialReauthRunner,
    KubernetesCredentialReauthRunner,
)
from mainloop.sse import notify_workspace_updated
from pydantic import BaseModel, ConfigDict, Field, StrictStr, field_validator

from models import (
    WorkspaceDev,
    WorkspaceLifecycle,
    WorkspaceManifest,
    WorkspaceObservedState,
)

router = APIRouter(prefix="/workspaces", tags=["workspaces"])
_reauth_runner: CredentialReauthRunner = KubernetesCredentialReauthRunner()
_reauth_owners: dict[str, tuple[str, str]] = {}


class CreateWorkspaceRequest(BaseModel):
    project_id: Annotated[StrictStr, Field(min_length=1)]
    branch: Annotated[StrictStr, Field(min_length=1)]
    dev: WorkspaceDev

    model_config = ConfigDict(extra="forbid", strict=True)

    @field_validator("branch")
    @classmethod
    def validate_branch_name(cls, value: str) -> str:
        WorkspaceManifest(branch=value, resource_class="default")
        return value


def _user_id(value: str | None) -> str:
    return value or "local-dev-user"


async def _require_owned_workspace(workspace_id: str, user_id: str) -> None:
    async with db.connection() as conn:
        exists = await conn.fetchval(
            """SELECT EXISTS (
                   SELECT 1 FROM workspace_bindings b
                   JOIN sessions s ON s.id=b.workspace_id
                   WHERE b.workspace_id=$1 AND s.user_id=$2
               )""",
            workspace_id,
            user_id,
        )
    if not exists:
        raise HTTPException(status_code=404, detail="Workspace not found")


async def _publish(user_id: str, lifecycle: WorkspaceLifecycle) -> None:
    await notify_workspace_updated(user_id, lifecycle.model_dump(mode="json"))


@router.post("", response_model=WorkspaceLifecycle)
async def create_workspace(
    request: CreateWorkspaceRequest,
    user_id: str | None = Header(default=None, alias="X-User-ID"),
):
    """Create a branch workspace and its independent Substrate actor."""
    owner = _user_id(user_id)
    workspace_id = str(uuid.uuid4())
    actor_name = f"ml-{workspace_id[:16]}"
    shim_token_secret_name = f"{actor_name}-shim"
    atespace = settings.substrate_atespace
    template = request.dev.actor_template or settings.substrate_actor_template

    async with db.connection() as conn:
        project = await conn.fetchrow(
            "SELECT id, html_url FROM projects WHERE id=$1 AND user_id=$2",
            request.project_id,
            owner,
        )
        if project is None:
            raise HTTPException(status_code=404, detail="Project not found")

        manifest = WorkspaceManifest(
            repo_url=project["html_url"],
            branch=request.branch,
            resource_class="default",
            dev=request.dev,
        )
        conversation_id = str(uuid.uuid4())
        now = datetime.now(UTC)
        async with conn.transaction():
            thread = await conn.fetchrow(
                "SELECT id FROM main_threads WHERE user_id=$1 ORDER BY created_at LIMIT 1",
                owner,
            )
            thread_id = thread["id"] if thread else str(uuid.uuid4())
            if thread is None:
                await conn.execute(
                    "INSERT INTO main_threads (id,user_id) VALUES ($1,$2)",
                    thread_id,
                    owner,
                )
            await conn.execute(
                "INSERT INTO conversations (id,user_id,title) VALUES ($1,$2,$3)",
                conversation_id,
                owner,
                f"{project['id']} · {request.branch}",
            )
            await conn.execute(
                """INSERT INTO sessions
                   (id,user_id,main_thread_id,title,description,prompt,conversation_id,
                    status,created_at,repo_url,project_id,branch_name,base_branch)
                   VALUES ($1,$2,$3,$4,$5,$6,$7,'active',$8,$9,$10,$11,$12)""",
                workspace_id,
                owner,
                thread_id,
                f"{project['id']} · {request.branch}",
                "Branch development workspace",
                "Development workspace",
                conversation_id,
                now,
                project["html_url"],
                request.project_id,
                request.branch,
                request.branch,
            )
            await conn.execute(
                """INSERT INTO workspace_bindings
                   (workspace_id,atespace,actor_name,actor_template,
                    shim_token_secret_name,observed_state,desired_state,created_at,updated_at)
                   VALUES ($1,$2,$3,$4,$5,'unknown','active',$6,$6)""",
                workspace_id,
                atespace,
                actor_name,
                template,
                shim_token_secret_name,
                now,
            )
            await conn.execute(
                """INSERT INTO workspace_lifecycles
                   (workspace_id,desired_state,observed_state,manifest,conditions,
                    last_activity_at,updated_at)
                   VALUES ($1,'running','unknown',$2::jsonb,'[]'::jsonb,$3,$3)""",
                workspace_id,
                json.dumps(manifest.model_dump(mode="json")),
                now,
            )

    try:
        provisioner = get_actor_provisioner()
        provisioned = await provisioner.create(
            atespace=atespace,
            actor_name=actor_name,
            template=template,
            shim_token_secret_name=shim_token_secret_name,
        )
        lifecycle = await workspace_adapter._record_observation(
            workspace_id, actor=provisioned.actor
        )
    except Exception:
        # The row and actor identity are durable before the external call. Keep them so a
        # refresh can reconcile an outcome that timed out instead of creating a second actor.
        lifecycle = await workspace_adapter._record_observation(
            workspace_id,
            failure=(
                WorkspaceObservedState.UNKNOWN,
                "ProvisioningUncertain",
                "Actor provisioning did not return a confirmed result. Refresh status before retrying.",
            ),
        )
        await _publish(owner, lifecycle)
        return JSONResponse(status_code=202, content=lifecycle.model_dump(mode="json"))
    await _publish(owner, lifecycle)
    return lifecycle


@router.get("", response_model=list[WorkspaceLifecycle])
async def list_workspaces(
    user_id: str | None = Header(default=None, alias="X-User-ID"),
):
    return await workspace_adapter.list_workspace_lifecycles(_user_id(user_id))


@router.get("/{workspace_id}", response_model=WorkspaceLifecycle)
async def get_workspace(
    workspace_id: str,
    user_id: str | None = Header(default=None, alias="X-User-ID"),
):
    owner = _user_id(user_id)
    await _require_owned_workspace(workspace_id, owner)
    lifecycle = await workspace_adapter.ensure_workspace_lifecycle(workspace_id)
    if lifecycle is None:
        raise HTTPException(status_code=404, detail="Workspace not found")
    return lifecycle


async def _run_operation(
    workspace_id: str,
    owner: str,
    operation,
) -> WorkspaceLifecycle:
    await _require_owned_workspace(workspace_id, owner)
    try:
        lifecycle = await operation(workspace_id)
    except ContractError as exc:
        current = await workspace_adapter.get_workspace_lifecycle(workspace_id)
        if current is not None:
            await _publish(owner, current)
        raise HTTPException(status_code=409, detail=str(exc)) from exc
    await _publish(owner, lifecycle)
    return lifecycle


@router.post("/{workspace_id}/suspend", response_model=WorkspaceLifecycle)
async def suspend_workspace(
    workspace_id: str,
    user_id: str | None = Header(default=None, alias="X-User-ID"),
):
    return await _run_operation(
        workspace_id, _user_id(user_id), workspace_adapter.suspend_workspace
    )


@router.post("/{workspace_id}/resume", response_model=WorkspaceLifecycle)
async def resume_workspace(
    workspace_id: str,
    user_id: str | None = Header(default=None, alias="X-User-ID"),
):
    return await _run_operation(
        workspace_id, _user_id(user_id), workspace_adapter.resume_workspace
    )


@router.post("/{workspace_id}/refresh", response_model=WorkspaceLifecycle)
async def refresh_workspace(
    workspace_id: str,
    user_id: str | None = Header(default=None, alias="X-User-ID"),
):
    return await _run_operation(
        workspace_id, _user_id(user_id), workspace_adapter.refresh_workspace_lifecycle
    )


@router.post("/{workspace_id}/touch", response_model=WorkspaceLifecycle)
async def touch_workspace(
    workspace_id: str,
    reason: str,
    user_id: str | None = Header(default=None, alias="X-User-ID"),
):
    owner = _user_id(user_id)
    await _require_owned_workspace(workspace_id, owner)
    try:
        lifecycle = await workspace_adapter.touch_workspace(workspace_id, reason=reason)
    except (ContractError, ValueError) as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    await _publish(owner, lifecycle)
    return lifecycle


@router.delete("/{workspace_id}", status_code=204)
async def delete_workspace(
    workspace_id: str,
    user_id: str | None = Header(default=None, alias="X-User-ID"),
):
    owner = _user_id(user_id)
    await _require_owned_workspace(workspace_id, owner)
    async with workspace_adapter._lock(workspace_id):
        async with db.connection() as conn:
            async with conn.transaction():
                row = await conn.fetchrow(
                    """SELECT b.atespace,b.actor_name,b.shim_token_secret_name,
                              b.desired_state,s.conversation_id
                       FROM workspace_bindings b JOIN sessions s ON s.id=b.workspace_id
                       WHERE b.workspace_id=$1 FOR UPDATE OF b""",
                    workspace_id,
                )
                if row is None:
                    raise HTTPException(status_code=404, detail="Workspace not found")
                open_deliveries = await workspace_adapter._delivery_states(
                    workspace_id, conn=conn
                )
                if open_deliveries:
                    raise HTTPException(
                        status_code=409,
                        detail="An open delivery must be reconciled before deleting this workspace.",
                    )
                await conn.execute(
                    "UPDATE workspace_bindings SET desired_state='deleting',updated_at=NOW() WHERE workspace_id=$1",
                    workspace_id,
                )
        try:
            await get_actor_provisioner().delete(
                atespace=row["atespace"],
                actor_name=row["actor_name"],
                shim_token_secret_name=row["shim_token_secret_name"],
            )
        except Exception as exc:
            async with db.connection() as conn:
                await conn.execute(
                    """UPDATE workspace_bindings SET desired_state=$2,updated_at=NOW()
                       WHERE workspace_id=$1 AND desired_state='deleting'""",
                    workspace_id,
                    row["desired_state"],
                )
            raise HTTPException(
                status_code=502,
                detail="Substrate did not confirm workspace deletion; refresh before retrying.",
            ) from exc
        async with db.connection() as conn:
            async with conn.transaction():
                await conn.execute(
                    "DELETE FROM native_deliveries WHERE session_id=$1", workspace_id
                )
                await conn.execute(
                    "DELETE FROM native_events WHERE session_id=$1", workspace_id
                )
                await conn.execute(
                    "DELETE FROM native_lineage WHERE session_id=$1", workspace_id
                )
                await conn.execute(
                    "DELETE FROM native_bindings WHERE session_id=$1", workspace_id
                )
                await conn.execute(
                    "DELETE FROM workspace_lifecycles WHERE workspace_id=$1",
                    workspace_id,
                )
                await conn.execute(
                    "DELETE FROM workspace_bindings WHERE workspace_id=$1", workspace_id
                )
                await conn.execute("DELETE FROM sessions WHERE id=$1", workspace_id)
                await conn.execute(
                    """DELETE FROM messages
                       WHERE conversation_id=$1
                         AND NOT EXISTS (
                             SELECT 1 FROM sessions s WHERE s.anchor_message_id=messages.id
                         )""",
                    row["conversation_id"],
                )
                await conn.execute(
                    """DELETE FROM conversations c WHERE c.id=$1
                       AND NOT EXISTS (
                           SELECT 1 FROM messages m WHERE m.conversation_id=c.id
                       )""",
                    row["conversation_id"],
                )
    return Response(status_code=204)


@router.get("/{workspace_id}/credentials")
async def list_workspace_credentials(
    workspace_id: str,
    user_id: str | None = Header(default=None, alias="X-User-ID"),
):
    owner = _user_id(user_id)
    await _require_owned_workspace(workspace_id, owner)
    _require_credential_owner(owner)
    broker = CredentialBroker()
    states = []
    for provider in ("codex", "claude"):
        try:
            status = await broker.status(provider)
        except CredentialBrokerError as exc:
            raise HTTPException(
                status_code=503, detail="Credential status is unavailable"
            ) from exc
        states.append(
            {
                "provider": provider,
                "available": status.available if status else False,
                "needs_signin": status.needs_signin if status else True,
                "expires_at": (
                    status.expires_at.isoformat()
                    if status and status.expires_at
                    else None
                ),
            }
        )
    return {"credentials": states}


@router.post("/{workspace_id}/credentials/{provider}/reauth")
async def start_workspace_credential_reauth(
    workspace_id: str,
    provider: str,
    user_id: str | None = Header(default=None, alias="X-User-ID"),
):
    owner = _user_id(user_id)
    await _require_owned_workspace(workspace_id, owner)
    _require_credential_owner(owner)
    if provider not in {"codex", "claude"}:
        raise HTTPException(status_code=404, detail="Credential provider not found")
    try:
        job = await _reauth_runner.start(provider, owner=owner)
    except Exception as exc:
        raise HTTPException(
            status_code=503, detail="Sign-in could not be started"
        ) from exc
    _reauth_owners[job.id] = (workspace_id, owner)
    return _reauth_status_payload(job)


@router.get("/{workspace_id}/credentials/reauth/{job_id}")
async def workspace_credential_reauth_status(
    workspace_id: str,
    job_id: str,
    user_id: str | None = Header(default=None, alias="X-User-ID"),
):
    owner = _user_id(user_id)
    await _require_owned_workspace(workspace_id, owner)
    if _reauth_owners.get(job_id) != (workspace_id, owner):
        raise HTTPException(status_code=404, detail="Sign-in job not found")
    job = await _reauth_runner.status(job_id)
    if job is None:
        raise HTTPException(status_code=404, detail="Sign-in job not found")
    if job.state in {"completed", "failed"}:
        _reauth_owners.pop(job_id, None)
    return _reauth_status_payload(job)


def _reauth_status_payload(job) -> dict:
    return {
        "id": job.id,
        "provider": job.provider,
        "state": job.state,
        "challenge": (
            {"url": job.challenge.url, "code": job.challenge.code}
            if job.challenge
            else None
        ),
    }


def _require_credential_owner(user_id: str) -> None:
    if user_id != settings.substrate_credential_owner_user_id:
        raise HTTPException(status_code=404, detail="Workspace not found")
