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
from mainloop.runtime import substrate_workspace as substrate_workspace_module
from mainloop.runtime.substrate_workspace import (
    SubstrateWorkspace,
    WorkspaceUnavailable,
    _Response,
)

FIXTURE_VALUE = "fixture-shim-value-one"
ROTATED_FIXTURE_VALUE = "fixture-shim-value-two"
FAKE_SHIM_NAME = "shim-fixture"


class FakeRouterAndShim:
    """In-process CONNECT/router and authenticated shim model for transport contracts."""

    def __init__(self, *, token_installed=True):
        self.suspended = False
        self.capacity = False
        self.expected_token = FIXTURE_VALUE if token_installed else None
        self.token_installed = token_installed
        self.inflight: set[tuple[str, str]] = set()
        self.turns: dict[tuple[str, str], dict] = {}
        self.journal_lines = [
            '{"type":"user"}',
            '{"type":"assistant"}',
            '{"type":"system","subtype":"turn_duration"}',
        ]
        self.connects: list[tuple[str, str]] = []
        self.requests: list[tuple[str, str, dict]] = []
        self.credentials: dict[str, str] = {}

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
        if (method, path) not in (
            ("GET", "/healthz"),
            ("POST", "/token"),
        ) and token != self.expected_token:
            return _Response(401, "unauthorized")
        if method == "POST" and path == "/token":
            if self.token_installed:
                return _Response(409, "token already set")
            self.token_installed = True
            self.expected_token = body.get("token")
            return _Response(201, "token set")
        if method == "GET" and path == "/healthz":
            if self.suspended:
                self.suspended = False
            return _Response(200, "ok")
        parsed = urlsplit(path)
        query = parse_qs(parsed.query)
        if method == "PUT" and parsed.path == "/credential":
            self.credentials[body["name"]] = body["contents"]
            return _Response(200, "credential stored")
        if method == "GET" and parsed.path == "/agent/ready":
            return _Response(
                200, json.dumps({"agent": query["agent"][0], "configured": True})
            )
        if method == "GET" and parsed.path == "/ports":
            return _Response(200, json.dumps({"ports": [3000, 5173, 3000]}))
        if method == "GET" and parsed.path == "/turn/status":
            key = (
                query.get("agent", [""])[0],
                query.get("session_key", [""])[0],
            )
            turn = self.turns.get(key)
            return (
                _Response(200, json.dumps(turn)) if turn else _Response(404, "no turn")
            )
        if method == "POST" and parsed.path == "/turn":
            agent = body.get("agent")
            key = (agent, body.get("session_key", ""))
            if key in self.inflight:
                return _Response(409, "turn already in flight")
            turn = {
                "id": str(uuid4()),
                "agent": agent,
                "session_key": key[1],
                "status": "running",
                "native_session_id": body.get("session_id") or f"native-{key[1]}",
                "events": [],
            }
            self.turns[key] = turn
            self.inflight.add(key)
            return _Response(202, json.dumps({"id": turn["id"], "status": "running"}))
        if method == "POST" and parsed.path == "/turn/stop":
            key = (body.get("agent"), body.get("session_key", ""))
            self.inflight.discard(key)
            turn = self.turns.get(key)
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
        logical_session_id="session-fixture",
        router_address="http://router-fixture:8081",
        timeout=2,
        credential_broker=FakeCredentialBroker(),
    )


class FakeCredentialBroker:
    async def codex_placeholder_auth(self) -> str:
        return json.dumps(
            {
                "tokens": {
                    "id_token": "fixture.header.synthetic",
                    "access_token": "fixture.header.synthetic",
                    "refresh_token": "",
                    "account_id": "fixture-account",
                },
                "last_refresh": "2026-09-24T00:00:00Z",
            }
        )

    async def claude_placeholder_token(self) -> str:
        return "synthetic-claude-egress-placeholder"


