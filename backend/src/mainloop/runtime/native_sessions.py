"""Sessions bound to a real native agent (Claude Code / Codex) under Herdr in the workspace pod.

Control-plane rules implemented here:
- A user message is recorded, then a delivery row is persisted as ``sending`` *before* the
  transport is touched. Each prompt is sent once; a transport error leaves it ``uncertain``
  ("delivery unknown") and it is never replayed automatically.
- The native journal is the receipt: a prompt record after the recorded cursor proves delivery,
  the turn-completion record proves completion, and the assistant text in between is mirrored
  into the session conversation (deterministic ids, so repeated syncs are idempotent).
- After pod replacement the agent is not live in Herdr; the next delivery restarts it with the
  native resume flag against the same native session id, then sends.

Context model (plan r7): a binding has a ``role``. ``main`` is the conversation agent whose window
Mainloop owns by rotation (a lineage of disposable native sessions; ``rotate``); ``child`` is a
delegated worker with a parent and a topic; ``agent`` is the r6 stand-alone session. Reports and
the pre-cut write-out are ordinary ledgered deliveries; a delivery that arrives while another is
open is ``queued`` by the control plane (E4: a mid-turn paste interleaves) and sent when idle.
"""

from __future__ import annotations

import asyncio
import base64
import logging
import uuid
from datetime import UTC, datetime, timedelta

from mainloop.config import settings
from mainloop.db import db
from mainloop.runtime.agent_api import hash_token, token_for
from mainloop.runtime.herdr import HerdrWorkspace, TransportError, WorkspaceUnavailable
from mainloop.runtime.journal import completed_turns, parse_journal
from mainloop.runtime.standing import content_hash

from models import NativeDeliveryInfo, NativeSessionInfo, SessionStatus

logger = logging.getLogger(__name__)

APPROVAL_POLICY = "bypass-permissions"
SEND_RECEIPT_GRACE = timedelta(seconds=60)
# A prompt seen in the journal whose turn never completes (agent exited or wedged, pod replaced):
# after this long, or as soon as the agent is no longer live, it becomes 'uncertain' (never
# replayed, never blocking) instead of holding the session in flight forever.
DELIVERED_MAX_AGE = timedelta(minutes=30)
_NS = uuid.UUID("6f0f7f0e-3f1e-4a3c-9d3b-0e4b6f5c2a11")
_locks: dict[str, asyncio.Lock] = {}
_workspaces: dict[str, HerdrWorkspace] = {}
_rotating: set[str] = set()
OPEN_STATES = ("recorded", "sending", "delivered")
WRITEOUT_TEXT = (
    "[mainloop:pre-cut] Your context window is about to be reset by Mainloop. Write out anything "
    "durable now with `mainloop note`, `mainloop decide` and `mainloop pending` (one command each), "
    "then reply with the single word: done"
)


def is_rotating(session_id: str) -> bool:
    return session_id in _rotating


def is_rotating(session_id: str) -> bool:
    return session_id in _rotating


def workspace_for(binding: dict) -> HerdrWorkspace:
    """One Herdr workspace pod per binding: ``main-0`` for the main thread, else ``workspace-0``."""
    pod = binding.get("pod") or settings.workspace_pod
    if pod not in _workspaces:
        _workspaces[pod] = HerdrWorkspace(pod=pod)
    return _workspaces[pod]


def rotation_due(
    *,
    context_tokens: int | None,
    baseline_tokens: int | None,
    turns: int,
    budget_tokens: int,
    budget_turns: int,
) -> str | None:
    """Deterministic rotation trigger. Tokens are measured above the lineage's first-turn baseline
    (a trivial Claude session already holds ~10-20k tokens of tools and system prompt).
    """
    if context_tokens is not None and baseline_tokens is not None:
        grown = context_tokens - baseline_tokens
        if grown >= budget_tokens:
            return f"tokens: context grew {grown} >= {budget_tokens} over baseline {baseline_tokens}"
    if turns >= budget_turns:
        return f"turns: {turns} >= {budget_turns}"
    return None


def _lock(session_id: str) -> asyncio.Lock:
    return _locks.setdefault(session_id, asyncio.Lock())


def agent_name(session_id: str, kind: str) -> str:
    return f"ml-{kind}-{session_id[:8]}"


