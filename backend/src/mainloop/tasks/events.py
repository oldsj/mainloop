"""Committed, at-least-once task notification outbox. GET is authoritative."""

import json
import uuid
from typing import Protocol

from models.task import Task, TaskUpdated


class TaskEventSink(Protocol):
    async def publish(self, owner_id: str, event: TaskUpdated) -> None: ...


async def reserve_event(conn, task: Task, event_key: str) -> str:
    if not conn.is_in_transaction():
        raise ValueError("transaction_required")
    event = TaskUpdated(
        event_id=uuid.uuid4().hex,
        task_id=task.id,
        version=task.version,
        attempt_id=task.current_attempt_id,
        root_task_id=task.root_task_id,
        parent_task_id=task.parent_task_id,
        occurred_at=task.updated_at,
    )
    row = await conn.fetchrow(
        """INSERT INTO task_events(id,task_id,owner_id,version,event_key,payload)
        VALUES($1,$2,$3,$4,$5,$6::jsonb) ON CONFLICT(task_id,event_key)
        DO NOTHING RETURNING id""",
        event.event_id,
        task.id,
        task.owner_id,
        task.version,
        event_key,
        event.model_dump_json(),
    )
    if row:
        return row["id"]
    return await conn.fetchval(
        "SELECT id FROM task_events WHERE task_id=$1 AND event_key=$2",
        task.id,
        event_key,
    )


async def dispatch_committed_events(database, sink: TaskEventSink, *, limit=100):
    # A separate connection can only see committed rows. A crash after publish may duplicate
    # an event; stable event IDs let consumers deduplicate and reconnect with GET.
    async with database.connection() as conn:
        rows = await conn.fetch(
            "SELECT * FROM task_events WHERE published_at IS NULL ORDER BY created_at,id LIMIT $1",
            limit,
        )
        for row in rows:
            payload = (
                json.loads(row["payload"])
                if isinstance(row["payload"], str)
                else row["payload"]
            )
            await sink.publish(row["owner_id"], TaskUpdated.model_validate(payload))
            await conn.execute(
                "UPDATE task_events SET published_at=NOW() WHERE id=$1 AND published_at IS NULL",
                row["id"],
            )
