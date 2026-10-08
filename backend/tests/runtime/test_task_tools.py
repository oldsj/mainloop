"""MCP task surface and per-call denial, without native turns or provider access."""

import unittest
from unittest.mock import AsyncMock, patch

from fastapi import HTTPException
from mainloop.mcp_app import TOOLS, invoke
from mainloop.runtime.agent_tools import AgentService, Ctx
from mainloop.runtime.policy import Actor, PolicyError, surface_tools
from mainloop.tasks.principal import TaskPrincipal
from mainloop.tasks.service import ports
from tests.runtime.test_context_model import FakeStore


class TaskToolTests(unittest.IsolatedAsyncioTestCase):
    async def test_discovery_and_direct_denial_for_every_role_grant_and_depth(self):
        service = AgentService(FakeStore())
        for role in ("main", "supervisor", "child", "agent"):
            for grant in ("coordination", "workspace", "none"):
                for depth in (0, 1, 2, 3):
                    actor = Actor(role, depth, grant)
                    ctx = Ctx({"session_id": "fixture"}, actor)
                    for tool in TOOLS.keys() - surface_tools(actor):
                        result = await invoke(service, ctx, tool, {})
                        self.assertTrue(result.isError, (actor, tool))
                        self.assertIn("[role]", result.content[0].text)

    async def test_task_schema_rejects_caller_authority_and_legacy_arguments(self):
        request = {
            "request_id": "r",
            "title": "task",
            "brief": "brief",
            "mode": "coordination",
        }
        service = AgentService(FakeStore())
        ctx = Ctx({"session_id": "fixture"}, Actor("main", 0))
        for field in (
            "owner_id",
            "role",
            "depth",
            "root_task_id",
            "agent_ref",
            "credential_ref",
            "kind",
            "topic",
        ):
            result = await invoke(
                service, ctx, "delegate", {**request, field: "escape"}
            )
            self.assertTrue(result.isError, field)
            self.assertIn("[validation]", result.content[0].text)

    async def test_missing_handoff_is_hidden_and_directly_denied(self):
        with patch.object(ports, "handoff", None):
            for actor in (Actor("main", 0), Actor("supervisor", 1)):
                self.assertNotIn("task_retry", surface_tools(actor))
                self.assertNotIn("task_reassign", surface_tools(actor))
                service = AgentService(FakeStore())
                with self.assertRaises(PolicyError):
                    await service.task_retry(Ctx({}, actor), task_id="t")

    async def test_task_child_requires_a_persisted_principal_not_session_parent_depth(
        self,
    ):
        store = FakeStore()
        store.bindings["main-1"].update(role="child", parent_session_id="fake-main")
        store.task_principal = AsyncMock(return_value=None)
        with self.assertRaises(HTTPException):
            await AgentService(store).authenticate("tok-main")
        store.task_principal.return_value = TaskPrincipal(
            "u",
            binding_id="main-1",
            role="child",
            task_id="child-task",
            attempt_id="attempt",
            root_task_id="root",
            depth=2,
        )
        ctx = await AgentService(store).authenticate("tok-main")
        self.assertEqual(ctx.actor.depth, 2)
        self.assertNotIn("delegate", surface_tools(ctx.actor))

    async def test_reports_need_exact_identity_and_event_fields(self):
        store = FakeStore()
        store.task_call = AsyncMock(return_value={"text": "claim recorded"})
        ctx = Ctx({}, Actor("child", 2))
        service = AgentService(store)
        result = await invoke(service, ctx, "report", {"summary": "turn finished"})
        self.assertTrue(result.isError)
        store.task_call.assert_not_awaited()
        payload = {
            "task_id": "t",
            "attempt_id": "a",
            "request_id": "event-1",
            "outcome": "progress",
            "summary": "checks running",
        }
        result = await invoke(service, ctx, "report", payload)
        self.assertFalse(result.isError)
        self.assertEqual(store.task_call.call_args.args[1], "report")
