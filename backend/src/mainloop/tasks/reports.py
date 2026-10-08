"""Durable report claims and transactionally queued, current-authority notifications.

No external send occurs here. Native reconciliation consumes the existing delivery ledger;
uncertain/sent deliveries are never replayed. Task events also expose owner attention through
SSE and authoritative task reads, without granting native continuation or approval authority.
"""

import uuid
from datetime import UTC, datetime

from mainloop.db import tasks as store
from mainloop.tasks import lifecycle
from mainloop.tasks.events import reserve_event

from models.task import TaskReport


def notification_id(event_id: str, recipient_key: str, target: str) -> str:
    return uuid.uuid5(
        uuid.NAMESPACE_URL, f"mainloop-report:{event_id}:{recipient_key}:{target}"
    ).hex


async def record(conn, principal, request: TaskReport) -> dict:
    store.require_transaction(conn)
    await store.admission_lock(conn)
    await store.validate_principal(conn, principal)
    if principal.role not in ("supervisor", "child") or (
        request.task_id,
        request.attempt_id,
    ) != (principal.task_id, principal.attempt_id):
        raise store.TaskError(403, "report_attempt_scope")
    task = await store.get_task(conn, request.task_id, principal, lock=True)
    payload_digest = store.digest(request.model_dump(mode="json"))
    previous = await conn.fetchrow(
        "SELECT * FROM task_reports WHERE attempt_id=$1 AND request_id=$2",
        request.attempt_id,
        request.request_id,
    )
    if previous:
        if previous["request_digest"] != payload_digest:
            raise store.TaskError(409, "report_request_conflict")
        return {
            "text": "report already recorded; notification is durable",
            "report_id": previous["id"],
            "report": store.decode(previous["snapshot"]),
        }
    if (
        request.outcome == "completed"
        and task.mode == "coordination"
        and await conn.fetchval(
            """SELECT EXISTS(SELECT 1 FROM tasks t JOIN task_attempts a ON a.task_id=t.id
           WHERE t.parent_task_id=$1 AND a.capacity_held)""",
            task.id,
        )
    ):
        raise store.TaskError(409, "live_children")
    report_id = uuid.uuid4().hex
    await conn.execute(
        """INSERT INTO task_reports(id,task_id,attempt_id,request_id,request_digest,snapshot)
           VALUES($1,$2,$3,$4,$5,$6::jsonb)""",
        report_id,
        task.id,
        request.attempt_id,
        request.request_id,
        payload_digest,
        request.model_dump_json(),
    )
    status, reason = task.status, task.reason
    if request.outcome == "completed":
        status, reason = "waiting", (
            "publication" if task.mode == "code" else "reconciliation"
        )
    elif request.outcome in ("blocked", "failed"):
        status, reason = "blocked", "reconciliation"
    updated = task.model_copy(
        update={
            "status": status,
            "reason": reason,
            "version": task.version + 1,
            "updated_at": datetime.now(UTC),
        }
    )
    key = f"report:{report_id}"
    await store.save_task(conn, updated, task.version, key)
    event_id = await reserve_event(conn, updated, key)
    if request.outcome != "progress":
        attempt = await lifecycle.load_attempt(conn, principal.attempt_id)
        await lifecycle.save_attempt(
            conn, attempt.model_copy(update={"result_ref": key})
        )
    recipients = ["main"]
    if task.parent_task_id:
        recipients.insert(0, f"parent-task:{task.parent_task_id}")
    for recipient in recipients:
        await conn.execute(
            "INSERT INTO task_event_deliveries(event_id,recipient_key,state) VALUES($1,$2,'pending') ON CONFLICT DO NOTHING",
            event_id,
            recipient,
        )
    return {
        "text": "report recorded; parent notification is pending (result is a claim)",
        "report_id": report_id,
        "event_id": event_id,
        "report": request.model_dump(mode="json"),
    }


