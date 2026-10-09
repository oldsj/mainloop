"""Central lifecycle guards and settlement for task-attempt native sessions.

A session that belongs to a task attempt (a delegated supervisor or child) is only as live as its
attempt. Every path that submits a turn, recreates or resumes a runtime, serves a preview,
archives or deletes rows, or ends the session consults ``check`` here, so no caller keeps its own
copy of the rule. Sessions that no attempt owns (the main thread, owner workspaces, session
children) retain their ordinary routing, but terminal, archived, deleted or revoked
bindings cannot admit new work.

Settlement is the only way an attempt leaves ``creating``/``active``/``draining``: the runtime is
confirmed gone first, the attempt is ``fenced`` with that evidence, the branch claim is released
by compare-and-swap on its generation, and only then does the attempt reach its final state and
give back its capacity slot. Everything here takes the caller's connection and never opens a
second one inside a lock.
"""

from __future__ import annotations

from contextlib import asynccontextmanager
from datetime import UTC, datetime

from mainloop.db import tasks as store

from models.task import Task, TaskAttempt

LIVE = ("creating", "active")
FINAL = ("superseded", "failed", "cancelled", "completed")

# What each guarded action needs the attempt to be.
ACTIONS: dict[str, tuple[str, ...]] = {
    # (Re)create or reconcile the runtime; the stable create request id makes a retry safe.
    "create": LIVE,
    # A new turn: owner message, brief, report or queued delivery.
    "submit": ("active",),
    "resume": ("active",),
    "preview": ("active",),
    # Cancel, complete, fail: only something still holding capacity can be ended.
    "terminal": ("creating", "active", "draining"),
    # Delete the kagent Session: after the attempt stopped taking work.
    "runtime_delete": ("draining", "fenced") + FINAL,
    # Remove or archive the rows: only after settlement.
    # Superseded history is managed by S3 retention, never the immediate-delete archive path.
    "archive": ("failed", "cancelled", "completed"),
    "delete": ("failed", "cancelled", "completed"),
}
# Actions that run work for the attempt, so its claim must still be the one it was admitted with.
_CLAIM_ACTIONS = ("create", "submit", "resume", "preview")


class LifecycleDenied(Exception):
    def __init__(self, code: str):
        super().__init__(code)
        self.code = code


async def facts(conn, session_id: str, *, lock: bool = False) -> dict | None:
    """Read the owning attempt and claim, or return None for an ordinary session.

    ``lock`` takes a share lock on the attempt row, so a submission recorded in the caller's
    transaction cannot interleave with a state change (which needs the row exclusively).
    """
    query = """SELECT a.id,a.state,a.writer_generation,a.role,t.mode,t.id AS task_id,
                  t.current_attempt_id,t.status AS task_status,c.held AS claim_held,c.generation AS claim_generation,
                  a.snapshot->'evidence_refs' ? 'kagent-create:first-dispatch-rejected-before-reservation' AS create_rejected,
                  (a.snapshot->>'brief_delivery_id' IS NULL AND EXISTS(
                    SELECT 1 FROM task_operations o WHERE o.snapshot->>'target_attempt_id'=a.id
                    AND o.kind IN ('retry','reassign') AND o.state<>'completed'
                  )) AS handoff_admission_pending
           FROM task_attempts a JOIN tasks t ON t.id=a.task_id
           LEFT JOIN workspace_writer_claims c ON c.attempt_id=a.id
           WHERE a.binding_id=$1"""
    if lock:
        query += " FOR SHARE OF a,t"
    row = await conn.fetchrow(query, session_id)
    return dict(row) if row else None


def denial(row: dict, action: str) -> str | None:
    if action == "create" and row.get("create_rejected"):
        return "create_rejected"
    if row["state"] not in ACTIONS[action]:
        return f"attempt_{row['state']}"
    if action in _CLAIM_ACTIONS:
        if row["state"] == "active" and row.get("handoff_admission_pending"):
            return "handoff_admission_pending"
        if row["task_status"] in ("completed", "failed", "cancelled"):
            return "task_terminal"
        if row["current_attempt_id"] != row["id"]:
            return "attempt_not_current"
        if row["mode"] == "code" and not (
            row["claim_held"] and row["claim_generation"] == row["writer_generation"]
        ):
            return "stale_writer_generation"
    return None