async def get_binding(session_id: str) -> dict | None:
    async with db.connection() as conn:
        row = await conn.fetchrow(
            "SELECT * FROM native_bindings WHERE session_id=$1", session_id
        )
    return dict(row) if row else None


async def _update_binding(session_id: str, **fields) -> None:
    sets = ", ".join(f"{k}=${i + 2}" for i, k in enumerate(fields))
    async with db.connection() as conn:
        await conn.execute(
            f"UPDATE native_bindings SET {sets}, updated_at=NOW() WHERE session_id=$1",  # nosec B608 - column names come from code, values are bound
            session_id,
            *fields.values(),
        )


async def _set_delivery(
    message_id: str,
    state: str,
    *,
    evidence_ref: str | None = None,
    detail: str | None = None,
    cursor_before: int | None = None,
) -> None:
    async with db.connection() as conn:
        await conn.execute(
            """UPDATE native_deliveries SET state=$2, evidence_ref=COALESCE($3, evidence_ref),
               detail=COALESCE($4, detail), cursor_before=COALESCE($5, cursor_before), updated_at=NOW()
               WHERE message_id=$1""",
            message_id,
            state,
            evidence_ref,
            detail,
            cursor_before,
        )


def config_name(binding: dict) -> str:
    """Agentctl binding config (ConfigMap ``<name>.env``) for this binding."""
    if binding["role"] == "main":
        return "claude-main"
    if binding["role"] == "child":
        return f"{binding['kind']}-child"
    return binding["kind"]


async def create_binding(
    session_id: str,
    kind: str,
    *,
    role: str = "agent",
    parent_session_id: str | None = None,
    topic_id: str | None = None,
) -> dict:
    # Claude takes the native session id up front (--session-id); Codex reports it in its journal.
    native_id = str(uuid.uuid4()) if kind == "claude" else None
    name = "ml-main" if role == "main" else agent_name(session_id, kind)
    pod = settings.main_pod if role == "main" else None
    token_hash = (
        hash_token(token_for(session_id)) if role in ("main", "child") else None
    )
    async with db.connection() as conn:
        await conn.execute(
            """INSERT INTO native_bindings (session_id, kind, agent_name, native_session_id, approval_policy,
                   role, pod, parent_session_id, topic_id, token_hash, model)
               VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)""",
            session_id,
            kind,
            name,
            native_id,
            APPROVAL_POLICY if role != "main" else "restricted: Bash(mainloop:*) only",
            role,
            pod,
            parent_session_id,
            topic_id,
            token_hash,
            settings.main_thread_model if role == "main" else None,
        )
        if role == "main" and native_id:
            await conn.execute(
                "INSERT INTO native_lineage (session_id, seq, native_session_id, started_reason) VALUES ($1,1,$2,'create')",
                session_id,
                native_id,
            )
    return await get_binding(session_id)  # type: ignore[return-value]


async def _open_count(session_id: str) -> int:
    async with db.connection() as conn:
        return await conn.fetchval(
            "SELECT count(*) FROM native_deliveries WHERE session_id=$1 AND state = ANY($2)",
            session_id,
            list(OPEN_STATES),
        )


async def submit_message(session_id: str, text: str, *, source: str = "user") -> str:
    """Record a message and its delivery intent, then deliver in the background.

    ``source``: ``user`` (typed in the UI; refused while a turn is open), ``report`` (a child's
    report; queued while a turn is open), ``writeout`` (the pre-cut turn), ``brief`` (a parent's
    task brief to a fresh child). A ``queued`` delivery is sent by ``sync`` once the agent is idle.
    """
    session = await db.get_session(session_id)
    if source == "user" and session_id in _rotating:
        raise ValueError(
            "The main thread is rotating its context window; try again in a moment."
        )
    # The in-flight check and the ledger insert are one critical section (per session), so two
    # concurrent submissions cannot both see an idle agent and interleave in one turn (E4).
    async with _lock(session_id):
        busy = await _open_count(session_id)
        # An 'uncertain' delivery does not block: the user decides whether to send again.
        if busy and source in ("user", "writeout", "brief"):
            raise ValueError(
                "A previous message is still in flight; wait for its reply before sending another."
            )
        state = (
            "queued"
            if busy or (source == "report" and session_id in _rotating)
            else "recorded"
        )
        message = await db.create_message(
            conversation_id=session.conversation_id, role="user", content=text
        )
        async with db.connection() as conn:
            await conn.execute(
                "INSERT INTO native_deliveries (message_id, session_id, state, source) VALUES ($1,$2,$3,$4)",
                message.id,
                session_id,
                state,
                source,
            )
    if state == "recorded":
        asyncio.create_task(_deliver(session_id, message.id, text))
    return message.id


