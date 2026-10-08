"""Postgres main-thread context and binding-scoped durable task operations.

Task reads and standing projections never add a native turn. Mutations share the task
application service; committed report notifications use its existing reconciler and ledger.
"""

from __future__ import annotations

import uuid

from mainloop.config import settings
from mainloop.db import db
from mainloop.runtime import native_sessions
from mainloop.runtime.standing import (
    RecentMessage,
    StandingInputs,
    TopicLine,
    render_standing,
)

from models import MainThread, Session, SessionStatus

INBOX = "inbox"


async def ensure_main_session(user_id: str) -> dict:
    """Return the user's single native main-thread binding, creating it on first use.

    Its conversation is the user's most recent main-thread conversation, so existing history
    carries over. The session row and its binding are written in one transaction, under a
    per-user lock, so a failure leaves neither and concurrent first requests share one.
    """
    query = """SELECT b.* FROM native_bindings b JOIN sessions s ON s.id=b.session_id
               WHERE b.role='main' AND s.user_id=$1 AND s.archived_at IS NULL ORDER BY b.created_at LIMIT 1"""
    async with db.connection() as conn:
        row = await conn.fetchrow(query, user_id)
    if row:
        return dict(row)
    async with db.connection() as conn:
        async with conn.transaction():
            await conn.execute(
                "SELECT pg_advisory_xact_lock(hashtext($1))", f"main-thread:{user_id}"
            )
            row = await conn.fetchrow(query, user_id)
            if row:
                return dict(row)
            # Reused on the next attempt if the transaction below rolls back.
            thread = await db.get_main_thread_by_user(user_id)
            if not thread:
                thread = await db.create_main_thread(
                    MainThread(user_id=user_id, workflow_run_id="native")
                )
            convs = await db.list_conversations(user_id, limit=1)
            conversation = convs[0] if convs else await db.create_conversation(user_id)
            session = await db.create_session(
                Session(
                    id=str(uuid.uuid4()),
                    user_id=user_id,
                    main_thread_id=thread.id,
                    title="Main thread",
                    description="Native Claude main thread",
                    prompt="",
                    conversation_id=conversation.id,
                    status=SessionStatus.WAITING_ON_USER,
                ),
                conn=conn,
            )
            return await native_sessions.create_binding(
                session.id, "claude", role="main", conn=conn
            )


async def _topic_lines(user_id: str) -> list[TopicLine]:
    async with db.connection() as conn:
        rows = await conn.fetch(
            """SELECT t.name, t.status_line,
                      (SELECT count(*) FROM topic_records r WHERE r.topic_id=t.id AND r.kind='pending' AND r.status='open') AS pending
               FROM topics t WHERE t.user_id=$1 ORDER BY t.updated_at DESC""",
            user_id,
        )
    return [TopicLine(r["name"], r["status_line"], r["pending"]) for r in rows]


