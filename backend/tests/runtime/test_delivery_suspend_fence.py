"""The delivery write shares the workspace row lock used by suspend reservation."""

from __future__ import annotations

import unittest
from contextlib import asynccontextmanager
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch

from mainloop.db import db
from mainloop.runtime.native_sessions import _record_delivery_message


class FakeConnection:
    def __init__(self, workspace):
        self.workspace = workspace
        self.events: list[str] = []

    @asynccontextmanager
    async def transaction(self):
        self.events.append("begin")
        try:
            yield self
        except Exception:
            self.events.append("rollback")
            raise
        else:
            self.events.append("commit")

    async def fetchrow(self, query, *_args):
        if "FROM workspace_bindings" in query:
            self.events.append("workspace-lock")
            if "FOR UPDATE" not in query:
                raise AssertionError(
                    "delivery recording must lock the workspace binding row"
                )
            return {"workspace_id": "session-1"}
        elif "FROM workspace_lifecycles" in query:
            self.events.append("lifecycle-read")
        else:
            raise AssertionError(f"unexpected fetchrow query: {query}")
        return self.workspace

    async def execute(self, query, *_args):
        if "INSERT INTO native_deliveries" in query:
            self.events.append("delivery-insert")


def fake_connection(connection):
    @asynccontextmanager
    async def acquire():
        yield connection

    return acquire()


class DeliverySuspendFenceTests(unittest.IsolatedAsyncioTestCase):
    async def test_message_and_delivery_are_recorded_after_the_workspace_lock(self):
        connection = FakeConnection(
            {"desired_state": "active", "observed_state": "running"}
        )

        async def create_message(**_kwargs):
            connection.events.append("message-insert")
            return SimpleNamespace(id="message-1")

        with (
            patch.object(
                db,
                "connection",
                return_value=fake_connection(connection),
            ),
            patch.object(
                db,
                "create_message",
                new=AsyncMock(side_effect=create_message),
            ) as create,
        ):
            message_id = await _record_delivery_message(
                session_id="session-1",
                conversation_id="conversation-1",
                text="hello",
                state="recorded",
                source="user",
            )

        self.assertEqual(message_id, "message-1")
        self.assertEqual(
            connection.events,
            [
                "begin",
                "workspace-lock",
                "lifecycle-read",
                "message-insert",
                "delivery-insert",
                "commit",
            ],
        )
        self.assertEqual(create.await_args.kwargs["conn"], connection)

    async def test_suspending_workspace_rejects_the_delivery_before_recording(self):
        connection = FakeConnection(
            {"desired_state": "suspended", "observed_state": "suspending"}
        )
        with (
            patch.object(
                db,
                "connection",
                return_value=fake_connection(connection),
            ),
            patch.object(
                db,
                "create_message",
                new=AsyncMock(),
            ) as create,
        ):
            with self.assertRaisesRegex(ValueError, "resume it before sending"):
                await _record_delivery_message(
                    session_id="session-1",
                    conversation_id="conversation-1",
                    text="hello",
                    state="recorded",
                    source="user",
                )

        self.assertEqual(
            connection.events,
            ["begin", "workspace-lock", "lifecycle-read", "rollback"],
        )
        create.assert_not_awaited()


if __name__ == "__main__":
    unittest.main()
