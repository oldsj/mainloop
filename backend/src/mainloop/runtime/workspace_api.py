"""Branch workspace endpoints: kagent Sessions that start with a repository checked out."""

import uuid
from typing import Annotated

from fastapi import APIRouter, Depends, HTTPException, Response
from mainloop.identity import current_user
from mainloop.runtime import workspaces
from mainloop.runtime.preview_proxy import workspace_preview_ports
from mainloop.services.github_repo import InvalidGithubRepo, parse_github_repo
from pydantic import (
    BaseModel,
    ConfigDict,
    Field,
    StrictStr,
    ValidationError,
    model_validator,
)

from models import (
    ProviderProfileId,
    WorkspaceDev,
    WorkspaceLifecycle,
    WorkspaceManifest,
)

router = APIRouter(prefix="/workspaces", tags=["workspaces"])


class CreateWorkspaceRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    # Exactly one of the two: an existing project, or a GitHub repository whose project is
    # found or created (``owner/name`` or ``https://github.com/owner/name[.git]``).
    project_id: StrictStr | None = None
    repo: StrictStr | None = None
    # Local branch to create or switch to; empty always means a new ``mainloop/<8 hex>`` branch.
    branch: StrictStr = ""
    # Branch, tag or commit to start from; empty means the project's default branch, or the
    # remote's default when none is recorded.
    ref: StrictStr = ""
    depth: Annotated[int, Field(ge=0, le=1000)] = 0
    dev: WorkspaceDev = WorkspaceDev()
    agent_kind: ProviderProfileId = "claude"

    @model_validator(mode="after")
    def exactly_one_target(self):
        if (self.project_id is None) == (self.repo is None):
            raise ValueError("Provide exactly one of project_id or repo")
        return self


async def _run(user_id: str, operation) -> WorkspaceLifecycle:
    """Run a workspaces operation, mapping its errors to HTTP and publishing the result."""
    try:
        lifecycle = await operation
    except workspaces.WorkspaceNotFound as exc:
        raise HTTPException(status_code=404, detail=str(exc)) from exc
    except workspaces.WorkspaceConflict as exc:
        raise HTTPException(status_code=409, detail=str(exc)) from exc
    except workspaces.WorkspaceRejected as exc:
        raise HTTPException(status_code=422, detail=str(exc)) from exc
    except workspaces.WorkspaceUnconfirmed as exc:
        raise HTTPException(status_code=502, detail=str(exc)) from exc
    await workspaces.publish(user_id, lifecycle)
    return lifecycle


@router.post("", status_code=201, response_model=WorkspaceLifecycle)
async def create_workspace(
    request: CreateWorkspaceRequest,
    owner: str = Depends(current_user),
):
    """Create a branch workspace: a session whose kagent Session has the repository cloned in."""
    from mainloop.providers import registry

    try:
        profile = registry().resolve(request.agent_kind, "agent", selecting=True)
    except ValueError as exc:
        raise HTTPException(status_code=422, detail=str(exc)) from exc
    branch = request.branch or f"mainloop/{uuid.uuid4().hex[:8]}"

    def manifest_for(repo_url: str, ref: str) -> WorkspaceManifest:
        try:
            return WorkspaceManifest(
                repo_url=repo_url,
                ref=ref,
                branch=branch,
                depth=request.depth,
                agent_kind=profile.id,
                dev=request.dev,
            )
        except ValidationError as exc:
            first = exc.errors()[0]
            where = ".".join(str(part) for part in first["loc"])
            raise HTTPException(
                status_code=422, detail=f"{where}: {first['msg']}"
            ) from exc

    if request.repo is not None:
        try:
            repo = parse_github_repo(request.repo)
        except InvalidGithubRepo as exc:
            raise HTTPException(status_code=422, detail=str(exc)) from exc
        # Refuse a bad branch or ref before the project is found or created.
        manifest_for(repo.html_url, request.ref)
        project = await workspaces.project_for_repo(owner, repo)
    else:
        project = await workspaces.project_for(owner, request.project_id)
        if project is None:
            raise HTTPException(status_code=404, detail="Project not found")
    manifest = manifest_for(
        project["html_url"], request.ref or project["default_branch"] or ""
    )
    return await _run(owner, workspaces.create(owner, project["id"], manifest))


@router.get("", response_model=list[WorkspaceLifecycle])
async def list_workspaces(
    owner: str = Depends(current_user),
):
    return await workspaces.list_for(owner)


@router.get("/{workspace_id}", response_model=WorkspaceLifecycle)
async def get_workspace(
    workspace_id: str,
    owner: str = Depends(current_user),
):
    try:
        return await workspaces.get(workspace_id, owner)
    except workspaces.WorkspaceNotFound as exc:
        raise HTTPException(status_code=404, detail=str(exc)) from exc


@router.post("/{workspace_id}/suspend", response_model=WorkspaceLifecycle)
async def suspend_workspace(
    workspace_id: str,
    owner: str = Depends(current_user),
):
    return await _run(owner, workspaces.suspend(workspace_id, owner))


@router.post("/{workspace_id}/resume", response_model=WorkspaceLifecycle)
async def resume_workspace(
    workspace_id: str,
    owner: str = Depends(current_user),
):
    return await _run(owner, workspaces.resume(workspace_id, owner))


@router.post("/{workspace_id}/refresh", response_model=WorkspaceLifecycle)
async def refresh_workspace(
    workspace_id: str,
    owner: str = Depends(current_user),
):
    return await _run(owner, workspaces.refresh(workspace_id, owner))


@router.delete("/{workspace_id}", status_code=204)
async def delete_workspace(
    workspace_id: str,
    owner: str = Depends(current_user),
):
    try:
        await workspaces.delete(workspace_id, owner)
    except workspaces.WorkspaceNotFound as exc:
        raise HTTPException(status_code=404, detail=str(exc)) from exc
    except workspaces.WorkspaceConflict as exc:
        raise HTTPException(status_code=409, detail=str(exc)) from exc
    except workspaces.WorkspaceUnconfirmed as exc:
        raise HTTPException(status_code=502, detail=str(exc)) from exc
    return Response(status_code=204)


@router.get("/{workspace_id}/ports")
async def list_workspace_preview_ports(
    workspace_id: str,
    owner: str = Depends(current_user),
):
    ports = await workspace_preview_ports(workspace_id, owner)
    if ports is None:
        raise HTTPException(status_code=404, detail="Workspace not found")
    return {"ports": ports}