class SubstrateWorkspaceTests(unittest.TestCase):
    def test_same_provider_bindings_have_distinct_logical_transports(self):
        first = {
            "session_id": "claude-session-a",
            "kind": "claude",
            "native_session_id": "native-a",
        }
        second = {
            "session_id": "claude-session-b",
            "kind": "claude",
            "native_session_id": "native-b",
        }
        actor_binding = SubstrateActorBinding(
            atespace="atespace-fixture",
            actor="actor-fixture",
            shim_token_secret_name=FAKE_SHIM_NAME,
        )
        keys = [
            (
                binding["session_id"],
                "atespace-fixture",
                "actor-fixture",
                FAKE_SHIM_NAME,
                "claude",
            )
            for binding in (first, second)
        ]
        with patch.object(
            settings, "substrate_actor_bindings", {"claude": actor_binding}
        ):
            try:
                ws_a = native_sessions.workspace_for(first)
                ws_b = native_sessions.workspace_for(second)
                self.assertIsNot(ws_a, ws_b)
                self.assertEqual(ws_a.logical_session_id, "claude-session-a")
                self.assertEqual(ws_b.logical_session_id, "claude-session-b")
                self.assertEqual(ws_a.native_session_id, "native-a")
                self.assertEqual(ws_b.native_session_id, "native-b")
                ws_a.set_native_session_id("native-a-updated")
                self.assertEqual(ws_b.native_session_id, "native-b")
            finally:
                for key in keys:
                    native_sessions._workspaces.pop(key, None)

    def test_branch_binding_uses_its_workspace_actor(self):
        binding = {
            "session_id": "workspace-session-fixture",
            "kind": "claude",
            "workspace_atespace": "workspace-space",
            "workspace_actor_name": "workspace-actor",
            "workspace_shim_token_secret_name": "workspace-shim",
        }
        key = (
            binding["session_id"],
            binding["workspace_atespace"],
            binding["workspace_actor_name"],
            binding["workspace_shim_token_secret_name"],
            binding["kind"],
        )
        with patch.object(settings, "substrate_actor_bindings", {}):
            try:
                workspace = native_sessions.workspace_for(binding)
                self.assertEqual(workspace.atespace, "workspace-space")
                self.assertEqual(workspace.actor, "workspace-actor")
                self.assertEqual(workspace.secret_name, "workspace-shim")
            finally:
                native_sessions._workspaces.pop(key, None)

    def test_incomplete_workspace_binding_does_not_fall_back_to_shared_actor(self):
        binding = {
            "session_id": "workspace-session-fixture",
            "kind": "claude",
            "workspace_atespace": "workspace-space",
            "workspace_actor_name": None,
            "workspace_shim_token_secret_name": "workspace-shim",
        }
        with patch.object(settings, "substrate_actor_bindings", {}):
            with self.assertRaisesRegex(RuntimeError, "incomplete actor route"):
                native_sessions.workspace_for(binding)

    def test_dynamic_shim_token_is_bootstrapped_before_authenticated_calls(self):
        async def exercise():
            router = FakeRouterAndShim(token_installed=False)
            workspace = fake_workspace(router)

            await workspace.send("agent-fixture", "fixture prompt")

            token_requests = [
                (index, body)
                for index, (method, path, body) in enumerate(router.requests)
                if method == "POST" and path == "/token"
            ]
            turn_requests = [
                index
                for index, (method, path, _body) in enumerate(router.requests)
                if method == "POST" and path == "/turn"
            ]
            self.assertEqual(token_requests, [(0, {"token": FIXTURE_VALUE})])
            self.assertTrue(turn_requests)
            self.assertLess(token_requests[0][0], turn_requests[0])
            self.assertEqual(router.expected_token, FIXTURE_VALUE)

        asyncio.run(exercise())

    def test_codex_start_and_resume_install_only_synthetic_auth(self):
        async def exercise():
            router = FakeRouterAndShim()
            workspace = FakeSubstrateWorkspace(
                router,
                atespace="atespace-fixture",
                actor="actor-fixture",
                agent="codex",
                shim_token_secret_name=FAKE_SHIM_NAME,
                router_address="http://router-fixture:8081",
                timeout=2,
                credential_broker=FakeCredentialBroker(),
            )
            await workspace.start(
                "codex", "agent-fixture", native_id=None, resume=False
            )
            installed = json.loads(router.credentials["codex-auth"])
            self.assertEqual(installed["tokens"]["account_id"], "fixture-account")
            self.assertEqual(installed["tokens"]["refresh_token"], "")
            self.assertEqual(
                installed["tokens"]["access_token"], "fixture.header.synthetic"
            )

            await workspace.start(
                "codex",
                "agent-fixture",
                native_id="native-fixture-id",
                resume=True,
            )
            writes = [
                request
                for request in router.requests
                if request[0] == "PUT" and request[1] == "/credential"
            ]
            self.assertEqual(len(writes), 2)

        asyncio.run(exercise())

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
            self.assertEqual(
                router.credentials["claude-token"],
                "synthetic-claude-egress-placeholder",
            )

            await workspace.send("agent-fixture", "fixture prompt")
            turn = next(
                body
                for method, path, body in router.requests
                if method == "POST" and path == "/turn"
            )
            self.assertNotIn("startup_options", turn)
            status = await workspace.agent_status("agent-fixture")
            self.assertEqual(status["status"], "running")
            self.assertEqual(
                await workspace.native_id("agent-fixture"), "native-session-fixture"
            )
            first = await workspace.journal(
                "agent-fixture", "native-session-fixture", 0
            )
            next_page = await workspace.journal(
                "agent-fixture", "native-session-fixture", 1
            )
            self.assertEqual(first.file, "/fake/fixture-session.jsonl")
            self.assertEqual(first.total_lines, 3)
            self.assertEqual(first.lines[0], (1, '{"type":"user"}'))
            self.assertEqual(next_page.lines[0], (2, '{"type":"assistant"}'))
            self.assertEqual(await workspace.listening_ports(), (3000, 5173))
            self.assertIn(
                ("CONNECT", "atespace-fixture/actor-fixture"), router.connects
            )
            sent = [
                body
                for method, path, body in router.requests
                if method == "POST" and path == "/turn"
            ]
            self.assertEqual(
                sent,
                [
                    {
                        "agent": "claude",
                        "prompt": "fixture prompt",
                        "session_key": "session-fixture",
                        "resume": False,
                    }
                ],
            )

        asyncio.run(exercise())

    def test_startup_options_are_forwarded_with_each_native_turn(self):
        async def exercise():
            router = FakeRouterAndShim()
            workspace = fake_workspace(router)
            extra = {
                "--cwd-rel": "main",
                "--token": "ml_" + "a" * 64,
                "--standing-b64": "c3RhbmRpbmcgY29udGV4dA==",
                "--approval-policy": "restricted: Bash(mainloop:*) only",
                "--model": "sonnet",
                "--effort": "medium",
            }

            await workspace.start(
                "claude", "ml-main", native_id=None, resume=False, extra=extra
            )
            await workspace.send("ml-main", "fixture prompt")

            turn = next(
                body
                for method, path, body in router.requests
                if method == "POST" and path == "/turn"
            )
            self.assertEqual(
                turn["startup_options"],
                {
                    "cwd_rel": "main",
                    "token": "ml_" + "a" * 64,
                    "standing_b64": "c3RhbmRpbmcgY29udGV4dA==",
                    "approval_policy": "restricted: Bash(mainloop:*) only",
                    "model": "sonnet",
                    "effort": "medium",
                },
            )

        asyncio.run(exercise())

    def test_established_native_session_is_marked_for_resume_on_next_turn(self):
        async def exercise():
            router = FakeRouterAndShim()
            workspace = fake_workspace(router)
            await workspace.start(
                "claude",
                "agent-fixture",
                native_id="native-established-session",
                resume=True,
            )
            await workspace.send("agent-fixture", "resumed fixture prompt")
            turn = next(
                body
                for method, path, body in router.requests
                if method == "POST" and path == "/turn"
            )
            self.assertEqual(turn["session_id"], "native-established-session")
            self.assertEqual(turn["session_key"], "session-fixture")
            self.assertTrue(turn["resume"])

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
                binding["session_id"],
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
                    substrate_workspace_module,
                    "CredentialBroker",
                    FakeCredentialBroker,
                ),
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
                                "session_key": "session-fixture",
                                "resume": False,
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
