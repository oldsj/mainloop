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

    INBOX_UPDATED = "inbox:updated"
    SESSION_UPDATED = "session:updated"
    SESSION_NEEDS_INPUT = "session:needs_input"
    SESSION_MESSAGE = "session:message"
    HEARTBEAT = "heartbeat"


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

    async def publish_to_user(self, user_id: str, event: SSEEvent):
        """Publish an event to all subscribers for a user."""
        async with self._lock:
            queues = self._user_queues.get(user_id, [])
            for queue in queues:
                try:
                    queue.put_nowait(event)
                except asyncio.QueueFull:
                    logger.warning(f"Queue full for user {user_id}, dropping event")


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
    except asyncio.CancelledError:
        # Graceful shutdown - don't propagate as error
        pass
    finally:
        await event_bus.unsubscribe_user(user_id, queue)


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


async def notify_session_updated(user_id: str, session_id: str, status: str, **extra):
    """Notify user that a session was updated."""
    await event_bus.publish_to_user(
        user_id,
        SSEEvent(
            event=EventType.SESSION_UPDATED,
            data={"session_id": session_id, "status": status, **extra},
        ),
    )


async def notify_session_needs_input(
    user_id: str, session_id: str, title: str, preview: str
):
    """Notify user that a session needs their input."""
    await event_bus.publish_to_user(
        user_id,
        SSEEvent(
            event=EventType.SESSION_NEEDS_INPUT,
            data={
                "session_id": session_id,
                "title": title,
                "preview": preview,
            },
        ),
    )


async def notify_session_message(
    user_id: str, session_id: str, message_id: str, role: str
):
    """Notify user of a new message in a session's conversation."""
    await event_bus.publish_to_user(
        user_id,
        SSEEvent(
            event=EventType.SESSION_MESSAGE,
            data={
                "session_id": session_id,
                "message_id": message_id,
                "role": role,
            },
        ),
    )