async def check(conn, session_id: str, action: str, *, lock: bool = False) -> None:
    row = await facts(conn, session_id, lock=lock)
    if row is None:
        binding = await conn.fetchrow(
            """SELECT b.role,b.mcp_grant_kind,b.token_hash,b.kagent_deleted_at,
                      s.status,s.archived_at
               FROM native_bindings b JOIN sessions s ON s.id=b.session_id
               WHERE b.session_id=$1""",
            session_id,
        )
        if binding and (
            binding["role"] == "supervisor"
            or (binding["role"] == "child" and binding["mcp_grant_kind"] == "workspace")
        ):
            raise LifecycleDenied("attempt_missing")
        if binding and action in _CLAIM_ACTIONS:
            if binding["status"] in FINAL or binding["archived_at"]:
                raise LifecycleDenied("session_terminal")
            if binding["kagent_deleted_at"] or (
                binding["mcp_grant_kind"] in ("workspace", "coordination")
                and not binding["token_hash"]
            ):
                raise LifecycleDenied("binding_revoked")
        return
    code = denial(row, action)
    if code:
        raise LifecycleDenied(code)
    if action in _CLAIM_ACTIONS and row["state"] == "active":
        binding = await conn.fetchrow(
            "SELECT * FROM native_bindings WHERE session_id=$1", session_id
        )
        await authenticate_binding(conn, dict(binding))


@asynccontextmanager
async def authority_locked(conn, session_id: str):
    """Serialize a task tree's admissions with ancestor authority mutations.

    A single root key deliberately serializes siblings too. This avoids taking a child
    lock before discovering its ancestor. Root routing is immutable for an attempt.
    Order: project policy, tree authority, publication, runtime, admission, rows.
    Ordinary bindings use their own identity as the authority key.
    """
    root = await conn.fetchval(
        """SELECT t.root_task_id FROM task_attempts a JOIN tasks t ON t.id=a.task_id
           WHERE a.binding_id=$1""",
        session_id,
    )
    key = f"mainloop:task-authority:{root or session_id}"
    await conn.execute("SELECT pg_advisory_lock(hashtextextended($1,0))", key)
    try:
        yield
    finally:
        await conn.execute("SELECT pg_advisory_unlock(hashtextextended($1,0))", key)


@asynccontextmanager
async def locked(conn, session_id: str):
    """Order external runtime admission against draining in every backend process.

    Publication locks, when needed, must precede this lock. Never take this lock
    while holding task admission. It remains held across external dispatch.
    """
    key = f"mainloop:task-runtime:{session_id}"
    async with authority_locked(conn, session_id):
        await conn.execute("SELECT pg_advisory_lock(hashtextextended($1,0))", key)
        try:
            yield
        finally:
            await conn.execute("SELECT pg_advisory_unlock(hashtextextended($1,0))", key)


@asynccontextmanager
async def guard(session_id: str, action: str, *, conn=None):
    """Guard an external action and yield its committed, lock-owning connection."""
    from mainloop.db import db
    from mainloop.push_gate import lifecycle as push_lifecycle

    if conn is None:
        async with db.connection() as owned:
            async with guard(session_id, action, conn=owned):
                yield owned
        return
    if conn.is_in_transaction():
        raise RuntimeError("runtime guard requires committed connection")
    async with push_lifecycle.locked(conn, session_id), locked(conn, session_id):
        await check(conn, session_id, action)
        yield conn


