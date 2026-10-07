"""Pure lifecycle checks and runtime seams; no cluster or live agents."""

import unittest
from contextlib import asynccontextmanager
from unittest.mock import AsyncMock, patch

from mainloop.runtime import native_sessions as ns
from mainloop.tasks import lifecycle


@asynccontextmanager
async def ordinary_guard(*_args):
    """In-memory ledger fixtures model ordinary sessions, not task persistence."""
    yield


class LifecycleTests(unittest.TestCase):
    def test_every_work_action_denies_terminal_stale_or_draining(self):
        row = dict(
            id="a",
            state="active",
            current_attempt_id="a",
            task_status="running",
            mode="code",
            claim_held=True,
            claim_generation=3,
            writer_generation=3,
        )
        for action in ("create", "submit", "resume", "preview"):
            self.assertIsNone(lifecycle.denial(row, action))
            for change in (
                {"state": "draining"},
                {"current_attempt_id": "old"},
                {"claim_held": False},
                {"claim_generation": 4},
                {"task_status": "cancelled"},
            ):
                self.assertIsNotNone(lifecycle.denial({**row, **change}, action))
        self.assertIsNotNone(lifecycle.denial(row, "archive"))
        self.assertIsNone(lifecycle.denial({**row, "state": "cancelled"}, "archive"))


class PinnedRoutingTests(unittest.IsolatedAsyncioTestCase):
    async def test_attempt_uses_pinned_agent_despite_registry_reload(self):
        binding = dict(session_id="s", role="supervisor", kind="claude")
        with patch.object(
            ns,
            "attempt_row",
            AsyncMock(
                return_value={"agent_ref": {"namespace": "frozen", "name": "original"}}
            ),
        ), patch.object(ns, "agent_ref", side_effect=AssertionError("registry used")):
            ref = await ns.binding_agent_ref(binding)
        self.assertEqual((ref.namespace, ref.name), ("frozen", "original"))

    async def test_missing_supervisor_attempt_cannot_route(self):
        with patch.object(ns, "attempt_row", AsyncMock(return_value=None)):
            with self.assertRaises(RuntimeError):
                await ns.binding_agent_ref(
                    dict(session_id="s", role="supervisor", kind="claude")
                )
