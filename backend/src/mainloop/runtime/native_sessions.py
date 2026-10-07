"""Sessions bound to a native Claude Code or Codex agent session in kagent.

Control-plane rules implemented here:
- A message is recorded, then a delivery row is persisted as ``sending`` *before* kagent is
  touched. Each message is sent once under its Mainloop message id (the A2A ``messageId``).
  kagent's documented "not accepted, retry the same message" is retried by the client with the
  same id; any other ambiguous outcome leaves the delivery ``uncertain`` ("delivery unknown") and
  is resolved by observing the A2A task (``GetTask``, ``ListTasks`` matching the message id),
  never by re-sending.
- The A2A task is the receipt: the task appearing proves delivery, its terminal state proves
  completion, and its text artifacts are mirrored into the session conversation under
  deterministic ids, so repeated syncs and reconnects are idempotent.
- kagent has no event cursor. After a restart or a dropped stream the current task replaces the
  projection (``GetTask`` or the first event of ``SubscribeToTask``).
- A kagent Session runs one non-quiescent task at a time, so Mainloop queues messages itself.

Context model: a binding has a ``role``. ``main`` is the conversation agent; ``child`` is a
delegated worker with a parent and a topic; ``agent`` is a stand-alone session. Reports are
ordinary ledgered deliveries; a delivery that arrives while another is open is ``queued`` by the
control plane and sent when idle.
"""

from __future__ import annotations

import asyncio
import json
import logging
import re
import uuid
from collections.abc import AsyncIterator, Coroutine
from datetime import UTC, datetime, timedelta
from typing import Any

from mainloop.config import settings
from mainloop.db import db
from mainloop.push_gate import lifecycle as push_lifecycle
from mainloop.runtime.agent_identity import hash_token, token_for
from mainloop.runtime.kagent_client import (
    A2AError,
    AgentRef,
    KagentClient,
    KagentError,
    KagentSession,
    OutcomeUnknown,
    RuntimeOperation,
    RuntimeState,
    SendNotAccepted,
    ServiceConfigurationError,
    SessionError,
    SessionWorkspace,
    StreamEvent,
    TaskNotFound,
    TaskProjection,
    Unreachable,
    assistant_message_id,
)
from mainloop.runtime.standing import content_hash
from mainloop.sse import notify_session_message

from models import NativeDeliveryInfo, NativeSessionInfo, SessionStatus

logger = logging.getLogger(__name__)

# A prompt with no task after this long is 'uncertain' (never replayed, never blocking).
SEND_RECEIPT_GRACE = timedelta(seconds=60)
_NS = uuid.UUID("6f0f7f0e-3f1e-4a3c-9d3b-0e4b6f5c2a11")
OPEN_STATES = ("recorded", "sending", "delivered")
# States a late observation may still resolve.
_RESOLVABLE = ("sending", "delivered", "uncertain")
# Ended by the user or by failure. Agent activity never moves a session out of these.
ENDED_STATUSES = frozenset({SessionStatus.CANCELLED, SessionStatus.FAILED})

# The conversation note written when a turn ends as ``cancelled``.
TURN_STOPPED_NOTE = "This turn was stopped before it finished."

# A delivery's ``detail`` is shown to the owner next to the message, so it is short and carries
# best-effort credential redaction. Error text from kagent, the harness or the stack is untrusted input.
DETAIL_MAX_CHARS = 300
_CREDENTIAL_FIELD_PATTERN = r"(?:authorization|token|secret|password|passwd|api[_-]?key|[A-Za-z_][A-Za-z0-9_]*_(?:TOKEN|KEY|SECRET|PASSWORD))"
# Quoted values may contain whitespace and escaped quotes (JSON or Python repr).
_ASSIGNMENT_VALUE_PATTERN = r"""(?:"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^\s,;}\]]+)"""
_SECRETS = (
    (
        re.compile(
            r"(?i)(\bAuthorization[\"']?\s*[:=]\s*)(?:[\"']?)[A-Za-z][A-Za-z0-9_-]*\s+[^\s\"',;}\]]+[\"']?"
        ),
        r"\1[redacted]",
    ),
    (re.compile(r"(?i)\bbearer\s+[A-Za-z0-9._~+/=-]+"), "Bearer [redacted]"),
    (
        re.compile(
            rf"(?i)(\b{_CREDENTIAL_FIELD_PATTERN}[\"']?\s*[=:]\s*){_ASSIGNMENT_VALUE_PATTERN}"
        ),
        r"\1[redacted]",
    ),
    (re.compile(r"(://)[^/\s:@]+:[^/\s@]+@"), r"\1[redacted]@"),  # credentials in a URL
    (
        re.compile(r"\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*"),
        "[redacted]",
    ),  # JWT
    (
        re.compile(
            r"\b(?:sk|ghp|gho|ghs|ghu|github_pat|xox[a-z])[-_][A-Za-z0-9_-]{8,}"
        ),
        "[redacted]",
    ),
    (
        re.compile(r"\b[A-Za-z0-9_-]{40,}\b"),
        "[redacted]",
    ),  # any other long opaque token
)


def safe_detail(text: str | None) -> str | None:
    """Return ``text`` as a one-line, length-bounded delivery detail with best-effort credential redaction."""
    if text is None:
        return None
    text = " ".join(str(text).split())
    for pattern, replacement in _SECRETS:
        text = pattern.sub(replacement, text)
    if len(text) > DETAIL_MAX_CHARS:
        text = text[: DETAIL_MAX_CHARS - 1].rstrip() + "…"
    return text or None


def describe_error(exc: BaseException) -> str:
    """Return the error class and message of a kagent or A2A failure, for a delivery's detail."""
    if isinstance(exc, A2AError):
        reason = f" ({exc.reason})" if exc.reason else ""
        return f"{type(exc).__name__}{reason}: {exc.message}"
    return f"{type(exc).__name__}: {exc}"


_locks: dict[str, asyncio.Lock] = {}
# Deliveries this process is currently reading a stream for; sync leaves them to the stream.
_streaming: set[str] = set()
_tasks: set[asyncio.Task] = set()


def next_status(
    current: SessionStatus, *, turn_open: bool, is_child: bool, reported: bool
) -> SessionStatus:
    """Session status from agent activity: what the mirror sets after each sync.

    A cancelled or failed session stays that way however the agent behaves afterwards. Otherwise
    an open turn is ``active``; an idle child that has reported is ``completed`` (its task is
    done, nothing is waiting on the user); anything else idle is ``waiting_on_user``.
    """
    if current in ENDED_STATUSES:
        return current
    if turn_open:
        return SessionStatus.ACTIVE
    if is_child and reported:
        return SessionStatus.COMPLETED
    return SessionStatus.WAITING_ON_USER


def _lock(session_id: str) -> asyncio.Lock:
    return _locks.setdefault(session_id, asyncio.Lock())


def _spawn(coro: Coroutine[Any, Any, Any]) -> None:
    task = asyncio.create_task(coro)
    _tasks.add(task)
    task.add_done_callback(_tasks.discard)


# --------------------------------------------------------------------------------------------
# kagent client and agent selection
# --------------------------------------------------------------------------------------------

_client: KagentClient | None = None


def get_client() -> KagentClient:
    global _client
    if _client is None:
        _client = KagentClient(
            settings.kagent_gateway_url,
            user_id=settings.kagent_user_id,
            control_token_file=settings.kagent_control_token_file,
            request_timeout=settings.kagent_request_timeout_seconds,
            stream_timeout=settings.kagent_turn_timeout_seconds,
            send_retry_budget=settings.kagent_send_retry_budget_seconds,
        )
    return _client


async def close_client() -> None:
    global _client
    if _client is not None:
        await _client.aclose()
        _client = None


def agent_name(kind: str, role: str = "agent") -> str:
    return agent_ref(kind, role).name


def agent_ref(kind: str, role: str = "agent") -> AgentRef:
    from mainloop.providers import registry

    ref = registry().resolve(kind, role).agents[role]
    return AgentRef(ref.namespace, ref.name)


def create_request_id(session_id: str) -> str:
    """Stable CreateSession request id: a retry returns the same kagent Session."""
    return str(uuid.uuid5(_NS, f"create-session:{session_id}"))


def _request_id(binding: dict) -> str:
    """Return the binding's create request id: the stable one, or the replacement's."""
    return binding.get("kagent_request_id") or create_request_id(binding["session_id"])


# --------------------------------------------------------------------------------------------
# Ledger (Postgres)
# --------------------------------------------------------------------------------------------


