"""Native runtime identity survives session summary serialization (no live services)."""

import unittest
from contextlib import asynccontextmanager
from datetime import datetime
from unittest.mock import AsyncMock, patch

from mainloop.db.postgres import Database


class SessionKindTests(unittest.IsolatedAsyncioTestCase):
    async def test_session_reads_preserve_binding_kind_and_unbound_rows(self):
        database = Database()
        database._pool = object()
        rows = [
            {
                "id": kind or "legacy",
                "user_id": "owner",
                "main_thread_id": "mt",
                "title": "session",
                "description": "test",
                "prompt": "test",
                "conversation_id": "conversation",
                "status": "waiting_on_user",
                "created_at": datetime(2026, 10, 7),
                "agent_kind": kind,
            }
            for kind in ("codex", "claude", None)
        ]
        connection = AsyncMock()
        connection.fetch.return_value = rows

        @asynccontextmanager
        async def connect():
            yield connection

        with patch.object(database, "connection", connect):
            sessions = await database.list_sessions("owner")
            self.assertEqual(
                [s.model_dump()["agent_kind"] for s in sessions],
                ["codex", "claude", None],
            )
            for row in rows:
                connection.fetchrow.return_value = row
                session = await database.get_session(row["id"])
                self.assertEqual(session.agent_kind, row["agent_kind"])

        # Reads derive identity from the binding, without requiring a new sessions column.
        self.assertIn("b.kind", connection.fetch.call_args.args[0])
        self.assertIn("b.kind", connection.fetchrow.call_args.args[0])