async def render_for_binding(binding: dict) -> str:
    """Standing context / carry-over for a binding, rendered from Postgres only."""
    session = await db.get_session(binding["session_id"])
    if binding["role"] != "main":
        if binding.get("task_id"):
            values = await PgStore().task_call(binding, "task_list", {})
            return render_standing(
                StandingInputs(
                    role=binding["role"],
                    tasks=values["tasks"],
                )
            )
        return render_standing(StandingInputs(role=binding["role"]))
    user_id = session.user_id
    async with db.connection() as conn:
        top = await conn.fetchrow(
            "SELECT id, name, status_line, checkpoint FROM topics WHERE user_id=$1 ORDER BY updated_at DESC LIMIT 1",
            user_id,
        )
        checkpoint, name = "", None
        if top:
            name = top["name"]
            recs = await conn.fetch(
                """SELECT kind, text FROM topic_records WHERE topic_id=$1 AND kind IN ('note','decision','report')
                   ORDER BY created_at DESC LIMIT 6""",
                top["id"],
            )
            checkpoint = top["checkpoint"] or "\n".join(
                [top["status_line"]]
                + [f"{r['kind']}: {r['text']}" for r in reversed(recs)]
            )
        pend = await conn.fetch(
            """SELECT r.text, t.name FROM topic_records r JOIN topics t ON t.id=r.topic_id
               WHERE t.user_id=$1 AND r.kind='pending' AND r.status='open' ORDER BY r.created_at LIMIT 20""",
            user_id,
        )
        # Last K visible messages; undelivered/in-flight ones are excluded (they are about to be
        # delivered as the next prompt).
        recent = await conn.fetch(
            """SELECT m.role, m.content FROM messages m
               WHERE m.conversation_id=$1
                 AND NOT EXISTS (SELECT 1 FROM native_deliveries d WHERE d.message_id=m.id
                                 AND (d.state = ANY($3) OR d.state='queued'))
               ORDER BY m.created_at DESC LIMIT $2""",
            session.conversation_id,
            settings.main_carry_over_messages,
            list(native_sessions.OPEN_STATES),
        )
    values = await PgStore().task_call(binding, "task_list", {})
    return render_standing(
        StandingInputs(
            role="main",
            tasks=values["tasks"],
            topics=await _topic_lines(user_id),
            current_topic=name,
            checkpoint=checkpoint,
            pending=[f"[{p['name']}] {p['text']}" for p in pend],
            recent=[RecentMessage(r["role"], r["content"]) for r in reversed(recent)],
        )
    )


async def auto_report(session_id: str, reply: str) -> None:
    """Retained native callback: turn completion grants no task result authority."""
    return None


