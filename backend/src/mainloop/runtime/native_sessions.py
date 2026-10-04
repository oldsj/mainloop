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
import logging
import uuid
from collections.abc import AsyncIterator, Coroutine
from datetime import UTC, datetime, timedelta
from typing import Any

from mainloop.config import settings
from mainloop.db import db
from mainloop.runtime import workspace_adapter
from mainloop.runtime.agent_api import hash_token, token_for
from mainloop.runtime.kagent_client import (
    A2AError,
    AgentRef,
    KagentClient,
    KagentError,
    KagentSession,
    OutcomeUnknown,
    RuntimeState,
    SendNotAccepted,
    SessionError,
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


def agent_name(kind: str) -> str:
    """Return the kagent Agent that runs a native agent kind."""
    if kind == "claude":
        return settings.kagent_claude_agent
    if kind == "codex":
        return settings.kagent_codex_agent
    raise ValueError(f"no kagent Agent is configured for native agent {kind}")


def agent_ref(kind: str) -> AgentRef:
    return AgentRef(settings.kagent_namespace, agent_name(kind))


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
    ) -> None:
        await connection.execute(
            """INSERT INTO native_bindings (session_id, kind, role, parent_session_id, topic_id, token_hash)
               VALUES ($1,$2,$3,$4,$5,$6)""",
            session_id,
            kind,
            role,
            parent_session_id,
            topic_id,
            token_hash,
        )

    async def update_binding(self, session_id: str, **fields) -> None:
        if not fields:
            return
        sets = [f"{k}=${i + 2}" for i, k in enumerate(fields)]
        sets.append("updated_at=NOW()")
        async with db.connection() as conn:
            await conn.execute(
                f"UPDATE native_bindings SET {', '.join(sets)} WHERE session_id=$1",  # nosec B608 - column names come from code, values are bound
                session_id,
                *fields.values(),
            )

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
            async with conn.transaction():
                moved = await conn.fetchval(
                    """UPDATE native_bindings
                       SET kagent_session_id=NULL, kagent_request_id=$3, standing_hash=NULL,
                           updated_at=NOW()
                       WHERE session_id=$1 AND kagent_session_id IS NOT DISTINCT FROM $2
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

    async def record_message(
        self,
        *,
        session_id: str,
        conversation_id: str,
        text: str,
        state: str,
        source: str,
    ) -> str:
        """Record the message and delivery under the workspace lock used by suspension."""
        async with db.connection() as conn:
            async with conn.transaction():
                binding = await conn.fetchrow(
                    """SELECT workspace_id,desired_state FROM workspace_bindings
                       WHERE workspace_id=$1 FOR UPDATE""",
                    session_id,
                )
                if binding and binding.get("desired_state") == "deleting":
                    raise ValueError(
                        "The workspace is being deleted; start another workspace."
                    )
                lifecycle = (
                    await conn.fetchrow(
                        """SELECT desired_state, observed_state FROM workspace_lifecycles
                           WHERE workspace_id=$1""",
                        session_id,
                    )
                    if binding
                    else None
                )
                if lifecycle and (
                    lifecycle["desired_state"] == "suspended"
                    or lifecycle["observed_state"] in {"suspending", "suspended"}
                ):
                    raise ValueError(
                        "The workspace is suspending or suspended; resume it before sending a message."
                    )

                message = await db.create_message(
                    conversation_id=conversation_id,
                    role="user",
                    content=text,
                    conn=conn,
                )
                await conn.execute(
                    "INSERT INTO native_deliveries (message_id, session_id, state, source) VALUES ($1,$2,$3,$4)",
                    message.id,
                    session_id,
                    state,
                    source,
                )
                if binding:
                    await conn.execute(
                        "UPDATE workspace_lifecycles SET last_activity_at=NOW(), updated_at=NOW() WHERE workspace_id=$1",
                        session_id,
                    )
        return message.id

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

    async def set_delivery(
        self,
        message_id: str,
        state: str,
        *,
        task_id: str | None = None,
        evidence_ref: str | None = None,
        detail: str | None = None,
    ) -> None:
        async with db.connection() as conn:
            await conn.execute(
                """UPDATE native_deliveries SET state=$2, task_id=COALESCE($3, task_id),
                   evidence_ref=COALESCE($4, evidence_ref),
                   detail=CASE WHEN $2 IN ('delivered', 'completed') THEN $5 ELSE COALESCE($5, detail) END,
                   updated_at=NOW()
                   WHERE message_id=$1""",
                message_id,
                state,
                task_id,
                evidence_ref,
                detail,
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
        async with db.connection() as conn:
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
        """Mark the oldest queued delivery ``recorded`` if (and only if) nothing is open. Atomic in
        SQL, and serialised with ``submit_message`` by the per-session lock."""
        async with db.connection() as conn:
            row = await conn.fetchrow(
                """UPDATE native_deliveries SET state='recorded', updated_at=NOW()
                   WHERE message_id = (SELECT message_id FROM native_deliveries
                                       WHERE session_id=$1 AND state='queued' ORDER BY created_at LIMIT 1)
                     AND state='queued'
                     AND NOT EXISTS (SELECT 1 FROM native_deliveries WHERE session_id=$1 AND state = ANY($2))
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
                """SELECT DISTINCT session_id FROM native_deliveries
                   WHERE state IN ('recorded','sending','delivered','queued')
                      OR (state='uncertain' AND updated_at > NOW() - INTERVAL '30 minutes')"""
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
    conn=None,
) -> dict:
    agent_name(kind)  # an unconfigured kind fails here, before a row exists
    token_hash = (
        hash_token(token_for(session_id)) if role in ("main", "child") else None
    )
    fields = dict(
        session_id=session_id,
        kind=kind,
        role=role,
        parent_session_id=parent_session_id,
        topic_id=topic_id,
        token_hash=token_hash,
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
    # Branch workspace turns touch and wake their actor before the delivery is recorded. Sessions
    # without a workspace binding have nothing to wake.
    if await workspace_adapter.get_workspace(session_id) is not None:
        await workspace_adapter.touch_workspace(session_id, reason="turn")
    if source == "user" and session.status in ENDED_STATUSES:
        raise ValueError(f"This session is {session.status.value}; start a new one.")
    # The in-flight check and the ledger insert are one critical section (per session), so two
    # concurrent submissions cannot both see an idle agent and interleave in one task.
    async with _lock(session_id):
        busy = await ledger.open_count(session_id)
        # An 'uncertain' delivery does not block: the user decides whether to send again.
        if busy and source in ("user", "brief"):
            raise ValueError(
                "A previous message is still in flight; wait for its reply before sending another."
            )
        state = "queued" if busy else "recorded"
        message_id = await ledger.record_message(
            session_id=session_id,
            conversation_id=session.conversation_id,
            text=text,
            state=state,
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


async def _ensure_kagent_session(binding: dict) -> KagentSession:
    """Return the binding's kagent Session, ready for a turn.

    It is created on first use and resumed if suspended. A Session kagent has deleted (the idle
    TTL, or out of band) is replaced once, under a fresh create request id; the replacement gets
    the standing context again, because ``standing_hash`` belongs to the Session it went to.
    """
    client = get_client()
    for replaced in (False, True):
        if binding["kagent_session_id"] is None:
            try:
                session = await client.create_session(
                    agent_ref(binding["kind"]), request_id=_request_id(binding)
                )
            except SessionError as exc:
                if replaced or not _create_hit_deleted(exc):
                    raise
                await _replace_kagent_session(binding)
                continue
            await ledger.update_binding(
                binding["session_id"], kagent_session_id=session.id, standing_hash=None
            )
            binding.update(kagent_session_id=session.id, standing_hash=None)
        else:
            live = await _live_session(binding["kagent_session_id"])
            if live is None:
                if replaced:
                    raise SessionError("the replacement kagent Session is already gone")
                await _replace_kagent_session(binding)
                continue
            session = live
        return await client.ensure_ready(
            session, timeout=settings.kagent_session_ready_timeout_seconds
        )
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
            try:
                binding = await get_binding(session_id)
                await _ensure_kagent_session(binding)  # not attempted => nothing sent
                prompt, standing_hash = await _with_standing(binding, text)
            except Exception as exc:
                logger.exception("delivery not attempted for %s", message_id)
                await ledger.transition(
                    message_id,
                    "failed",
                    from_states=("recorded",),
                    detail=f"not sent: {type(exc).__name__}: {exc}",
                )
                prompt = None
            # The claim is atomic, so a cancel or a second process cannot also send it.
            if prompt is not None and not await ledger.transition(
                message_id, "sending", from_states=("recorded",)
            ):
                return
        if prompt is not None:
            events = get_client().send_message(
                agent_ref(binding["kind"]),
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
    try:
        async for event in events:
            proj.apply(event)
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
                detail=f"not sent: {exc}",
            )
            return None
        if proj.task_id:
            return await _resolve(session_id, message_id, binding, proj, str(exc))
        if isinstance(exc, SendNotAccepted):
            # kagent accepted nothing, even after the same-message retries: a definite non-delivery.
            detail = f"not sent: kagent did not accept the message ({exc.message})"
        else:
            detail = f"send rejected: {exc.message}"
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
    agent = agent_ref(binding["kind"])
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


async def _finalize(
    session_id: str, message_id: str, proj: TaskProjection
) -> str | None:
    """Close the delivery from a terminal projection and mirror the reply, once.

    Returns the reply when the task completed (for the child fallback report).
    """
    state = proj.normalised_state
    if state == "completed":
        new_state, detail = "completed", None
    elif state == "canceled":
        new_state, detail = "failed", "task was cancelled"
    else:
        new_state = "failed"
        detail = f"task {state}: {proj.failure_text}".rstrip(": ")
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
    agent = agent_ref(binding["kind"])
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
        events = get_client().subscribe_to_task(agent_ref(binding["kind"]), task_id)
        reply = await _consume(session_id, message_id, binding, events, snapshot=True)
    finally:
        _streaming.discard(message_id)
    await _after(session_id, reply)


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
        agent = agent_ref(binding["kind"])
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


async def reconcile_loop(interval: float = 3.0) -> None:
    """Background mirror for sessions with open work, so replies and reports do not depend on a
    browser polling."""
    next_idle_check = 0.0
    while True:
        try:
            for sid in await ledger.sessions_with_open_work():
                await sync(sid)
            loop = asyncio.get_running_loop()
            if loop.time() >= next_idle_check:
                await workspace_adapter.suspend_idle_workspaces()
                next_idle_check = loop.time() + 60.0
        except Exception:
            logger.exception("reconcile loop iteration failed")
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
            detail=r["detail"],
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
        agent_name=agent_name(binding["kind"]),
        kagent_session_id=binding["kagent_session_id"],
        session_state=state,
        model=binding["model"],
        turns=binding["turns"],
        turn_in_flight=any(d.state in (*OPEN_STATES, "queued") for d in deliveries),
        deliveries=deliveries,
        note=note,
    )
