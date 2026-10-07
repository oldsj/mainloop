"""Resolve PR and merge repository scope from the live binding and stored workspace rows."""

from __future__ import annotations

from mainloop.runtime.policy import PolicyError
from mainloop.services.github_repo import InvalidGithubRepo, parse_github_repo


class ScopeUnavailable(ValueError):
    """The persisted session does not prove the requested project/workspace scope."""


def _repo(value: str):
    try:
        return parse_github_repo(value or "")
    except (InvalidGithubRepo, TypeError):
        raise ScopeUnavailable from None


def _feature_branch(value: str | None) -> bool:
    if not value or value.startswith(("-", "/", "refs/")) or value.endswith(("/", ".")):
        return False
    if any(c.isspace() or ord(c) < 32 or ord(c) == 127 for c in value):
        return False
    if any(c in value for c in "~^:?*[\\"):
        return False
    if any(part in value for part in ("..", "@{", "//")) or value == "@":
        return False
    return all(
        not part.startswith(".") and not part.endswith(".lock")
        for part in value.split("/")
    )


def repository_scope(
    authority: dict,
    *,
    project_id: str,
    branch: str | None = None,
    require_runtime: bool = False,
) -> str:
    """Return the canonical project repository or reject incomplete/mismatched authority.

    Main coordination authority keeps its existing owner-project behavior. Children retain the
    existing exact workspace checks. An enrolled owner workspace additionally requires its
    project, session repository, workspace repository, session branch and workspace branch to
    agree, and a live kagent runtime when the caller is preparing a merge.
    """
    try:
        canonical = _repo(authority["full_name"])
        if (
            _repo(authority["html_url"]).full_name.lower()
            != canonical.full_name.lower()
            or canonical.full_name.lower()
            != f"{authority['owner']}/{authority['name']}".lower()
        ):
            raise ScopeUnavailable

        role = authority["role"]
        grant = authority["mcp_grant_kind"]
        if require_runtime and not authority.get("kagent_session_id"):
            raise ScopeUnavailable
        if role == "main" and grant == "coordination":
            return canonical.full_name

        if (role, grant) not in {
            ("child", "coordination"),
            ("agent", "workspace"),
        }:
            raise ScopeUnavailable
        if authority.get("session_project_id") != project_id:
            raise ScopeUnavailable

        workspace_repo = _repo(authority.get("workspace_repo"))
        session_repo = _repo(authority.get("session_repo"))
        if (
            workspace_repo.full_name.lower() != canonical.full_name.lower()
            or session_repo.full_name.lower() != canonical.full_name.lower()
        ):
            raise ScopeUnavailable

        stored_branch = authority.get("workspace_branch")
        if not stored_branch:
            raise ScopeUnavailable
        if role == "agent":
            if (
                not _feature_branch(stored_branch)
                or authority.get("session_branch") != stored_branch
                or not authority.get("kagent_session_id")
            ):
                raise ScopeUnavailable
        if branch is not None and branch != stored_branch:
            if branch == authority.get("default_branch"):
                raise PolicyError("branch", "default branch is not allowed")
            raise PolicyError("branch", "branch does not match this workspace")

        return canonical.full_name
    except (KeyError, TypeError, InvalidGithubRepo):
        raise ScopeUnavailable from None


async def resolve_project_authority(
    conn,
    binding: dict,
    project_id: str,
    *,
    branch: str | None = None,
    require_runtime: bool = False,
) -> tuple[dict, str] | None:
    """Load and validate one live, owner-owned project binding from PostgreSQL."""
    row = await conn.fetchrow(
        """SELECT p.*,b.role,b.mcp_grant_kind,b.kagent_session_id,
                  s.project_id AS session_project_id,s.repo_url AS session_repo,
                  s.branch_name AS session_branch,s.status AS session_status,
                  s.archived_at,b.kagent_deleted_at,
                  w.repo AS workspace_repo,w.branch AS workspace_branch
           FROM native_bindings b JOIN sessions s ON s.id=b.session_id
           JOIN projects p ON p.user_id=s.user_id AND p.id=$4
           LEFT JOIN workspaces w ON w.session_id=s.id
           WHERE b.session_id=$1 AND s.user_id=$2 AND b.token_hash=$3
             AND b.kagent_deleted_at IS NULL AND s.archived_at IS NULL
             AND s.status NOT IN ('completed','failed','cancelled')
           FOR SHARE OF b,s""",
        binding["session_id"],
        binding["user_id"],
        binding.get("token_hash"),
        project_id,
    )
    if row is None:
        return None
    authority = dict(row)
    repository = repository_scope(
        authority,
        project_id=project_id,
        branch=branch,
        require_runtime=require_runtime,
    )
    return authority, repository


def workspace_identity(binding: dict) -> tuple[str, str]:
    """Resolve an enrolled workspace's display repository and branch from DB-backed fields."""
    project_id = binding.get("session_project_id")
    if not project_id:
        raise ScopeUnavailable
    repository = repository_scope(
        binding,
        project_id=project_id,
        branch=binding.get("workspace_branch"),
    )
    return repository, binding["workspace_branch"]
