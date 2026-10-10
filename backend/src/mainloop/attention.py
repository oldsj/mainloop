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
HISTORY_LIMIT = 100
# Shared SQL eligibility avoids loading historical attempts/requests into Python.
_CURRENT_ATTEMPT = """
    a.id=t.current_attempt_id AND a.state='active'
    AND t.status NOT IN ('completed','failed','cancelled')
    AND a.snapshot->>'archived_at' IS NULL
    AND a.snapshot->>'superseded_at' IS NULL
    AND a.snapshot->>'native_deleted_at' IS NULL
"""
_SESSION_ATTEMPT_JOIN = """
    LEFT JOIN task_attempts a ON COALESCE(a.binding_id,a.session_id)=s.id
    LEFT JOIN tasks t ON t.id=a.task_id
"""


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
    if status in (SessionStatus.FAILED, SessionStatus.CANCELLED):
        return status
    if session["archived_at"]:
        return SessionStatus.IDLE if status == SessionStatus.WAITING_ON_USER else status
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


async def read(
    database, owner_id, *, session_id=None, project_id=None, include_activity=True
):
    if not database._pool:
        return OwnerProjection(AttentionSnapshot(), {}, [], [])
    # Pending attention is complete, never truncated by the independent history cap.
    # All surfaces consume one transaction snapshot, including reassignment/finality.
    async with database.connection() as conn, conn.transaction(
        isolation="repeatable_read", readonly=True
    ):
        queues = await conn.fetch(
            f"""SELECT q.*,r.snapshot AS request_snapshot FROM queue_items q
                LEFT JOIN native_hitl_requests r ON r.id=q.hitl_request_id AND r.owner_id=$1
                LEFT JOIN tasks t ON t.id=q.task_id AND t.owner_id=$1
                LEFT JOIN task_attempts a ON a.id=t.current_attempt_id
                WHERE q.user_id=$1 AND q.status='pending' AND (
                  (q.item_type IN ('question','approval','review','plan_review')
                   AND (t.id IS NULL OR ({_CURRENT_ATTEMPT})))
                  OR (q.item_type='hitl_request' AND NOT r.superseded
                    AND r.snapshot->>'availability'='pending' AND EXISTS (
                      SELECT 1 FROM jsonb_array_elements(r.snapshot->'leaves') leaf
                      LEFT JOIN native_observed_sessions o
                        ON o.owner_id=$1 AND o.gateway=leaf->>'gateway'
                        AND o.runtime_session_id=leaf->>'runtime_session_id'
                      JOIN sessions s ON s.id=COALESCE(leaf->>'binding_id',o.snapshot->>'binding_id')
                      JOIN native_bindings b ON b.session_id=s.id
                      {_SESSION_ATTEMPT_JOIN}
                      WHERE s.user_id=$1 AND leaf->>'owner_id'=$1
                        AND s.archived_at IS NULL AND s.status NOT IN ('completed','failed','cancelled')
                        AND b.kagent_deleted_at IS NULL
                        AND b.kagent_session_id=leaf->>'runtime_session_id'
                        AND (a.id IS NULL OR ({_CURRENT_ATTEMPT}))
                    )))
                ORDER BY q.created_at DESC,q.id""",  # nosec B608 - code-owned SQL fragments; all values are bound
            owner_id,
        )
        projections = {
            row["hitl_request_id"]: HITLProjection.model_validate(
                store.decode(row["request_snapshot"])
            )
            for row in queues
            if row["item_type"] == "hitl_request"
        }
        leaves = [leaf for request in projections.values() for leaf in request.leaves]
        observed = await conn.fetch(
            """SELECT o.gateway,o.runtime_session_id,o.snapshot FROM native_observed_sessions o
               JOIN unnest($2::text[],$3::text[]) AS wanted(gateway,runtime_session_id)
                 USING(gateway,runtime_session_id) WHERE o.owner_id=$1""",
            owner_id,
            [leaf.gateway for leaf in leaves if leaf.binding_id is None],
            [leaf.runtime_session_id for leaf in leaves if leaf.binding_id is None],
        )
        bindings = {
            (row["gateway"], row["runtime_session_id"]): store.decode(
                row["snapshot"]
            ).get("binding_id")
            for row in observed
        }
        required_ids = list(
            {
                leaf.binding_id or bindings.get((leaf.gateway, leaf.runtime_session_id))
                for leaf in leaves
                if leaf.owner_id == owner_id
            }
            - {None}
        )
        rows = await conn.fetch(
            f"""WITH recent AS (
                  SELECT id FROM sessions WHERE user_id=$1 AND archived_at IS NULL
                    AND ($4::text IS NULL OR project_id=$4)
                  ORDER BY created_at DESC,id DESC LIMIT $2
                ), wanted AS (
                  SELECT id FROM recent
                  UNION SELECT s.id FROM sessions s {_SESSION_ATTEMPT_JOIN}
                    WHERE s.user_id=$1 AND s.archived_at IS NULL
                      AND ($4::text IS NULL OR s.project_id=$4)
                      AND s.status NOT IN ('completed','failed','cancelled')
                      AND (a.id IS NULL OR ({_CURRENT_ATTEMPT}))
                      AND (s.status IN ('pending','active','implementing','under_review')
                        OR t.snapshot->'owner_decision'->>'attempt_id'=a.id)
                  UNION SELECT id FROM sessions WHERE user_id=$1 AND (id=ANY($3::text[]) OR id=$5)
                )
                SELECT s.*,b.kind AS agent_kind,b.kagent_session_id,b.kagent_deleted_at,
                       b.parent_session_id,b.role,topic.name AS topic,
                       EXISTS(SELECT 1 FROM native_deliveries d WHERE d.session_id=s.id
                         AND d.state IN ('recorded','sending','delivered')) AS turn_open
                FROM wanted JOIN sessions s ON s.id=wanted.id
                LEFT JOIN native_bindings b ON b.session_id=s.id
                LEFT JOIN topics topic ON topic.id=b.topic_id""",  # nosec B608 - code-owned SQL fragments; all values are bound
            owner_id,
            HISTORY_LIMIT,
            required_ids,
            project_id,
            session_id,
        )
        sessions = {row["id"]: dict(row) for row in rows}
        rows = await conn.fetch(
            """SELECT a.*,t.current_attempt_id,t.status AS task_status,t.snapshot->>'title' AS task_title,
                      t.snapshot AS task_snapshot FROM task_attempts a
               JOIN tasks t ON t.id=a.task_id WHERE t.owner_id=$1
                 AND COALESCE(a.binding_id,a.session_id)=ANY($2::text[])""",
            owner_id,
            list(sessions),
        )
        attempts = {}
        for row in rows:
            row = dict(row)
            row["snapshot"] = store.decode(row["snapshot"])
            row["task_snapshot"] = store.decode(row["task_snapshot"])
            attempts[row["binding_id"] or row["session_id"]] = row
        answered = set(
            await conn.fetchval(
                """SELECT COALESCE(array_agg(leaf_key),ARRAY[]::text[])
               FROM native_hitl_response_members WHERE owner_id=$1 AND leaf_key=ANY($2::text[])""",
                owner_id,
                [leaf.key() for leaf in leaves],
            )
        )

        items, input_sessions, seen_leaves = [], set(), set()
        attention_queue_ids = set()
        for row in queues:
            queue = database._row_to_queue_item(row)
            if row["item_type"] == "hitl_request":
                request = projections[row["hitl_request_id"]]
                source_sessions, keys = set(), set()
                for leaf in request.leaves:
                    if leaf.owner_id != owner_id or leaf.key() in answered:
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
                items.append(
                    AttentionItem(
                        id=f"hitl:{request.id}",
                        kind="approval" if approval else "question",
                        title=queue.title,
                        needed=(
                            "Review and approve or reject the pending tool calls."
                            if approval
                            else "Answer the session's questions."
                        ),
                        href=f"/?inbox={quote(request.id, safe='')}#attention-{quote(request.id, safe='')}",
                        session_ids=sorted(source_sessions),
                        queue_item=queue,
                    )
                )
            else:
                items.append(
                    AttentionItem(
                        id=f"queue:{row['id']}",
                        kind=(
                            "question" if row["item_type"] == "question" else "approval"
                        ),
                        title=queue.title,
                        needed=queue.content,
                        href=f"/?inbox={quote(row['id'], safe='')}#attention-{quote(row['id'], safe='')}",
                        queue_item=queue,
                    )
                )
            attention_queue_ids.add(row["id"])
        for sid, attempt in attempts.items():
            decision = attempt["task_snapshot"].get("owner_decision")
            session = sessions.get(sid)
            if (
                not current_attempt(attempt)
                or not decision
                or decision["attempt_id"] != attempt["id"]
                or not session
                or session["archived_at"]
                or session["status"] in TERMINAL
            ):
                continue
            items.append(
                AttentionItem(
                    id=f"task:{attempt['task_id']}",
                    kind="owner_decision",
                    title=attempt["task_title"],
                    needed=decision["action"],
                    href=f"/tasks/{quote(attempt['task_id'], safe='')}",
                    task_id=attempt["task_id"],
                    session_ids=[sid],
                )
            )
        # History is capped by SQL before any report or queue snapshot is decoded.
        activity_rows = (
            await conn.fetch(
                """WITH queue_history AS (
                 SELECT 'queue:' || q.id AS id,q.title,q.content,
                        q.task_id,q.created_at FROM queue_items q
                 WHERE q.user_id=$1 AND q.id<>ALL($2::text[])
                 ORDER BY q.created_at DESC,q.id DESC LIMIT $3
               ), report_history AS (
                 SELECT 'report:' || r.id AS id,t.snapshot->>'title' AS title,
                        'Provider report (' || (r.snapshot->>'outcome') || '; unverified): '
                          || (r.snapshot->>'summary') AS content,r.task_id,r.created_at
                 FROM task_reports r JOIN tasks t ON t.id=r.task_id
                 WHERE t.owner_id=$1 ORDER BY r.created_at DESC,r.id DESC LIMIT $3
               ) SELECT * FROM (SELECT * FROM queue_history UNION ALL SELECT * FROM report_history) history
                 ORDER BY created_at DESC,id DESC LIMIT $3""",
                owner_id,
                list(attention_queue_ids),
                HISTORY_LIMIT,
            )
            if include_activity
            else []
        )

    owner_sessions = {sid for item in items for sid in item.session_ids}
    projected_sessions = {}
    for sid, row in sessions.items():
        projected_sessions[sid] = database._row_to_session(row).model_copy(
            update={
                "status": session_status(
                    row, attempts.get(sid), needs_input=sid in input_sessions
                ),
                "needs_owner": sid in owner_sessions,
                "parent_session_id": row["parent_session_id"],
                "topic": row["topic"],
            }
        )
    activity = [
        ActivityItem(
            id=row["id"],
            title=row["title"],
            content=row["content"],
            href=f"/tasks/{quote(row['task_id'], safe='')}" if row["task_id"] else None,
            created_at=row["created_at"],
        )
        for row in activity_rows
    ]
    listed = [
        projected_sessions[sid]
        for sid, row in sessions.items()
        if row["role"] != "main"
        and row["archived_at"] is None
        and (project_id is None or row["project_id"] == project_id)
    ]
    listed.sort(key=lambda session: session.created_at, reverse=True)
    return OwnerProjection(
        AttentionSnapshot(items=items, count=len(items), sessions=listed),
        projected_sessions,
        listed,
        activity,
    )
