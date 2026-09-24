"""In-process fake-router coverage; tests never open sockets or access Kubernetes."""

from __future__ import annotations

import asyncio
import json
import unittest
from unittest.mock import AsyncMock, patch
from urllib.parse import parse_qs, urlsplit
from uuid import uuid4

from mainloop.config import SubstrateActorBinding, settings
from mainloop.runtime import native_sessions
from mainloop.runtime.herdr import WorkspaceUnavailable
from mainloop.runtime.substrate_workspace import SubstrateWorkspace, _Response

FIXTURE_VALUE = "fixture-shim-value-one"
ROTATED_FIXTURE_VALUE = "fixture-shim-value-two"
FAKE_SHIM_NAME = "shim-fixture"


class FakeRouterAndShim:
    """In-process CONNECT/router and authenticated shim model for transport contracts."""

    def __init__(self):
        self.suspended = False
        self.capacity = False
        self.expected_token = FIXTURE_VALUE
        self.inflight: set[str] = set()
        self.turns: dict[str, dict] = {}
        self.journal_lines = [
            '{"type":"user"}',
            '{"type":"assistant"}',
            '{"type":"system","subtype":"turn_duration"}',
        ]
        self.connects: list[tuple[str, str]] = []
        self.requests: list[tuple[str, str, dict]] = []

    def request(
        self,
        *,
        actor: str,
        atespace: str,
        method: str,
        path: str,
        token: str | None,
        body: dict | None,
        **_unused,
    ) -> _Response:
        self.connects.append(("CONNECT", f"{atespace}/{actor}"))
        if self.capacity:
            return _Response(503, "capacity unavailable")
        body = body or {}
        self.requests.append((method, path, body))
        if (method, path) != ("GET", "/healthz") and token != self.expected_token:
            return _Response(401, "unauthorized")
        if method == "GET" and path == "/healthz":
            if self.suspended:
                self.suspended = False
            return _Response(200, "ok")
        parsed = urlsplit(path)
        query = parse_qs(parsed.query)
        if method == "GET" and parsed.path == "/agent/ready":
            return _Response(
                200, json.dumps({"agent": query["agent"][0], "configured": True})
            )
        if method == "GET" and parsed.path == "/turn/status":
            turn = self.turns.get(query.get("agent", [""])[0])
            return (
                _Response(200, json.dumps(turn)) if turn else _Response(404, "no turn")
            )
        if method == "POST" and parsed.path == "/turn":
            agent = body.get("agent")
            if agent in self.inflight:
                return _Response(409, "turn already in flight")
            turn = {
                "id": str(uuid4()),
                "agent": agent,
                "status": "running",
                "native_session_id": body.get("session_id") or "native-fixture-id",
                "events": [],
            }
            self.turns[agent] = turn
            self.inflight.add(agent)
            return _Response(202, json.dumps({"id": turn["id"], "status": "running"}))
        if method == "POST" and parsed.path == "/turn/stop":
            agent = body.get("agent")
            self.inflight.discard(agent)
            turn = self.turns.get(agent)
            if turn:
                turn["status"] = "interrupted"
            return _Response(200, json.dumps({"status": "interrupted"}))
        if method == "GET" and parsed.path == "/journal":
            start = int(query.get("from", ["0"])[0])
            limit = int(query.get("limit", ["200"])[0])
            document = {
                "file": "/fake/fixture-session.jsonl",
                "total_lines": len(self.journal_lines),
                "lines": [
                    {"line": n, "text": line}
                    for n, line in enumerate(self.journal_lines, 1)
                    if n > start
                ][:limit],
            }
            return _Response(200, json.dumps(document))
        return _Response(404, "not found")


class FakeSubstrateWorkspace(SubstrateWorkspace):
    def __init__(self, router: FakeRouterAndShim, **kwargs):
        super().__init__(**kwargs)
        self.router = router
        self.fixture_value = FIXTURE_VALUE
        self.fixture_reads = 0

    async def _token(self) -> str:
        if self._token_value is None:
            self.fixture_reads += 1
            self._token_value = self.fixture_value
        return self._token_value

    async def _exchange(
        self, method: str, path: str, *, token: str | None, body: dict | None
    ):
        return self.router.request(
            actor=self.actor,
            atespace=self.atespace,
            method=method,
            path=path,
            token=token,
            body=body,
        )


