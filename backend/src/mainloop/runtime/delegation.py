"""Postgres side of the context model: main-thread bootstrap, topics and records, delegation,
child reports, status/read from control-plane state, and standing-context rendering.

Nothing here talks to an agent except through ``native_sessions.submit_message`` (the ledgered
delivery path). Status and read never add a turn to any native session (D9).
"""

from __future__ import annotations

import uuid
from datetime import UTC, datetime

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
    carries over.
    """
    async with db.connection() as conn:
        row = await conn.fetchrow(
            """SELECT b.* FROM native_bindings b JOIN sessions s ON s.id=b.session_id
               WHERE b.role='main' AND s.user_id=$1 ORDER BY b.created_at LIMIT 1""",
            user_id,
        )
    if row:
        return dict(row)
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
            description="Native Claude main thread (window owned by Mainloop)",
            prompt="",
            conversation_id=conversation.id,
            status=SessionStatus.WAITING_ON_USER,
        )
    )
    return await native_sessions.create_binding(session.id, "claude", role="main")


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
        # delivered as the next prompt) and so is the protocol traffic of the pre-cut turn.
        recent = await conn.fetch(
            """SELECT m.role, m.content FROM messages m
               WHERE m.conversation_id=$1
                 AND NOT EXISTS (SELECT 1 FROM native_deliveries d WHERE d.message_id=m.id
                                 AND (d.state = ANY($3) OR d.state='queued' OR d.source='writeout'))
               ORDER BY m.created_at DESC LIMIT $2""",
            session.conversation_id,
            settings.main_carry_over_messages,
            list(native_sessions.OPEN_STATES),
        )
    lineage = ""
    if binding["lineage_seq"] > 1:
        lineage = (
            f"This is native session #{binding['lineage_seq']} of the main thread; earlier ones were "
            "rotated by Mainloop. Records above are authoritative; the recent messages are only a carry-over."
        )
    return render_standing(
        StandingInputs(
            role="main",
            topics=await _topic_lines(user_id),
            current_topic=name,
            checkpoint=checkpoint,
            pending=[f"[{p['name']}] {p['text']}" for p in pend],
            recent=[RecentMessage(r["role"], r["content"]) for r in reversed(recent)],
            lineage_note=lineage,
        )
    )


async def auto_report(session_id: str, reply: str) -> None:
    """Fallback signal: a child finished a turn without calling ``mainloop report``."""
    binding = await native_sessions.get_binding(session_id)
    if binding is None or binding["reported_at"] is not None:
        return
    await PgStore().deliver_report(
        binding,
        {"id": binding["topic_id"]} if binding["topic_id"] else None,
        reply[:4000],
        True,
    )


class PgStore:
    """``agent_api.Store`` over Postgres and the native-session delivery path."""

    async def binding_by_token_hash(self, token_hash: str) -> dict | None:
        async with db.connection() as conn:
            row = await conn.fetchrow(
                """SELECT b.*, s.user_id FROM native_bindings b JOIN sessions s ON s.id=b.session_id
                   WHERE b.token_hash=$1""",
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
                """SELECT b.session_id, b.kind, b.turns_in_lineage, b.reported_at, b.updated_at,
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
                    "turns": r["turns_in_lineage"],
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

    async def spawn_child(
        self, parent: dict, topic: dict, kind: str, title: str, brief: str
    ) -> str:
        parent_session = await db.get_session(parent["session_id"])
        conversation = await db.create_conversation(parent["user_id"], title=title)
        text = (
            f"Task brief from Mainloop (topic: {topic['name']})\n\n{brief}\n\n"
            'When finished, run: mainloop report --summary "<what you did and concluded, under 1500 characters>"'
        )
        session = await db.create_session(
            Session(
                id=str(uuid.uuid4()),
                user_id=parent["user_id"],
                main_thread_id=parent_session.main_thread_id,
                title=title[:80],
                description=f"Child of the main thread, topic {topic['name']}",
                prompt=text,
                conversation_id=conversation.id,
                status=SessionStatus.ACTIVE,
            )
        )
        await native_sessions.create_binding(
            session.id,
            kind,
            role="child",
            parent_session_id=parent["session_id"],
            topic_id=topic["id"],
        )
        await native_sessions.submit_message(session.id, text, source="brief")
        return session.id

    async def deliver_report(
        self, child: dict, topic: dict | None, summary: str, fallback: bool
    ) -> str:
        """Record the report as evidence on the topic and deliver it to the parent as a message."""
        async with db.connection() as conn:
            claimed = await conn.fetchval(
                "UPDATE native_bindings SET reported_at=NOW() WHERE session_id=$1 AND reported_at IS NULL RETURNING session_id",
                child["session_id"],
            )
            if claimed is None:
                return ""
            session = await db.get_session(child["session_id"])
            if topic and topic.get("id"):
                await conn.execute(
                    "INSERT INTO topic_records (id, topic_id, kind, text, session_id, evidence_ref) VALUES ($1,$2,'report',$3,$4,$5)",
                    str(uuid.uuid4()),
                    topic["id"],
                    summary,
                    child["session_id"],
                    child.get("journal_ref"),
                )
                await conn.execute(
                    "UPDATE topics SET updated_at=NOW() WHERE id=$1", topic["id"]
                )
        # Reporting is how a child's task ends: it is done, not waiting on the user. A session the
        # user already cancelled stays cancelled.
        if session.status not in native_sessions.ENDED_STATUSES:
            await db.update_session(
                child["session_id"],
                status=SessionStatus.COMPLETED,
                summary=summary,
                completed_at=datetime.now(UTC),
            )
        label = (
            " (fallback: the child ended a turn without reporting; this is its last reply)"
            if fallback
            else ""
        )
        text = f"[report from child {child['session_id'][:8]} '{session.title}'{label}]\n{summary}"
        return await native_sessions.submit_message(
            child["parent_session_id"], text, source="report"
        )

    async def standing_text(self, binding: dict) -> str:
        return await render_for_binding(binding)