class Ledger:
    """Postgres side of the binding and delivery ledger."""

    async def get_binding(self, session_id: str, *, conn=None) -> dict | None:
        query = "SELECT * FROM native_bindings WHERE session_id=$1"
        if conn is None:
            async with db.connection() as connection:
                row = await connection.fetchrow(query, session_id)
        else:
            row = await conn.fetchrow(query, session_id)
        return dict(row) if row else None

    async def create_binding(
        self,
        connection,
        *,
        session_id: str,
        kind: str,
        role: str,
        parent_session_id: str | None,
        topic_id: str | None,
        token_hash: str | None,
        mcp_grant_kind: str,
        credential_ref: dict | None,
    ) -> None:
        await connection.execute(
            """INSERT INTO native_bindings
               (session_id,kind,role,parent_session_id,topic_id,token_hash,
                mcp_grant_kind,credential_ref)
               VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb)""",
            session_id,
            kind,
            role,
            parent_session_id,
            topic_id,
            token_hash,
            mcp_grant_kind,
            json.dumps(credential_ref) if credential_ref is not None else None,
        )

    async def update_binding(self, session_id: str, **fields) -> None:
        if not fields:
            return
        sets = [f"{k}=${i + 2}" for i, k in enumerate(fields)]
        sets.append("updated_at=NOW()")
        async with db.connection() as conn:
            async with push_lifecycle.locked(conn, session_id, revoke=False):
                if "kagent_session_id" in fields and settings.push_gate_enabled:
                    current = await conn.fetchval(
                        "SELECT kagent_session_id FROM native_bindings WHERE session_id=$1",
                        session_id,
                    )
                    if current != fields["kagent_session_id"]:
                        from mainloop.push_gate import store

                        await store.revalidate_on_runtime_replacement(conn, session_id)
                await conn.execute(
                    f"UPDATE native_bindings SET {', '.join(sets)} WHERE session_id=$1",  # nosec B608 - column names come from code, values are bound
                    session_id,
                    *fields.values(),
                )
                if fields.get("kagent_session_id"):
                    await push_lifecycle.enroll(conn, session_id)

    async def replace_kagent_session(
        self, session_id: str, old_kagent_session_id: str | None, request_id: str
    ) -> bool:
        """Point the binding at a Session not created yet, after kagent deleted the old one.

        The new create request id is stored before kagent is called, so the replacement is as
        idempotent as the first create. Deliveries still open on the old Session can never
        finish there; they become ``uncertain`` (never replayed). False when another pass
        already replaced it.
        """
        async with db.connection() as conn:
            async with push_lifecycle.locked(conn, session_id):
                if settings.push_gate_enabled:
                    current = await conn.fetchrow(
                        "SELECT kagent_session_id,child_start_failure FROM native_bindings WHERE session_id=$1",
                        session_id,
                    )
                    if (
                        not current
                        or current["kagent_session_id"] != old_kagent_session_id
                        or current["child_start_failure"] is not None
                    ):
                        return False
                    from mainloop.push_gate import store

                    await store.revalidate_on_runtime_replacement(conn, session_id)
                async with conn.transaction():
                    moved = await conn.fetchval(
                        """UPDATE native_bindings
                           SET kagent_session_id=NULL, kagent_request_id=$3, standing_hash=NULL,
                               updated_at=NOW()
                           WHERE session_id=$1 AND kagent_session_id IS NOT DISTINCT FROM $2
                             AND child_start_failure IS NULL
                           RETURNING session_id""",
                        session_id,
                        old_kagent_session_id,
                        request_id,
                    )
                    if moved is None:
                        return False
                    await conn.execute(
                        """UPDATE native_deliveries
                           SET state='uncertain',
                               detail='the kagent Session was deleted; not replaying',
                               updated_at=NOW()
                           WHERE session_id=$1 AND state IN ('sending','delivered')""",
                        session_id,
                    )
        return True

    async def bump_turns(self, session_id: str) -> None:
        async with db.connection() as conn:
            await conn.execute(
                "UPDATE native_bindings SET turns=turns+1, updated_at=NOW() WHERE session_id=$1",
                session_id,
            )

    @staticmethod
    async def _lock_deliveries(conn, session_id: str) -> None:
        """Serialise changes that open a delivery, across processes, until the transaction ends.

        The REST and MCP containers both write the ledger, so the one-open-turn rule cannot rest
        on the in-process lock. This is a database lock only; it is never held across a call to
        kagent.
        """
        await conn.execute(
            "SELECT pg_advisory_xact_lock(hashtext($1))",
            f"native-deliveries:{session_id}",
        )

    @staticmethod
    async def _insert_message(
        conn, session_id, conversation_id, text, state, source
    ) -> str:
        message = await db.create_message(
            conversation_id=conversation_id, role="user", content=text, conn=conn
        )
        await conn.execute(
            "INSERT INTO native_deliveries (message_id, session_id, state, source) VALUES ($1,$2,$3,$4)",
            message.id,
            session_id,
            state,
            source,
        )
        return message.id

    async def record_submission(
        self, *, session_id: str, conversation_id: str, text: str, source: str
    ) -> tuple[str, str]:
        """Record a message as ``recorded`` (nothing open) or ``queued`` (a turn is open).

        The open-turn check and the insert are one transaction under a per-session advisory
        lock, so two writers in different processes cannot both find the session idle. A ``user``
        or ``brief`` message that finds a turn open is refused with ``ValueError``; a ``report``
        is queued. An ``uncertain`` delivery does not block: the owner decides whether to resend.

        After the owner stopped a turn the queue is held: a ``report`` is queued although nothing
        is open, and the owner's next ``user`` message releases the hold and goes first. The queued
        messages then follow it, one turn at a time.
        """
        async with db.connection() as conn, conn.transaction():
            await self._lock_deliveries(conn, session_id)
            busy = await conn.fetchval(
                """SELECT EXISTS(SELECT 1 FROM native_deliveries
                   WHERE session_id=$1 AND state = ANY($2))""",
                session_id,
                list(OPEN_STATES),
            )
            if busy and source in ("user", "brief"):
                raise ValueError(
                    "A previous message is still in flight; wait for its reply before sending another."
                )
            held = await conn.fetchval(
                "SELECT queue_held FROM native_bindings WHERE session_id=$1", session_id
            )
            if source == "user" and held:
                await conn.execute(
                    "UPDATE native_bindings SET queue_held=FALSE WHERE session_id=$1",
                    session_id,
                )
                held = False
            state = "queued" if busy or held else "recorded"
            message_id = await self._insert_message(
                conn, session_id, conversation_id, text, state, source
            )
        return message_id, state

    async def delivery_state(self, message_id: str) -> str | None:
        async with db.connection() as conn:
            return await conn.fetchval(
                "SELECT state FROM native_deliveries WHERE message_id=$1", message_id
            )

    async def recorded_deliveries(self, session_id: str) -> list[tuple[str, str]]:
        """Deliveries kagent has never seen (``recorded``), oldest first, with their text."""
        async with db.connection() as conn:
            rows = await conn.fetch(
                """SELECT d.message_id, m.content FROM native_deliveries d
                   JOIN messages m ON m.id=d.message_id
                   WHERE d.session_id=$1 AND d.state='recorded' ORDER BY d.created_at""",
                session_id,
            )
        return [(r["message_id"], r["content"]) for r in rows]

    async def open_count(self, session_id: str) -> int:
        async with db.connection() as conn:
            return await conn.fetchval(
                "SELECT count(*) FROM native_deliveries WHERE session_id=$1 AND state = ANY($2)",
                session_id,
                list(OPEN_STATES),
            )

    async def active_count(self, session_id: str) -> int:
        """Deliveries that are, or are about to be, a turn: open ones and those queued behind.

        Messages held after a stop are not about to be a turn, so they do not count."""
        async with db.connection() as conn:
            return await conn.fetchval(
                """SELECT count(*) FROM native_deliveries d WHERE d.session_id=$1
                   AND (d.state = ANY($2)
                        OR (d.state='queued' AND NOT COALESCE(
                            (SELECT queue_held FROM native_bindings WHERE session_id=$1), FALSE)))""",
                session_id,
                list(OPEN_STATES),
            )

    async def get_workspace(
        self, session_id: str, *, conn=None
    ) -> SessionWorkspace | None:
        """Return the Session ``workspace`` of a branch workspace; None for any other session."""
        query = "SELECT repo, ref, branch, depth FROM workspaces WHERE session_id=$1"
        if conn is None:
            async with db.connection() as connection:
                row = await connection.fetchrow(query, session_id)
        else:
            row = await conn.fetchrow(query, session_id)
        if row is None:
            return None
        return SessionWorkspace(
            repo=row["repo"], ref=row["ref"], branch=row["branch"], depth=row["depth"]
        )

    async def get_development_environment(self, session_id: str):
        from mainloop.db.environments import decode

        async with db.connection() as conn:
            value = await conn.fetchval(
                "SELECT development_environment FROM workspaces WHERE session_id=$1",
                session_id,
            )
        return decode(value) if value is not None else None

    async def record_composition(self, session_id: str, session: KagentSession):
        from dataclasses import asdict

        async with db.connection() as conn:
            await conn.execute(
                "UPDATE workspaces SET reported_development_environment=$2::jsonb, runtime_composition=$3::jsonb WHERE session_id=$1",
                session_id,
                (
                    json.dumps(asdict(session.development_environment))
                    if session.development_environment
                    else None
                ),
                (
                    json.dumps(asdict(session.runtime_composition))
                    if session.runtime_composition
                    else None
                ),
            )

    async def undeleted_archived(self) -> list[dict]:
        """Return archived sessions whose kagent Session kagent has not confirmed deleted."""
        async with db.connection() as conn:
            rows = await conn.fetch(
                """SELECT b.session_id FROM native_bindings b
                   JOIN sessions s ON s.id=b.session_id
                   WHERE s.archived_at IS NOT NULL AND b.kagent_session_id IS NOT NULL
                     AND b.kagent_deleted_at IS NULL"""
            )
        return [dict(r) for r in rows]

    async def mark_kagent_deleted(self, session_id: str) -> None:
        async with db.connection() as conn:
            async with push_lifecycle.locked(conn, session_id, revoke=True):
                await conn.execute(
                    "UPDATE native_bindings SET kagent_deleted_at=NOW() WHERE session_id=$1",
                    session_id,
                )

    async def transition(
        self,
        message_id: str,
        state: str,
        *,
        from_states: tuple[str, ...],
        task_id: str | None = None,
        evidence_ref: str | None = None,
        detail: str | None = None,
    ) -> bool:
        """Move a delivery only if it is still in ``from_states``; true when this call moved it."""
        detail = safe_detail(detail)
        async with db.connection() as conn, conn.transaction():
            if state in OPEN_STATES:
                session_id = await conn.fetchval(
                    "SELECT session_id FROM native_deliveries WHERE message_id=$1",
                    message_id,
                )
                if session_id is None:
                    return False
                await self._lock_deliveries(conn, session_id)
                if await conn.fetchval(
                    """SELECT EXISTS(SELECT 1 FROM native_deliveries
                       WHERE session_id=$1 AND message_id<>$2 AND state = ANY($3))""",
                    session_id,
                    message_id,
                    list(OPEN_STATES),
                ):
                    # A late receipt for an uncertain delivery must not reopen it alongside
                    # the owner's newer turn. It stays observable and is never resent.
                    return False
            if state == "sending":
                # Serialize the send claim with durable startup disposal intent.
                binding = await conn.fetchrow(
                    """SELECT b.child_start_failure FROM native_bindings b
                       JOIN native_deliveries d ON d.session_id=b.session_id
                       WHERE d.message_id=$1 FOR UPDATE OF b""",
                    message_id,
                )
                if binding and binding["child_start_failure"]:
                    return False
            row = await conn.fetchval(
                """UPDATE native_deliveries SET state=$2, task_id=COALESCE($3, task_id),
                   evidence_ref=COALESCE($4, evidence_ref),
                   detail=CASE WHEN $2 IN ('delivered', 'completed') THEN $5 ELSE COALESCE($5, detail) END,
                   updated_at=NOW()
                   WHERE message_id=$1 AND state = ANY($6) RETURNING message_id""",
                message_id,
                state,
                task_id,
                evidence_ref,
                detail,
                list(from_states),
            )
        return row is not None

    async def settle_cancelled(
        self,
        message_id: str,
        *,
        from_states: tuple[str, ...],
        conversation_id: str,
        note_id: str,
        note: str,
        task_id: str | None = None,
        detail: str | None = None,
        partial: str | None = None,
    ) -> bool:
        """Move a delivery to the terminal ``cancelled`` state and write the conversation note, in
        one transaction. True when this call moved it; false (nothing written) when the delivery
        was no longer in ``from_states``. The note id is deterministic, so a replay adds nothing.
        """
        detail = safe_detail(detail)
        async with db.connection() as conn, conn.transaction():
            session_id = await conn.fetchval(
                "SELECT session_id FROM native_deliveries WHERE message_id=$1",
                message_id,
            )
            if session_id is None:
                return False
            # Settle and hold under the same lock as submission/promotion. A stream in the
            # MCP process may observe cancellation before the REST stop request returns.
            await self._lock_deliveries(conn, session_id)
            moved = await conn.fetchval(
                """UPDATE native_deliveries SET state='cancelled', task_id=COALESCE($2, task_id),
                   evidence_ref=COALESCE($3, evidence_ref), detail=$4, updated_at=NOW()
                   WHERE message_id=$1 AND state = ANY($5) RETURNING message_id""",
                message_id,
                task_id,
                f"a2a:task/{task_id}" if task_id else None,
                detail,
                list(from_states),
            )
            if moved is None:
                return False
            await conn.execute(
                "UPDATE native_bindings SET queue_held=TRUE WHERE session_id=$1",
                session_id,
            )
            saved = await conn.fetchval(
                "SELECT partial_text FROM native_deliveries WHERE message_id=$1",
                message_id,
            )
            note = stopped_message(partial or saved) if partial or saved else note
            await conn.execute(
                """INSERT INTO messages (id, conversation_id, role, content, created_at)
                   VALUES ($1,$2,'assistant',$3,NOW()) ON CONFLICT (id) DO NOTHING""",
                note_id,
                conversation_id,
                note,
            )
            await conn.execute(
                "UPDATE conversations SET updated_at=NOW() WHERE id=$1", conversation_id
            )
        return True

    async def remember_partial(self, message_id: str, text: str) -> None:
        """Keep observed text even when CancelTask/GetTask later omit their artifacts."""
        if not text:
            return
        async with db.connection() as conn:
            await conn.execute(
                """UPDATE native_deliveries SET partial_text=$2
                   WHERE message_id=$1 AND state = ANY($3)""",
                message_id,
                text,
                list(_RESOLVABLE),
            )

    async def remember_child_start_failure(self, session_id: str, reason: str) -> bool:
        """Disposal intent wins only before any process claims the initial brief.

        A delivery that reached kagent (it has a task) is a claim whatever state it ended in: a
        first turn the owner stopped is ``cancelled`` with a task and ``turns`` still 0.
        """
        async with db.connection() as conn, conn.transaction():
            binding = await conn.fetchrow(
                "SELECT role, turns FROM native_bindings WHERE session_id=$1 FOR UPDATE",
                session_id,
            )
            if not binding or binding["role"] != "child" or binding["turns"]:
                return False
            claimed = await conn.fetchval(
                """SELECT EXISTS(SELECT 1 FROM native_deliveries WHERE session_id=$1
                   AND (state IN ('sending','delivered','completed','uncertain')
                        OR task_id IS NOT NULL))""",
                session_id,
            )
            if claimed:
                return False
            await conn.execute(
                """UPDATE native_bindings SET child_start_failure=COALESCE(child_start_failure,$2),
                   updated_at=NOW() WHERE session_id=$1""",
                session_id,
                reason,
            )
        return True

    async def deliveries(self, session_id: str) -> list[dict]:
        async with db.connection() as conn:
            rows = await conn.fetch(
                "SELECT * FROM native_deliveries WHERE session_id=$1 ORDER BY created_at",
                session_id,
            )
        return [dict(r) for r in rows]

    async def resolvable_deliveries(self, session_id: str) -> list[dict]:
        """Deliveries a task observation can still move, with the message text."""
        async with db.connection() as conn:
            rows = await conn.fetch(
                """SELECT d.*, m.content FROM native_deliveries d JOIN messages m ON m.id=d.message_id
                   WHERE d.session_id=$1 AND d.state = ANY($2) ORDER BY d.created_at""",
                session_id,
                list(_RESOLVABLE),
            )
        return [dict(r) for r in rows]

    async def promote_queued(self, session_id: str) -> tuple[str, str] | None:
        """Mark the oldest queued delivery ``recorded`` if (and only if) nothing is open. Takes the
        same advisory lock as ``record_submission``, so a submission cannot slip in between. Nothing
        is promoted while the queue is held after a stop."""
        async with db.connection() as conn, conn.transaction():
            await self._lock_deliveries(conn, session_id)
            row = await conn.fetchrow(
                """UPDATE native_deliveries SET state='recorded', updated_at=NOW()
                   WHERE message_id = (SELECT message_id FROM native_deliveries
                                       WHERE session_id=$1 AND state='queued' ORDER BY created_at LIMIT 1)
                     AND state='queued'
                     AND NOT EXISTS (SELECT 1 FROM native_deliveries WHERE session_id=$1 AND state = ANY($2))
                     AND NOT COALESCE((SELECT queue_held FROM native_bindings WHERE session_id=$1), FALSE)
                   RETURNING message_id""",
                session_id,
                list(OPEN_STATES),
            )
            if row is None:
                return None
            text = await conn.fetchval(
                "SELECT content FROM messages WHERE id=$1", row["message_id"]
            )
        return row["message_id"], text

    async def fail_open(self, session_id: str, detail: str) -> list[dict]:
        """Close every open or queued delivery as failed; return what was open, with the state each
        had before (``state``) and its task id."""
        detail = safe_detail(detail)
        async with db.connection() as conn:
            rows = await conn.fetch(
                """WITH prior AS (
                       SELECT message_id, state FROM native_deliveries
                       WHERE session_id=$1
                         AND state IN ('recorded','sending','delivered','queued','uncertain')
                       FOR UPDATE)
                   UPDATE native_deliveries d SET state='failed', detail=$2, updated_at=NOW()
                   FROM prior WHERE d.message_id = prior.message_id
                   RETURNING d.message_id, d.task_id, prior.state AS state""",
                session_id,
                detail,
            )
        return [dict(r) for r in rows]

    async def mirror_reply(
        self, conversation_id: str, message_id: str, text: str
    ) -> bool:
        async with db.connection() as conn:
            result = await conn.execute(
                "INSERT INTO messages (id, conversation_id, role, content, created_at) VALUES ($1,$2,'assistant',$3,NOW()) ON CONFLICT (id) DO NOTHING",
                message_id,
                conversation_id,
                text,
            )
        return str(result).endswith(" 1")

    async def sessions_with_open_work(self) -> list[str]:
        async with db.connection() as conn:
            rows = await conn.fetch(
                """SELECT DISTINCT d.session_id FROM native_deliveries d
                   WHERE d.state IN ('recorded','sending','delivered')
                      OR (d.state='queued' AND NOT COALESCE(
                          (SELECT queue_held FROM native_bindings WHERE session_id=d.session_id), FALSE))
                      OR (d.state='uncertain' AND d.updated_at > NOW() - INTERVAL '30 minutes')"""
            )
        return [r["session_id"] for r in rows]

    async def topic_name(self, topic_id: str) -> str | None:
        async with db.connection() as conn:
            return await conn.fetchval("SELECT name FROM topics WHERE id=$1", topic_id)