def fake_workspace(router: FakeRouterAndShim, *, shim_name=FAKE_SHIM_NAME):
    return FakeSubstrateWorkspace(
        router,
        atespace="atespace-fixture",
        actor="actor-fixture",
        agent="claude",
        shim_token_secret_name=shim_name,
        router_address="http://router-fixture:8081",
        timeout=2,
    )


class SubstrateWorkspaceTests(unittest.TestCase):
    def test_start_send_status_native_id_and_journal_pages(self):
        async def exercise():
            router = FakeRouterAndShim()
            router.suspended = True
            workspace = fake_workspace(router)
            ident = await workspace.start(
                "claude", "agent-fixture", native_id=None, resume=False
            )
            self.assertEqual(ident["actor"], "actor-fixture")
            self.assertFalse(router.suspended)

            await workspace.send("agent-fixture", "fixture prompt")
            status = await workspace.agent_status("agent-fixture")
            self.assertEqual(status["status"], "running")
            self.assertEqual(
                await workspace.native_id("agent-fixture"), "native-fixture-id"
            )
            first = await workspace.journal("agent-fixture", "native-fixture-id", 0)
            next_page = await workspace.journal("agent-fixture", "native-fixture-id", 1)
            self.assertEqual(first.file, "/fake/fixture-session.jsonl")
            self.assertEqual(first.total_lines, 3)
            self.assertEqual(first.lines[0], (1, '{"type":"user"}'))
            self.assertEqual(next_page.lines[0], (2, '{"type":"assistant"}'))
            self.assertIn(
                ("CONNECT", "atespace-fixture/actor-fixture"), router.connects
            )
            sent = [
                body
                for method, path, body in router.requests
                if method == "POST" and path == "/turn"
            ]
            self.assertEqual(sent, [{"agent": "claude", "prompt": "fixture prompt"}])

        asyncio.run(exercise())

    def test_capacity_503_maps_to_workspace_unavailable(self):
        async def exercise():
            router = FakeRouterAndShim()
            router.capacity = True
            with self.assertRaises(WorkspaceUnavailable):
                await fake_workspace(router).require_ready()

        asyncio.run(exercise())

    def test_concurrent_turn_and_bad_token_are_rejected(self):
        async def exercise():
            router = FakeRouterAndShim()
            workspace = fake_workspace(router)
            await workspace.send("agent-fixture", "first fixture prompt")
            with self.assertRaisesRegex(RuntimeError, "409"):
                await workspace.send("agent-fixture", "second fixture prompt")

            router.expected_token = ROTATED_FIXTURE_VALUE
            with self.assertRaisesRegex(RuntimeError, "401"):
                await workspace.agent_status("agent-fixture")

        asyncio.run(exercise())

    def test_401_refreshes_cached_secret_for_safe_request(self):
        async def exercise():
            router = FakeRouterAndShim()
            workspace = fake_workspace(router)
            await workspace.send("agent-fixture", "initial fixture prompt")
            router.expected_token = ROTATED_FIXTURE_VALUE
            workspace.fixture_value = ROTATED_FIXTURE_VALUE

            status = await workspace.agent_status("agent-fixture")

            self.assertEqual(status["status"], "running")
            self.assertEqual(workspace.fixture_reads, 2)
            status_requests = [
                request
                for request in router.requests
                if request[0] == "GET" and request[1].startswith("/turn/status?")
            ]
            self.assertEqual(len(status_requests), 2)

        asyncio.run(exercise())

    def test_401_clears_secret_without_replaying_turn(self):
        async def exercise():
            router = FakeRouterAndShim()
            workspace = fake_workspace(router)
            await workspace.send("agent-fixture", "initial fixture prompt")
            router.expected_token = ROTATED_FIXTURE_VALUE
            workspace.fixture_value = ROTATED_FIXTURE_VALUE
            previous_turn_count = sum(
                1
                for request in router.requests
                if request[0] == "POST" and request[1] == "/turn"
            )

            with self.assertRaisesRegex(RuntimeError, "401"):
                await workspace.send("agent-fixture", "one-shot fixture prompt")

            turn_requests = [
                request
                for request in router.requests
                if request[0] == "POST" and request[1] == "/turn"
            ]
            self.assertEqual(len(turn_requests), previous_turn_count + 1)
            self.assertEqual(workspace.fixture_reads, 1)
            self.assertIsNone(workspace._token_value)

        asyncio.run(exercise())

    def test_native_sessions_delivery_uses_configured_actor(self):
        async def exercise():
            router = FakeRouterAndShim()
            binding = {
                "session_id": "session-fixture",
                "kind": "claude",
                "role": "agent",
                "agent_name": "agent-fixture",
                "native_session_id": "native-fixture-id",
                "journal_cursor": 0,
                "generation": 1,
                "journal_ref": None,
                "model": None,
            }
            workspace_binding = SubstrateActorBinding(
                atespace="atespace-configured",
                actor="actor-configured",
                shim_token_secret_name=FAKE_SHIM_NAME,
            )
            key = (
                "substrate",
                workspace_binding.atespace,
                workspace_binding.actor,
                workspace_binding.shim_token_secret_name,
                "claude",
            )

            async def fake_exchange(workspace, method, path, *, token, body):
                return router.request(
                    actor=workspace.actor,
                    atespace=workspace.atespace,
                    method=method,
                    path=path,
                    token=token,
                    body=body,
                )

            async def fake_token(_workspace):
                return FIXTURE_VALUE

            with (
                patch.object(settings, "workspace_runtime", "substrate"),
                patch.object(
                    settings,
                    "substrate_router_address",
                    "http://router-fixture:8081",
                ),
                patch.object(
                    settings,
                    "substrate_shim_secret_namespace",
                    "namespace-fixture",
                ),
                patch.object(
                    settings,
                    "substrate_actor_bindings",
                    {"claude": workspace_binding},
                ),
                patch.object(SubstrateWorkspace, "_exchange", fake_exchange),
                patch.object(SubstrateWorkspace, "_token", fake_token),
                patch.object(
                    native_sessions,
                    "get_binding",
                    new=AsyncMock(return_value=binding),
                ),
                patch.object(native_sessions, "_update_binding", new=AsyncMock()),
                patch.object(
                    native_sessions, "_set_delivery", new=AsyncMock()
                ) as set_delivery,
                patch.object(native_sessions, "sync", new=AsyncMock()) as sync,
            ):
                native_sessions._workspaces.pop(key, None)
                self.assertEqual(settings.workspace_runtime, "substrate")
                self.assertIsInstance(
                    native_sessions.workspace_for(binding), SubstrateWorkspace
                )
                await asyncio.wait_for(
                    native_sessions._deliver(
                        "session-fixture",
                        "message-fixture",
                        "configured fixture prompt",
                    ),
                    timeout=2,
                )
                workspace = native_sessions.workspace_for(binding)
                self.assertIsInstance(workspace, SubstrateWorkspace)
                self.assertEqual(
                    (workspace.atespace, workspace.actor, workspace.secret_name),
                    ("atespace-configured", "actor-configured", FAKE_SHIM_NAME),
                )
                self.assertEqual(workspace.secret_namespace, "namespace-fixture")
                self.assertIn(
                    ("CONNECT", "atespace-configured/actor-configured"), router.connects
                )
                self.assertEqual(
                    [
                        request
                        for request in router.requests
                        if request[0] == "POST" and request[1] == "/turn"
                    ],
                    [
                        (
                            "POST",
                            "/turn",
                            {
                                "agent": "claude",
                                "prompt": "configured fixture prompt",
                                "session_id": "native-fixture-id",
                            },
                        )
                    ],
                )
                set_delivery.assert_awaited_once_with(
                    "message-fixture", "sending", cursor_before=3
                )
                sync.assert_awaited_once_with("session-fixture")
            native_sessions._workspaces.pop(key, None)

        asyncio.run(exercise())


if __name__ == "__main__":
    unittest.main()
