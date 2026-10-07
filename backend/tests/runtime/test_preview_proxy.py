"""Fake-backed host, port policy, and proxy framing tests."""

from __future__ import annotations

import asyncio
import http.client
import io
import socket
import unittest
from unittest.mock import AsyncMock, patch

from mainloop.config import settings
from mainloop.runtime import preview_proxy
from starlette.requests import Request
from starlette.websockets import WebSocket
from tests.runtime.test_task_provisioning import ordinary_guard

_PREVIEW_HOST = "5173--workspace-1--preview.localhost:8001"
_OWNER = "the-owner"
_REAL_WAKE_WORKSPACE = preview_proxy._wake_workspace


def make_request(method: str, headers: dict[str, str], body: bytes = b"") -> Request:
    raw_headers = [
        (key.lower().encode(), value.encode()) for key, value in headers.items()
    ]

    async def receive():
        return {"type": "http.request", "body": body, "more_body": False}

    return Request(
        {
            "type": "http",
            "http_version": "1.1",
            "method": method,
            "scheme": "http",
            "path": "/",
            "raw_path": b"/",
            "query_string": b"",
            "root_path": "",
            "headers": raw_headers,
            "server": ("preview.localhost", 8001),
            "client": ("127.0.0.1", 12345),
        },
        receive,
    )


def make_websocket(headers: dict[str, str]) -> tuple[WebSocket, list[dict]]:
    raw_headers = [
        (key.lower().encode(), value.encode()) for key, value in headers.items()
    ]
    sent: list[dict] = []

    async def receive():
        return {"type": "websocket.disconnect", "code": 1000}

    async def send(message):
        sent.append(message)

    return (
        WebSocket(
            {
                "type": "websocket",
                "asgi": {"version": "3.0"},
                "http_version": "1.1",
                "scheme": "ws",
                "path": "/",
                "raw_path": b"/",
                "query_string": b"",
                "root_path": "",
                "headers": raw_headers,
                "server": ("preview.localhost", 8001),
                "client": ("127.0.0.1", 12345),
                "subprotocols": [],
            },
            receive,
            send,
        ),
        sent,
    )


async def _run_sync_in_test(function, *args, **kwargs):
    """Keep fake router operations deterministic without a test executor thread."""
    await asyncio.sleep(0)
    return function(*args, **kwargs)


