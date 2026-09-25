"""Adapter-to-reconciler coverage for bounded native journal paging."""

from __future__ import annotations

import asyncio
import json
import unittest
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch

from mainloop.runtime import native_sessions
from mainloop.runtime.substrate_workspace import JournalSlice

from models import SessionStatus


def journal_fixture() -> list[tuple[int, str]]:
    lines = [(line, '{"type":"progress"}') for line in range(1, 201)]
    lines.extend(
        [
            (
                201,
                json.dumps(
                    {
                        "type": "user",
                        "uuid": "fixture-prompt",
                        "timestamp": "2026-09-24T00:00:00Z",
                        "message": {"role": "user", "content": "fixture prompt"},
                    }
                ),
            ),
            (
                202,
                json.dumps(
                    {
                        "type": "assistant",
                        "uuid": "fixture-reply",
                        "timestamp": "2026-09-24T00:00:01Z",
                        "message": {
                            "role": "assistant",
                            "content": [
                                {"type": "text", "text": "fixture reply past line 200"}
                            ],
                        },
                    }
                ),
            ),
            (
                203,
                json.dumps(
                    {
                        "type": "system",
                        "subtype": "turn_duration",
                        "timestamp": "2026-09-24T00:00:02Z",
                    }
                ),
            ),
        ]
    )
    return lines


class _Connection:
    def __init__(self):
        self.executions: list[tuple[str, tuple]] = []

    async def fetch(self, _query: str, *_args):
        return []

    async def execute(self, query: str, *args):
        self.executions.append((query, args))
        return "INSERT 0 1"


class _ConnectionContext:
    def __init__(self, connection: _Connection):
        self.connection = connection

    async def __aenter__(self):
        return self.connection

    async def __aexit__(self, *_args):
        return None


class _PagedWorkspace:
    def __init__(self):
        self.lines = journal_fixture()
        self.calls: list[int] = []

    async def journal(self, _name: str, _native_id: str, from_line: int):
        self.calls.append(from_line)
        return JournalSlice(
            "/fake/fixture-session.jsonl",
            len(self.lines),
            [line for line in self.lines if line[0] > from_line][:200],
        )

    async def credential_rejected(self):
        return False


class NativeSessionReconcileTests(unittest.TestCase):
    def test_completion_after_line_200_is_mirrored_once(self):
        async def exercise():
            binding = {
                "session_id": "fixture-session",
                "kind": "claude",
                "agent_name": "fixture-agent",
                "native_session_id": "fixture-native-id",
                "journal_cursor": 0,
                "journal_ref": None,
                "turns_in_lineage": 0,
                "continuations": 0,
                "context_tokens": None,
                "baseline_tokens": None,
                "role": "agent",
                "reported_at": None,
                "generation": 1,
            }
            conn = _Connection()
            ws = _PagedWorkspace()
            session = SimpleNamespace(
                conversation_id="fixture-conversation",
                status=SessionStatus.WAITING_ON_USER,
            )

            async def update_binding(_session_id: str, **fields):
                binding.update(fields)

            with (
                patch.object(
                    native_sessions,
                    "get_binding",
                    new=AsyncMock(side_effect=lambda _sid: dict(binding)),
                ),
                patch.object(native_sessions, "workspace_for", return_value=ws),
                patch.object(native_sessions, "_update_binding", new=update_binding),
                patch.object(
                    native_sessions.db,
                    "get_session",
                    new=AsyncMock(return_value=session),
                ),
                patch.object(
                    native_sessions.db,
                    "connection",
                    side_effect=lambda: _ConnectionContext(conn),
                ),
                patch.object(
                    native_sessions, "_open_count", new=AsyncMock(return_value=0)
                ),
                patch.object(native_sessions.db, "update_session", new=AsyncMock()),
            ):
                await native_sessions._sync_locked("fixture-session")
                await native_sessions._sync_locked("fixture-session")

            mirrored = [
                args
                for query, args in conn.executions
                if "INSERT INTO messages" in query and "'assistant'" in query
            ]
            self.assertEqual(len(mirrored), 1)
            self.assertEqual(mirrored[0][2], "fixture reply past line 200")
            self.assertEqual(binding["journal_cursor"], 203)
            self.assertEqual(ws.calls, [0, 200, 203])

        asyncio.run(exercise())


if __name__ == "__main__":
    unittest.main()