async def _start_extra(binding: dict) -> tuple[dict[str, str], str | None]:
    """Agentctl options for main/child bindings: scratch cwd, scoped token, standing context."""
    if binding["role"] == "agent":
        return {}, None
    from mainloop.runtime.delegation import render_for_binding

    standing = await render_for_binding(binding)
    extra = {
        "--cwd-rel": (
            "main" if binding["role"] == "main" else f"children/{binding['agent_name']}"
        ),
        "--token": token_for(binding["session_id"]),
        "--standing-b64": base64.b64encode(standing.encode()).decode(),
    }
    if binding["role"] == "main":
        extra["--model"] = settings.main_thread_model
        extra["--effort"] = settings.main_thread_effort
    return extra, content_hash(standing)


async def _ensure_agent(session_id: str, binding: dict) -> dict:
    """Make sure the agent is live in Herdr, resuming the native session after pod replacement."""
    ws = workspace_for(binding)
    pod = await ws.require_ready()
    name = binding["agent_name"]
    status = await ws.agent_status(name)
    fields: dict = {}
    if status is None:
        # A journal already seen for this native session id means an earlier run: resume it.
        resume = binding["journal_ref"] is not None
        extra, standing_hash = await _start_extra(binding)
        ident = await ws.start(
            config_name(binding),
            name,
            native_id=binding["native_session_id"],
            resume=resume,
            extra=extra,
        )
        fields.update(
            herdr_pane_id=ident.get("pane_id"),
            herdr_terminal_id=ident.get("terminal_id"),
            herdr_workspace_id=ident.get("workspace_id"),
            generation=binding["generation"] + (1 if resume else 0),
        )
        if standing_hash:
            fields["standing_hash"] = standing_hash
    else:
        fields.update(
            herdr_pane_id=status.get("pane_id"),
            herdr_terminal_id=status.get("terminal_id"),
        )
    fields["pod_uid"] = pod.uid
    await _update_binding(session_id, **fields)
    return await get_binding(session_id)  # type: ignore[return-value]


async def _deliver(session_id: str, message_id: str, text: str) -> None:
    async with _lock(session_id):
        try:
            binding = await get_binding(session_id)
            ws = workspace_for(binding)
            binding = await _ensure_agent(
                session_id, binding
            )  # not attempted => nothing sent
            cursor_before = 0
            if binding["native_session_id"]:
                cursor_before = (
                    await ws.journal(
                        binding["agent_name"], binding["native_session_id"], 10**9
                    )
                ).total_lines
            await _set_delivery(message_id, "sending", cursor_before=cursor_before)
        except Exception as exc:
            logger.exception("delivery not attempted for %s", message_id)
            await _set_delivery(
                message_id, "failed", detail=f"not sent: {type(exc).__name__}: {exc}"
            )
            return
        try:
            await ws.send(binding["agent_name"], text)
        except TransportError as exc:
            await _set_delivery(
                message_id,
                "uncertain",
                detail=f"transport error, outcome unknown: {exc}",
            )
            return
        except RuntimeError as exc:
            await _set_delivery(message_id, "failed", detail=f"send rejected: {exc}")
            return
        except Exception as exc:
            logger.exception("delivery outcome unknown for %s", message_id)
            await _set_delivery(
                message_id, "uncertain", detail=f"unexpected error after send: {exc}"
            )
            return
    await sync(session_id)


async def sync(session_id: str) -> None:
    """Mirror new journal evidence into Postgres, then run the follow-up actions (queued
    deliveries, child fallback report, rotation) that are only safe outside the binding lock.
    """
    follow = await _sync_locked(session_id)
    if not follow:
        return
    if follow.get("fallback_report"):
        from mainloop.runtime.delegation import auto_report

        await auto_report(session_id, follow["fallback_report"])
    if follow.get("idle") and session_id not in _rotating:
        binding = await get_binding(session_id)
        if binding and binding["role"] == "main":
            reason = rotation_due(
                context_tokens=binding["context_tokens"],
                baseline_tokens=binding["baseline_tokens"],
                turns=binding["turns_in_lineage"],
                budget_tokens=settings.main_rotate_tokens,
                budget_turns=settings.main_rotate_turns,
            )
            if reason:
                asyncio.create_task(rotate(session_id, reason))
                return
        await _promote_queued(session_id)