ledger = Ledger()


async def get_binding(session_id: str, *, conn=None) -> dict | None:
    return await ledger.get_binding(session_id, conn=conn)


async def create_binding(
    session_id: str,
    kind: str,
    *,
    role: str = "agent",
    parent_session_id: str | None = None,
    topic_id: str | None = None,
    mcp_grant_kind: str | None = None,
    conn=None,
) -> dict:
    from mainloop.providers import registry

    kind = registry().resolve(kind, role, selecting=True).id
    from mainloop.runtime.agent_credentials import (
        credential_reference,
        reference_data,
    )

    grant_kind = mcp_grant_kind or (
        "coordination" if role in ("main", "child") else "none"
    )
    valid_grant = (role, grant_kind) in {
        ("main", "coordination"),
        ("child", "coordination"),
        ("agent", "workspace"),
    }
    if grant_kind not in ("none", "coordination", "workspace") or (
        grant_kind != "none" and not valid_grant
    ):
        raise ValueError("MCP grant kind does not match the native binding role")
    token_hash = hash_token(token_for(session_id)) if valid_grant else None
    credential_ref = (
        reference_data(credential_reference(session_id)) if valid_grant else None
    )
    fields = dict(
        session_id=session_id,
        kind=kind,
        role=role,
        parent_session_id=parent_session_id,
        topic_id=topic_id,
        token_hash=token_hash,
        mcp_grant_kind=grant_kind,
        credential_ref=credential_ref,
    )
    if conn is None:
        async with db.connection() as connection:
            await ledger.create_binding(connection, **fields)
        return await get_binding(session_id)  # type: ignore[return-value]
    await ledger.create_binding(conn, **fields)
    return await get_binding(session_id, conn=conn)  # type: ignore[return-value]


