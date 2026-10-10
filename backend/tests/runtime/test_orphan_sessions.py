"""The operator sweep for kagent Sessions that have no ``native_bindings`` row."""

from __future__ import annotations

import unittest
from unittest.mock import AsyncMock, patch

import httpx
from mainloop.config import settings
from mainloop.runtime import native_sessions as ns
from mainloop.runtime import orphan_sessions
from mainloop.runtime.kagent_client import (
    AgentRef,
    KagentClient,
    RuntimeOperation,
    RuntimeState,
)
from tests.runtime.kagent_fake import FakeKagent

BOUND = "00000000-0000-4000-8000-0000000000b1"
ORPHAN_A = "00000000-0000-4000-8000-0000000000a1"
ORPHAN_B = "00000000-0000-4000-8000-0000000000a2"
ORPHAN_C = "00000000-0000-4000-8000-0000000000a3"
FOREIGN = "00000000-0000-4000-8000-0000000000f1"
UNKNOWN_AGENT = "00000000-0000-4000-8000-0000000000f2"
READY = (RuntimeState.READY, RuntimeOperation.NONE)


class SweepTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.fake = FakeKagent()
        self.fake.sessions.update(
            {
                BOUND: READY,
                ORPHAN_A: READY,
                ORPHAN_B: (RuntimeState.SUSPENDED, RuntimeOperation.NONE),
                ORPHAN_C: (RuntimeState.DELETED, RuntimeOperation.NONE),
            }
        )
        ours = AgentRef(
            settings.kagent_namespace, settings.kagent_workspace_claude_agent
        ).encode()
        for sid in self.fake.sessions:
            self.fake.session_agents[sid] = ours
        http = httpx.AsyncClient(
            transport=self.fake.transport(), base_url="http://kagent.test"
        )
        ns._client = KagentClient("http://kagent.test", user_id="mainloop", client=http)
        self.addAsyncCleanup(ns.close_client)
        self.bound = {BOUND}
        self.unresolved = 0
        patcher = patch.object(
            orphan_sessions,
            "_bound_ids",
            AsyncMock(side_effect=lambda: (set(self.bound), self.unresolved)),
        )
        patcher.start()
        self.addCleanup(patcher.stop)
        self.lines: list[str] = []

    def out(self, line: str) -> None:
        self.lines.append(line)

    async def test_it_lists_the_sessions_no_binding_names_and_deletes_nothing(self):
        status = await orphan_sessions.sweep(False, self.out)
        self.assertEqual(status, 0)
        text = "\n".join(self.lines)
        self.assertIn(ORPHAN_A, text)
        self.assertIn(ORPHAN_B, text)
        self.assertNotIn(BOUND, text)
        self.assertNotIn(ORPHAN_C, text)  # already deleted
        self.assertEqual(self.fake.session_calls("DeleteSession"), [])

    async def test_delete_removes_only_the_orphans(self):
        status = await orphan_sessions.sweep(True, self.out)
        self.assertEqual(status, 0)
        self.assertEqual(self.fake.sessions[ORPHAN_A][0], RuntimeState.DELETED)
        self.assertEqual(self.fake.sessions[ORPHAN_B][0], RuntimeState.DELETED)
        self.assertEqual(self.fake.sessions[BOUND][0], RuntimeState.READY)

    async def test_a_session_bound_since_the_listing_is_not_deleted(self):
        real = orphan_sessions._bound_ids
        calls = {"n": 0}

        async def claim_a_after_the_listing():
            calls["n"] += 1
            if calls["n"] > 1:
                return {BOUND, ORPHAN_A}, 0
            return await real()

        with patch.object(orphan_sessions, "_bound_ids", claim_a_after_the_listing):
            await orphan_sessions.sweep(True, self.out)
        self.assertEqual(self.fake.sessions[ORPHAN_A][0], RuntimeState.READY)
        self.assertEqual(self.fake.sessions[ORPHAN_B][0], RuntimeState.DELETED)
        self.assertIn("skipped (now bound)", "\n".join(self.lines))

    async def test_it_follows_the_page_tokens(self):
        self.fake.list_page_size = 1
        orphans, _ = await orphan_sessions.find_orphans()
        self.assertEqual({s.id for s in orphans}, {ORPHAN_A, ORPHAN_B})
        self.assertEqual(len(self.fake.session_calls("ListSessions")), 4)

    async def test_unresolved_bindings_are_warned_about(self):
        self.unresolved = 2
        await orphan_sessions.sweep(False, self.out)
        self.assertIn("2 binding(s) have no kagent Session id", self.lines[0])

    async def test_nothing_to_do(self):
        self.bound |= {ORPHAN_A, ORPHAN_B}
        self.assertEqual(await orphan_sessions.sweep(True, self.out), 0)
        self.assertIn("No orphaned kagent Sessions.", self.lines)

    async def test_unresolved_create_blocks_deletion(self):
        self.unresolved = 1
        self.assertEqual(await orphan_sessions.sweep(True, self.out), 1)
        self.assertEqual(self.fake.session_calls("DeleteSession"), [])

    async def test_unresolved_create_appearing_after_listing_blocks_deletion(self):
        with patch.object(
            orphan_sessions,
            "_bound_ids",
            AsyncMock(side_effect=[({BOUND}, 0), ({BOUND}, 1), ({BOUND}, 1)]),
        ):
            self.assertEqual(await orphan_sessions.sweep(True, self.out), 1)
        self.assertEqual(self.fake.session_calls("DeleteSession"), [])

    async def test_sessions_on_other_instances_agents_are_ignored(self):
        # Production and mainloop-dev share one kagent principal; neither sweeps the other.
        self.fake.sessions[FOREIGN] = READY
        self.fake.session_agents[FOREIGN] = AgentRef(
            settings.kagent_namespace, "claude-workspace-dev"
        ).encode()
        self.fake.sessions[UNKNOWN_AGENT] = READY
        orphans, _ = await orphan_sessions.find_orphans()
        self.assertEqual({s.id for s in orphans}, {ORPHAN_A, ORPHAN_B})
        status = await orphan_sessions.sweep(True, self.out)
        self.assertEqual(status, 0)
        self.assertNotIn(FOREIGN, "\n".join(self.lines))
        self.assertNotIn(UNKNOWN_AGENT, "\n".join(self.lines))
        self.assertEqual(self.fake.sessions[FOREIGN][0], RuntimeState.READY)
        self.assertEqual(self.fake.sessions[UNKNOWN_AGENT][0], RuntimeState.READY)

    def test_instance_agents_are_the_configured_ones(self):
        agents = orphan_sessions.instance_agents()
        self.assertIn(("kagent", settings.kagent_main_agent), agents)
        self.assertIn(("kagent", settings.kagent_codex_agent), agents)
        with patch.object(
            settings, "kagent_workspace_claude_agent", "claude-workspace-dev"
        ):
            self.assertIn(
                ("kagent", "claude-workspace-dev"), orphan_sessions.instance_agents()
            )
        self.assertNotIn(("kagent", "claude-workspace-dev"), agents)

    async def test_a_failed_delete_is_reported_and_the_exit_status_is_nonzero(self):
        with patch.object(
            ns.get_client(),
            "delete_session",
            AsyncMock(side_effect=ns.KagentError("down")),
        ):
            status = await orphan_sessions.sweep(True, self.out)
        self.assertEqual(status, 1)
        self.assertIn("not deleted", "\n".join(self.lines))


if __name__ == "__main__":
    unittest.main()