async def _promote_queued(session_id: str) -> None:
    """Send the oldest queued delivery if (and only if) nothing is open. Atomic in SQL, and
    serialised with ``submit_message`` by the per-session lock."""
    async with _lock(session_id), db.connection() as conn:
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
            return
        text = await conn.fetchval(
            "SELECT content FROM messages WHERE id=$1", row["message_id"]
        )
    asyncio.create_task(_deliver(session_id, row["message_id"], text))


async def _sync_locked(session_id: str) -> dict | None:
    async with _lock(session_id):
        binding = await get_binding(session_id)
        if binding is None:
            return None
        ws = workspace_for(binding)
        try:
            if not binding["native_session_id"]:
                nid = await ws.native_id(binding["agent_name"])
                if not nid:
                    return None
                await _update_binding(session_id, native_session_id=nid)
                binding["native_session_id"] = nid
            jl = await ws.journal(
                binding["agent_name"],
                binding["native_session_id"],
                binding["journal_cursor"],
            )
        except (TransportError, WorkspaceUnavailable) as exc:
            logger.info("sync skipped for %s: %s", session_id, exc)
            return None
        if jl.file is None:
            return None
        ref = jl.file.rsplit("/", 1)[-1]
        events = parse_journal(
            binding["kind"],
            jl.lines,
            file_ref=ref,
            native_id=binding["native_session_id"],
            agent=binding["agent_name"],
        )
        session = await db.get_session(session_id)
        async with db.connection() as conn:
            pending = [
                dict(r)
                for r in await conn.fetch(
                    """SELECT d.*, m.content FROM native_deliveries d JOIN messages m ON m.id=d.message_id
                   WHERE d.session_id=$1 AND d.state IN ('sending','uncertain','delivered') ORDER BY d.created_at""",
                    session_id,
                )
            ]
        # Receipts and completion, by correlating prompt text after the recorded cursor.
        turns, safe = completed_turns(events)
        for d in pending:
            want = d["content"].strip()
            hit = next(
                (
                    e
                    for e in events
                    if e.kind == "prompt"
                    and e.cursor > (d["cursor_before"] or 0)
                    and want in (e.text or "")
                ),
                None,
            )
            if hit is None:
                if (
                    d["state"] == "sending"
                    and datetime.now(UTC) - d["updated_at"] > SEND_RECEIPT_GRACE
                ):
                    await _set_delivery(
                        d["message_id"],
                        "uncertain",
                        detail="no journal receipt after send; not replaying",
                    )
                continue
            done = next((t for t in turns if hit.cursor in t.prompt_cursors), None)
            if done is not None:
                await _set_delivery(
                    d["message_id"], "completed", evidence_ref=done.evidence_ref
                )
            elif d["state"] != "delivered":
                await _set_delivery(
                    d["message_id"], "delivered", evidence_ref=hit.evidence_ref
                )
            else:
                age = datetime.now(UTC) - d["updated_at"]
                gone = False
                if age > SEND_RECEIPT_GRACE:
                    try:
                        gone = (await ws.agent_status(binding["agent_name"])) is None
                    except TransportError:
                        gone = False
                if gone or age > DELIVERED_MAX_AGE:
                    await _set_delivery(
                        d["message_id"],
                        "uncertain",
                        detail="prompt was received but its turn never completed"
                        + (" (agent no longer live)" if gone else " (timed out)")
                        + "; not replaying",
                    )
        new_reply = None
        for t in turns:
            if not t.reply:
                continue
            mid = str(uuid.uuid5(_NS, f"{session_id}:{ref}:{t.end_cursor}"))
            async with db.connection() as conn:
                await conn.execute(
                    "INSERT INTO messages (id, conversation_id, role, content, created_at) VALUES ($1,$2,'assistant',$3,NOW()) ON CONFLICT (id) DO NOTHING",
                    mid,
                    session.conversation_id,
                    t.reply,
                )
            new_reply = t.reply
        # Continuation events (native compaction): recorded, never replayed. The standing
        # context reaches a compacted worker through its SessionStart(compact) hook.
        compactions = [
            e for e in events if e.native_type == "claude.system.compact_boundary"
        ]
        for e in compactions:
            async with db.connection() as conn:
                await conn.execute(
                    """INSERT INTO native_events (id, session_id, kind, detail, evidence_ref)
                       VALUES ($1,$2,'continuation','compact_boundary',$3) ON CONFLICT DO NOTHING""",
                    str(uuid.uuid4()),
                    session_id,
                    e.evidence_ref,
                )
            if binding["role"] == "main":
                logger.warning(
                    "native compaction fired on the main thread (%s): rotation budget is too high",
                    e.evidence_ref,
                )
        model = next((e.model for e in reversed(events) if e.model), None)
        ctx = [e.context_tokens for e in events if e.context_tokens]
        fields: dict = {
            "journal_cursor": max(binding["journal_cursor"], safe),
            "journal_ref": ref,
            "turns_in_lineage": binding["turns_in_lineage"] + len(turns),
            "continuations": binding["continuations"] + len(compactions),
        }
        if ctx:
            fields["context_tokens"] = ctx[-1]
            if binding["baseline_tokens"] is None:
                fields["baseline_tokens"] = ctx[0]
        if model:
            fields["model"] = model
        await _update_binding(session_id, **fields)
        open_n = await _open_count(session_id)
        new_status = SessionStatus.ACTIVE if open_n else SessionStatus.WAITING_ON_USER
        if session.status != new_status:
            await db.update_session(session_id, status=new_status)
        follow: dict = {"idle": open_n == 0}
        if binding["role"] == "child" and new_reply:
            fresh = await get_binding(session_id)
            if fresh and fresh["reported_at"] is None:
                follow["fallback_report"] = new_reply
        return follow