# --------------------------------------------------------------------------------------------
# Submit and deliver
# --------------------------------------------------------------------------------------------


async def submit_message(session_id: str, text: str, *, source: str = "user") -> str:
    """Record a message and its delivery intent, then deliver in the background.

    ``source``: ``user`` (typed in the UI; refused while a turn is open), ``report`` (a child's
    report; queued while a turn is open), ``brief`` (a parent's task brief to a fresh child). A
    ``queued`` delivery is sent by ``sync`` once the agent is idle.
    """
    session = await db.get_session(session_id)
    if source == "user" and session.status in ENDED_STATUSES:
        raise ValueError(f"This session is {session.status.value}; start a new one.")
    # The in-process lock orders this with delivery, suspend and delete in this process. The
    # one-open-turn rule itself is enforced in PostgreSQL by ``record_submission`` (the MCP
    # container is a second writer).
    async with _lock(session_id):
        # Archiving revokes the session's token and deletes its kagent Session (under this lock,
        # once no turn is open), so a message recorded after the archive could never be sent.
        # Read again here: the archive may have landed since the read above.
        if source == "user" and (await db.get_session(session_id)).archived_at:
            raise ValueError("This session is archived; start a new one.")
        message_id, state = await ledger.record_submission(
            session_id=session_id,
            conversation_id=session.conversation_id,
            text=text,
            source=source,
        )
    if state == "recorded":
        _spawn_deliver(session_id, message_id, text)
    return message_id


def _spawn_deliver(session_id: str, message_id: str, text: str) -> None:
    # Marked before the task runs, so a sync in between leaves the message to this delivery.
    _streaming.add(message_id)
    _spawn(_deliver(session_id, message_id, text))


def _create_hit_deleted(exc: SessionError) -> bool:
    """CreateSession refused the request id because its Session was deleted."""
    return exc.grpc_status == 9 and "deleted" in str(exc).lower()


async def _live_session(session_id: str) -> KagentSession | None:
    """Return the kagent Session, or None when kagent has deleted it (idle TTL or out of band)."""
    try:
        session = await get_client().get_session(session_id)
    except SessionError as exc:
        if exc.grpc_status == 5:  # NOT_FOUND
            return None
        raise
    if session.state in (RuntimeState.DELETING, RuntimeState.DELETED):
        return None
    return session


async def _replace_kagent_session(binding: dict) -> None:
    old = binding["kagent_session_id"]
    logger.warning(
        "kagent Session %s of %s is gone; creating a new one",
        old or _request_id(binding),
        binding["session_id"],
    )
    # When another pass replaced it first this changes nothing; either way continue from what
    # is stored.
    await ledger.replace_kagent_session(binding["session_id"], old, str(uuid.uuid4()))
    binding.update(await ledger.get_binding(binding["session_id"]) or {})


class ChildStartPending(Exception):
    """The initial brief is unsent; reconcile the same Session before disposal."""


class ChildStartRejected(SessionError):
    """Create rejected the request before reservation; no actor needs disposal."""


class ChildStartDisposed(SessionError):
    """Startup failed after the reserved actor's absence/disposal was confirmed."""


async def _fail_child_start(binding: dict, reason: str) -> None:
    current = await db.get_session(binding["session_id"])
    if current is not None and current.status not in ENDED_STATUSES:
        await db.update_session(
            binding["session_id"], status=SessionStatus.FAILED, error=reason
        )
    await ledger.fail_open(binding["session_id"], f"child startup failed: {reason}")