async def authenticate_binding(
    conn, binding: dict, *, allow_creating=False, allow_completed_read=False
):
    """Resolve current scope and ancestry, optionally reading a completed leaf.

    The read exception permits only this leaf's completed product state. Its
    attempt/claim must still be active, and ancestors retain every normal gate.
    Execution callers never enable this exception.
    """
    from mainloop.tasks.principal import TaskPrincipal

    row = await conn.fetchrow(
        """SELECT a.*,t.owner_id,t.project_id,t.root_task_id,t.parent_task_id,t.mode,
                  t.status AS task_status,t.current_attempt_id,
                  s.user_id,s.project_id AS session_project_id,b.role AS binding_role,
                  b.mcp_grant_kind,b.token_hash,b.kagent_deleted_at,s.archived_at,
                  c.held,c.generation,c.owner_id AS claim_owner,c.repository,c.branch,
                  w.repo,w.branch AS workspace_branch,
                  pt.owner_id AS parent_owner,pt.project_id AS parent_project,
                  pt.root_task_id AS parent_root,pt.parent_task_id AS grandparent,
                  pa.state AS parent_state,pa.role AS parent_role,pa.depth AS parent_depth,
                  pt.status AS parent_status,pa.task_id AS parent_attempt_task,
                  pa.session_id AS parent_session,pa.workspace_id AS parent_workspace,
                  pa.binding_id AS parent_binding,pb.role AS parent_binding_role,
                  pb.mcp_grant_kind AS parent_grant,ps.user_id AS parent_user,
                  ps.project_id AS parent_session_project,
                  pb.token_hash AS parent_token,pb.kagent_deleted_at AS parent_deleted,
                  ps.archived_at AS parent_archived,
                  pc.held AS parent_held,pc.generation AS parent_generation,
                  pa.writer_generation AS parent_writer_generation,pt.mode AS parent_mode
           FROM task_attempts a JOIN tasks t ON t.id=a.task_id
           JOIN native_bindings b ON b.session_id=a.binding_id
           JOIN sessions s ON s.id=b.session_id
           LEFT JOIN workspaces w ON w.session_id=s.id
           LEFT JOIN workspace_writer_claims c ON c.attempt_id=a.id
           LEFT JOIN tasks pt ON pt.id=t.parent_task_id
           LEFT JOIN task_attempts pa ON pa.id=pt.current_attempt_id
           LEFT JOIN native_bindings pb ON pb.session_id=pa.binding_id
           LEFT JOIN sessions ps ON ps.id=pb.session_id
           LEFT JOIN workspace_writer_claims pc ON pc.attempt_id=pa.id
           WHERE a.binding_id=$1""",
        binding["session_id"],
    )
    if row is None:
        raise LifecycleDenied("attempt_missing")
    depth = {"supervisor": 1, "child": 2}.get(row["role"])
    if (
        depth is None
        or row["depth"] != depth
        or row["role"] != binding["role"]
        or row["binding_role"] != row["role"]
        or row["state"]
        not in (("creating", "active") if allow_creating else ("active",))
        or row["current_attempt_id"] != row["id"]
        or row["session_id"] != row["binding_id"]
        or row["workspace_id"] != row["binding_id"]
        or row["owner_id"] != row["user_id"]
        or row["project_id"] != row["session_project_id"]
        or (
            row["task_status"] in ("completed", "failed", "cancelled")
            and not (allow_completed_read and row["task_status"] == "completed")
        )
        or row["mcp_grant_kind"]
        != ("workspace" if row["mode"] == "code" else "coordination")
        or row["token_hash"] != binding.get("token_hash")
        or not row["token_hash"]
        or row["kagent_deleted_at"]
        or row["archived_at"]
    ):
        raise LifecycleDenied("attempt_scope")
    if depth == 1:
        valid = row["parent_task_id"] is None and row["root_task_id"] == row["task_id"]
    else:
        valid = (
            row["parent_task_id"] is not None
            and row["grandparent"] is None
            and row["parent_root"] == row["root_task_id"] == row["parent_task_id"]
            and row["parent_owner"] == row["owner_id"]
            and row["parent_project"] == row["project_id"]
            and row["parent_state"] == "active"
            and row["parent_status"] not in ("completed", "failed", "cancelled")
            and row["parent_attempt_task"] == row["parent_task_id"]
            and row["parent_session"]
            == row["parent_workspace"]
            == row["parent_binding"]
            and row["parent_binding_role"] == "supervisor"
            and row["parent_grant"]
            == ("workspace" if row["parent_mode"] == "code" else "coordination")
            and row["parent_user"] == row["owner_id"]
            and row["parent_session_project"] == row["project_id"]
            and row["parent_role"] == "supervisor"
            and row["parent_depth"] == 1
            and row["parent_token"]
            and not row["parent_deleted"]
            and not row["parent_archived"]
            and (
                row["parent_mode"] != "code"
                or (
                    row["parent_held"]
                    and row["parent_generation"] == row["parent_writer_generation"]
                )
            )
        )
    if not valid:
        raise LifecycleDenied("attempt_ancestry")
    if depth == 2:
        parent = await conn.fetchrow(
            "SELECT * FROM native_bindings WHERE session_id=$1", row["parent_binding"]
        )
        # Validate the parent's complete persisted scope too, including its checkout
        # claim and self-rooted task. Never propagate the leaf's read exception.
        await authenticate_binding(conn, dict(parent))
    if row["mode"] == "code":
        from mainloop.services.github_repo import parse_github_repo

        if not (
            row["held"]
            and row["writer_generation"] is not None
            and row["generation"] == row["writer_generation"]
            and row["claim_owner"] == row["owner_id"]
            and row["branch"] == row["workspace_branch"]
            and row["repo"]
            and parse_github_repo(row["repo"]).full_name.lower() == row["repository"]
        ):
            raise LifecycleDenied("stale_writer_generation")
    return TaskPrincipal(
        row["owner_id"],
        binding_id=row["binding_id"],
        role=row["role"],
        task_id=row["task_id"],
        attempt_id=row["id"],
        project_id=row["project_id"],
        root_task_id=row["root_task_id"],
        depth=depth,
    )