async def _recipient(conn, task, key):
    if key == "main":
        row = await conn.fetchrow(
            """SELECT b.*,s.user_id,s.status,s.archived_at,s.conversation_id
               FROM native_bindings b JOIN sessions s ON s.id=b.session_id
               WHERE b.role='main' AND b.mcp_grant_kind='coordination' AND s.user_id=$1
                 AND s.archived_at IS NULL AND b.token_hash IS NOT NULL AND b.kagent_deleted_at IS NULL
                 AND s.status NOT IN ('completed','failed','cancelled')
               ORDER BY b.created_at LIMIT 1""",
            task.owner_id,
        )
        return (dict(row), None) if row else (None, None)
    # Keys are server-authored at report commit. Never follow arbitrary session/topic input.
    if key != f"parent-task:{task.parent_task_id}":
        return None, None
    parent = await lifecycle.load_task(conn, task.parent_task_id)
    if (
        parent.owner_id != task.owner_id
        or parent.project_id != task.project_id
        or parent.root_task_id != task.root_task_id
    ):
        return None, None
    attempt = await lifecycle.load_attempt(conn, parent.current_attempt_id)
    if not attempt:
        return None, None
    row = await conn.fetchrow(
        """SELECT b.*,s.user_id,s.status,s.archived_at,s.conversation_id
           FROM native_bindings b JOIN sessions s ON s.id=b.session_id WHERE b.session_id=$1""",
        attempt.binding_id,
    )
    if not row:
        return None, None
    try:
        await lifecycle.authenticate_binding(conn, dict(row))
    except (lifecycle.LifecycleDenied, ValueError):
        return None, None
    return dict(row), attempt.id


STALE_RECIPIENT = "report recipient authority changed"


async def _lock_recipient(conn, session_id):
    # The ledger advisory lock must already be held. This is also revocation's row lock.
    await conn.fetchrow(
        """SELECT b.session_id FROM native_bindings b JOIN sessions s ON s.id=b.session_id
           WHERE b.session_id=$1 FOR SHARE OF b,s""",
        session_id,
    )


async def validate_delivery(conn, message_id):
    """Validate managed unsent reports under the ledger lock; never lock their outbox row.

    Dispatch holds that row before the ledger lock. Native claiming therefore reads it without
    a row lock and cancels only the native delivery; dispatch later retains/reroutes the intent.
    Legacy, unmanaged ledger notifications keep their existing behavior.
    """
    item = await conn.fetchrow(
        """SELECT d.recipient_key,d.target_attempt_id,e.task_id,n.session_id,n.state
           FROM task_event_deliveries d JOIN task_events e ON e.id=d.event_id
           JOIN native_deliveries n ON n.message_id=d.delivery_id
           WHERE d.delivery_id=$1 AND e.event_key LIKE 'report:%'""",
        message_id,
    )
    if not item or item["state"] not in ("queued", "recorded"):
        return True
    await _lock_recipient(conn, item["session_id"])
    task = await lifecycle.load_task(conn, item["task_id"])
    binding, attempt_id = await _recipient(conn, task, item["recipient_key"])
    if (
        binding
        and binding["session_id"] == item["session_id"]
        and attempt_id == item["target_attempt_id"]
    ):
        return True
    await conn.execute(
        """UPDATE native_deliveries SET state='cancelled',detail=$2,updated_at=NOW()
           WHERE message_id=$1 AND state IN ('queued','recorded')""",
        message_id,
        STALE_RECIPIENT,
    )
    return False


