"""PostgreSQL lock owner and purpose-separated grants for later lifecycle callers.

Callers retain a dedicated connection for the entire lock context, including any future
upstream dispatch. No transaction is held across network I/O. Always lock project then grant.
"""

import hashlib
import secrets
from contextlib import asynccontextmanager

from mainloop.push_gate.authorization import (
    DELEGATED_WRITER_DEPTHS,
    WORKSPACE_WRITER_PAIRS,
    authorize,
    protected_reason,
)
from mainloop.runtime.policy import PolicyError
from mainloop.services.github_repo import parse_github_repo
from mainloop.services.workspace_authority import (
    ScopeUnavailable,
    _feature_branch,
    repository_scope,
)

from models.push_gate import (
    ProtectedBranchPolicy,
    PublicationAttempt,
    PublicationState,
    PushGrant,
)


def token_hash(token: str) -> str:
    return hashlib.sha256(token.encode()).hexdigest()


@asynccontextmanager
async def publication_lock(conn, grant_id: str):
    """Cross-process serialization; lifecycle revocation must use this same lock."""
    key = f"push-grant:{grant_id}"
    await conn.fetchval("SELECT pg_advisory_lock(hashtextextended($1,0))", key)
    try:
        yield
    finally:
        await conn.fetchval("SELECT pg_advisory_unlock(hashtextextended($1,0))", key)


@asynccontextmanager
async def policy_lock(conn, project_id: str):
    key = f"push-policy:{project_id}"
    await conn.fetchval("SELECT pg_advisory_lock(hashtextextended($1,0))", key)
    try:
        yield
    finally:
        await conn.fetchval("SELECT pg_advisory_unlock(hashtextextended($1,0))", key)


def _decode(value, model):
    return (
        model.model_validate_json(value)
        if isinstance(value, str)
        else model.model_validate(value)
    )


async def load_policy(conn, project_id: str) -> ProtectedBranchPolicy:
    row = await conn.fetchrow(
        "SELECT policy FROM push_branch_policies WHERE project_id=$1", project_id
    )
    if row is None:
        raise ValueError("policy_unavailable")
    policy = _decode(row["policy"], ProtectedBranchPolicy)
    default = await conn.fetchval(
        "SELECT default_branch FROM projects WHERE id=$1", project_id
    )
    if not default:
        raise ValueError("metadata_unavailable")
    if default != policy.default_branch:
        raise ValueError("policy_metadata_stale")
    return policy


async def set_policy(conn, policy: ProtectedBranchPolicy):
    """Trusted owner API primitive. Preserve observed defaults until explicit release.

    Explicit release is intentionally absent from this slice.
    """
    async with policy_lock(conn, policy.project_id):
        default = await conn.fetchval(
            "SELECT default_branch FROM projects WHERE id=$1", policy.project_id
        )
        if not default or default != policy.default_branch:
            raise ValueError("policy_metadata_stale")
        row = await conn.fetchrow(
            "SELECT policy FROM push_branch_policies WHERE project_id=$1",
            policy.project_id,
        )
        if row:
            previous = _decode(row["policy"], ProtectedBranchPolicy)
            if policy.version != previous.version + 1:
                raise ValueError("policy_version")
            policy = policy.model_copy(
                update={
                    "previous_defaults": tuple(
                        sorted(
                            set(
                                (
                                    *previous.previous_defaults,
                                    previous.default_branch,
                                    *policy.previous_defaults,
                                )
                            )
                            - {policy.default_branch, ""}
                        )
                    )
                }
            )
        elif policy.version != 1:
            raise ValueError("policy_version")
        await conn.execute(
            """INSERT INTO push_branch_policies(project_id,version,policy)
            VALUES($1,$2,$3::jsonb) ON CONFLICT(project_id) DO UPDATE
            SET version=EXCLUDED.version,policy=EXCLUDED.policy""",
            policy.project_id,
            policy.version,
            policy.model_dump_json(),
        )


def stored_grant(row) -> PushGrant:
    """Decode a ``push_grants`` row; the proof columns are the authority and must agree
    with the ``grant_data`` copy (row needs ``grant_data,attempt_id,writer_generation``).
    """
    grant = _decode(row["grant_data"], PushGrant)
    if row["attempt_id"] != grant.attempt_id:
        raise ValueError("attempt_not_current")
    if row["writer_generation"] != grant.writer_generation:
        raise ValueError("stale_writer_generation")
    return grant