async def _settle_child_start_failure(binding: dict) -> None:
    """Resume admitted lifecycle work on the same actor before revoking its identity."""
    reason = binding["child_start_failure"]
    if binding["kagent_session_id"] is None:
        # A response or the binding write may have been lost after reservation.
        # Recover the same receipt; never allocate another request or replay the brief.
        try:
            session = await _create_bound_session(binding)
        except SessionError as exc:
            if _create_hit_deleted(exc):
                await _fail_child_start(binding, reason)
                raise ChildStartDisposed(f"child startup failed: {reason}") from exc
            raise ChildStartPending(reason) from exc
        except KagentError as exc:
            raise ChildStartPending(reason) from exc
        binding["kagent_session_id"] = session.id
    # Also persist an identity held only in the failed creator's local binding.
    await ledger.update_binding(
        binding["session_id"], kagent_session_id=binding["kagent_session_id"]
    )
    try:
        session = await get_client().get_session(binding["kagent_session_id"])
    except SessionError as exc:
        if exc.grpc_status != 5:
            raise ChildStartPending(reason) from exc
        session = None
    except KagentError as exc:
        raise ChildStartPending(reason) from exc
    if session is not None:
        try:
            if not session.settled and session.operation == RuntimeOperation.CREATE:
                recovered = await _create_bound_session(binding)
                if recovered.id != session.id:
                    raise ChildStartPending(
                        "create reconciliation returned another actor"
                    )
                session = recovered
            if not session.settled and session.operation != RuntimeOperation.DELETE:
                raise ChildStartPending(reason)
            if session.state != RuntimeState.DELETED or not session.settled:
                session = await get_client().delete_session(session.id)
        except KagentError as exc:
            raise ChildStartPending(reason) from exc
        if session.state != RuntimeState.DELETED or not session.settled:
            raise ChildStartPending(reason)
    await _fail_child_start(binding, reason)
    raise ChildStartDisposed(f"child startup failed: {reason}")


async def _remember_child_start_failure(binding: dict, reason: str) -> bool:
    remembered = await ledger.remember_child_start_failure(
        binding["session_id"], reason
    )
    if remembered:
        binding["child_start_failure"] = reason
    return remembered


async def _create_session_with_credentials(
    binding: dict,
    refs: tuple,
    *,
    require_workspace: bool = False,
) -> KagentSession:
    workspace = await ledger.get_workspace(binding["session_id"])
    if require_workspace and workspace is None:
        raise RuntimeError("persisted workspace create contract is unavailable")
    # The workspace is read from its one stored copy on every create, so a replacement Session
    # resends exactly what the first one got (kagent rejects a changed workspace under one id).
    from mainloop.runtime.kagent_client import DevelopmentEnvironment

    value = await ledger.get_development_environment(binding["session_id"])
    options = {}
    if value is not None:
        options["development_environment"] = DevelopmentEnvironment(
            value["image"], value["platform"], value["policy_identity"]
        )
    session = await get_client().create_session(
        agent_ref(binding["kind"], binding["role"]),
        request_id=_request_id(binding),
        credentials=refs,
        workspace=workspace,
        **options,
    )
    if (
        session.development_environment is not None
        or session.runtime_composition is not None
    ):
        await ledger.record_composition(binding["session_id"], session)
    return session


async def _create_bound_session(binding: dict) -> KagentSession:
    from mainloop.runtime.agent_credentials import publish_for_binding

    refs = ()
    if binding.get("mcp_grant_kind") in ("coordination", "workspace"):
        if not binding.get("token_hash"):
            raise RuntimeError("binding identity is revoked")
        try:
            refs = (await publish_for_binding(binding),)
        except Exception as exc:
            if binding.get("child_start_failure"):
                raise ChildStartPending(str(exc)) from exc
            raise
    elif binding.get("token_hash"):
        raise RuntimeError("binding identity has no persisted MCP grant")
    return await _create_session_with_credentials(binding, refs)


async def reconcile_revoked_workspace_creation(binding: dict) -> KagentSession:
    """Recover a cancelled workspace's original uncertain CreateSession without reenrolling it.

    Cancellation clears the bearer hash before Secret cleanup. If the first CreateSession reply
    was lost, retry its frozen request ID, checkout and persisted credential reference so kagent
    returns the same runtime. Never publish the reference or restore the hash on this path.
    """
    from mainloop.runtime.agent_credentials import reference_from_data

    async with db.connection() as conn:
        row = await conn.fetchrow(
            """SELECT b.*,s.status AS session_status
               FROM native_bindings b JOIN sessions s ON s.id=b.session_id
               WHERE b.session_id=$1 FOR SHARE OF b,s""",
            binding["session_id"],
        )
    if (
        row is None
        or row["role"] != "agent"
        or row["mcp_grant_kind"] != "workspace"
        or row["token_hash"] is not None
        or row["kagent_session_id"] is not None
        or row["session_status"] != SessionStatus.CANCELLED.value
    ):
        raise RuntimeError(
            "cancelled workspace create is not eligible for reconciliation"
        )
    reference = reference_from_data(row["credential_ref"])
    if reference is None:
        raise RuntimeError("persisted workspace credential reference is unavailable")
    return await _create_session_with_credentials(
        dict(row), (reference,), require_workspace=True
    )


async def _ensure_kagent_session(binding: dict) -> KagentSession:
    """Return the binding's kagent Session, ready for a turn.

    It is created on first use and resumed if suspended. A Session kagent has deleted (the idle
    TTL, or out of band) is replaced once, under a fresh create request id; the replacement gets
    the standing context again, because ``standing_hash`` belongs to the Session it went to.
    """
    client = get_client()
    binding.update(await ledger.get_binding(binding["session_id"]) or {})
    for replaced in (False, True):
        if binding["kagent_session_id"] is None:
            try:
                session = await _create_bound_session(binding)
            except OutcomeUnknown as exc:
                if binding["role"] == "child" and not binding["turns"]:
                    await _remember_child_start_failure(
                        binding, f"creation outcome unknown: {exc}"
                    )
                    raise ChildStartPending(str(exc)) from exc
                raise
            except SessionError as exc:
                if binding.get("child_start_failure"):
                    if _create_hit_deleted(exc):
                        await _fail_child_start(binding, binding["child_start_failure"])
                        raise ChildStartDisposed(str(exc)) from exc
                    raise ChildStartPending(str(exc)) from exc
                if replaced or not _create_hit_deleted(exc):
                    if (
                        binding["role"] == "child"
                        and not binding["turns"]
                        and exc.grpc_status in (3, 7, 16)
                    ):
                        raise ChildStartRejected(
                            str(exc), grpc_status=exc.grpc_status
                        ) from exc
                    raise
                await _replace_kagent_session(binding)
                continue
            # Keep the admitted identity locally even if its database write fails.
            binding.update(kagent_session_id=session.id, standing_hash=None)
            await ledger.update_binding(
                binding["session_id"], kagent_session_id=session.id, standing_hash=None
            )
            binding.update(await ledger.get_binding(binding["session_id"]) or {})
        else:
            if binding.get("child_start_failure"):
                await _settle_child_start_failure(binding)
            live = await _live_session(binding["kagent_session_id"])
            if live is None:
                if replaced:
                    raise SessionError("the replacement kagent Session is already gone")
                await _replace_kagent_session(binding)
                continue
            session = live
        if binding.get("child_start_failure"):
            await _settle_child_start_failure(binding)
        try:
            return await client.ensure_ready(
                session, timeout=settings.kagent_session_ready_timeout_seconds
            )
        except ServiceConfigurationError:
            raise
        except KagentError as exc:
            if binding["role"] != "child" or binding["turns"]:
                raise
            if not await _remember_child_start_failure(
                binding, f"readiness failed: {exc}"
            ):
                raise ChildStartPending(str(exc)) from exc
            await _settle_child_start_failure(binding)
            raise
    raise AssertionError("unreachable")


async def _with_standing(binding: dict, text: str) -> tuple[str, str | None]:
    """Prefix the first message of a main or child session with its standing context.

    Returns the prompt and the standing hash to record once kagent has accepted it.
    """
    if binding["role"] == "agent" or binding["standing_hash"]:
        return text, None
    from mainloop.runtime.delegation import render_for_binding

    standing = await render_for_binding(binding)
    return f"{standing}\n\n---\n\n{text}", content_hash(standing)