class PgStore:
    """``agent_tools.Store`` over Postgres and the native-session delivery path."""

    async def binding_by_token_hash(self, token_hash: str) -> dict | None:
        async with db.connection() as conn:
            row = await conn.fetchrow(
                """SELECT b.*,s.user_id,s.project_id AS session_project_id,
                          s.repo_url AS session_repo,s.branch_name AS session_branch,
                          s.status,s.archived_at,p.full_name,p.owner,p.name,p.html_url,
                          w.repo AS workspace_repo,w.branch AS workspace_branch
                   FROM native_bindings b JOIN sessions s ON s.id=b.session_id
                   LEFT JOIN projects p ON p.id=s.project_id AND p.user_id=s.user_id
                   LEFT JOIN workspaces w ON w.session_id=s.id
                   WHERE b.token_hash=$1 AND s.archived_at IS NULL
                     AND b.kagent_deleted_at IS NULL
                     AND s.status NOT IN ('completed','failed','cancelled')""",
                token_hash,
            )
        return dict(row) if row else None

    async def get_binding(self, session_id: str) -> dict | None:
        async with db.connection() as conn:
            row = await conn.fetchrow(
                """SELECT b.*, s.user_id FROM native_bindings b JOIN sessions s ON s.id=b.session_id
                   WHERE b.session_id=$1""",
                session_id,
            )
        return dict(row) if row else None

    async def count_live_children(self, parent_session_id: str | None) -> int:
        async with db.connection() as conn:
            return await conn.fetchval(
                """SELECT count(*) FROM native_bindings b JOIN sessions s ON s.id=b.session_id
                   WHERE b.role='child' AND b.reported_at IS NULL
                     AND s.status NOT IN ('failed','cancelled','completed')
                     AND NOT EXISTS (SELECT 1 FROM native_deliveries d WHERE d.session_id=b.session_id
                                     AND d.source='brief' AND d.state='failed')
                     AND ($1::text IS NULL OR b.parent_session_id=$1)""",
                parent_session_id,
            )

    async def topic(self, user_id: str, name: str, *, create: bool) -> dict | None:
        async with db.connection() as conn:
            row = await conn.fetchrow(
                "SELECT * FROM topics WHERE user_id=$1 AND name=$2", user_id, name
            )
            if row is None and create:
                row = await conn.fetchrow(
                    """INSERT INTO topics (id, user_id, name) VALUES ($1,$2,$3)
                       ON CONFLICT (user_id, name) DO UPDATE SET updated_at=NOW() RETURNING *""",
                    str(uuid.uuid4()),
                    user_id,
                    name,
                )
        return dict(row) if row else None

    async def set_topic_status(self, topic_id: str, status_line: str) -> None:
        async with db.connection() as conn:
            await conn.execute(
                "UPDATE topics SET status_line=$2, updated_at=NOW() WHERE id=$1",
                topic_id,
                status_line,
            )

    async def topic_index(self, user_id: str) -> list[TopicLine]:
        return await _topic_lines(user_id)

    async def add_record(
        self, topic_id: str, kind: str, text: str, session_id: str | None
    ) -> str:
        rid = str(uuid.uuid4())
        async with db.connection() as conn:
            await conn.execute(
                "INSERT INTO topic_records (id, topic_id, kind, text, session_id) VALUES ($1,$2,$3,$4,$5)",
                rid,
                topic_id,
                kind,
                text,
                session_id,
            )
            await conn.execute(
                "UPDATE topics SET updated_at=NOW() WHERE id=$1", topic_id
            )
        return rid

    async def close_pending(self, user_id: str, record_id: str) -> bool:
        """Close one open pending item by id prefix; ambiguous or unknown prefixes close nothing."""
        async with db.connection() as conn:
            rows = await conn.fetch(
                """SELECT r.id FROM topic_records r JOIN topics t ON t.id=r.topic_id
                   WHERE t.user_id=$1 AND r.kind='pending' AND r.status='open'
                     AND left(r.id, length($2::text)) = $2::text""",
                user_id,
                record_id,
            )
            if len(rows) != 1:
                return False
            await conn.execute(
                "UPDATE topic_records SET status='done' WHERE id=$1", rows[0]["id"]
            )
        return True

    async def children_state(self, parent_session_id: str) -> list[dict]:
        async with db.connection() as conn:
            rows = await conn.fetch(
                """SELECT b.session_id, b.kind, b.turns, b.reported_at, b.updated_at,
                          s.title, s.status, t.name AS topic,
                          (SELECT d.state FROM native_deliveries d WHERE d.session_id=b.session_id
                             ORDER BY d.created_at DESC LIMIT 1) AS last_delivery,
                          (SELECT m.content FROM messages m WHERE m.conversation_id=s.conversation_id
                             AND m.role='assistant' ORDER BY m.created_at DESC LIMIT 1) AS last_reply
                   FROM native_bindings b JOIN sessions s ON s.id=b.session_id
                   LEFT JOIN topics t ON t.id=b.topic_id
                   WHERE b.parent_session_id=$1 AND s.archived_at IS NULL ORDER BY b.created_at""",
                parent_session_id,
            )
        out = []
        for r in rows:
            if r["status"] == "cancelled":
                state = "cancelled"
            elif r["reported_at"] is not None:
                state = "reported"
            elif r["last_delivery"] in ("recorded", "sending", "delivered", "queued"):
                state = "working"
            elif r["last_delivery"] == "uncertain":
                state = "delivery-unknown"
            elif r["last_delivery"] == "failed":
                state = "failed-to-start"
            else:
                state = "idle"
            reply = r["last_reply"] or ""
            out.append(
                {
                    "session_id": r["session_id"],
                    "kind": r["kind"],
                    "title": r["title"],
                    "topic": r["topic"] or INBOX,
                    "status": r["status"],
                    "state": state,
                    "turns": r["turns"],
                    "last_activity": r["updated_at"].strftime("%H:%M:%SZ"),
                    "last_reply": " ".join(reply.split())[:300] or None,
                }
            )
        return out

    async def cancel_session(self, session_id: str) -> str:
        return await native_sessions.cancel(session_id)

    async def archive_children(
        self, user_id: str, parent_session_id: str, session_ids: list[str] | None
    ) -> list[str]:
        return await db.archive_sessions(
            user_id, session_ids=session_ids, parent_session_id=parent_session_id
        )

    async def messages(self, session_id: str, offset: int, limit: int) -> list[dict]:
        session = await db.get_session(session_id)
        async with db.connection() as conn:
            rows = await conn.fetch(
                "SELECT role, content FROM messages WHERE conversation_id=$1 ORDER BY created_at OFFSET $2 LIMIT $3",
                session.conversation_id,
                offset,
                limit,
            )
        return [dict(r) for r in rows]

    async def task_principal(self, binding: dict):
        from mainloop.tasks import lifecycle

        async with db.connection() as conn:
            principal = await lifecycle.authenticate_binding(conn, binding)
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

    async def task_call(self, binding: dict, action: str, arguments: dict) -> dict:
        from mainloop.db import tasks as task_store
        from mainloop.tasks import lifecycle, reports, service
        from mainloop.tasks.principal import TaskPrincipal

        from models.task import TaskAction, TaskCreate, TaskReassign, TaskReport

        async with db.connection() as conn:
            async with lifecycle.authority_locked(
                conn, binding["session_id"]
            ), conn.transaction():
                fresh = await conn.fetchrow(
                    """SELECT b.*,s.user_id,s.status,s.archived_at FROM native_bindings b
                       JOIN sessions s ON s.id=b.session_id WHERE b.session_id=$1 FOR SHARE OF b,s""",
                    binding["session_id"],
                )
                if (
                    not fresh
                    or not fresh["token_hash"]
                    or fresh["token_hash"] != binding.get("token_hash")
                    or fresh["archived_at"]
                    or fresh["kagent_deleted_at"]
                    or fresh["status"] in ("completed", "failed", "cancelled")
                ):
                    raise task_store.TaskError(403, "inactive_principal")
                if (
                    fresh["role"] == "main"
                    and fresh["mcp_grant_kind"] == "coordination"
                ):
                    principal = TaskPrincipal(
                        fresh["user_id"], binding_id=fresh["session_id"], role="main"
                    )
                else:
                    principal = await lifecycle.authenticate_binding(conn, dict(fresh))
                if action == "identity":
                    attempt = await lifecycle.load_attempt(conn, principal.attempt_id)
                    task = await task_store.get_task(conn, principal.task_id, principal)
                    return {
                        "task_id": task.id,
                        "attempt_id": attempt.id,
                        "root_task_id": task.root_task_id,
                        "parent_task_id": task.parent_task_id,
                        "workspace_id": attempt.workspace_id,
                        "writer_generation": attempt.writer_generation,
                        "attempt_number": attempt.number,
                    }
                if action in ("task_get", "task_history"):
                    value = await service.read_native(
                        conn, principal, arguments["task_id"]
                    )
                    return {
                        "text": f"{value.task.title}: {value.task.status}",
                        **value.model_dump(mode="json"),
                    }
                if action == "task_list":
                    tasks = await task_store.list_tasks(conn, principal, **arguments)
                    values = [
                        await service.read_native(conn, principal, task.id)
                        for task in tasks
                    ]
                    return {
                        "text": "\n".join(
                            f"{v.task.id} {v.task.title}: {v.task.status}"
                            for v in values
                        )
                        or "(no tasks)",
                        "tasks": [v.model_dump(mode="json") for v in values],
                    }
                if action == "report":
                    return await reports.record(
                        conn, principal, TaskReport.model_validate(arguments)
                    )
                if action == "delegate":
                    request = TaskCreate.model_validate(arguments)
                    value = await service.mutate(conn, principal, "create", request)
                else:
                    task_id = arguments["task_id"]
                    payload = {k: v for k, v in arguments.items() if k != "task_id"}
                    kind = {
                        "task_cancel": "cancel",
                        "task_retry": "retry",
                        "task_reassign": "reassign",
                    }[action]
                    request = (
                        TaskReassign if kind == "reassign" else TaskAction
                    ).model_validate(payload)
                    value = await service.mutate(
                        conn, principal, kind, request, task_id=task_id
                    )
                return {
                    "text": f"task operation {value.id}: {value.state}"
                    + (f" ({value.reason})" if value.reason else ""),
                    **value.model_dump(mode="json"),
                }

    async def standing_text(self, binding: dict) -> str:
        return await render_for_binding(binding)
