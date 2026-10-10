"""Observation failures never alter delivery state or replay a prompt."""

import asyncio
import logging
from datetime import UTC, datetime, timedelta

from mainloop.config import settings
from mainloop.db import db, health

from models.orchestration_health import is_health_part

logger = logging.getLogger(__name__)


async def collect(session_id, message_id, task_id, context_id, artifacts):
    if not task_id or not any(
        any(is_health_part(artifact.name, part.data) for part in artifact.parts)
        for artifact in artifacts
    ):
        return
    try:
        async with db.connection() as conn, conn.transaction():
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
        async with db.connection() as conn, conn.transaction():
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
    deadline = asyncio.get_running_loop().time() + 5
    for row in rows[:100]:
        if asyncio.get_running_loop().time() >= deadline:
            return True
        try:
            binding = await ns.get_binding(row["session_id"])
            task = await asyncio.wait_for(
                ns.get_client().get_task(
                    await ns.binding_agent_ref(binding), row["task_id"]
                ),
                timeout=min(2, deadline - asyncio.get_running_loop().time()),
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


async def retention():
    async with db.connection() as conn, conn.transaction():
        await health.retain(conn)