async def _deliver(session_id: str, message_id: str, text: str) -> None:
    _streaming.add(message_id)
    reply: str | None = None
    try:
        async with _lock(session_id):
            # 'recorded' means kagent has never seen the message. Any other state means it was
            # cancelled or another pass already took it: there is nothing to send.
            if await ledger.delivery_state(message_id) != "recorded":
                return
            binding = None
            try:
                binding = await get_binding(session_id)
                await _ensure_kagent_session(binding)  # not attempted => nothing sent
                prompt, standing_hash = await _with_standing(binding, text)
            except ChildStartPending as exc:
                await ledger.transition(
                    message_id,
                    "recorded",
                    from_states=("recorded",),
                    detail=f"startup reconciliation pending: {exc}",
                )
                return
            except ServiceConfigurationError as exc:
                await ledger.transition(
                    message_id,
                    "failed",
                    from_states=("recorded",),
                    detail=f"not sent: {describe_error(exc)}",
                )
                prompt = None
            except Exception as exc:
                logger.exception("delivery not attempted for %s", message_id)
                if (
                    binding is not None
                    and binding["role"] == "child"
                    and not binding["turns"]
                ):
                    if isinstance(exc, ChildStartDisposed):
                        return
                    had_intent = bool(binding.get("child_start_failure"))
                    if not await _remember_child_start_failure(binding, str(exc)):
                        return
                    if (
                        isinstance(exc, ChildStartRejected)
                        and binding["kagent_session_id"] is None
                        and not had_intent
                    ):
                        await _fail_child_start(binding, str(exc))
                        return
                    try:
                        await _settle_child_start_failure(binding)
                    except ChildStartDisposed:
                        return
                    except Exception as pending:
                        # Observation, lifecycle or DB errors are not disposal evidence.
                        # Durable intent and the recorded brief remain retryable.
                        await ledger.transition(
                            message_id,
                            "recorded",
                            from_states=("recorded",),
                            detail=f"startup reconciliation pending: {pending}",
                        )
                        return
                await ledger.transition(
                    message_id,
                    "failed",
                    from_states=("recorded",),
                    detail=f"not sent: {describe_error(exc)}",
                )
                prompt = None
            # The claim is atomic, so a cancel or a second process cannot also send it.
            if prompt is not None and not await ledger.transition(
                message_id, "sending", from_states=("recorded",)
            ):
                return
        if prompt is not None:
            events = get_client().send_message(
                agent_ref(binding["kind"], binding["role"]),
                text=prompt,
                message_id=message_id,
                context_id=binding["kagent_session_id"],
            )
            reply = await _consume(
                session_id, message_id, binding, events, standing_hash=standing_hash
            )
    finally:
        _streaming.discard(message_id)
    # Outside the lock: _after may promote the next queued delivery, which takes it.
    await _after(session_id, reply)


async def _consume(
    session_id: str,
    message_id: str,
    binding: dict,
    events: AsyncIterator[StreamEvent],
    *,
    snapshot: bool = False,
    standing_hash: str | None = None,
) -> str | None:
    """Fold a task event stream into the ledger. Returns the reply if this call completed it.

    ``snapshot`` is for ``SubscribeToTask``: its first event is the current task, which replaces
    the projection rather than extending it. The delivery was already settled, so a failed
    follow is left to the next sync.
    """
    proj = TaskProjection()
    recorded = False
    saved_text = ""
    try:
        async for event in events:
            proj.apply(event)
            if proj.text and proj.text != saved_text:
                await ledger.remember_partial(message_id, proj.text)
                saved_text = proj.text
            if proj.task_id and not recorded:
                recorded = True
                await ledger.transition(
                    message_id,
                    "delivered",
                    from_states=_RESOLVABLE,
                    task_id=proj.task_id,
                    evidence_ref=f"a2a:task/{proj.task_id}",
                )
                if standing_hash:
                    # kagent has the standing context now; never prefix it again.
                    await ledger.update_binding(session_id, standing_hash=standing_hash)
    except ServiceConfigurationError as exc:
        if snapshot or proj.task_id:
            # Existing delivery evidence survives an observation refusal.
            logger.info(
                "control configuration failure following %s: %s", message_id, exc
            )
            return None
        await ledger.transition(
            message_id,
            "failed",
            from_states=_RESOLVABLE + ("recorded",),
            detail=describe_error(exc),
        )
        return None
    except TaskNotFound:
        await ledger.transition(
            message_id,
            "uncertain",
            from_states=_RESOLVABLE,
            detail="the task no longer exists; not replaying",
        )
        return None
    except (SendNotAccepted, Unreachable, A2AError) as exc:
        if snapshot:
            # Following an existing task: the delivery is already settled; the next sync retries.
            logger.info("follow of %s failed: %s", message_id, exc)
            return None
        if isinstance(exc, Unreachable):
            await ledger.transition(
                message_id,
                "failed",
                from_states=_RESOLVABLE + ("recorded",),
                detail=f"not sent: {describe_error(exc)}",
            )
            return None
        if proj.task_id:
            return await _resolve(session_id, message_id, binding, proj, str(exc))
        if isinstance(exc, SendNotAccepted):
            # kagent accepted nothing, even after the same-message retries: a definite non-delivery.
            detail = (
                f"not sent: kagent did not accept the message ({describe_error(exc)})"
            )
        else:
            detail = f"send rejected: {describe_error(exc)}"
        await ledger.transition(
            message_id,
            "failed",
            from_states=_RESOLVABLE + ("recorded",),
            detail=detail,
        )
        return None
    except OutcomeUnknown as exc:
        logger.info("stream for %s broke: %s", message_id, exc)
        return await _resolve(session_id, message_id, binding, proj, str(exc))
    except Exception as exc:
        logger.exception("delivery outcome unknown for %s", message_id)
        await ledger.transition(
            message_id,
            "uncertain",
            from_states=_RESOLVABLE,
            detail=f"unexpected error after send: {exc}",
        )
        return None
    if proj.terminal:
        return await _finalize(session_id, message_id, proj)
    if proj.task_id is None and not proj.parked:
        # The stream ended without ever naming a task: the outcome is unobserved.
        return await _resolve(session_id, message_id, binding, proj, "stream ended")
    return None


async def _resolve(
    session_id: str,
    message_id: str,
    binding: dict,
    proj: TaskProjection,
    why: str,
) -> str | None:
    """After an ambiguous outcome, observe the task. Never re-sends.

    With a task id the current task replaces the projection. Without one, ``ListTasks`` is
    searched for the message id; if nothing shows the message, the delivery is ``uncertain``.
    """
    agent = agent_ref(binding["kind"], binding["role"])
    try:
        client = get_client()
        if proj.task_id:
            task = await client.get_task(agent, proj.task_id)
        else:
            task = await client.find_task_for_message(
                agent, binding["kagent_session_id"], message_id
            )
    except (KagentError, TaskNotFound) as exc:
        await ledger.transition(
            message_id,
            "uncertain",
            from_states=_RESOLVABLE,
            detail=f"transport error, outcome unknown ({why}); not replaying: {exc}",
        )
        return None
    if task is None:
        await ledger.transition(
            message_id,
            "uncertain",
            from_states=_RESOLVABLE,
            detail=f"no task shows this message after: {why}; not replaying",
        )
        return None
    proj.replace(task)
    await ledger.transition(
        message_id,
        "delivered",
        from_states=_RESOLVABLE,
        task_id=task.id,
        evidence_ref=f"a2a:task/{task.id}",
    )
    if proj.terminal:
        return await _finalize(session_id, message_id, proj)
    return None


def _stop_note_id(message_id: str) -> str:
    return str(uuid.uuid5(_NS, f"turn-cancelled:{message_id}"))


def stopped_message(partial: str | None) -> str:
    """Return the conversation message of a stopped turn: the partial reply, then the stop note.

    The note is always the last paragraph, so a client can show the text above it marked as stopped.
    """
    partial = (partial or "").strip()
    return f"{partial}\n\n{TURN_STOPPED_NOTE}" if partial else TURN_STOPPED_NOTE


async def _settle_cancelled(
    session_id: str,
    message_id: str,
    task_id: str | None,
    *,
    from_states: tuple[str, ...],
    detail: str,
    partial: str | None = None,
) -> bool:
    """Close a delivery as ``cancelled`` with its conversation note, atomically. True when this
    call did it (and so announces it); false when the delivery had already left ``from_states``.
    ``partial`` is the reply streamed before the stop; it is kept above the note.
    """
    session = await db.get_session(session_id)
    note_id = _stop_note_id(message_id)
    moved = await ledger.settle_cancelled(
        message_id,
        from_states=from_states,
        conversation_id=session.conversation_id,
        note_id=note_id,
        note=TURN_STOPPED_NOTE,
        task_id=task_id,
        detail=detail,
        partial=partial,
    )
    if moved:
        await notify_session_message(session.user_id, session_id, note_id, "assistant")
    return moved


async def _finalize(
    session_id: str, message_id: str, proj: TaskProjection
) -> str | None:
    """Close the delivery from a terminal projection and mirror the reply, once.

    Returns the reply when the task completed (for the child fallback report).
    """
    state = proj.normalised_state
    if state == "canceled":
        # Whoever observes the cancellation first (a stop, the stream, a sync) settles it the
        # same way; the others find the delivery already terminal. The partial reply is kept.
        await _settle_cancelled(
            session_id,
            message_id,
            proj.task_id,
            from_states=_RESOLVABLE,
            detail="task was cancelled",
            partial=proj.text,
        )
        return None
    if state == "completed":
        new_state, detail = "completed", None
    else:
        new_state = "failed"
        detail = f"task {state}: {proj.failure_text.strip() or 'kagent gave no reason'}"
    moved = await ledger.transition(
        message_id,
        new_state,
        from_states=_RESOLVABLE,
        task_id=proj.task_id,
        evidence_ref=f"a2a:task/{proj.task_id}" if proj.task_id else None,
        detail=detail,
    )
    if not moved:
        return None
    reply = proj.text
    if reply:
        session = await db.get_session(session_id)
        reply_id = assistant_message_id(session_id, proj.task_id or message_id)
        if await ledger.mirror_reply(session.conversation_id, reply_id, reply):
            await notify_session_message(
                session.user_id, session_id, reply_id, "assistant"
            )
    if state == "completed":
        await ledger.bump_turns(session_id)
        return reply or None
    return None


