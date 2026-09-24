"""Workspace lifecycle endpoints for manifest and actor state."""

from fastapi import APIRouter, Header, HTTPException
from mainloop.db import db
from mainloop.runtime import workspace_adapter
from mainloop.runtime.contracts import ContractError
from mainloop.sse import notify_workspace_updated

from models import WorkspaceLifecycle

router = APIRouter(prefix="/workspaces", tags=["workspaces"])


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
