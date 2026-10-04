"""In-process fake-router coverage; tests never open sockets or access Kubernetes."""

from __future__ import annotations

import asyncio
import json
import unittest
from urllib.parse import urlsplit

from mainloop.runtime.substrate_workspace import (
    SubstrateWorkspace,
    WorkspaceUnavailable,
    _Response,
)

FIXTURE_VALUE = "fixture-shim-value-one"
ROTATED_FIXTURE_VALUE = "fixture-shim-value-two"


class FakeRouterAndShim:
    """In-process CONNECT/router and authenticated shim model for the port-discovery path."""

    def __init__(self, *, token_installed=True):
        self.capacity = False
        self.expected_token = FIXTURE_VALUE if token_installed else None
        self.token_installed = token_installed
        self.requests: list[tuple[str, str, dict]] = []

    def request(
        self,
        *,
        method: str,
        path: str,
        token: str | None,
        body: dict | None,
        **_unused,
    ) -> _Response:
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
        if method == "GET" and urlsplit(path).path == "/ports":
            return _Response(200, json.dumps({"ports": [3000, 5173, 3000]}))
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
        return self.router.request(method=method, path=path, token=token, body=body)


def fake_workspace(router: FakeRouterAndShim):
    return FakeSubstrateWorkspace(
        router,
        atespace="atespace-fixture",
        actor="actor-fixture",
        agent="claude",
        shim_token_secret_name="shim-fixture",  # nosec B106 - Secret object name, not a secret value
        router_address="http://router-fixture:8081",
        timeout=2,
    )


class SubstrateWorkspaceTests(unittest.TestCase):
    def test_listening_ports_are_sorted_and_deduplicated(self):
        async def exercise():
            ports = await fake_workspace(FakeRouterAndShim()).listening_ports()
            self.assertEqual(ports, (3000, 5173))

        asyncio.run(exercise())

    def test_dynamic_shim_token_is_bootstrapped_before_authenticated_calls(self):
        async def exercise():
            router = FakeRouterAndShim(token_installed=False)
            await fake_workspace(router).listening_ports()

            order = [(method, path) for method, path, _ in router.requests]
            self.assertEqual(order, [("POST", "/token"), ("GET", "/ports")])
            self.assertEqual(router.requests[0][2], {"token": FIXTURE_VALUE})
            self.assertEqual(router.expected_token, FIXTURE_VALUE)

        asyncio.run(exercise())

    def test_capacity_503_maps_to_workspace_unavailable(self):
        async def exercise():
            router = FakeRouterAndShim()
            router.capacity = True
            with self.assertRaises(WorkspaceUnavailable):
                await fake_workspace(router).listening_ports()

        asyncio.run(exercise())

    def test_401_refreshes_cached_secret_for_safe_request(self):
        async def exercise():
            router = FakeRouterAndShim()
            workspace = fake_workspace(router)
            await workspace.listening_ports()
            router.expected_token = ROTATED_FIXTURE_VALUE
            workspace.fixture_value = ROTATED_FIXTURE_VALUE

            self.assertEqual(await workspace.listening_ports(), (3000, 5173))
            self.assertEqual(workspace.fixture_reads, 2)

        asyncio.run(exercise())

    def test_a_persistent_401_is_a_shim_error(self):
        async def exercise():
            router = FakeRouterAndShim()
            router.expected_token = ROTATED_FIXTURE_VALUE
            with self.assertRaises(RuntimeError):
                await fake_workspace(router).listening_ports()

        asyncio.run(exercise())

    def test_invalid_router_and_names_are_rejected(self):
        for kwargs in (
            {"router_address": "https://router-fixture"},
            {"atespace": "Not_A_Label"},
            {"agent": "other"},
        ):
            with self.subTest(kwargs=kwargs), self.assertRaises(ValueError):
                FakeSubstrateWorkspace(
                    FakeRouterAndShim(),
                    **{
                        "atespace": "atespace-fixture",
                        "actor": "actor-fixture",
                        "agent": "claude",
                        "shim_token_secret_name": "shim-fixture",
                        "router_address": "http://router-fixture:8081",
                        **kwargs,
                    },
                )


if __name__ == "__main__":
    unittest.main()