async def _after(session_id: str, reply: str | None) -> None:
    """Follow-up actions that are only safe outside the stream: status, the child fallback
    report, and the next queued delivery."""
    binding = await get_binding(session_id)
    session = await db.get_session(session_id)
    if binding is None or session is None:
        return
    open_n = await ledger.open_count(session_id)
    is_child = binding["role"] == "child"
    new_status = next_status(
        session.status,
        turn_open=bool(open_n),
        is_child=is_child,
        reported=bool(binding["reported_at"]),
    )
    if session.status != new_status:
        await db.update_session(session_id, status=new_status)
    if (
        is_child
        and reply
        and new_status not in ENDED_STATUSES
        and binding["reported_at"] is None
    ):
        from mainloop.runtime.delegation import auto_report

        await auto_report(session_id, reply)
    if open_n == 0:
        await _promote_queued(session_id)


async def _promote_queued(session_id: str) -> None:
    async with _lock(session_id):
        promoted = await ledger.promote_queued(session_id)
    if promoted is not None:
        _spawn_deliver(session_id, *promoted)


# --------------------------------------------------------------------------------------------
# Sync / reconcile
# --------------------------------------------------------------------------------------------


async def sync(session_id: str) -> None:
    """Observe the A2A task of every unresolved delivery, replace the projection with it, and
    run the follow-ups. Safe to call at any time and from several places."""
    binding = await get_binding(session_id)
    if binding is None:
        return
    for message_id, text in await ledger.recorded_deliveries(session_id):
        if message_id not in _streaming:
            # Left 'recorded' by a restart before the send: kagent never saw it, so delivering it
            # now is the first send, not a replay. Without this the session stays busy for good.
            _spawn_deliver(session_id, message_id, text)
    if binding["kagent_session_id"] is None:
        return
    reply: str | None = None
    for delivery in await ledger.resolvable_deliveries(session_id):
        if delivery["message_id"] in _streaming:
            continue
        reply = await _observe(session_id, binding, delivery) or reply
    await _after(session_id, reply)


async def _observe(session_id: str, binding: dict, delivery: dict) -> str | None:
    message_id = delivery["message_id"]
    agent = agent_ref(binding["kind"], binding["role"])
    client = get_client()
    try:
        if delivery["task_id"]:
            task = await client.get_task(agent, delivery["task_id"])
        else:
            task = await client.find_task_for_message(
                agent, binding["kagent_session_id"], message_id
            )
    except TaskNotFound:
        await ledger.transition(
            message_id,
            "uncertain",
            from_states=_RESOLVABLE,
            detail="the task no longer exists; not replaying",
        )
        return None
    except KagentError as exc:
        logger.info("sync of %s skipped: %s", message_id, exc)
        if not isinstance(exc, Unreachable) and await _session_gone(binding):
            # Its tasks went with the Session; nothing will ever show this message again.
            await ledger.transition(
                message_id,
                "uncertain",
                from_states=_RESOLVABLE,
                detail="the kagent Session was deleted; not replaying",
            )
        else:
            await _expire_sending(delivery, "the task lookup keeps failing")
        return None
    if task is None:
        await _expire_sending(delivery, "no task shows this message after send")
        return None
    proj = TaskProjection()
    proj.replace(task)
    await ledger.transition(
        message_id,
        "delivered",
        from_states=_RESOLVABLE,
        task_id=task.id,
        evidence_ref=f"a2a:task/{task.id}",
    )
    if proj.terminal:
        return await _finalize(session_id, message_id, proj)
    if not proj.parked and message_id not in _streaming:
        _streaming.add(message_id)
        _spawn(_follow(session_id, message_id, binding, task.id))
    return None


async def _session_gone(binding: dict) -> bool:
    try:
        return await _live_session(binding["kagent_session_id"]) is None
    except KagentError:
        return False


async def _expire_sending(delivery: dict, why: str) -> None:
    """Expire a ``sending`` delivery with no receipt after the grace period to ``uncertain``, so a
    failing or empty lookup cannot keep the session busy for good. It is still never replayed.
    """
    if (
        delivery["state"] == "sending"
        and datetime.now(UTC) - delivery["updated_at"] > SEND_RECEIPT_GRACE
    ):
        await ledger.transition(
            delivery["message_id"],
            "uncertain",
            from_states=("sending",),
            detail=f"{why}; not replaying",
        )


async def _follow(
    session_id: str, message_id: str, binding: dict, task_id: str
) -> None:
    """Reattach to a running task after a restart or a dropped stream (SubscribeToTask)."""
    try:
        events = get_client().subscribe_to_task(
            agent_ref(binding["kind"], binding["role"]), task_id
        )
        reply = await _consume(session_id, message_id, binding, events, snapshot=True)
    finally:
        _streaming.discard(message_id)
    await _after(session_id, reply)


class StopUnconfirmed(Exception):
    """kagent did not confirm the stop. The ledger was not touched; it is safe to try again."""


async def stop_turn(session_id: str) -> str:
    """Stop the open turn with CancelTask, keeping the session and its kagent Session.

    The delivery ends as ``cancelled`` (one transaction with the conversation note) only after
    kagent returned the task cancelled; nothing is written before that, so a kagent error leaves
    the ledger as it was (the error propagates). The next message is a new task on the same
    Session. Returns ``stopped``, ``finished`` (the turn completed or failed before the cancel
    reached it, and was recorded as that) or ``no_open_turn``. The partial reply is kept above the
    stop note, and messages queued behind the turn wait for the owner's next message. Raises :class:`StopUnconfirmed`
    when kagent shows no task for a send in flight yet, or leaves the task running.
    """
    reply: str | None = None
    async with _lock(session_id):
        binding = await get_binding(session_id)
        if binding is None:
            raise ValueError("Session has no native agent binding")
        outcome = "no_open_turn"
        # Stop only the turn this request observed. Another process may accept the owner's
        # next message after cancellation commits; this stop must never cancel that new turn.
        open_now = [
            d for d in await ledger.deliveries(session_id) if d["state"] in OPEN_STATES
        ]
        for delivery in open_now:
            result, finished_reply = await _stop_delivery(session_id, binding, delivery)
            reply = finished_reply or reply
            if outcome != "stopped":
                outcome = result
    # Outside the lock, as after any turn: status and the child fallback report. The queue stays
    # held, so no queued delivery starts a turn here.
    await _after(session_id, reply)
    return outcome


async def _stop_delivery(
    session_id: str, binding: dict, delivery: dict, *, retried: bool = False
) -> tuple[str, str | None]:
    message_id = delivery["message_id"]
    if delivery["state"] == "recorded":
        # kagent never saw it: there is no task to cancel.
        moved = await _settle_cancelled(
            session_id,
            message_id,
            None,
            from_states=("recorded",),
            detail="stopped by user before it was sent",
        )
        if not moved and not retried:
            # Another writer (the MCP container) may have claimed it to ``sending`` after this
            # stop read it, so kagent can now hold a task for it. Re-read and stop it as the
            # in-flight turn it has become, once; reporting ``finished`` would leave it running.
            current = next(
                (
                    d
                    for d in await ledger.deliveries(session_id)
                    if d["message_id"] == message_id
                ),
                None,
            )
            if current is not None and current["state"] in ("sending", "delivered"):
                fresh = await get_binding(session_id) or binding
                return await _stop_delivery(session_id, fresh, current, retried=True)
        return await _stop_result(message_id), None
    if binding["kagent_session_id"] is None:
        raise StopUnconfirmed(
            "The agent session is not ready, so the turn cannot be stopped."
        )
    agent = agent_ref(binding["kind"], binding["role"])
    client = get_client()
    task_id = delivery["task_id"]
    if task_id is None:
        task = await client.find_task_for_message(
            agent, binding["kagent_session_id"], message_id
        )
        if task is None:
            raise StopUnconfirmed(
                "The message was just sent and kagent shows no task for it yet; try again."
            )
        task_id = task.id
    task = await client.cancel_task(agent, task_id)
    proj = TaskProjection()
    proj.replace(task)
    if proj.normalised_state == "canceled":
        await _settle_cancelled(
            session_id,
            message_id,
            task_id,
            from_states=OPEN_STATES,
            detail="stopped by user",
            partial=await _partial_reply(client, agent, task_id, proj),
        )
        return await _stop_result(message_id), None
    if proj.terminal:
        # The turn ended first (kagent returns a finished task unchanged): record how it ended.
        return "finished", await _finalize(session_id, message_id, proj)
    raise StopUnconfirmed(
        f"kagent still shows the task as {proj.normalised_state or 'unknown'} after the cancel."
    )


