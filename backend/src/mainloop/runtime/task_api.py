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
