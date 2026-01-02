"""Server-Sent Events support for real-time updates."""

import asyncio
import json
import logging
import uuid
from collections import defaultdict
from dataclasses import dataclass, field
from datetime import datetime
from enum import Enum
from typing import Any, AsyncGenerator

from fastapi import Request
from fastapi.responses import StreamingResponse

logger = logging.getLogger(__name__)


class EventType(str, Enum):
    """SSE event types."""

    # Global events
    TASK_UPDATED = "task:updated"
    INBOX_UPDATED = "inbox:updated"
    HEARTBEAT = "heartbeat"

    # Task log events
    LOG = "log"
    STATUS = "status"
    END = "end"


@dataclass
class SSEEvent:
    """An SSE event to send to clients."""

    event: str
    data: dict[str, Any]
    id: str = field(default_factory=lambda: str(uuid.uuid4())[:8])

    def encode(self) -> str:
        """Encode as SSE format."""
        lines = [
            f"id: {self.id}",
            f"event: {self.event}",
            f"data: {json.dumps(self.data)}",
            "",  # Empty line to end the event
        ]
        return "\n".join(lines) + "\n"


class EventBus:
    """Simple in-process event bus for SSE.

    For production scaling, this should be backed by Redis pub/sub.
    """

    def __init__(self):
        # user_id -> list of queues
        self._user_queues: dict[str, list[asyncio.Queue]] = defaultdict(list)
        # task_id -> list of queues (for log streaming)
        self._task_queues: dict[str, list[asyncio.Queue]] = defaultdict(list)
        self._lock = asyncio.Lock()

    async def subscribe_user(self, user_id: str) -> asyncio.Queue:
        """Subscribe to events for a user."""
        queue: asyncio.Queue = asyncio.Queue()
        async with self._lock:
            self._user_queues[user_id].append(queue)
        logger.info(f"User {user_id} subscribed to events")
        return queue

    async def unsubscribe_user(self, user_id: str, queue: asyncio.Queue):
        """Unsubscribe from user events."""
        async with self._lock:
            if user_id in self._user_queues:
                try:
                    self._user_queues[user_id].remove(queue)
                    if not self._user_queues[user_id]:
                        del self._user_queues[user_id]
                except ValueError:
                    pass
        logger.info(f"User {user_id} unsubscribed from events")

    async def subscribe_task(self, task_id: str) -> asyncio.Queue:
        """Subscribe to events for a task (log streaming)."""
        queue: asyncio.Queue = asyncio.Queue()
        async with self._lock:
            self._task_queues[task_id].append(queue)
        logger.info(f"Subscribed to task {task_id} logs")
        return queue

    async def unsubscribe_task(self, task_id: str, queue: asyncio.Queue):
        """Unsubscribe from task events."""
        async with self._lock:
            if task_id in self._task_queues:
                try:
                    self._task_queues[task_id].remove(queue)
                    if not self._task_queues[task_id]:
                        del self._task_queues[task_id]
                except ValueError:
                    pass
        logger.info(f"Unsubscribed from task {task_id} logs")

    async def publish_to_user(self, user_id: str, event: SSEEvent):
        """Publish an event to all subscribers for a user."""
        async with self._lock:
            queues = self._user_queues.get(user_id, [])
            for queue in queues:
                try:
                    queue.put_nowait(event)
                except asyncio.QueueFull:
                    logger.warning(f"Queue full for user {user_id}, dropping event")

    async def publish_to_task(self, task_id: str, event: SSEEvent):
        """Publish an event to all subscribers for a task."""
        async with self._lock:
            queues = self._task_queues.get(task_id, [])
            for queue in queues:
                try:
                    queue.put_nowait(event)
                except asyncio.QueueFull:
                    logger.warning(f"Queue full for task {task_id}, dropping event")


# Global event bus instance
event_bus = EventBus()