async def _delegated_writer(
    conn, row, grant: PushGrant, repository: str, *, bind: bool = False
) -> PushGrant:
    """Current-attempt and writer-generation proof for a supervisor/child coding writer.

    One statement gives a consistent view of the attempt, its task and the branch claim.
    Callers hold the grant lock; lifecycle fencing must hold it across its durable change
    so a fence cannot interleave between this read and a dispatched write.
    """
    a = await conn.fetchrow(
        """SELECT a.id,a.state,a.role,a.depth,a.writer_generation,a.binding_id,a.session_id,
            t.owner_id,t.project_id,t.mode,t.status,t.current_attempt_id,
            c.generation AS claim_generation,c.held AS claim_held,
            c.attempt_id AS claim_attempt_id
        FROM task_attempts a JOIN tasks t ON t.id=a.task_id
        LEFT JOIN workspace_writer_claims c ON c.owner_id=t.owner_id
            AND c.repository=$2 AND c.branch=$3
        WHERE a.binding_id=$1 AND a.session_id=$1""",
        grant.session_id,
        repository.lower(),
        grant.branch,
    )
    if a is None:
        raise ValueError("attempt_unavailable")
    if (
        a["owner_id"] != grant.owner_id
        or a["project_id"] != grant.project_id
        or a["mode"] != "code"
        or a["role"] != row["role"]
        or a["depth"] != DELEGATED_WRITER_DEPTHS[row["role"]]
    ):
        raise ValueError("attempt_scope")
    if a["status"] in {"completed", "failed", "cancelled"}:
        raise ValueError("task_terminal")
    if a["current_attempt_id"] != a["id"] or a["state"] != "active":
        raise ValueError("attempt_not_current")
    if not a["claim_held"] or a["claim_attempt_id"] != a["id"]:
        raise ValueError("writer_claim_lost")
    if (
        a["writer_generation"] is None
        or a["writer_generation"] != a["claim_generation"]
    ):
        raise ValueError("stale_writer_generation")
    # The grant must have been issued for exactly this attempt and claim generation, so an
    # older un-revoked bearer cannot survive the same attempt re-taking the claim.
    if bind:
        if grant.attempt_id not in (None, a["id"]):
            raise ValueError("attempt_not_current")
        if grant.writer_generation not in (None, a["claim_generation"]):
            raise ValueError("stale_writer_generation")
        return grant.model_copy(
            update={"attempt_id": a["id"], "writer_generation": a["claim_generation"]}
        )
    if grant.attempt_id != a["id"]:
        raise ValueError("attempt_not_current")
    if grant.writer_generation != a["claim_generation"]:
        raise ValueError("stale_writer_generation")
    return grant


async def live_grant(conn, grant: PushGrant, *, bind: bool = False) -> PushGrant:
    """Resolve live authority; ``bind`` (issue only) records the live attempt/generation."""
    row = await conn.fetchrow(
        """SELECT p.*,b.role,b.mcp_grant_kind,b.kagent_session_id,
            b.token_hash,b.kagent_deleted_at,s.project_id AS session_project_id,
            s.repo_url AS session_repo,s.branch_name AS session_branch,
            s.status AS session_status,s.archived_at,s.user_id,
            w.repo AS workspace_repo,w.branch AS workspace_branch
        FROM native_bindings b JOIN sessions s ON s.id=b.session_id
        JOIN projects p ON p.id=$2 AND p.user_id=s.user_id
        JOIN workspaces w ON w.session_id=s.id WHERE s.id=$1""",
        grant.session_id,
        grant.project_id,
    )
    if (
        not row
        or row["user_id"] != grant.owner_id
        or grant.workspace_id != grant.session_id
    ):
        raise ValueError("binding_unavailable")
    if row["archived_at"] is not None:
        raise ValueError("session_archived")
    if row["session_status"] in {"completed", "failed", "cancelled"}:
        raise ValueError("session_terminal")
    if not row["token_hash"] or row["kagent_deleted_at"] is not None:
        raise ValueError("grant_revoked")
    if (row["role"], row["mcp_grant_kind"]) not in WORKSPACE_WRITER_PAIRS or (
        grant.role,
        grant.grant_kind,
    ) != (row["role"], row["mcp_grant_kind"]):
        raise ValueError("grant_kind")
    if row["kagent_session_id"] != grant.runtime_identity:
        raise ValueError("runtime_mismatch")
    # Delegated writers use the owner-workspace scope checks (feature branch, session and
    # workspace agreement, live runtime); the attempt checks below add delegation proof.
    scope_row = dict(row)
    if row["role"] != "agent":
        scope_row["role"] = "agent"
    try:
        repository = repository_scope(
            scope_row,
            project_id=grant.project_id,
            branch=grant.branch,
            require_runtime=True,
        )
    except (ScopeUnavailable, PolicyError):
        raise ValueError("binding_mismatch") from None
    if repository.lower() != grant.repository.lower():
        raise ValueError("repository_mismatch")
    if row["role"] != "agent":
        return await _delegated_writer(
            conn, row, grant, parse_github_repo(row["full_name"]).full_name, bind=bind
        )
    if grant.attempt_id is not None or grant.writer_generation is not None:
        raise ValueError("attempt_scope")
    return grant