async def authenticate_session(binding: dict):
    from mainloop.db import db

    async with db.connection() as conn:
        attempt = await facts(conn, binding["session_id"])
        if attempt is None and (binding["role"], binding["mcp_grant_kind"]) == (
            "child",
            "coordination",
        ):
            return None  # Session delegation remains until S2 replaces it.
        principal = await authenticate_binding(conn, binding)
        if binding["mcp_grant_kind"] == "workspace":
            from mainloop.services.workspace_authority import delegated_facts

            binding.update(await delegated_facts(conn, binding["session_id"]))
        binding.update(
            task_id=principal.task_id,
            attempt_id=principal.attempt_id,
            root_task_id=principal.root_task_id,
            depth=principal.depth,
        )
        return principal


async def check_session(session_id: str, action: str, *, conn=None) -> None:
    """``check`` for callers outside a transaction."""
    from mainloop.db import db

    if conn is None:
        async with db.connection() as owned:
            return await check_session(session_id, action, conn=owned)
    await check(conn, session_id, action)


async def permitted(session_id: str, action: str, *, conn=None) -> bool:
    try:
        await check_session(session_id, action, conn=conn)
    except LifecycleDenied:
        return False
    return True


# --------------------------------------------------------------------------------------------
# Attempt state
# --------------------------------------------------------------------------------------------

CREATE_DISPATCH = "kagent-create:dispatch-may-have-started"
CREATE_REJECTED = "kagent-create:first-dispatch-rejected-before-reservation"


async def create_dispatch(session_id: str, *, conn=None) -> bool:
    """Persist conservative dispatch history using the existing attempt audit refs."""
    from mainloop.db import db

    if conn is None:
        async with db.connection() as owned:
            return await create_dispatch(session_id, conn=owned)
    async with conn.transaction():
        row = await conn.fetchrow(
            "SELECT id FROM task_attempts WHERE binding_id=$1 FOR UPDATE", session_id
        )
        if row is None:
            return False
        attempt = await load_attempt(conn, row["id"])
        if CREATE_REJECTED in attempt.evidence_refs:
            raise LifecycleDenied("create_rejected")
        if CREATE_DISPATCH in attempt.evidence_refs:
            return False
        await save_attempt(
            conn,
            attempt.model_copy(
                update={"evidence_refs": (*attempt.evidence_refs, CREATE_DISPATCH)}
            ),
        )
        return True


async def create_rejected(session_id: str, *, conn=None) -> None:
    """Commit absence evidence and close admission together, before returning to the caller.

    The caller holds the runtime guard across dispatch and this write. A crash after
    commit leaves a draining attempt, never an identity eligible for another create.
    """
    from mainloop.db import db

    if conn is None:
        async with db.connection() as owned:
            return await create_rejected(session_id, conn=owned)
    async with conn.transaction():
        row = await conn.fetchrow(
            "SELECT id FROM task_attempts WHERE binding_id=$1 FOR UPDATE", session_id
        )
        attempt = await load_attempt(conn, row["id"])
        await save_attempt(
            conn,
            attempt.model_copy(
                update={
                    "state": "draining",
                    "evidence_refs": (*attempt.evidence_refs, CREATE_REJECTED),
                }
            ),
        )


async def load_attempt(
    conn, attempt_id: str, *, lock: bool = False
) -> TaskAttempt | None:
    row = await conn.fetchrow(
        "SELECT task_id FROM task_attempts WHERE id=$1", attempt_id
    )
    if row is None:
        return None
    if lock:
        await conn.fetchrow(
            "SELECT id FROM task_attempts WHERE id=$1 FOR UPDATE", attempt_id
        )
    return next(
        (a for a in await store.attempts(conn, row["task_id"]) if a.id == attempt_id),
        None,
    )


