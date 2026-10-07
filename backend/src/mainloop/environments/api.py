"""Owner REST routes. Deliberately absent from agent MCP tools."""

import uuid
from contextlib import asynccontextmanager
from typing import Annotated, Literal

from fastapi import APIRouter, Depends, HTTPException
from mainloop.config import settings
from mainloop.db import db
from mainloop.db import environments as store
from mainloop.environments.registry import (
    VALIDATOR_VERSION,
    AnonymousOCIRegistry,
    RegistryClient,
    RegistryError,
    refresh,
    validate,
)
from mainloop.identity import current_user
from pydantic import BaseModel, ConfigDict

from models.environment import (
    DevEnvironment,
    EnvironmentVersion,
    RegisterEnvironment,
    SelectEnvironment,
)

router = APIRouter(tags=["environments"])


def registry_client():
    return AnonymousOCIRegistry()


@asynccontextmanager
async def transaction():
    try:
        async with db.connection() as conn, conn.transaction():
            yield conn
    except store.EnvironmentError as exc:
        raise HTTPException(exc.status, str(exc)) from exc


def new_id():
    return uuid.uuid4().hex


@router.post("/environments", status_code=201)
async def register(
    request: RegisterEnvironment,
    client: Annotated[RegistryClient, Depends(registry_client)],
    owner: str = Depends(current_user),
):
    env = DevEnvironment(
        id=new_id(),
        owner_id=owner,
        name=request.name,
        source_kind=request.source_kind,
        watched_tag=request.watched_tag,
        definition=request.definition,
    )
    if request.source_kind == "definition_repo":
        value = EnvironmentVersion(
            id=new_id(),
            environment_id=env.id,
            definition=request.definition,
            validation_status="pending_build",
            validator_version=VALIDATOR_VERSION,
            provenance_kind="mainloop_built",
            validation_result={"build": "not_run"},
        )
    else:
        try:
            value = await validate(
                client,
                request.image,
                request.architecture,
                settings.environment_registry_allowlist,
                env.id,
                new_id(),
            )
        except RegistryError as exc:
            raise HTTPException(422, str(exc)) from exc
    async with transaction() as conn:
        await store.register(conn, env, value)
    return {"environment": env, "version": value}


@router.get("/environments")
async def list_environments(owner: str = Depends(current_user)):
    async with transaction() as conn:
        return await store.list_environments(conn, owner)


@router.get("/environments/{environment_id}")
async def get_environment(environment_id: str, owner: str = Depends(current_user)):
    async with transaction() as conn:
        return await store.environment(conn, environment_id, owner)


@router.get("/environments/{environment_id}/versions")
async def versions(environment_id: str, owner: str = Depends(current_user)):
    async with transaction() as conn:
        await store.environment(conn, environment_id, owner)
        return await store.versions(conn, environment_id)


@router.get("/environments/{environment_id}/versions/{version_id}")
async def version(
    environment_id: str, version_id: str, owner: str = Depends(current_user)
):
    async with transaction() as conn:
        await store.environment(conn, environment_id, owner)
        return await store.version(conn, environment_id, version_id)


class RefreshRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")
    version_id: str


@router.post("/environments/{environment_id}/refresh", status_code=201)
async def refresh_tag(
    environment_id: str,
    request: RefreshRequest,
    client: Annotated[RegistryClient, Depends(registry_client)],
    owner: str = Depends(current_user),
):
    async with transaction() as conn:
        env = await store.environment(conn, environment_id, owner)
        previous = await store.version(conn, environment_id, request.version_id)
    try:
        value = await refresh(
            client, env, previous, settings.environment_registry_allowlist, new_id()
        )
    except RegistryError as exc:
        raise HTTPException(422, str(exc)) from exc
    async with transaction() as conn:
        await store.environment(conn, environment_id, owner, lock=True)
        await store.add_version(conn, value)
    return value


@router.put("/environments/{environment_id}/default")
async def set_default(
    environment_id: str, request: RefreshRequest, owner: str = Depends(current_user)
):
    async with transaction() as conn:
        return await store.set_default(conn, environment_id, owner, request.version_id)


class GrantRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")
    permission: Literal["use", "derive"]


@router.put("/environments/{environment_id}/grants/{project_id}")
async def grant(
    environment_id: str,
    project_id: str,
    request: GrantRequest,
    owner: str = Depends(current_user),
):
    async with transaction() as conn:
        await store.grant(conn, environment_id, owner, project_id, request.permission)
    return {
        "environment_id": environment_id,
        "project_id": project_id,
        "permission": request.permission,
    }


@router.delete("/environments/{environment_id}/grants/{project_id}", status_code=204)
async def revoke(
    environment_id: str, project_id: str, owner: str = Depends(current_user)
):
    async with transaction() as conn:
        await store.grant(conn, environment_id, owner, project_id, None)


@router.put("/projects/{project_id}/environment")
async def select(
    project_id: str, request: SelectEnvironment, owner: str = Depends(current_user)
):
    async with transaction() as conn:
        return await store.select(conn, project_id, owner, request)


@router.get("/projects/{project_id}/environment")
async def selection(project_id: str, owner: str = Depends(current_user)):
    async with transaction() as conn:
        return await store.selection(conn, project_id, owner)
