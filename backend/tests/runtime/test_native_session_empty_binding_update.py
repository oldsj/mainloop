"""Regression coverage for an unchanged native child binding on consecutive turns."""

from __future__ import annotations

import asyncio
import unittest
from unittest.mock import AsyncMock, patch

from mainloop.runtime import native_sessions
from mainloop.runtime.standing import content_hash


class _Connection:
    def __init__(self):
        self.executions: list[tuple[str, tuple]] = []

    async def execute(self, query: str, *args):
        self.executions.append((query, args))


class _ConnectionContext:
    def __init__(self, connection: _Connection):
        self.connection = connection

    async def __aenter__(self):
        return self.connection

    async def __aexit__(self, *_args):
        return None


class _RunningWorkspace:
    def __init__(self):
        self.sent: list[str] = []

    async def require_ready(self):
        return None

    async def agent_status(self, _name: str):
        return {"status": "running"}

    def set_resume_history(self, _resume: bool):
        return None

    def set_startup_options(self, _options: dict):
        return None

    async def prepare_credentials(self):
        return None

    async def send(self, _name: str, text: str):
        self.sent.append(text)


class NativeSessionEmptyBindingUpdateTests(unittest.TestCase):
    def test_two_turns_with_unchanged_child_context_use_valid_update_sql(self):
        async def exercise():
            standing = "Stable child context"
            binding = {
                "session_id": "child-session-fixture",
                "kind": "claude",
                "role": "child",
                "agent_name": "ml-claude-child-fixture",
                "native_session_id": None,
                "journal_ref": "journal-fixture",
                "generation": 1,
                "standing_hash": content_hash(standing),
                "approval_policy": "bypass-permissions",
            }
            connection = _Connection()
            workspace = _RunningWorkspace()

            async def render_child_context(_binding):
                return standing

            with (
                patch.object(
                    native_sessions,
                    "get_binding",
                    new=AsyncMock(side_effect=lambda _sid: dict(binding)),
                ),
                patch.object(native_sessions, "workspace_for", return_value=workspace),
                patch.object(
                    native_sessions,
                    "token_for",
                    return_value="ml_" + "a" * 64,
                ),
                patch(
                    "mainloop.runtime.delegation.render_for_binding",
                    new=AsyncMock(side_effect=render_child_context),
                ),
                patch.object(
                    native_sessions.db,
                    "connection",
                    side_effect=lambda: _ConnectionContext(connection),
                ),
                patch.object(native_sessions, "_set_delivery", new=AsyncMock()),
                patch.object(native_sessions, "sync", new=AsyncMock()),
            ):
                await native_sessions._deliver(
                    binding["session_id"], "message-one", "first turn"
                )
                await native_sessions._deliver(
                    binding["session_id"], "message-two", "second turn"
                )

            self.assertEqual(workspace.sent, ["first turn", "second turn"])
            self.assertEqual(len(connection.executions), 2)
            for query, args in connection.executions:
                self.assertIn("SET updated_at=NOW()", query)
                self.assertNotIn("SET ,", query)
                self.assertEqual(args, (binding["session_id"],))

        asyncio.run(exercise())


if __name__ == "__main__":
    unittest.main()