async def _partial_reply(
    client: KagentClient, agent: AgentRef, task_id: str, proj: TaskProjection
) -> str:
    """Return the reply streamed before a stop: from the cancel response, else the task read
    again (a cancel response may leave the artifacts out). A failed read keeps nothing.
    """
    if proj.text:
        return proj.text
    try:
        again = TaskProjection()
        again.replace(await client.get_task(agent, task_id))
        return again.text
    except KagentError:
        return ""


async def _stop_result(message_id: str) -> str:
    """Report ``stopped`` when the delivery is cancelled (by this call or by a concurrent
    observer of the same cancellation), otherwise ``finished``."""
    return (
        "stopped"
        if await ledger.delivery_state(message_id) == "cancelled"
        else "finished"
    )


async def cancel(session_id: str) -> str:
    """End a native session: cancel its open tasks and close its open deliveries.

    The status is set first and is sticky, so no later sync brings the session back, and open
    deliveries are failed so the reconcile loop stops visiting it. Returns ``stopped``,
    ``not_running`` (nothing was running) or ``unknown`` (a cancel could not be confirmed; the
    agent may still be running, and it is not retried blindly).
    """
    binding = await get_binding(session_id)
    if binding is not None and binding["role"] == "main":
        raise ValueError("The main thread cannot be cancelled.")
    async with _lock(session_id):
        await db.update_session(session_id, status=SessionStatus.CANCELLED)
        opened = await ledger.fail_open(session_id, "cancelled by user")
        if binding is None or binding["kagent_session_id"] is None:
            return "not_running"
        agent = agent_ref(binding["kind"], binding["role"])
        client = get_client()
        outcome = "not_running"
        for delivery in opened:
            if delivery["state"] == "queued" or delivery["state"] == "recorded":
                continue  # never sent
            try:
                task_id = delivery["task_id"]
                if task_id is None:
                    task = await client.find_task_for_message(
                        agent, binding["kagent_session_id"], delivery["message_id"]
                    )
                    task_id = task.id if task else None
                if task_id is None:
                    if delivery["state"] == "sending":
                        # The send may still land and start a task this cancel cannot see.
                        outcome = "unknown"
                    continue
                await client.cancel_task(agent, task_id)
                outcome = "stopped" if outcome != "unknown" else outcome
            except KagentError as exc:
                logger.warning(
                    "cancel of %s: task cancel unconfirmed: %s", session_id, exc
                )
                outcome = "unknown"
        return outcome


async def delete_kagent_session(session_id: str) -> bool:
    """Delete the kagent Session of an archived (or deleted) session. True once kagent confirmed.

    Not confirmed (kagent unreachable, outcome unknown) leaves ``kagent_deleted_at`` unset, and
    ``reconcile_archived_deletes`` retries; DeleteSession is idempotent. A Session kagent no
    longer knows counts as deleted. A turn open or queued (a message recorded just before the
    archive) is not cut off: nothing is deleted and False is returned, so the same retry deletes
    the Session once the turn has settled. ``submit_message`` takes this lock too, and refuses
    an archived session, so no turn can start after this check.
    """
    async with _lock(session_id):
        binding = await get_binding(session_id)
        if binding is None or binding["kagent_session_id"] is None:
            return True
        if await ledger.active_count(session_id):
            logger.info("kagent delete of %s waits for its open turn", session_id)
            return False
        try:
            await get_client().delete_session(binding["kagent_session_id"])
        except SessionError as exc:
            if exc.grpc_status != 5:  # NOT_FOUND: already gone
                _log_step_failure("archived_delete", session_id, exc)
                return False
        except KagentError as exc:
            _log_step_failure("archived_delete", session_id, exc)
            return False
        await ledger.mark_kagent_deleted(session_id)
        return True


async def reconcile_archived_deletes() -> None:
    for row in await ledger.undeleted_archived():
        sid = row["session_id"]
        await _reconcile_step(
            "archived_delete", lambda sid=sid: delete_kagent_session(sid), sid
        )


def _log_step_failure(step: str, session_id: str | None, exc: BaseException) -> None:
    """Log one structured event for a failed reconcile step."""
    logger.error(
        "reconcile step failed: step=%s session_id=%s error_class=%s",
        step,
        session_id or "-",
        type(exc).__name__,
        exc_info=exc,
        extra={
            "event": "reconcile_step_failed",
            "step": step,
            "session_id": session_id or "-",
            "error_class": type(exc).__name__,
        },
    )


async def _reconcile_step(step: str, run, session_id: str | None = None) -> None:
    """Run one reconcile step; a failure is logged and never stops the steps after it."""
    try:
        await run()
    except asyncio.CancelledError:
        raise
    except Exception as exc:
        _log_step_failure(step, session_id, exc)


async def reconcile_once(*, sweep: bool) -> None:
    """Run one reconcile pass.

    Every step has its own ``try``: one session's failed sync, or a failing sweep step, must not
    starve the others. ``sweep`` adds the slower housekeeping steps.
    """
    from mainloop.runtime.hitl_continuation import reconcile_hitl_responses
    from mainloop.runtime.hitl_observer import observe_hitl_once

    await _reconcile_step("hitl_observation", observe_hitl_once)
    await _reconcile_step("hitl_responses", reconcile_hitl_responses)
    sids: list[str] = []

    async def list_open() -> None:
        sids.extend(await ledger.sessions_with_open_work())

    await _reconcile_step("list_open_work", list_open)
    for sid in sids:
        await _reconcile_step("sync", lambda sid=sid: sync(sid), sid)
    if not sweep:
        return
    from mainloop.runtime import workspaces
    from mainloop.runtime.agent_credentials import reconcile_cleanup

    await _reconcile_step("credential_cleanup", reconcile_cleanup)
    await _reconcile_step("archived_deletes", reconcile_archived_deletes)
    await _reconcile_step("suspend_idle", workspaces.suspend_idle)


async def reconcile_loop(interval: float = 3.0) -> None:
    """Background mirror for sessions with open work, so replies and reports do not depend on a
    browser polling."""
    next_idle_check = 0.0
    while True:
        loop = asyncio.get_running_loop()
        due = loop.time() >= next_idle_check
        try:
            await reconcile_once(sweep=due)
        except Exception:
            logger.exception("reconcile loop iteration failed")
        if due:
            next_idle_check = loop.time() + settings.workspace_idle_check_seconds
        await asyncio.sleep(interval)


async def identity(session_id: str) -> NativeSessionInfo | None:
    binding = await get_binding(session_id)
    if binding is None:
        return None
    rows = await ledger.deliveries(session_id)
    topic = (
        await ledger.topic_name(binding["topic_id"]) if binding["topic_id"] else None
    )
    deliveries = [
        NativeDeliveryInfo(
            message_id=r["message_id"],
            state=r["state"],
            task_id=r["task_id"],
            evidence_ref=r["evidence_ref"],
            detail=safe_detail(r["detail"]),
            source=r["source"],
        )
        for r in rows
    ]
    state, note = None, None
    if binding["kagent_session_id"]:
        try:
            state = (
                await get_client().get_session(binding["kagent_session_id"])
            ).state.name.lower()
        except Unreachable as exc:
            note = f"kagent unreachable: {exc}"
        except KagentError as exc:
            note = f"kagent session unavailable: {exc}"
    if any(d.state == "uncertain" for d in deliveries):
        note = "delivery unknown: the last message was not replayed; check the reply, then send again if needed"
    return NativeSessionInfo(
        session_id=session_id,
        kind=binding["kind"],
        role=binding["role"],
        parent_session_id=binding["parent_session_id"],
        topic=topic,
        agent_name=agent_name(binding["kind"], binding["role"]),
        kagent_session_id=binding["kagent_session_id"],
        session_state=state,
        model=binding["model"],
        turns=binding["turns"],
        turn_in_flight=any(d.state in OPEN_STATES for d in deliveries),
        deliveries=deliveries,
        note=note,
    )
