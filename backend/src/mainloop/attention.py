"""One owner-attention projection from durable task and native-input state.

Reads never dispatch native turns or grant response authority. HITL keeps its canonical
card and response route. Read/unread and publication activity cannot create attention.
"""

from dataclasses import dataclass
from urllib.parse import quote

from mainloop.db import tasks as store

from models.attention import ActivityItem, AttentionItem, AttentionSnapshot
from models.hitl import HITLProjection
from models.session import Session, SessionStatus

TERMINAL = frozenset({"completed", "failed", "cancelled"})
FINISHED_ATTEMPTS = TERMINAL | {"superseded", "fenced"}


def current_attempt(row):
    return (
        row["id"] == row["current_attempt_id"]
        and row["state"] == "active"
        and row["task_status"] not in TERMINAL
        and not row["snapshot"].get("archived_at")
        and not row["snapshot"].get("superseded_at")
        and not row["snapshot"].get("native_deleted_at")
    )


def live_session(session, attempt):
    return (
        session is not None
        and session["archived_at"] is None
        and session["status"] not in TERMINAL
        and session["kagent_session_id"] is not None
        and session["kagent_deleted_at"] is None
        and (attempt is None or current_attempt(attempt))
    )


def session_status(session, attempt, *, needs_input=False):
    """Task/attempt finality outranks historical session flags and native activity."""
    if attempt:
        if attempt["id"] != attempt["current_attempt_id"]:
            return SessionStatus.SUPERSEDED
        if attempt["task_status"] in TERMINAL:
            return SessionStatus(attempt["task_status"])
        if attempt["state"] in FINISHED_ATTEMPTS or attempt["snapshot"].get(
            "archived_at"
        ):
            return SessionStatus(
                attempt["state"] if attempt["state"] in TERMINAL else "superseded"
            )
    status = SessionStatus(session["status"])
    if (
        status in (SessionStatus.FAILED, SessionStatus.CANCELLED)
        or session["archived_at"]
    ):
        return status
    if needs_input:
        return SessionStatus.WAITING_ON_USER
    if status == SessionStatus.WAITING_ON_USER:
        return SessionStatus.ACTIVE if session["turn_open"] else SessionStatus.IDLE
    return status


@dataclass
class OwnerProjection:
    attention: AttentionSnapshot
    sessions: dict[str, Session]
    listed_sessions: list[Session]
    activity: list[ActivityItem]