async def load_task(conn, task_id: str, *, lock: bool = False) -> Task:
    row = await conn.fetchrow(
        (
            "SELECT snapshot FROM tasks WHERE id=$1 FOR UPDATE"
            if lock
            else "SELECT snapshot FROM tasks WHERE id=$1"
        ),
        task_id,
    )
    return Task.model_validate(store.decode(row["snapshot"]))


async def save_attempt(conn, attempt: TaskAttempt) -> TaskAttempt:
    """Persist state/identity/evidence. Routing columns never change here (a trigger enforces it)."""
    store.require_transaction(conn)
    attempt = attempt.model_copy(update={"updated_at": datetime.now(UTC)})
    await conn.execute(
        """UPDATE task_attempts SET state=$2,session_id=$3,binding_id=$4,workspace_id=$5,
               writer_generation=$6,capacity_held=$7,snapshot=$8::jsonb,updated_at=NOW()
           WHERE id=$1""",
        attempt.id,
        attempt.state,
        attempt.session_id,
        attempt.binding_id,
        attempt.workspace_id,
        attempt.writer_generation,
        attempt.state in ("creating", "active", "draining"),
        attempt.model_dump_json(),
    )
    return attempt


async def transition(
    conn,
    attempt_id: str,
    to_state: str,
    *,
    from_states: tuple[str, ...],
    evidence: str | None = None,
) -> TaskAttempt | None:
    """Compare-and-swap the attempt's state. None when it was not in ``from_states``."""
    store.require_transaction(conn)
    attempt = await load_attempt(conn, attempt_id, lock=True)
    if attempt is None or attempt.state not in from_states:
        return None
    refs = attempt.evidence_refs
    if evidence and evidence not in refs:
        refs = (*refs, evidence)
    return await save_attempt(
        conn, attempt.model_copy(update={"state": to_state, "evidence_refs": refs})
    )


async def settle(
    conn, attempt_id: str, final: str, *, evidence: str
) -> TaskAttempt | None:
    """Fence a drained attempt, release its claim and finish it. One transaction.

    ``evidence`` is confirmed deletion, a deleted-request tombstone, or the durable
    proven first-dispatch no-start rejection. A later control refusal is insufficient.
    A claim is
    released only by compare-and-swap on the generation the attempt was admitted with, so a
    stale caller cannot free a branch that was transferred meanwhile. Returns None when the
    attempt was not draining or fenced (another pass already settled it).
    """
    if final not in FINAL:
        raise ValueError("settlement needs a final state")
    store.require_transaction(conn)
    await store.admission_lock(conn)
    attempt = await load_attempt(conn, attempt_id)
    if attempt is None:
        return None
    task = await load_task(conn, attempt.task_id, lock=True)
    attempt = await load_attempt(conn, attempt_id, lock=True)
    if attempt is None or attempt.state not in ("draining", "fenced"):
        return None
    from mainloop.push_gate import store as push_store

    if task.mode == "code" and await push_store.unresolved_for_branch(
        conn,
        task.owner_id,
        (
            await conn.fetchval(
                "SELECT full_name FROM projects WHERE id=$1", task.project_id
            )
        ),
        task.checkout.branch,
    ):
        return None
    attempt = await save_attempt(
        conn,
        attempt.model_copy(
            update={
                "state": "fenced",
                "evidence_refs": (
                    (*attempt.evidence_refs, evidence)
                    if evidence not in attempt.evidence_refs
                    else attempt.evidence_refs
                ),
            }
        ),
    )
    claim = await conn.fetchrow(
        "SELECT owner_id,repository,branch,generation FROM workspace_writer_claims WHERE attempt_id=$1 AND held",
        attempt.id,
    )
    if claim is not None:
        await store.release_writer(
            conn,
            owner_id=claim["owner_id"],
            repository=claim["repository"],
            branch=claim["branch"],
            generation=attempt.writer_generation,
            attempt_id=attempt.id,
            fence_evidence_ref=evidence,
        )
    attempt = await save_attempt(conn, attempt.model_copy(update={"state": final}))
    if task.current_attempt_id == attempt.id:
        updated = task.model_copy(
            update={
                "status": final if final != "superseded" else task.status,
                "reason": None,
                "current_attempt_id": None,
                "version": task.version + 1,
                "updated_at": datetime.now(UTC),
            }
        )
        await store.save_task(
            conn, updated, task.version, f"attempt:{attempt.id}:{final}"
        )
    return attempt


