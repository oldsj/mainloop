"""Owner task REST reads and fail-closed action seams."""

from contextlib import asynccontextmanager

from fastapi import APIRouter, Depends, HTTPException
from mainloop.db import db
from mainloop.db import tasks as store
from mainloop.identity import current_user
from mainloop.providers import registry
from mainloop.tasks import service
from mainloop.tasks.principal import TaskPrincipal

from models.task import ProjectProviderUpdate, TaskAction, TaskCreate, TaskReassign

router = APIRouter(tags=["tasks"])


@asynccontextmanager
async def transaction():
    try:
        async with db.connection() as conn, conn.transaction():
            yield conn
    except store.TaskError as exc:
        raise HTTPException(exc.status, detail={"reason": exc.code}) from exc


@router.get("/tasks")
async def list_tasks(
    project_id: str | None = None,
    parent_task_id: str | None = None,
    owner: str = Depends(current_user),
):
    async with transaction() as conn:
        principal = TaskPrincipal(owner)
        values = await store.list_tasks(
            conn, principal, project_id=project_id, parent_task_id=parent_task_id
        )
        return [await service.read(conn, principal, value.id) for value in values]


@router.get("/tasks/{task_id}")
async def get_task(task_id: str, owner: str = Depends(current_user)):
    async with transaction() as conn:
        return await service.read(conn, TaskPrincipal(owner), task_id)


@router.get("/task-operations/{operation_id}")
async def get_operation(operation_id: str, owner: str = Depends(current_user)):
    async with transaction() as conn:
        return await store.operation(conn, operation_id, TaskPrincipal(owner))


@router.post("/tasks", status_code=202)
async def create_task(request: TaskCreate, owner: str = Depends(current_user)):
    async with transaction() as conn:
        return await service.mutate(conn, TaskPrincipal(owner), "create", request)


@router.post("/tasks/{task_id}/retry", status_code=202)
async def retry_task(
    task_id: str, request: TaskAction, owner: str = Depends(current_user)
):
    async with transaction() as conn:
        return await service.mutate(
            conn, TaskPrincipal(owner), "retry", request, task_id=task_id
        )


@router.post("/tasks/{task_id}/reassign", status_code=202)
async def reassign_task(
    task_id: str, request: TaskReassign, owner: str = Depends(current_user)
):
    async with transaction() as conn:
        return await service.mutate(
            conn, TaskPrincipal(owner), "reassign", request, task_id=task_id
        )


@router.post("/tasks/{task_id}/cancel", status_code=202)
async def cancel_task(
    task_id: str, request: TaskAction, owner: str = Depends(current_user)
):
    async with transaction() as conn:
        return await service.mutate(
            conn, TaskPrincipal(owner), "cancel", request, task_id=task_id
        )


@router.get("/projects/{project_id}/default-provider")
async def get_preference(project_id: str, owner: str = Depends(current_user)):
    async with transaction() as conn:
        return await store.preference(conn, project_id, owner)


@router.put("/projects/{project_id}/default-provider")
async def set_preference(
    project_id: str, request: ProjectProviderUpdate, owner: str = Depends(current_user)
):
    if request.profile_id is not None:
        try:
            registry().resolve(request.profile_id, "supervisor", selecting=True)
        except ValueError as exc:
            raise HTTPException(422, detail=str(exc)) from exc
    async with transaction() as conn:
        return await store.set_preference(conn, project_id, owner, request)


@router.get("/projects/{project_id}/smoke-observations")
async def smoke_observations(
    project_id: str, branch: str | None = None, owner: str = Depends(current_user)
):
    """Sanitized ledger facts for an operator smoke run; grants stay private."""
    from mainloop.config import settings
    from mainloop.runtime.native_sessions import OPEN_STATES

    async with transaction() as conn:
        if not await conn.fetchval(
            "SELECT id FROM projects WHERE id=$1 AND user_id=$2", project_id, owner
        ):
            raise HTTPException(404, "Project not found")
        holders = await conn.fetch(
            """SELECT DISTINCT t.id FROM task_attempts a JOIN tasks t ON t.id=a.task_id
            WHERE a.capacity_held AND t.owner_id=$1 AND t.parent_task_id IS NULL""",
            owner,
        )
        deliveries = await conn.fetch(
            """SELECT d.message_id,d.state FROM native_deliveries d
            JOIN sessions s ON s.id=d.session_id
            JOIN native_bindings b ON b.session_id=s.id
            WHERE s.user_id=$1 AND (s.project_id=$2 OR b.role='main')
            AND d.state=ANY($3) ORDER BY d.message_id LIMIT 101""",
            owner,
            project_id,
            [*OPEN_STATES, "queued", "uncertain"],
        )
        pushes = await conn.fetch(
            """SELECT p.request_id,p.state,p.branch FROM push_publications p
            JOIN push_grants g ON g.id=p.grant_id
            WHERE g.owner_id=$1 AND g.project_id=$2 AND p.branch=$3 ORDER BY p.updated_at DESC,p.request_id LIMIT 101""",
            owner,
            project_id,
            branch,
        )
        return {
            "parent_capacity": settings.task_max_children_per_parent,
            "capacity_holders": [row["id"] for row in holders],
            "global_capacity_available": await conn.fetchval(
                "SELECT count(*) FROM task_attempts WHERE capacity_held"
            )
            < settings.task_max_children_global,
            "deliveries_busy": bool(deliveries),
            "deliveries_truncated": len(deliveries) > 100,
            "deliveries": [dict(row) for row in deliveries[:100]],
            "push_confirmed": await conn.fetchval(
                """SELECT EXISTS(SELECT 1 FROM push_publications p
                JOIN push_grants g ON g.id=p.grant_id
                WHERE g.owner_id=$1 AND g.project_id=$2 AND p.branch=$3
                AND p.state='confirmed')""",
                owner,
                project_id,
                branch,
            ),
            "pushes_truncated": len(pushes) > 100,
            "pushes": [dict(row) for row in pushes[:100]],
        }