async def _enqueue(conn, event_id, key):
    item = await conn.fetchrow(
        """SELECT d.*,e.task_id,e.event_key FROM task_event_deliveries d
           JOIN task_events e ON e.id=d.event_id WHERE d.event_id=$1 AND d.recipient_key=$2
           FOR UPDATE OF d""",
        event_id,
        key,
    )
    if not item or item["state"] in ("delivered", "uncertain", "cancelled"):
        return
    task = await lifecycle.load_task(conn, item["task_id"])
    binding, attempt_id = await _recipient(conn, task, key)
    old_session = None
    if item["delivery_id"]:
        old_session = await conn.fetchval(
            "SELECT session_id FROM native_deliveries WHERE message_id=$1",
            item["delivery_id"],
        )
    from mainloop.runtime import native_sessions as ns

    # Resolve identities without row locks; order both old/new ledger locks before any rows.
    for sid in sorted(
        {
            sid
            for sid in (old_session, binding["session_id"] if binding else None)
            if sid
        }
    ):
        await ns.ledger._lock_deliveries(conn, sid)
    if binding:
        await _lock_recipient(conn, binding["session_id"])
        current, current_attempt = await _recipient(conn, task, key)
        if (
            not current
            or current["session_id"] != binding["session_id"]
            or current_attempt != attempt_id
        ):
            return  # Identity changed: next pass resolves and locks the new recipient first.
        binding = current
    if item["delivery_id"]:
        old = await conn.fetchrow(
            "SELECT * FROM native_deliveries WHERE message_id=$1 FOR UPDATE",
            item["delivery_id"],
        )
        stale_cancelled = (
            old["state"] == "cancelled" and old["detail"] == STALE_RECIPIENT
        )
        if old["state"] not in ("recorded", "queued") and not stale_cancelled:
            # Receipt/uncertainty belongs to its original attempt. Never replay it to a successor.
            state = (
                "uncertain"
                if old["state"] == "uncertain"
                else (
                    "cancelled"
                    if old["state"] in ("cancelled", "failed")
                    else "delivered"
                )
            )
            await conn.execute(
                "UPDATE task_event_deliveries SET state=$3,updated_at=NOW() WHERE event_id=$1 AND recipient_key=$2",
                event_id,
                key,
                state,
            )
            return
        if (
            not stale_cancelled
            and binding
            and old["session_id"] == binding["session_id"]
            and item["target_attempt_id"] == attempt_id
        ):
            return
        await conn.execute(
            "UPDATE native_deliveries SET state='cancelled',detail='report recipient authority changed',updated_at=NOW() WHERE message_id=$1",
            item["delivery_id"],
        )
        await conn.execute(
            "UPDATE task_event_deliveries SET delivery_id=NULL,target_attempt_id=NULL,state='pending',updated_at=NOW() WHERE event_id=$1 AND recipient_key=$2",
            event_id,
            key,
        )
    if binding is None:
        return  # Retain the intent until an authorized current recipient exists.
    await lifecycle.check(conn, binding["session_id"], "submit", lock=True)
    report = await conn.fetchrow(
        "SELECT snapshot FROM task_reports WHERE id=$1",
        item["event_key"].removeprefix("report:"),
    )
    request = TaskReport.model_validate(store.decode(report["snapshot"]))
    message_id = notification_id(event_id, key, attempt_id or binding["session_id"])
    # Always enqueue through the existing ledger. Its promotion owns ordering and native dispatch.
    text = f"[report from child task {task.id} attempt {request.attempt_id}; {request.outcome}; unverified claim]\n{request.summary}"
    await conn.execute(
        "INSERT INTO messages(id,conversation_id,role,content) VALUES($1,$2,'user',$3) ON CONFLICT(id) DO NOTHING",
        message_id,
        binding["conversation_id"],
        text,
    )
    await conn.execute(
        "INSERT INTO native_deliveries(message_id,session_id,state,source) VALUES($1,$2,'queued','report') ON CONFLICT(message_id) DO UPDATE SET state='queued',detail=NULL,updated_at=NOW() WHERE native_deliveries.state='cancelled' AND native_deliveries.detail='report recipient authority changed'",
        message_id,
        binding["session_id"],
    )
    await conn.execute(
        "UPDATE task_event_deliveries SET delivery_id=$3,target_attempt_id=$4,state='queued',updated_at=NOW() WHERE event_id=$1 AND recipient_key=$2",
        event_id,
        key,
        message_id,
        attempt_id,
    )


async def dispatch_pending(database, *, limit=100):
    async with database.connection() as conn:
        rows = []
        # Give pending notifications and queued-delivery reconciliation independent bounded
        # scans. Busy queues cannot monopolize admission; unavailable recipients rotate too.
        for state in ("pending", "queued"):
            rows.extend(
                await conn.fetch(
                    """SELECT d.event_id,d.recipient_key,a.binding_id FROM task_event_deliveries d
                   JOIN task_events e ON e.id=d.event_id
                   JOIN task_reports r ON ('report:' || r.id)=e.event_key
                   JOIN task_attempts a ON a.id=r.attempt_id
                   WHERE e.event_key LIKE 'report:%' AND d.state=$2
                   ORDER BY d.updated_at,e.created_at,e.id,d.recipient_key LIMIT $1""",
                    limit,
                    state,
                )
            )
    for row in rows:
        async with database.connection() as conn:
            # Tree lock precedes delivery/row locks, matching parent authority mutations.
            async with lifecycle.authority_locked(
                conn, row["binding_id"]
            ), conn.transaction():
                await _enqueue(conn, row["event_id"], row["recipient_key"])
                await conn.execute(
                    "UPDATE task_event_deliveries SET updated_at=NOW() WHERE event_id=$1 AND recipient_key=$2",
                    row["event_id"],
                    row["recipient_key"],
                )
    await _complete_coordination(database)