class PreviewProxyTests(unittest.TestCase):
    def setUp(self):
        # A running workspace: nothing to resume. The wake tests below replace this.
        patcher = patch.object(
            preview_proxy, "_wake_workspace", new=AsyncMock(return_value=True)
        )
        patcher.start()
        self.addCleanup(patcher.stop)
        guard = patch("mainloop.tasks.lifecycle.guard", ordinary_guard)
        guard.start()
        self.addCleanup(guard.stop)

    def test_preview_host_is_one_label_under_the_base_domain(self):
        base = "http://localhost:8001"
        for host, expected in (
            ("5173--workspace-1--preview.localhost:8001", (5173, "workspace-1")),
            ("5173--Workspace-1--PREVIEW.LOCALHOST:8001", (5173, "workspace-1")),
            ("5173--workspace-1--preview.localhost.:8001", (5173, "workspace-1")),
            ("3000--a--b--preview.localhost:8001", (3000, "a--b")),
            (
                "65535--11111111-2222-3333-4444-555555555555--preview.localhost:8001",
                (65535, "11111111-2222-3333-4444-555555555555"),
            ),
        ):
            with self.subTest(host=host):
                self.assertEqual(
                    preview_proxy.parse_preview_host(host, base),
                    preview_proxy.PreviewHost(*expected),
                )
        wildcard = "https://example.test"
        self.assertEqual(
            preview_proxy.parse_preview_host(
                "3000--workspace-1--preview.example.test", wildcard
            ),
            preview_proxy.PreviewHost(3000, "workspace-1"),
        )

    def test_malformed_preview_hosts_are_not_previews(self):
        base = "http://localhost:8001"
        for invalid in (
            "",
            "localhost:8001",
            "5173--workspace-1--preview.example.test:8001",
            "5173--workspace-1--preview.localhost:3000",
            "5173--workspace-1--preview.localhost",
            "65536--workspace-1--preview.localhost:8001",
            "0--workspace-1--preview.localhost:8001",
            "05173--workspace-1--preview.localhost:8001",
            "5173--bad---preview.localhost:8001",
            "5173----preview.localhost:8001",
            "5173--workspace-1.preview.localhost:8001",
            "5173--workspace-1--other.localhost:8001",
            "workspace-1--preview.localhost:8001",
            "5173--workspace-1--preview.localhost/path:8001",
            "5173--workspace_1--preview.localhost:8001",
            f"5173--{'a' * 60}--preview.localhost:8001",
        ):
            with self.subTest(invalid=invalid):
                self.assertIsNone(preview_proxy.parse_preview_host(invalid, base))

    def test_a_nested_subdomain_is_rejected(self):
        for base, valid in (
            ("http://localhost:8001", "5173--workspace-1--preview.localhost:8001"),
            ("https://example.test", "5173--workspace-1--preview.example.test"),
        ):
            self.assertIsNotNone(preview_proxy.parse_preview_host(valid, base))
            for nested in (
                f"evil.{valid}",
                f"5173--workspace-1--preview.evil.{valid.split('--preview.')[1]}",
                f"x.y.{valid}",
            ):
                with self.subTest(nested=nested):
                    self.assertIsNone(preview_proxy.parse_preview_host(nested, base))

    def test_preview_url_uses_configured_wildcard_origin(self):
        for base, expected in (
            (
                "http://localhost:8001",
                "http://5173--workspace-1--preview.localhost:8001",
            ),
            ("https://example.test", "https://5173--workspace-1--preview.example.test"),
            (
                "https://example.test:8443",
                "https://5173--workspace-1--preview.example.test:8443",
            ),
        ):
            with self.subTest(base=base):
                with patch.object(settings, "substrate_preview_base_url", base):
                    url = preview_proxy.preview_url("workspace-1", 5173)
                    self.assertEqual(url, expected)
                    host = url.split("://", 1)[1]
                    self.assertEqual(
                        preview_proxy.parse_preview_host(host, base),
                        preview_proxy.PreviewHost(5173, "workspace-1"),
                    )

    def test_preview_traffic_restarts_the_idle_debounce(self):
        async def exercise():
            with patch.object(
                preview_proxy.workspaces, "touch", new=AsyncMock()
            ) as touch:
                await preview_proxy._touch_preview("workspace-1")
            touch.assert_awaited_once_with("workspace-1")

        asyncio.run(exercise())

    def test_http_preview_checks_the_configured_owner_and_ignores_identity_headers(
        self,
    ):
        async def exercise():
            with patch.object(
                preview_proxy, "_resolve_target", new=AsyncMock(return_value=None)
            ) as resolve:
                response = await preview_proxy._preview_http(
                    make_request(
                        "GET",
                        {
                            "host": _PREVIEW_HOST,
                            "x-user-id": "someone-else",
                            "cf-access-authenticated-user-email": "someone-else",
                        },
                    ),
                    preview_proxy.PreviewHost(5173, "workspace-1"),
                )

            self.assertEqual(response.status_code, 404)
            resolve.assert_awaited_once_with("workspace-1", _OWNER)

        with patch.object(settings, "owner_id", _OWNER):
            asyncio.run(exercise())

    def test_websocket_preview_checks_the_configured_owner(self):
        async def exercise():
            with (
                patch.object(
                    preview_proxy, "_resolve_target", new=AsyncMock(return_value=None)
                ) as resolve,
                patch.object(preview_proxy, "_touch_preview", new=AsyncMock()) as touch,
            ):
                websocket, sent = make_websocket(
                    {"host": _PREVIEW_HOST, "x-user-id": "someone-else"}
                )
                await preview_proxy._preview_websocket(
                    websocket, preview_proxy.PreviewHost(5173, "workspace-1")
                )
                self.assertEqual(sent[0]["type"], "websocket.close")
                self.assertEqual(sent[0]["code"], 4404)
                resolve.assert_awaited_once_with("workspace-1", _OWNER)
                touch.assert_not_awaited()

        with patch.object(settings, "owner_id", _OWNER):
            asyncio.run(exercise())

    def test_an_undeclared_port_is_refused_without_touching_the_idle_clock(self):
        target = preview_proxy.PreviewTarget(
            workspace_id="workspace-1",
            user_id=_OWNER,
            atespace="kagent",
            actor="session-sess-1",
            ports={5173: "web"},
        )
        headers = {"host": _PREVIEW_HOST}

        async def exercise():
            with (
                patch.object(
                    preview_proxy, "_resolve_target", new=AsyncMock(return_value=target)
                ),
                patch.object(preview_proxy, "_touch_preview", new=AsyncMock()) as touch,
                patch.object(preview_proxy, "_connect_router") as connect,
            ):
                response = await preview_proxy._preview_http(
                    make_request("GET", headers),
                    preview_proxy.PreviewHost(9999, "workspace-1"),
                )
                self.assertEqual(response.status_code, 403)
                websocket, sent = make_websocket(headers)
                await preview_proxy._preview_websocket(
                    websocket, preview_proxy.PreviewHost(9999, "workspace-1")
                )
                self.assertEqual(sent[0]["type"], "websocket.close")
                self.assertEqual(sent[0]["code"], 4403)
                connect.assert_not_called()
                touch.assert_not_awaited()

        with patch.object(settings, "owner_id", _OWNER):
            asyncio.run(exercise())

    def test_a_declared_port_touches_the_idle_clock_once(self):
        target = preview_proxy.PreviewTarget(
            workspace_id="workspace-1",
            user_id=_OWNER,
            atespace="kagent",
            actor="session-sess-1",
            ports={5173: "web"},
        )

        async def exercise():
            with (
                patch.object(
                    preview_proxy, "_resolve_target", new=AsyncMock(return_value=target)
                ),
                patch.object(preview_proxy, "_touch_preview", new=AsyncMock()) as touch,
                patch.object(
                    preview_proxy,
                    "_connect_router",
                    side_effect=preview_proxy._PreviewForwardedFailure("fixture stop"),
                ),
                patch.object(preview_proxy.asyncio, "to_thread", new=_run_sync_in_test),
            ):
                await preview_proxy._preview_http(
                    make_request("GET", {"host": _PREVIEW_HOST}),
                    preview_proxy.PreviewHost(5173, "workspace-1"),
                )
                touch.assert_awaited_once_with("workspace-1")

        with patch.object(settings, "owner_id", _OWNER):
            asyncio.run(exercise())

    def test_a_declared_port_wakes_the_workspace_before_the_router_connect(self):
        target = preview_proxy.PreviewTarget(
            workspace_id="workspace-1",
            user_id=_OWNER,
            atespace="kagent",
            actor="session-sess-1",
            ports={5173: "web"},
        )
        order: list[str] = []

        async def wake(_target):
            order.append("wake")
            return True

        def connect(*args, **kwargs):
            order.append("connect")
            raise preview_proxy._PreviewForwardedFailure("fixture stop")

        async def exercise():
            with (
                patch.object(
                    preview_proxy, "_resolve_target", new=AsyncMock(return_value=target)
                ),
                patch.object(preview_proxy, "_touch_preview", new=AsyncMock()),
                patch.object(preview_proxy, "_wake_workspace", new=wake),
                patch.object(preview_proxy, "_connect_router", side_effect=connect),
                patch.object(preview_proxy.asyncio, "to_thread", new=_run_sync_in_test),
            ):
                await preview_proxy._preview_http(
                    make_request("GET", {"host": _PREVIEW_HOST}),
                    preview_proxy.PreviewHost(5173, "workspace-1"),
                )
            self.assertEqual(order, ["wake", "connect"])

        with patch.object(settings, "owner_id", _OWNER):
            asyncio.run(exercise())

    def test_a_workspace_that_cannot_be_resumed_is_not_connected_to(self):
        target = preview_proxy.PreviewTarget(
            workspace_id="workspace-1",
            user_id=_OWNER,
            atespace="kagent",
            actor="session-sess-1",
            ports={5173: "web"},
        )

        async def exercise():
            with (
                patch.object(
                    preview_proxy, "_resolve_target", new=AsyncMock(return_value=target)
                ),
                patch.object(preview_proxy, "_touch_preview", new=AsyncMock()),
                patch.object(
                    preview_proxy, "_wake_workspace", new=AsyncMock(return_value=False)
                ),
                patch.object(preview_proxy, "_connect_router") as connect,
            ):
                response = await preview_proxy._preview_http(
                    make_request("GET", {"host": _PREVIEW_HOST}),
                    preview_proxy.PreviewHost(5173, "workspace-1"),
                )
                self.assertEqual(response.status_code, 502)
                websocket, sent = make_websocket(
                    {"host": _PREVIEW_HOST, "sec-websocket-key": "a2V5"}
                )
                with patch.object(
                    preview_proxy.asyncio, "open_connection", new=AsyncMock()
                ) as open_connection:
                    await preview_proxy._preview_websocket(
                        websocket, preview_proxy.PreviewHost(5173, "workspace-1")
                    )
                self.assertEqual(sent[0]["type"], "websocket.close")
                self.assertEqual(sent[0]["code"], 1013)
                connect.assert_not_called()
                open_connection.assert_not_awaited()

        with patch.object(settings, "owner_id", _OWNER):
            asyncio.run(exercise())

    def test_a_websocket_wakes_the_workspace_before_the_router_connect(self):
        target = preview_proxy.PreviewTarget(
            workspace_id="workspace-1",
            user_id=_OWNER,
            atespace="kagent",
            actor="session-sess-1",
            ports={5173: "web"},
        )
        order: list[str] = []

        async def wake(_target):
            order.append("wake")
            return True

        async def open_connection(*args, **kwargs):
            order.append("connect")
            raise ConnectionRefusedError("fixture stop")

        async def exercise():
            with (
                patch.object(
                    preview_proxy, "_resolve_target", new=AsyncMock(return_value=target)
                ),
                patch.object(preview_proxy, "_touch_preview", new=AsyncMock()),
                patch.object(preview_proxy, "_wake_workspace", new=wake),
                patch.object(
                    preview_proxy.asyncio, "open_connection", new=open_connection
                ),
            ):
                websocket, _ = make_websocket(
                    {"host": _PREVIEW_HOST, "sec-websocket-key": "a2V5"}
                )
                await preview_proxy._preview_websocket(
                    websocket, preview_proxy.PreviewHost(5173, "workspace-1")
                )
            self.assertEqual(order[0], "wake")
            self.assertIn("connect", order)

        with patch.object(settings, "owner_id", _OWNER):
            asyncio.run(exercise())

    def test_refused_previews_do_not_wake_the_workspace(self):
        target = preview_proxy.PreviewTarget(
            workspace_id="workspace-1",
            user_id=_OWNER,
            atespace="kagent",
            actor="session-sess-1",
            ports={5173: "web"},
        )

        async def exercise():
            wake = AsyncMock(return_value=True)
            for resolved, port in ((None, 5173), (target, 9999)):
                with (
                    patch.object(
                        preview_proxy,
                        "_resolve_target",
                        new=AsyncMock(return_value=resolved),
                    ),
                    patch.object(preview_proxy, "_touch_preview", new=AsyncMock()),
                    patch.object(preview_proxy, "_wake_workspace", new=wake),
                ):
                    await preview_proxy._preview_http(
                        make_request("GET", {"host": _PREVIEW_HOST}),
                        preview_proxy.PreviewHost(port, "workspace-1"),
                    )
                    websocket, _ = make_websocket({"host": _PREVIEW_HOST})
                    await preview_proxy._preview_websocket(
                        websocket, preview_proxy.PreviewHost(port, "workspace-1")
                    )
            wake.assert_not_awaited()

        with patch.object(settings, "owner_id", _OWNER):
            asyncio.run(exercise())

    def test_a_foreign_origin_is_refused_before_any_preview_action(self):
        target = preview_proxy.PreviewTarget(
            workspace_id="workspace-1",
            user_id=_OWNER,
            atespace="kagent",
            actor="session-sess-1",
            ports={5173: "web"},
        )
        foreign = (
            "http://evil.example",
            "http://localhost:8001",  # the API's origin, not the preview's
            "http://3000--workspace-2--preview.localhost:8001",  # a sibling preview
            "http://preview.localhost:8001",  # the preview domain itself
            "http://x.5173--workspace-1--preview.localhost:8001",
            "https://5173--workspace-1--preview.localhost:8001",  # another scheme
            "http://5173--workspace-1--preview.localhost:9",  # another port
            "null",
            "",
        )

        async def exercise():
            resolve = AsyncMock(return_value=target)
            touch = AsyncMock()
            wake = AsyncMock(return_value=True)
            connect = patch.object(preview_proxy, "_connect_router")
            open_connection = AsyncMock()
            with (
                patch.object(preview_proxy, "_resolve_target", new=resolve),
                patch.object(preview_proxy, "_touch_preview", new=touch),
                patch.object(preview_proxy, "_wake_workspace", new=wake),
                connect as connect_router,
                patch.object(
                    preview_proxy.asyncio, "open_connection", new=open_connection
                ),
            ):
                for origin in foreign:
                    for method in ("POST", "PUT", "PATCH", "DELETE"):
                        with self.subTest(origin=origin, method=method):
                            response = await preview_proxy._preview_http(
                                make_request(
                                    method,
                                    {"host": _PREVIEW_HOST, "origin": origin},
                                ),
                                preview_proxy.PreviewHost(5173, "workspace-1"),
                            )
                            self.assertEqual(response.status_code, 403)
                    with self.subTest(origin=origin, method="websocket"):
                        websocket, sent = make_websocket(
                            {
                                "host": _PREVIEW_HOST,
                                "origin": origin,
                                "sec-websocket-key": "a2V5",
                            }
                        )
                        await preview_proxy._preview_websocket(
                            websocket, preview_proxy.PreviewHost(5173, "workspace-1")
                        )
                        self.assertEqual([m["type"] for m in sent], ["websocket.close"])
                        self.assertEqual(sent[0]["code"], 4403)
            resolve.assert_not_awaited()
            touch.assert_not_awaited()
            wake.assert_not_awaited()
            connect_router.assert_not_called()
            open_connection.assert_not_awaited()

        with patch.object(settings, "owner_id", _OWNER):
            asyncio.run(exercise())

    def test_the_previews_own_origin_and_no_origin_are_forwarded(self):
        target = preview_proxy.PreviewTarget(
            workspace_id="workspace-1",
            user_id=_OWNER,
            atespace="kagent",
            actor="session-sess-1",
            ports={5173: "web"},
        )
        own = f"http://{_PREVIEW_HOST}"

        async def exercise():
            for origin in (own, own.upper(), own + "/", None):
                headers = {"host": _PREVIEW_HOST}
                if origin is not None:
                    headers["origin"] = origin
                with self.subTest(origin=origin, kind="http"):
                    resolve = AsyncMock(return_value=target)
                    connect = patch.object(
                        preview_proxy,
                        "_connect_router",
                        side_effect=ConnectionRefusedError("fixture stop"),
                    )
                    with (
                        patch.object(preview_proxy, "_resolve_target", new=resolve),
                        patch.object(preview_proxy, "_touch_preview", new=AsyncMock()),
                        connect as connect_router,
                        patch.object(
                            preview_proxy.asyncio, "to_thread", new=_run_sync_in_test
                        ),
                    ):
                        response = await preview_proxy._preview_http(
                            make_request("POST", headers, b"{}"),
                            preview_proxy.PreviewHost(5173, "workspace-1"),
                        )
                    self.assertEqual(
                        response.status_code, 502
                    )  # got past the Origin gate
                    self.assertEqual(resolve.await_count, 2)
                    connect_router.assert_called_once()
                with self.subTest(origin=origin, kind="websocket"):
                    resolve = AsyncMock(return_value=target)
                    open_connection = AsyncMock(
                        side_effect=ConnectionRefusedError("fixture stop")
                    )
                    with (
                        patch.object(preview_proxy, "_resolve_target", new=resolve),
                        patch.object(preview_proxy, "_touch_preview", new=AsyncMock()),
                        patch.object(
                            preview_proxy.asyncio,
                            "open_connection",
                            new=open_connection,
                        ),
                    ):
                        websocket, sent = make_websocket(
                            {**headers, "sec-websocket-key": "a2V5"}
                        )
                        await preview_proxy._preview_websocket(
                            websocket, preview_proxy.PreviewHost(5173, "workspace-1")
                        )
                    self.assertGreaterEqual(resolve.await_count, 2)
                    open_connection.assert_awaited()
                    self.assertNotEqual(sent[0].get("code"), 4403)

        with patch.object(settings, "owner_id", _OWNER):
            asyncio.run(exercise())

    def test_safe_methods_ignore_the_origin(self):
        async def exercise():
            for method in ("GET", "HEAD", "OPTIONS"):
                with self.subTest(method=method):
                    resolve = AsyncMock(return_value=None)
                    with patch.object(preview_proxy, "_resolve_target", new=resolve):
                        response = await preview_proxy._preview_http(
                            make_request(
                                method,
                                {
                                    "host": _PREVIEW_HOST,
                                    "origin": "http://evil.example",
                                },
                            ),
                            preview_proxy.PreviewHost(5173, "workspace-1"),
                        )
                    self.assertEqual(response.status_code, 404)  # resolved, not refused
                    resolve.assert_awaited_once()

        with patch.object(settings, "owner_id", _OWNER):
            asyncio.run(exercise())

    def test_wake_workspace_resumes_for_the_target_and_reports_failure(self):
        target = preview_proxy.PreviewTarget(
            workspace_id="workspace-1",
            user_id=_OWNER,
            atespace="kagent",
            actor="session-sess-1",
            ports={5173: "web"},
        )

        async def exercise():
            real = _REAL_WAKE_WORKSPACE  # the class-level stub replaces the module name
            with patch.object(
                preview_proxy.workspaces,
                "wake_for_preview",
                new=AsyncMock(return_value=True),
            ) as wake:
                self.assertTrue(await real(target))
                wake.assert_awaited_once_with("workspace-1", _OWNER)
            for error in (
                preview_proxy.workspaces.WorkspaceNotFound("gone"),
                preview_proxy.workspaces.WorkspaceConflict("refused"),
                preview_proxy.workspaces.WorkspaceUnconfirmed("unknown"),
            ):
                with patch.object(
                    preview_proxy.workspaces,
                    "wake_for_preview",
                    new=AsyncMock(side_effect=error),
                ):
                    self.assertFalse(await real(target))

        asyncio.run(exercise())

    def test_post_that_commits_then_disconnects_is_not_replayed(self):
        target = preview_proxy.PreviewTarget(
            workspace_id="workspace-1",
            user_id="local-dev-user",
            atespace="fixture-space",
            actor="fixture-actor",
            ports={5173: "web"},
        )
        committed: list[bytes] = []

        class DisconnectingReader:
            def __init__(self):
                self._lines = iter(
                    (b"HTTP/1.1 200 Connection Established\r\n", b"\r\n")
                )
                self.closed = False

            def readline(self, _size=-1):
                try:
                    return next(self._lines)
                except StopIteration as exc:
                    raise socket.timeout("fake upstream disconnected") from exc

            def close(self):
                self.closed = True

        class CommitThenDisconnectSocket:
            def __init__(self):
                self.reader = DisconnectingReader()
                self.sent: list[bytes] = []
                self.closed = False

            def settimeout(self, _timeout):
                pass

            def makefile(self, _mode):
                return self.reader

            def sendall(self, payload):
                self.sent.append(payload)
                if payload.startswith(b"POST "):
                    _headers, body = payload.split(b"\r\n\r\n", 1)
                    committed.append(body)

            def close(self):
                self.closed = True

        fake_upstream = CommitThenDisconnectSocket()

        async def exercise():
            with (
                patch.object(preview_proxy.asyncio, "to_thread", new=_run_sync_in_test),
                patch.object(
                    preview_proxy, "_resolve_target", new=AsyncMock(return_value=target)
                ),
                patch.object(preview_proxy, "_touch_preview", new=AsyncMock()),
                patch.object(
                    preview_proxy.socket,
                    "create_connection",
                    return_value=fake_upstream,
                ),
                patch.object(
                    settings, "substrate_router_address", "http://router.fixture:8081"
                ),
            ):
                response = await preview_proxy._preview_http(
                    make_request(
                        "POST", {"host": _PREVIEW_HOST}, b"synthetic mutation"
                    ),
                    preview_proxy.PreviewHost(5173, "workspace-1"),
                )
            return response

        response = asyncio.run(exercise())
        self.assertEqual(response.status_code, 502)
        self.assertIn(b"outcome is unknown", response.body)
        self.assertEqual(committed, [b"synthetic mutation"])
        self.assertEqual(len(fake_upstream.sent), 2)
        self.assertTrue(fake_upstream.reader.closed)
        self.assertTrue(fake_upstream.closed)

    def test_router_connect_failure_before_forwarding_is_retried_once(self):
        target = preview_proxy.PreviewTarget(
            workspace_id="workspace-1",
            user_id="local-dev-user",
            atespace="fixture-space",
            actor="fixture-actor",
            ports={5173: "web"},
        )
        headers = http.client.HTTPMessage()
        headers["Content-Length"] = "2"
        upstream = preview_proxy._UpstreamHTTP(
            sock=type("FakeSocket", (), {"close": lambda _self: None})(),
            reader=io.BytesIO(b"ok"),
            status=200,
            headers=headers,
        )
        failures = 0

        def connect(*_args, **_kwargs):
            nonlocal failures
            failures += 1
            if failures == 1:
                raise preview_proxy._PreviewPreForwardFailure("CONNECT unavailable")
            return upstream

        async def exercise():
            with (
                patch.object(preview_proxy.asyncio, "to_thread", new=_run_sync_in_test),
                patch.object(
                    preview_proxy, "_resolve_target", new=AsyncMock(return_value=target)
                ),
                patch.object(preview_proxy, "_touch_preview", new=AsyncMock()),
                patch.object(preview_proxy, "_connect_router", side_effect=connect),
            ):
                response = await preview_proxy._preview_http(
                    make_request("GET", {"host": _PREVIEW_HOST}),
                    preview_proxy.PreviewHost(5173, "workspace-1"),
                )
                body = b"".join([chunk async for chunk in response.body_iterator])
            return response, body

        response, body = asyncio.run(exercise())

        self.assertEqual(response.status_code, 200)
        self.assertEqual(body, b"ok")
        self.assertEqual(failures, 2)

    def test_activity_lease_refreshes_before_idle_deadline_and_stops_on_close(self):
        async def exercise():
            touches: list[float] = []

            async def touch(_workspace_id: str):
                touches.append(asyncio.get_running_loop().time())

            with (
                patch.object(preview_proxy, "_PREVIEW_TOUCH_INTERVAL_SECONDS", 0.015),
                patch.object(
                    preview_proxy, "_touch_preview", new=AsyncMock(side_effect=touch)
                ),
            ):
                for active_connection in ("websocket", "http-stream"):
                    async with preview_proxy._preview_activity_lease(
                        f"workspace-{active_connection}"
                    ):
                        await asyncio.sleep(0.07)
                        self.assertTrue(touches)
                        now = asyncio.get_running_loop().time()
                        self.assertLess(now - touches[-1], 0.05)
                    count_at_close = len(touches)
                    await asyncio.sleep(0.04)
                    self.assertEqual(len(touches), count_at_close)

                touches.clear()
                async with preview_proxy._preview_activity_lease("shared-workspace"):
                    async with preview_proxy._preview_activity_lease(
                        "shared-workspace"
                    ):
                        await asyncio.sleep(0.07)
                self.assertGreaterEqual(len(touches), 3)
                self.assertLessEqual(len(touches), 5)

        asyncio.run(exercise())

    def test_chunked_response_body_is_decoded_for_streaming_response(self):
        import http.client

        headers = http.client.HTTPMessage()
        headers["Transfer-Encoding"] = "chunked"
        reader = io.BytesIO(b"4\r\nWiki\r\n5\r\npedia\r\n0\r\n\r\n")
        self.assertEqual(
            list(preview_proxy._response_body(reader, headers, no_body=False)),
            [b"Wiki", b"pedia"],
        )

    def test_large_chunk_is_read_in_bounded_blocks(self):
        class TrackingReader(io.BytesIO):
            max_requested = 0

            def read(self, size=-1):
                self.max_requested = max(self.max_requested, size)
                return super().read(size)

        payload = b"x" * (preview_proxy._RESPONSE_READ_BLOCK_BYTES * 3 + 17)
        reader = TrackingReader(
            f"{len(payload):X}\r\n".encode() + payload + b"\r\n0\r\n\r\n"
        )
        headers = http.client.HTTPMessage()
        headers["Transfer-Encoding"] = "chunked"

        chunks = list(preview_proxy._response_body(reader, headers, no_body=False))

        self.assertEqual(b"".join(chunks), payload)
        self.assertGreater(len(chunks), 1)
        self.assertLessEqual(
            reader.max_requested, preview_proxy._RESPONSE_READ_BLOCK_BYTES
        )

    def test_stalled_response_read_closes_upstream(self):
        class StalledReader:
            closed = False

            def read(self, _size=-1):
                raise socket.timeout("fixture stalled")

            def close(self):
                self.closed = True

        class FakeSocket:
            closed = False

            def close(self):
                self.closed = True

        async def exercise():
            reader = StalledReader()
            sock = FakeSocket()
            headers = http.client.HTTPMessage()
            headers["Content-Length"] = "1"
            upstream = preview_proxy._UpstreamHTTP(sock, reader, 200, headers)
            body = preview_proxy._stream_response_body(upstream, "workspace-1", False)
            with patch.object(
                preview_proxy.asyncio, "to_thread", new=_run_sync_in_test
            ):
                with self.assertRaises(socket.timeout):
                    await anext(body)
            self.assertTrue(reader.closed)
            self.assertTrue(sock.closed)

        asyncio.run(exercise())

    def test_router_socket_read_timeout_remains_set_for_stream_body(self):
        class FakeSocket:
            def __init__(self):
                self.reader = io.BytesIO(
                    b"HTTP/1.1 200 Connection Established\r\n\r\n"
                    b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"
                )
                self.timeout = None
                self.sent: list[bytes] = []
                self.closed = False

            def settimeout(self, value):
                self.timeout = value

            def makefile(self, _mode):
                return self.reader

            def sendall(self, payload):
                self.sent.append(payload)

            def close(self):
                self.closed = True

        target = preview_proxy.PreviewTarget(
            workspace_id="workspace-1",
            user_id="owner",
            atespace="fixture-space",
            actor="fixture-actor",
            ports={},
        )
        upstream_socket = FakeSocket()
        with (
            patch.object(
                settings, "substrate_router_address", "http://router.fixture:8081"
            ),
            patch.object(
                preview_proxy.socket, "create_connection", return_value=upstream_socket
            ),
        ):
            upstream = preview_proxy._connect_router(
                target,
                5173,
                2.5,
                method="GET",
                path="/",
                headers=[],
                body=b"",
            )

        self.assertEqual(upstream_socket.timeout, 2.5)
        self.assertEqual(len(upstream_socket.sent), 2)
        upstream.close()
        self.assertTrue(upstream_socket.closed)

    def test_gateway_and_credential_headers_are_not_forwarded_to_the_dev_server(self):
        class FakeSocket:
            def __init__(self):
                self.reader = io.BytesIO(
                    b"HTTP/1.1 200 Connection Established\r\n\r\n"
                    b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"
                )
                self.sent: list[bytes] = []

            def settimeout(self, _value):
                pass

            def makefile(self, _mode):
                return self.reader

            def sendall(self, payload):
                self.sent.append(payload)

            def close(self):
                pass

        target = preview_proxy.PreviewTarget(
            workspace_id="workspace-1",
            user_id="owner",
            atespace="fixture-space",
            actor="fixture-actor",
            ports={},
        )
        sock = FakeSocket()
        sent_headers = [
            ("Accept", "text/html"),
            ("X-CSRF-Token", "abc"),  # an app's own header still reaches the app
            ("X-Requested-With", "fetch"),
            ("Authorization", "Bearer secret"),
            ("Cookie", "mainloop=secret"),
            ("X-User-ID", "someone"),
            ("Tailscale-User-Login", "owner@example.com"),
            ("Tailscale-User-Name", "Owner"),
            ("X-Forwarded-For", "100.64.0.1"),
            ("X-Forwarded-Host", "5173--w--preview.example"),
            ("X-Forwarded-Proto", "https"),
            ("Forwarded", "for=100.64.0.1"),
            ("X-Real-IP", "100.64.0.1"),
            ("Proxy-Authorization", "x"),
        ]
        with (
            patch.object(
                settings, "substrate_router_address", "http://router.fixture:8081"
            ),
            patch.object(preview_proxy.socket, "create_connection", return_value=sock),
        ):
            preview_proxy._connect_router(
                target,
                5173,
                2.0,
                method="GET",
                path="/",
                headers=sent_headers,
                body=b"",
            ).close()
        names = {
            line.split(":", 1)[0].lower()
            for line in sock.sent[1].decode("latin1").split("\r\n")[1:]
            if ":" in line
        }
        self.assertEqual(
            names,
            {
                "accept",
                "x-csrf-token",
                "x-requested-with",
                "host",
                "connection",
                "content-length",
            },
        )

    def test_exactly_one_host_header_reaches_the_router(self):
        class FakeSocket:
            def __init__(self):
                self.reader = io.BytesIO(
                    b"HTTP/1.1 200 Connection Established\r\n\r\n"
                    b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"
                )
                self.sent: list[bytes] = []

            def settimeout(self, _value):
                pass

            def makefile(self, _mode):
                return self.reader

            def sendall(self, payload):
                self.sent.append(payload)

            def close(self):
                pass

        target = preview_proxy.PreviewTarget(
            workspace_id="workspace-1",
            user_id="owner",
            atespace="fixture-space",
            actor="fixture-actor",
            ports={},
        )
        sock = FakeSocket()
        with (
            patch.object(
                settings, "substrate_router_address", "http://router.fixture:8081"
            ),
            patch.object(preview_proxy.socket, "create_connection", return_value=sock),
        ):
            preview_proxy._connect_router(
                target,
                5173,
                2.0,
                method="GET",
                path="/",
                headers=[
                    ("Host", "5173--workspace-1--preview.example:8001"),
                    ("HOST", "second.example"),
                    ("Accept", "text/html"),
                ],
                body=b"",
            ).close()
        request = sock.sent[1].decode("latin1")
        hosts = [
            line
            for line in request.split("\r\n")[1:]
            if line.lower().startswith("host:")
        ]
        self.assertEqual(hosts, ["Host: actor-upstream:5173"])
        self.assertNotIn("preview.example", request)

    def test_forwardable_header_covers_the_websocket_upgrade_too(self):
        for name in (
            "Cookie",
            "tailscale-user-login",
            "X-Forwarded-For",
            "X-Real-Ip",
            "Forwarded",
        ):
            self.assertFalse(preview_proxy._forwardable_header(name), name)
        for name in ("User-Agent", "Accept-Language", "X-CSRF-Token", "Sec-Fetch-Mode"):
            self.assertTrue(preview_proxy._forwardable_header(name), name)

    def test_websocket_frame_parser_handles_masked_and_unmasked_payloads(self):
        async def exercise():
            reader = asyncio.StreamReader()
            reader.feed_data(
                preview_proxy._encode_websocket_frame(1, b"hello", masked=False)
            )
            self.assertEqual(
                await preview_proxy._read_websocket_frame(reader), (True, 1, b"hello")
            )
            reader.feed_data(
                preview_proxy._encode_websocket_frame(2, b"bytes", masked=True)
            )
            self.assertEqual(
                await preview_proxy._read_websocket_frame(reader), (True, 2, b"bytes")
            )

        asyncio.run(exercise())

    def test_target_is_the_kagent_session_actor_and_ports_are_the_stored_ones(self):
        async def exercise():
            row = {
                "workspace_id": "workspace-1",
                "kagent_session_id": "sess-1",
                "ports": {5173: "web", 8000: "api"},
            }
            with (
                patch.object(
                    preview_proxy.workspaces,
                    "preview_row",
                    new=AsyncMock(return_value=row),
                ) as lookup,
                patch.object(settings, "kagent_actor_atespace", "kagent"),
                patch.object(
                    settings,
                    "substrate_preview_base_url",
                    "http://localhost:8001",
                ),
            ):
                target = await preview_proxy._resolve_target("workspace-1", "owner")
                ports = await preview_proxy.workspace_preview_ports(
                    "workspace-1", "owner"
                )
            lookup.assert_awaited_with("workspace-1", "owner")
            self.assertEqual(
                (target.atespace, target.actor, target.ports),
                ("kagent", "session-sess-1", {5173: "web", 8000: "api"}),
            )
            self.assertEqual(
                ports,
                [
                    {
                        "port": 5173,
                        "name": "web",
                        "url": "http://5173--workspace-1--preview.localhost:8001",
                    },
                    {
                        "port": 8000,
                        "name": "api",
                        "url": "http://8000--workspace-1--preview.localhost:8001",
                    },
                ],
            )

        asyncio.run(exercise())

    def test_another_owners_or_missing_workspace_has_no_target(self):
        async def exercise():
            with patch.object(
                preview_proxy.workspaces,
                "preview_row",
                new=AsyncMock(return_value=None),
            ):
                self.assertIsNone(
                    await preview_proxy._resolve_target("workspace-1", "other")
                )
                self.assertIsNone(
                    await preview_proxy.workspace_preview_ports("workspace-1", "other")
                )

        asyncio.run(exercise())


if __name__ == "__main__":
    unittest.main()
