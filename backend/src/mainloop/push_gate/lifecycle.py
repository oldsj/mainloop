"""Disabled-by-default lifecycle integration; no runtime credential publication."""

from contextlib import asynccontextmanager

from mainloop.config import settings
from mainloop.push_gate import store
from mainloop.push_gate.authorization import WORKSPACE_WRITER_PAIRS, protected_reason
from mainloop.services.github_repo import parse_github_repo

from models.push_gate import ProtectedBranchPolicy, PushGrant


@asynccontextmanager
async def locked(conn, session_id: str, *, revoke: bool = False):
    """Keep a dedicated connection and ordered locks through the durable mutation."""
    if not settings.push_gate_enabled:
        yield
        return
    project_id = await conn.fetchval(
        "SELECT project_id FROM sessions WHERE id=$1", session_id
    )
    if project_id is None:
        yield
        return
    async with store.policy_lock(conn, project_id), store.publication_lock(
        conn, session_id
    ):
        if revoke:
            await store.revoke(conn, session_id)
        yield


async def enroll(conn, session_id: str):
    """Issue only after confirmed identity; return bearer solely to a future caller."""
    if not settings.push_gate_enabled:
        return None
    async with locked(conn, session_id):
        row = await conn.fetchrow(
            """SELECT s.user_id,s.project_id,s.branch_name,p.full_name,p.default_branch,
                      n.kagent_session_id,n.role,n.mcp_grant_kind FROM sessions s
               JOIN projects p ON p.id=s.project_id JOIN native_bindings n ON n.session_id=s.id
               JOIN workspaces w ON w.session_id=s.id WHERE s.id=$1""",
            session_id,
        )
        if not row or not row["default_branch"] or not row["kagent_session_id"]:
            return None
        # Coordination grants and non-writer roles never receive push authority. Delegated
        # writers are further proven against their current attempt and writer claim by issue.
        if (row["role"], row["mcp_grant_kind"]) not in WORKSPACE_WRITER_PAIRS:
            return None
        policy_row = await conn.fetchrow(
            "SELECT policy FROM push_branch_policies WHERE project_id=$1",
            row["project_id"],
        )
        if policy_row is None:
            await store.set_policy(
                conn,
                ProtectedBranchPolicy(
                    project_id=row["project_id"],
                    version=1,
                    default_branch=row["default_branch"],
                ),
            )
        try:
            policy = await store.load_policy(conn, row["project_id"])
        except ValueError:
            return None
        if protected_reason(row["branch_name"], policy):
            return None
        previous = await conn.fetchrow(
            "SELECT grant_data,revoked_at FROM push_grants WHERE id=$1", session_id
        )
        if previous and previous["revoked_at"] is None:
            return None
        version = (
            store._decode(previous["grant_data"], PushGrant).version + 1
            if previous
            else 1
        )
        grant = PushGrant(
            id=session_id,
            owner_id=row["user_id"],
            project_id=row["project_id"],
            repository=parse_github_repo(row["full_name"]).full_name,
            branch=row["branch_name"],
            workspace_id=session_id,
            session_id=session_id,
            runtime_identity=row["kagent_session_id"],
            version=version,
            role=row["role"],
            grant_kind=row["mcp_grant_kind"],
        )
        try:
            return await store.issue(conn, grant)
        except ValueError:
            # Archived, terminal, revoked enrollment and scope mismatches confer no authority.
            return None


async def projection(conn, session_id: str) -> tuple[str, str | None]:
    row = await conn.fetchrow(
        """SELECT s.project_id,w.branch,p.default_branch FROM sessions s
           JOIN workspaces w ON w.session_id=s.id LEFT JOIN projects p ON p.id=s.project_id
           WHERE s.id=$1""",
        session_id,
    )
    if not row or not row["default_branch"]:
        return "read_only", "missing_metadata"
    if row["branch"] == row["default_branch"]:
        return "read_only", "default_branch"
    policy = None
    try:
        policy = await store.load_policy(conn, row["project_id"])
    except ValueError as exc:
        if str(exc) != "policy_unavailable":
            return "read_only", "missing_metadata"
    else:
        reason = protected_reason(row["branch"], policy)
        if reason:
            return "read_only", reason
    if not settings.push_gate_enabled:
        return "read_only", "disabled"
    if policy is None:
        return "read_only", "no_grant"
    grant_row = await conn.fetchrow(
        """SELECT grant_data,revoked_at,attempt_id,writer_generation
        FROM push_grants WHERE id=$1""",
        session_id,
    )
    if grant_row and grant_row["revoked_at"] is None:
        try:
            await store.live_grant(conn, store.stored_grant(grant_row))
        except ValueError:
            pass
        else:
            return "branch", None
    return "read_only", "no_grant"