async def _complete_coordination(database):
    """Settle explicit coordination results only after the native runtime is confirmed gone.

    Keep capacity held on unknown deletion, and let the current turn finish before draining.
    This uses the existing lifecycle/credential/runtime APIs; repository claims never complete here.
    """
    from mainloop.push_gate import lifecycle as push_lifecycle
    from mainloop.runtime import native_sessions as ns
    from mainloop.runtime.agent_credentials import revoke

    async with database.connection() as conn:
        rows = await conn.fetch(
            """SELECT a.id,a.binding_id FROM tasks t JOIN task_attempts a ON a.id=t.current_attempt_id
               JOIN task_reports r ON ('report:' || r.id)=a.snapshot->>'result_ref'
               WHERE t.mode='coordination' AND t.status='waiting' AND t.snapshot->>'reason'='reconciliation'
                 AND r.snapshot->>'outcome'='completed' AND a.state IN ('active','draining')""",
        )
    for row in rows:
        sid = row["binding_id"]
        async with database.connection() as conn:
            async with push_lifecycle.locked(conn, sid), lifecycle.locked(
                conn, sid
            ), conn.transaction():
                await store.admission_lock(conn)
                attempt = await lifecycle.load_attempt(conn, row["id"])
                task = await lifecycle.load_task(conn, attempt.task_id, lock=True)
                if (
                    task.current_attempt_id != attempt.id
                    or task.status != "waiting"
                    or task.reason != "reconciliation"
                ):
                    continue
                if await conn.fetchval(
                    "SELECT EXISTS(SELECT 1 FROM tasks t JOIN task_attempts a ON a.task_id=t.id WHERE t.parent_task_id=$1 AND a.capacity_held)",
                    task.id,
                ):
                    continue
                if await conn.fetchval(
                    "SELECT EXISTS(SELECT 1 FROM native_deliveries WHERE session_id=$1 AND state = ANY($2))",
                    sid,
                    [*ns.OPEN_STATES, "queued", "uncertain"],
                ):
                    continue
                if await conn.fetchval(
                    "SELECT EXISTS(SELECT 1 FROM task_operations WHERE task_id=$1 AND kind='cancel' AND state NOT IN ('completed','blocked'))",
                    task.id,
                ):
                    continue
                await lifecycle.transition(
                    conn, attempt.id, "draining", from_states=("active", "draining")
                )
        await revoke(sid)
        if not await ns.delete_kagent_session(sid):
            continue
        async with database.connection() as conn:
            async with push_lifecycle.locked(conn, sid), lifecycle.locked(
                conn, sid
            ), conn.transaction():
                # A known runtime id and confirmed deletion are required, even after restart.
                binding = await ns.get_binding(sid, conn=conn)
                if (
                    not binding
                    or not binding["kagent_session_id"]
                    or not binding["kagent_deleted_at"]
                ):
                    continue
                await store.admission_lock(conn)
                if await conn.fetchval(
                    "SELECT EXISTS(SELECT 1 FROM task_operations WHERE task_id=$1 AND kind='cancel' AND state NOT IN ('completed','blocked'))",
                    attempt.task_id,
                ):
                    continue
                settled = await lifecycle.settle(
                    conn,
                    row["id"],
                    "completed",
                    evidence=f"kagent-deleted:{binding['kagent_session_id']}",
                )
                if settled is not None:
                    await conn.execute(
                        "UPDATE sessions SET status='completed',completed_at=NOW() WHERE id=$1",
                        sid,
                    )
