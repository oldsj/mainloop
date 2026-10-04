"""Workspace lifecycle endpoints for branch workspace actors."""

from fastapi import APIRouter, Header, HTTPException, Response
from mainloop.config import settings
from mainloop.db import db
from mainloop.runtime import workspace_adapter
from mainloop.runtime.actor_provisioner import get_actor_provisioner
from mainloop.runtime.credential_broker import CredentialBroker, CredentialBrokerError
from mainloop.runtime.credential_reauth import (
    CredentialReauthRunner,
    KubernetesCredentialReauthRunner,
)
from mainloop.runtime.preview_proxy import workspace_preview_ports
from mainloop.runtime.workspace_adapter import ContractError
from mainloop.sse import notify_workspace_updated

from models import WorkspaceLifecycle

router = APIRouter(prefix="/workspaces", tags=["workspaces"])
_reauth_runner: CredentialReauthRunner = KubernetesCredentialReauthRunner()
_reauth_owners: dict[str, tuple[str, str]] = {}


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


WORKSPACES_MOVING = "workspaces move to kagent in a later slice"


@router.post("", status_code=409)
async def create_workspace() -> None:
    """Refuse new branch workspaces until they are created as kagent Sessions.

    Agent turns already run in kagent; a workspace created here would put its repository and
    preview in a separate Substrate actor that the agent never sees. Existing workspaces keep
    their other routes.
    """
    raise HTTPException(status_code=409, detail=WORKSPACES_MOVING)


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


@router.get("/{workspace_id}/ports")
async def list_workspace_preview_ports(
    workspace_id: str,
    user_id: str | None = Header(default=None, alias="X-User-ID"),
):
    ports = await workspace_preview_ports(workspace_id, _user_id(user_id))
    if ports is None:
        raise HTTPException(status_code=404, detail="Workspace not found")
    return {"ports": ports}


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
