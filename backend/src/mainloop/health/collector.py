"""Observation failures never alter delivery state or replay a prompt.

Delivery handling only schedules imports. One background worker per event loop
imports them through a bounded queue, and every database wait is time-limited,
so a health-only lock or a slow import cannot hold a terminal delivery. Imports
dropped or abandoned here are recovered later from completed receipts.
"""

import asyncio
import logging
from datetime import UTC, datetime, timedelta

from mainloop.config import settings
from mainloop.db import db, health

from models.orchestration_health import is_health_part

logger = logging.getLogger(__name__)

MAX_PENDING = 64
COLLECT_SECONDS = 5
LOCK_TIMEOUT = "2s"
RECOVERY_SECONDS = 5

_queue: asyncio.Queue | None = None
_worker: asyncio.Task | None = None


def _has_health(task_id, artifacts):
    return bool(task_id) and any(
        any(is_health_part(artifact.name, part.data) for part in artifact.parts)
        for artifact in artifacts
    )


def schedule(session_id, message_id, task_id, context_id, artifacts):
    """Queue an import without waiting; a full queue leaves it to recovery."""
    global _queue, _worker
    if not _has_health(task_id, artifacts):
        return
    loop = asyncio.get_running_loop()
    if (
        _queue is None
        or _worker is None
        or _worker.done()
        or _worker.get_loop() is not loop
    ):
        _queue = asyncio.Queue(MAX_PENDING)
        _worker = loop.create_task(_drain_queue(_queue))
    try:
        _queue.put_nowait((session_id, message_id, task_id, context_id, artifacts))
    except asyncio.QueueFull:
        logger.warning(
            "health import backlog full; session %s left to recovery", session_id
        )


async def _drain_queue(queue):
    while True:
        receipt = await queue.get()
        try:
            await collect(*receipt)
        except Exception:
            logger.warning("health import failed for session %s", receipt[0])
        finally:
            queue.task_done()


async def drain():
    """Wait for scheduled imports on this loop (tests and orderly shutdown)."""
    if (
        _queue is not None
        and _worker is not None
        and not _worker.done()
        and _worker.get_loop() is asyncio.get_running_loop()
    ):
        await _queue.join()


async def collect(session_id, message_id, task_id, context_id, artifacts):
    if not _has_health(task_id, artifacts):
        return
    try:
        async with asyncio.timeout(COLLECT_SECONDS):
            async with db.connection() as conn, conn.transaction():
                await conn.execute(f"SET LOCAL lock_timeout = '{LOCK_TIMEOUT}'")
                await health.ingest(
                    conn,
                    session_id,
                    message_id,
                    task_id,
                    context_id,
                    artifacts,
                    gateway=settings.kagent_gateway_url,
                )
        return True
    except Exception:
        # Never log provider payloads or exception strings; retry the same receipt.
        logger.warning("health persistence failed for session %s", session_id)
        await mark_unavailable(session_id, message_id, task_id)
        return False


async def mark_unavailable(session_id, message_id, task_id):
    try:
        async with asyncio.timeout(COLLECT_SECONDS):
            async with db.connection() as conn, conn.transaction():
                await conn.execute(f"SET LOCAL lock_timeout = '{LOCK_TIMEOUT}'")
                attr = await health.attribution(conn, session_id, message_id, task_id)
                if attr:
                    await health.diagnostic(
                        conn,
                        attr,
                        settings.kagent_gateway_url,
                        task_id,
                        "source_unavailable",
                    )
    except Exception:
        logger.warning("health gap could not be persisted for session %s", session_id)


async def recover(owner, *, session_id=None):
    """Re-import recent receipts. Returns True when the result may be incomplete.

    The deadline covers every await, including lookups, imports and gap writes.
    """
    try:
        async with asyncio.timeout(RECOVERY_SECONDS):
            return await _recover(owner, session_id)
    except TimeoutError:
        return True


async def _recover(owner, session_id):
    from mainloop.runtime import native_sessions as ns

    async with db.connection() as conn:
        rows = await conn.fetch(
            """SELECT d.message_id,d.session_id,d.task_id FROM native_deliveries d
               JOIN sessions s ON s.id=d.session_id JOIN native_bindings b ON b.session_id=s.id
               WHERE s.user_id=$1 AND d.task_id IS NOT NULL AND b.kagent_session_id IS NOT NULL
                 AND d.created_at>$2 AND ($3::text IS NULL OR s.id=$3)
               ORDER BY d.created_at DESC LIMIT 101""",
            owner,
            datetime.now(UTC) - timedelta(days=settings.health_event_retention_days),
            session_id,
        )
    # Includes terminal receipts: a consumer restart must recover them too.
    for row in rows[:100]:
        try:
            binding = await ns.get_binding(row["session_id"])
            task = await asyncio.wait_for(
                ns.get_client().get_task(
                    await ns.binding_agent_ref(binding), row["task_id"]
                ),
                timeout=2,
            )
            if await collect(
                row["session_id"],
                row["message_id"],
                task.id,
                task.context_id,
                task.artifacts,
            ):
                async with db.connection() as conn:
                    await conn.execute(
                        "UPDATE orchestration_health_collection SET reasons=array_remove(reasons,'source_unavailable') WHERE owner_id=$1 AND gateway=$2 AND session_id=$3 AND a2a_task_id=$4",
                        owner,
                        settings.kagent_gateway_url,
                        row["session_id"],
                        row["task_id"],
                    )
        except Exception:
            await mark_unavailable(row["session_id"], row["message_id"], row["task_id"])
    return len(rows) > 100