async def read(database, owner_id):
    if not database._pool:
        return OwnerProjection(AttentionSnapshot(), {}, [], [])
    # All surfaces consume the same snapshot; no mixtures of pre/post-reassignment state.
    async with database.connection() as conn, conn.transaction(
        isolation="repeatable_read", readonly=True
    ):
        rows = await conn.fetch(
            """SELECT s.*,b.kind AS agent_kind,b.kagent_session_id,b.kagent_deleted_at,
                      b.parent_session_id,b.role,t.name AS topic,
                      EXISTS(SELECT 1 FROM native_deliveries d WHERE d.session_id=s.id
                        AND d.state IN ('recorded','sending','delivered')) AS turn_open
               FROM sessions s LEFT JOIN native_bindings b ON b.session_id=s.id
               LEFT JOIN topics t ON t.id=b.topic_id WHERE s.user_id=$1""",
            owner_id,
        )
        sessions = {row["id"]: dict(row) for row in rows}
        rows = await conn.fetch(
            """SELECT a.*,t.current_attempt_id,t.status AS task_status,t.snapshot->>'title' AS task_title,
                      t.snapshot AS task_snapshot FROM task_attempts a
               JOIN tasks t ON t.id=a.task_id WHERE t.owner_id=$1""",
            owner_id,
        )
        attempts = {}
        for row in rows:
            row = dict(row)
            row["snapshot"] = store.decode(row["snapshot"])
            row["task_snapshot"] = store.decode(row["task_snapshot"])
            attempts[row["binding_id"] or row["session_id"]] = row
        queues = await conn.fetch(
            "SELECT * FROM queue_items WHERE user_id=$1 ORDER BY created_at DESC,id",
            owner_id,
        )
        request_rows = await conn.fetch(
            "SELECT id,snapshot FROM native_hitl_requests WHERE owner_id=$1 AND NOT superseded",
            owner_id,
        )
        observed = await conn.fetch(
            "SELECT gateway,runtime_session_id,snapshot FROM native_observed_sessions WHERE owner_id=$1",
            owner_id,
        )
        bindings = {
            (row["gateway"], row["runtime_session_id"]): store.decode(
                row["snapshot"]
            ).get("binding_id")
            for row in observed
        }
        answered = set(
            await conn.fetchval(
                "SELECT COALESCE(array_agg(leaf_key),ARRAY[]::text[]) FROM native_hitl_response_members WHERE owner_id=$1",
                owner_id,
            )
        )
        reports = await conn.fetch(
            """SELECT r.id,r.task_id,r.attempt_id,r.snapshot,r.created_at,t.snapshot->>'title' AS title
               FROM task_reports r JOIN tasks t ON t.id=r.task_id
               WHERE t.owner_id=$1 ORDER BY r.created_at DESC,r.id""",
            owner_id,
        )

    projections = {
        r["id"]: HITLProjection.model_validate(store.decode(r["snapshot"]))
        for r in request_rows
    }
    items, activity, input_sessions, seen_leaves = [], [], set(), set()
    attention_queue_ids = set()
    for row in queues:
        if row["status"] != "pending":
            continue
        queue = database._row_to_queue_item(row)
        if row["item_type"] == "hitl_request":
            request = projections.get(row["hitl_request_id"])
            if not request or request.availability != "pending":
                continue
            pending = [leaf for leaf in request.leaves if leaf.key() not in answered]
            # No verified current source means no actionable owner request.
            source_sessions = set()
            keys = set()
            for leaf in pending:
                if leaf.owner_id != owner_id:
                    continue
                sid = leaf.binding_id or bindings.get(
                    (leaf.gateway, leaf.runtime_session_id)
                )
                session, attempt = sessions.get(sid), attempts.get(sid)
                if (
                    not live_session(session, attempt)
                    or session["kagent_session_id"] != leaf.runtime_session_id
                ):
                    continue
                source_sessions.add(sid)
                keys.add(leaf.key())
            if not keys or keys <= seen_leaves:
                continue
            seen_leaves.update(keys)
            input_sessions.update(source_sessions)
            approval = request.payload.type == "tool_approval_request"
            needed = (
                "Review and approve or reject the pending tool calls."
                if approval
                else "Answer the session's questions."
            )
            items.append(
                AttentionItem(
                    id=f"hitl:{request.id}",
                    kind="approval" if approval else "question",
                    title=queue.title,
                    needed=needed,
                    href=f"/?inbox={quote(request.id, safe='')}#attention-{quote(request.id, safe='')}",
                    session_ids=sorted(source_sessions),
                    queue_item=queue,
                )
            )
            attention_queue_ids.add(row["id"])
        elif row["item_type"] in ("question", "approval", "review", "plan_review"):
            task = next(
                (a for a in attempts.values() if a["task_id"] == row["task_id"]), None
            )
            if task and not current_attempt(task):
                continue
            items.append(
                AttentionItem(
                    id=f"queue:{row['id']}",
                    kind="question" if row["item_type"] == "question" else "approval",
                    title=queue.title,
                    needed=queue.content,
                    href=f"/?inbox={quote(row['id'], safe='')}#attention-{quote(row['id'], safe='')}",
                    queue_item=queue,
                )
            )
            attention_queue_ids.add(row["id"])
    for attempt in attempts.values():
        task = attempt["task_snapshot"]
        if (
            not current_attempt(attempt)
            or task.get("reason") != "owner_decision"
            or attempt["task_status"] != "blocked"
        ):
            continue
        sid = attempt["binding_id"] or attempt["session_id"]
        session = sessions.get(sid)
        if not session or session["archived_at"] or session["status"] in TERMINAL:
            continue
        report = next(
            (
                store.decode(r["snapshot"])
                for r in reports
                if r["attempt_id"] == attempt["id"]
                and store.decode(r["snapshot"]).get("owner_action")
            ),
            None,
        )
        if report:
            items.append(
                AttentionItem(
                    id=f"task:{attempt['task_id']}",
                    kind="owner_decision",
                    title=attempt["task_title"],
                    needed=report["owner_action"],
                    href=f"/tasks/{quote(attempt['task_id'], safe='')}",
                    task_id=attempt["task_id"],
                    session_ids=[sid],
                )
            )
    owner_sessions = {sid for item in items for sid in item.session_ids}
    projected_sessions = {}
    for sid, row in sessions.items():
        session = database._row_to_session(row)
        projected_sessions[sid] = session.model_copy(
            update={
                "status": session_status(
                    row, attempts.get(sid), needs_input=sid in input_sessions
                ),
                "needs_owner": sid in owner_sessions,
                "parent_session_id": row["parent_session_id"],
                "topic": row["topic"],
            }
        )
    for row in queues:
        if row["id"] in attention_queue_ids:
            continue
        activity.append(
            ActivityItem(
                id=f"queue:{row['id']}",
                title=row["title"],
                content=row["content"],
                href=(
                    f"/tasks/{quote(row['task_id'], safe='')}"
                    if row["task_id"]
                    else None
                ),
                created_at=row["created_at"],
            )
        )
    for row in reports:
        report = store.decode(row["snapshot"])
        activity.append(
            ActivityItem(
                id=f"report:{row['id']}",
                title=row["title"],
                content=f"Provider report ({report['outcome']}; unverified): {report['summary']}",
                href=f"/tasks/{quote(row['task_id'], safe='')}",
                created_at=row["created_at"],
            )
        )
    activity.sort(key=lambda item: item.created_at, reverse=True)
    listed = [
        projected_sessions[sid]
        for sid, row in sessions.items()
        if row["role"] != "main" and row["archived_at"] is None
    ]
    listed.sort(key=lambda session: session.created_at, reverse=True)
    return OwnerProjection(
        AttentionSnapshot(items=items, count=len(items), sessions=listed),
        projected_sessions,
        listed,
        activity[:100],
    )
