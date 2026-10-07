"""Resolve PR and merge repository scope from the live binding and stored workspace rows."""

from __future__ import annotations

from mainloop.runtime.agent_credentials import DELEGATED_PAIRS
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


# Each delegated role is bound to exactly one hierarchy depth.
_DELEGATED_DEPTH = {"supervisor": 1, "child": 2}


def _delegated_attempt(authority: dict, role: str, stored_branch: str) -> None:
    """Verify the current task attempt and branch claim behind a delegated workspace grant.

    The binding is only as good as the durable attempt that owns it: the attempt must be the
    task's current one, active, of this role and depth, and the branch claim must still be held
    by that attempt at the writer generation the attempt was admitted with. Ancestry is read from
    the persisted task rows, so a missing or forged parent denies.
    """
    try:
        attempt_id = authority["attempt_id"]
        if (
            not attempt_id
            or authority["attempt_state"] != "active"
            or authority["attempt_binding_id"] != authority["session_id"]
            or authority["attempt_workspace_id"] != authority["session_id"]
            or authority["task_current_attempt_id"] != attempt_id
            or authority["attempt_role"] != role
            or authority["attempt_depth"] != _DELEGATED_DEPTH[role]
            or authority["task_mode"] != "code"
            or authority["task_status"] in ("completed", "failed", "cancelled")
            or authority["task_owner_id"] != authority["user_id"]
            or authority["task_project_id"] != authority["session_project_id"]
            or authority["attempt_writer_generation"] is None
            or authority["claim_attempt_id"] != attempt_id
            or authority["claim_generation"] != authority["attempt_writer_generation"]
            or not authority["claim_held"]
            or authority["claim_branch"] != stored_branch
        ):
            raise ScopeUnavailable
        if role == "supervisor":
            if (
                authority["task_parent_id"] is not None
                or authority["task_root_id"] != authority["task_id"]
            ):
                raise ScopeUnavailable
        else:
            # A child's parent task must be a root task whose current attempt is an active
            # supervisor of the same owner/project. Grandchildren cannot exist: the parent of a
            # child is never itself a child.
            if (
                authority["task_parent_id"] is None
                or authority["parent_owner_id"] != authority["task_owner_id"]
                or authority["parent_project_id"] != authority["task_project_id"]
                or authority["parent_parent_id"] is not None
                or authority["parent_root_id"] != authority["task_root_id"]
                or authority["parent_attempt_state"] != "active"
                or authority["parent_attempt_role"] != "supervisor"
                or authority["parent_attempt_id"] != authority["parent_current_id"]
            ):
                raise ScopeUnavailable
    except KeyError:
        raise ScopeUnavailable from None


def repository_scope(
    authority: dict,
    *,
    project_id: str,
    branch: str | None = None,
    require_runtime: bool = False,
) -> str:
    """Return the canonical project repository or reject incomplete/mismatched authority.

    Main coordination authority keeps its existing owner-project behavior. Session children
    retain the existing exact workspace checks. An enrolled owner workspace additionally
    requires its project, session repository, workspace repository, session branch and
    workspace branch to agree, and a live kagent runtime when the caller is preparing a merge.
    Delegated supervisor and child workspaces require the same agreement plus their current task
    attempt, writer generation and ancestry (``_delegated_attempt``).
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

        delegated = (role, grant) in DELEGATED_PAIRS
        if (role, grant) not in {
            ("child", "coordination"),
            ("agent", "workspace"),
        } | DELEGATED_PAIRS:
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
        if role == "agent" or delegated:
            if (
                not _feature_branch(stored_branch)
                or authority.get("session_branch") != stored_branch
                or not authority.get("kagent_session_id")
            ):
                raise ScopeUnavailable
        if delegated:
            _delegated_attempt(authority, role, stored_branch)
        if branch is not None and branch != stored_branch:
            if branch == authority.get("default_branch"):
                raise PolicyError("branch", "default branch is not allowed")
            raise PolicyError("branch", "branch does not match this workspace")

        return canonical.full_name
    except (KeyError, TypeError, InvalidGithubRepo):
        raise ScopeUnavailable from None


async def delegated_facts(conn, session_id: str, *, lock: bool = False) -> dict:
    """Read the attempt, task, parent and branch-claim facts behind a delegated binding.

    Empty when the session has no attempt. ``lock`` takes share locks on the attempt and claim
    rows, so a caller inside a transaction cannot race a fence or release that commits after the
    read. The facts come from one statement, so one snapshot decides.
    """
    query = """SELECT a.id AS attempt_id,a.state AS attempt_state,a.role AS attempt_role,
                  a.depth AS attempt_depth,a.binding_id AS attempt_binding_id,
                  a.workspace_id AS attempt_workspace_id,
                  a.writer_generation AS attempt_writer_generation,
                  t.owner_id AS task_owner_id,t.project_id AS task_project_id,
                  t.mode AS task_mode,t.parent_task_id AS task_parent_id,
                  t.id AS task_id,t.status AS task_status,
                  t.root_task_id AS task_root_id,
                  t.current_attempt_id AS task_current_attempt_id,
                  pt.owner_id AS parent_owner_id,pt.project_id AS parent_project_id,
                  pt.parent_task_id AS parent_parent_id,pt.root_task_id AS parent_root_id,
                  pt.current_attempt_id AS parent_current_id,
                  pa.id AS parent_attempt_id,pa.state AS parent_attempt_state,
                  pa.role AS parent_attempt_role,
                  c.generation AS claim_generation,c.held AS claim_held,
                  c.attempt_id AS claim_attempt_id,c.branch AS claim_branch
           FROM task_attempts a JOIN tasks t ON t.id=a.task_id
           LEFT JOIN tasks pt ON pt.id=t.parent_task_id
           LEFT JOIN task_attempts pa ON pa.id=pt.current_attempt_id
           LEFT JOIN workspace_writer_claims c ON c.attempt_id=a.id
           WHERE a.binding_id=$1"""
    if lock:
        query += " FOR SHARE OF a,t"
    row = await conn.fetchrow(query, session_id)
    return dict(row) if row else {}


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
        """SELECT p.*,b.session_id,b.role,b.mcp_grant_kind,b.kagent_session_id,
                  s.user_id,s.project_id AS session_project_id,s.repo_url AS session_repo,
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
    if (authority["role"], authority["mcp_grant_kind"]) in DELEGATED_PAIRS:
        from mainloop.tasks import lifecycle

        try:
            await lifecycle.authenticate_binding(conn, binding)
        except (lifecycle.LifecycleDenied, ValueError) as exc:
            raise ScopeUnavailable from exc
        authority.update(await delegated_facts(conn, binding["session_id"], lock=True))
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