async def _wait_delivery(message_id: str, session_id: str, timeout: float) -> str:
    deadline = asyncio.get_event_loop().time() + timeout
    state = "recorded"
    while asyncio.get_event_loop().time() < deadline:
        await sync(session_id)
        async with db.connection() as conn:
            state = await conn.fetchval(
                "SELECT state FROM native_deliveries WHERE message_id=$1", message_id
            )
        if state in ("completed", "failed", "uncertain"):
            return state
        await asyncio.sleep(2)
    return f"timeout({state})"


async def rotate(
    session_id: str, reason: str, *, writeout_timeout: float = 180
) -> dict:
    """Cut the main thread to a fresh native session (Mainloop owns the window, not the model).

    1. one receipt-tracked pre-cut turn asks the agent to write durable facts through the CLI;
    2. the old native session is stopped and the lineage records old id -> new id;
    3. a fresh native session starts with the carry-over (standing context, topic index,
       checkpoint, pending intent, last K visible messages), rendered from Postgres.
    The new native journal contains none of the old transcript.
    """
    if session_id in _rotating:
        return {"status": "already-rotating"}
    _rotating.add(session_id)
    try:
        binding = await get_binding(session_id)
        if binding is None or binding["role"] != "main":
            return {"status": "not-a-main-thread"}
        if await _open_count(session_id):
            return {"status": "busy"}
        mid = await submit_message(session_id, WRITEOUT_TEXT, source="writeout")
        writeout = await _wait_delivery(mid, session_id, writeout_timeout)
        async with _lock(session_id):
            binding = await get_binding(session_id)
            ws = workspace_for(binding)
            try:
                await ws.stop(binding["agent_name"])
            except (
                Exception
            ) as exc:  # the old session stays authoritative; nothing was switched
                logger.exception("rotation aborted: could not stop the old agent")
                return {
                    "status": "aborted",
                    "detail": f"stop failed: {exc}",
                    "writeout": writeout,
                }
            new_id = str(uuid.uuid4())
            seq = binding["lineage_seq"] + 1
            async with db.connection() as conn:
                # Nothing of the old lineage can be resolved after the cut (new journal, cursor 0):
                # close its open rows as unknown rather than leaving the session "in flight".
                await conn.execute(
                    """UPDATE native_deliveries SET state='uncertain', updated_at=NOW(),
                       detail='the native session was rotated before this turn completed; not replaying'
                       WHERE session_id=$1 AND state = ANY($2)""",
                    session_id,
                    list(OPEN_STATES),
                )
                await conn.execute(
                    "UPDATE native_lineage SET ended_reason=$3, writeout=$4, ended_at=NOW() WHERE session_id=$1 AND seq=$2",
                    session_id,
                    binding["lineage_seq"],
                    reason,
                    writeout,
                )
                await conn.execute(
                    "INSERT INTO native_lineage (session_id, seq, native_session_id, started_reason) VALUES ($1,$2,$3,$4)",
                    session_id,
                    seq,
                    new_id,
                    reason,
                )
            await _update_binding(
                session_id,
                native_session_id=new_id,
                journal_cursor=0,
                journal_ref=None,
                context_tokens=None,
                baseline_tokens=None,
                turns_in_lineage=0,
                lineage_seq=seq,
                generation=binding["generation"] + 1,
            )
            binding = await get_binding(session_id)
            binding = await _ensure_agent(
                session_id, binding
            )  # fresh session + carry-over
            async with db.connection() as conn:
                await conn.execute(
                    "UPDATE native_lineage SET carry_over_hash=$3 WHERE session_id=$1 AND seq=$2",
                    session_id,
                    seq,
                    binding["standing_hash"],
                )
        return {
            "status": "rotated",
            "new_native_session_id": new_id,
            "lineage_seq": seq,
            "writeout": writeout,
            "reason": reason,
        }
    finally:
        _rotating.discard(session_id)
        asyncio.create_task(
            sync(session_id)
        )  # promote queued reports into the new session