async def event_stream(
    user_id: str,
    request: Request,
    heartbeat_interval: int = 30,
) -> AsyncGenerator[str, None]:
    """Generate SSE events for a user.

    Sends heartbeat pings every heartbeat_interval seconds to keep connection alive.
    """
    queue = await event_bus.subscribe_user(user_id)

    try:
        # Send initial connected event
        yield SSEEvent(
            event="connected",
            data={"user_id": user_id, "timestamp": datetime.utcnow().isoformat()},
        ).encode()

        while True:
            # Check if client disconnected
            if await request.is_disconnected():
                break

            try:
                # Wait for event with timeout for heartbeat
                event = await asyncio.wait_for(
                    queue.get(),
                    timeout=heartbeat_interval,
                )
                yield event.encode()
            except asyncio.TimeoutError:
                # Send heartbeat
                yield SSEEvent(
                    event=EventType.HEARTBEAT,
                    data={"timestamp": datetime.utcnow().isoformat()},
                ).encode()
    finally:
        await event_bus.unsubscribe_user(user_id, queue)


async def task_log_stream(
    task_id: str,
    user_id: str,
    request: Request,
    poll_interval: int = 2,
) -> AsyncGenerator[str, None]:
    """Generate SSE events for task logs.

    Polls K8s logs and streams them to the client.
    Also listens for task status changes.
    """
    from mainloop.db import db
    from mainloop.services.k8s_jobs import get_job_logs

    queue = await event_bus.subscribe_task(task_id)
    last_log_length = 0
    namespace = f"task-{task_id[:8]}"

    try:
        # Send initial status
        task = await db.get_worker_task(task_id)
        if task:
            yield SSEEvent(
                event=EventType.STATUS,
                data={"status": task.status.value, "task_id": task_id},
            ).encode()

        while True:
            if await request.is_disconnected():
                break

            # Check for queued events first (status changes from event bus)
            try:
                event = queue.get_nowait()
                yield event.encode()
                if event.event == EventType.END:
                    break
            except asyncio.QueueEmpty:
                pass

            # Poll for new logs
            try:
                logs = await get_job_logs(task_id, namespace)
                if logs and len(logs) > last_log_length:
                    new_logs = logs[last_log_length:]
                    last_log_length = len(logs)
                    yield SSEEvent(
                        event=EventType.LOG,
                        data={"logs": new_logs, "task_id": task_id},
                    ).encode()
            except Exception as e:
                logger.debug(f"Failed to get logs for task {task_id}: {e}")

            # Check task status
            task = await db.get_worker_task(task_id)
            if task and task.status.value in ("completed", "failed", "cancelled"):
                yield SSEEvent(
                    event=EventType.STATUS,
                    data={"status": task.status.value, "task_id": task_id},
                ).encode()
                yield SSEEvent(
                    event=EventType.END,
                    data={"task_id": task_id},
                ).encode()
                break

            await asyncio.sleep(poll_interval)
    finally:
        await event_bus.unsubscribe_task(task_id, queue)


def create_sse_response(generator: AsyncGenerator[str, None]) -> StreamingResponse:
    """Create an SSE StreamingResponse."""
    return StreamingResponse(
        generator,
        media_type="text/event-stream",
        headers={
            "Cache-Control": "no-cache",
            "Connection": "keep-alive",
            "X-Accel-Buffering": "no",  # Disable nginx buffering
        },
    )


# Helper functions to publish events from other parts of the app


async def notify_task_updated(user_id: str, task_id: str, status: str, **extra):
    """Notify user that a task was updated."""
    await event_bus.publish_to_user(
        user_id,
        SSEEvent(
            event=EventType.TASK_UPDATED,
            data={"task_id": task_id, "status": status, **extra},
        ),
    )
    # Also notify task log subscribers
    await event_bus.publish_to_task(
        task_id,
        SSEEvent(
            event=EventType.STATUS,
            data={"task_id": task_id, "status": status, **extra},
        ),
    )


async def notify_inbox_updated(
    user_id: str, item_id: str | None = None, unread_count: int | None = None
):
    """Notify user that inbox was updated."""
    data: dict[str, Any] = {}
    if item_id:
        data["item_id"] = item_id
    if unread_count is not None:
        data["unread_count"] = unread_count

    await event_bus.publish_to_user(
        user_id,
        SSEEvent(
            event=EventType.INBOX_UPDATED,
            data=data,
        ),
    )