async def drain_handoff(conn, attempt: TaskAttempt):
    """Publication/runtime locks precede this helper; revoke before state mutation."""
    from mainloop.runtime.agent_credentials import revoke_deferred

    store.require_transaction(conn)
    if attempt.state != "failed":
        await revoke_deferred(conn, attempt.binding_id)
    elif CREATE_REJECTED not in attempt.evidence_refs:
        raise store.TaskError(409, "failed_source_not_no_start")
    await store.admission_lock(conn)
    await load_task(conn, attempt.task_id, lock=True)
    current = await load_attempt(conn, attempt.id, lock=True)
    if current is None or current.state not in (
        "active",
        "creating",
        "draining",
        "failed",
    ):
        raise store.TaskError(409, "source_state_changed")
    if (current.binding_id, current.writer_generation) != (
        attempt.binding_id,
        attempt.writer_generation,
    ):
        raise store.TaskError(409, "stale_writer_generation")
    if current.state != "failed":
        await save_attempt(conn, current.model_copy(update={"state": "draining"}))


async def block_failed_delivery(conn, attempt_id: str, message_id: str):
    """Project an authoritative FAILED first/sole brief under authority/runtime locks.

    Reuse the source drain to revoke authority before task/admission row locks. Failure
    of a turn proves neither runtime termination nor that a writer never edited files.
    """
    from mainloop.runtime.native_sessions import safe_detail

    store.require_transaction(conn)
    attempt = await load_attempt(conn, attempt_id)
    if attempt is None or attempt.state != "active":
        return
    if attempt.brief_delivery_id != message_id:
        return
    task = await load_task(conn, attempt.task_id)
    if task.current_attempt_id != attempt.id or task.status in FINAL:
        return
    delivery = await conn.fetchrow(
        """SELECT d.detail FROM native_deliveries d
           WHERE d.message_id=$1 AND d.session_id=$2 AND d.state='failed'
             AND d.source='brief' AND d.task_id IS NOT NULL
             AND d.evidence_ref='a2a:task/' || d.task_id || '#failed'
             AND NOT EXISTS (SELECT 1 FROM native_deliveries other
               WHERE other.session_id=d.session_id AND other.message_id<>d.message_id)""",
        message_id,
        attempt.binding_id,
    )
    if delivery is None:
        return
    # record_submission takes the same tree authority lock before its transaction.
    # No second delivery can appear between this sole-brief check and revocation.
    await drain_handoff(conn, attempt)
    task = await load_task(conn, attempt.task_id, lock=True)
    if task.current_attempt_id != attempt.id or task.status in FINAL:
        return  # A concurrent terminal outcome wins; never reopen it.
    detail = (
        safe_detail(delivery["detail"]) or "native delivery failed without a reason"
    )
    await transition(
        conn,
        attempt.id,
        "draining",
        from_states=("draining",),
        evidence=f"native-delivery-failed:{message_id}: {detail}",
    )
    await store.save_task(
        conn,
        task.model_copy(
            update={
                "status": "blocked",
                "reason": "reconciliation",
                "version": task.version + 1,
                "updated_at": datetime.now(UTC),
            }
        ),
        task.version,
        f"attempt:{attempt.id}:delivery-failed:{message_id}",
    )


async def supersede_handoff(conn, attempt: TaskAttempt, evidence: str):
    """Evidence must be validated by the configured S3 adapter before this call."""
    store.require_transaction(conn)
    await store.admission_lock(conn)
    await load_task(conn, attempt.task_id, lock=True)
    current = await load_attempt(conn, attempt.id, lock=True)
    if current is None:
        raise store.TaskError(409, "source_missing")
    if (current.binding_id, current.writer_generation) != (
        attempt.binding_id,
        attempt.writer_generation,
    ):
        raise store.TaskError(409, "stale_writer_generation")
    if current.state == "failed":
        # Failed no-start has already relinquished its claim through S1 settlement.
        if CREATE_REJECTED not in current.evidence_refs:
            raise store.TaskError(409, "failed_source_not_no_start")
        current = await save_attempt(
            conn, current.model_copy(update={"state": "superseded"})
        )
    else:
        current = await settle(conn, attempt.id, "superseded", evidence=evidence)
    if current is None:
        raise store.TaskError(409, "source_state_changed")
    return await save_attempt(
        conn, current.model_copy(update={"superseded_at": datetime.now(UTC)})
    )