async def issue(conn, grant: PushGrant) -> str:
    """Issue once for a live workspace; returns a push-only bearer, never an MCP bearer."""
    if grant.id != grant.session_id or not _feature_branch(grant.branch):
        raise ValueError("binding_unavailable")
    async with policy_lock(conn, grant.project_id), publication_lock(conn, grant.id):
        grant = await live_grant(conn, grant, bind=True)
        policy = await load_policy(conn, grant.project_id)
        reason = protected_reason(grant.branch, policy)
        if reason:
            raise ValueError(reason)
        if (
            not grant.active
            or grant.archived
            or grant.terminal
            or (grant.role, grant.grant_kind) not in WORKSPACE_WRITER_PAIRS
        ):
            raise ValueError("grant_unavailable")
        previous = await conn.fetchrow(
            "SELECT grant_data,revoked_at FROM push_grants WHERE id=$1", grant.id
        )
        if previous:
            old = _decode(previous["grant_data"], PushGrant)
            if previous["revoked_at"] is None or grant.version != old.version + 1:
                raise ValueError("grant_version")
            if (old.owner_id, old.project_id, old.session_id) != (
                grant.owner_id,
                grant.project_id,
                grant.session_id,
            ):
                raise ValueError("binding_unavailable")
        elif grant.version != 1:
            raise ValueError("grant_version")
        token = "push_" + secrets.token_urlsafe(32)
        await conn.execute(
            """INSERT INTO push_grants(id,token_hash,owner_id,project_id,session_id,grant_data,
                attempt_id,writer_generation)
            VALUES($1,$2,$3,$4,$5,$6::jsonb,$7,$8) ON CONFLICT(id) DO UPDATE
            SET token_hash=EXCLUDED.token_hash,grant_data=EXCLUDED.grant_data,revoked_at=NULL,
                attempt_id=EXCLUDED.attempt_id,writer_generation=EXCLUDED.writer_generation""",
            grant.id,
            token_hash(token),
            grant.owner_id,
            grant.project_id,
            grant.session_id,
            grant.model_dump_json(),
            grant.attempt_id,
            grant.writer_generation,
        )
        return token


async def revoke(conn, grant_id: str):
    async with publication_lock(conn, grant_id):
        await conn.execute(
            "UPDATE push_grants SET revoked_at=COALESCE(revoked_at,now()) WHERE id=$1",
            grant_id,
        )


async def revalidate_on_runtime_replacement(conn, grant_id: str):
    """Revoke the old identity; caller must issue a fresh grant after replacement.

    Hold publication_lock around both revocation and the lifecycle runtime mutation;
    this helper can be called inside that context (PostgreSQL locks are reentrant).
    """
    await revoke(conn, grant_id)


@asynccontextmanager
async def authorized(conn, token: str, repository: str, updates, is_ancestor):
    """Keep permission and future dispatch serialized with revocation/policy updates."""
    row = await conn.fetchrow(
        "SELECT id,project_id FROM push_grants WHERE token_hash=$1", token_hash(token)
    )
    if row is None:
        raise ValueError("grant_unavailable")
    async with policy_lock(conn, row["project_id"]), publication_lock(conn, row["id"]):
        current = await conn.fetchrow(
            """SELECT grant_data,revoked_at,attempt_id,writer_generation
            FROM push_grants WHERE id=$1 AND token_hash=$2""",
            row["id"],
            token_hash(token),
        )
        if current is None or current["revoked_at"] is not None:
            raise ValueError("grant_revoked")
        grant = await live_grant(conn, stored_grant(current))
        policy = await load_policy(conn, grant.project_id)
        reason = authorize(grant, policy, repository, updates, is_ancestor)
        if reason:
            raise ValueError(reason)
        unresolved = await conn.fetchval(
            "SELECT EXISTS(SELECT 1 FROM push_publications WHERE grant_id=$1 AND state IN ('dispatching','unknown'))",
            grant.id,
        )
        if unresolved:
            raise ValueError("publication_unresolved")
        yield grant, policy


async def record_attempt(conn, attempt: PublicationAttempt):
    if attempt.state != PublicationState.PENDING:
        raise ValueError("initial_state")
    async with publication_lock(conn, attempt.grant_id):
        existing = await conn.fetchrow(
            "SELECT attempt FROM push_publications WHERE grant_id=$1 AND request_id=$2",
            attempt.grant_id,
            attempt.request_id,
        )
        if existing:
            if _decode(existing["attempt"], PublicationAttempt) != attempt:
                raise ValueError("request_identity_conflict")
            return
        await conn.execute(
            """INSERT INTO push_publications(grant_id,request_id,attempt,state)
            VALUES($1,$2,$3::jsonb,'pending')""",
            attempt.grant_id,
            attempt.request_id,
            attempt.model_dump_json(),
        )


TRANSITIONS = {
    PublicationState.PENDING: {PublicationState.DISPATCHING, PublicationState.REJECTED},
    PublicationState.DISPATCHING: {
        PublicationState.CONFIRMED,
        PublicationState.REJECTED,
        PublicationState.UNKNOWN,
    },
}


async def transition(conn, grant_id: str, request_id: str, target: PublicationState):
    async with publication_lock(conn, grant_id):
        async with conn.transaction():
            row = await conn.fetchrow(
                "SELECT state FROM push_publications WHERE grant_id=$1 AND request_id=$2 FOR UPDATE",
                grant_id,
                request_id,
            )
            if row is None or target not in TRANSITIONS.get(
                PublicationState(row["state"]), set()
            ):
                raise ValueError("invalid_transition")
            await conn.execute(
                "UPDATE push_publications SET state=$3,updated_at=now() WHERE grant_id=$1 AND request_id=$2",
                grant_id,
                request_id,
                target.value,
            )