async def reconcile_loop(interval: float = 3.0) -> None:
    """Background mirror for sessions with open work, so replies, reports and rotation do not
    depend on a browser polling."""
    while True:
        try:
            async with db.connection() as conn:
                ids = [
                    r["session_id"]
                    for r in await conn.fetch(
                        """SELECT DISTINCT session_id FROM native_deliveries
                           WHERE state IN ('recorded','sending','delivered','queued')
                              OR (state='uncertain' AND updated_at > NOW() - INTERVAL '30 minutes')"""
                    )
                ]
            for sid in ids:
                if sid not in _rotating:
                    await sync(sid)
        except Exception:
            logger.exception("reconcile loop iteration failed")
        await asyncio.sleep(interval)


async def identity(session_id: str) -> NativeSessionInfo | None:
    binding = await get_binding(session_id)
    if binding is None:
        return None
    async with db.connection() as conn:
        rows = await conn.fetch(
            "SELECT * FROM native_deliveries WHERE session_id=$1 ORDER BY created_at",
            session_id,
        )
        topic = (
            await conn.fetchval(
                "SELECT name FROM topics WHERE id=$1", binding["topic_id"]
            )
            if binding["topic_id"]
            else None
        )
    deliveries = [
        NativeDeliveryInfo(
            message_id=r["message_id"],
            state=r["state"],
            evidence_ref=r["evidence_ref"],
            detail=r["detail"],
            source=r["source"],
        )
        for r in rows
    ]
    ws = workspace_for(binding)
    ready, live, uid, note = False, None, None, None
    try:
        pod = await ws.pod_state()
        ready, uid = pod.ready, pod.uid
        if ready:
            live = (await ws.agent_status(binding["agent_name"])) is not None
    except TransportError as exc:
        note = f"workspace unreachable: {exc}"
    if any(d.state == "uncertain" for d in deliveries):
        note = "delivery unknown: the last prompt was not replayed; check the reply, then send again if needed"
    return NativeSessionInfo(
        session_id=session_id,
        kind=binding["kind"],
        role=binding["role"],
        parent_session_id=binding["parent_session_id"],
        topic=topic,
        agent_name=binding["agent_name"],
        native_session_id=binding["native_session_id"],
        model=binding["model"],
        approval_policy=binding["approval_policy"],
        herdr_pane_id=binding["herdr_pane_id"],
        herdr_terminal_id=binding["herdr_terminal_id"],
        herdr_workspace_id=binding["herdr_workspace_id"],
        workspace_pod=ws.pod,
        workspace_pod_uid=uid,
        workspace_ready=ready,
        agent_live=live,
        generation=binding["generation"],
        lineage_seq=binding["lineage_seq"],
        context_tokens=binding["context_tokens"],
        baseline_tokens=binding["baseline_tokens"],
        turns_in_lineage=binding["turns_in_lineage"],
        continuations=binding["continuations"],
        rotating=session_id in _rotating,
        journal_cursor=binding["journal_cursor"],
        journal_ref=binding["journal_ref"],
        turn_in_flight=any(d.state in (*OPEN_STATES, "queued") for d in deliveries),
        deliveries=deliveries,
        note=note,
    )
