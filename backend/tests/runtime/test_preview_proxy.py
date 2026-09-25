"""Fake-backed host, port policy, and proxy framing tests."""

from __future__ import annotations

import asyncio
import http.client
import io
import os
import socket
import unittest
from unittest.mock import AsyncMock, patch

from mainloop.config import settings
from mainloop.runtime import preview_proxy
from starlette.requests import Request
from starlette.websockets import WebSocket

_PREVIEW_HOST = "5173--workspace-1.preview.localhost:8001"


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
    def test_preview_host_requires_matching_domain_port_and_safe_labels(self):
        base = "http://preview.localhost:8001"
        parsed = preview_proxy.parse_preview_host(
            "5173--workspace-1.preview.localhost:8001", base
        )
        self.assertEqual(parsed, preview_proxy.PreviewHost(5173, "workspace-1"))
        for invalid in (
            "5173--workspace-1.example.test:8001",
            "5173--workspace-1.preview.localhost:3000",
            "65536--workspace-1.preview.localhost:8001",
            "5173--bad-.preview.localhost:8001",
            "5173--workspace-1.preview.localhost/path:8001",
        ):
            with self.subTest(invalid=invalid):
                self.assertIsNone(preview_proxy.parse_preview_host(invalid, base))

    def test_manifest_port_formats_are_validated_and_deduplicated(self):
        ports = preview_proxy.declared_ports(
            {
                "forwardPorts": [5173, "invalid", 70000, True],
                "dev": {
                    "ports": [
                        {"name": "web", "number": 5173},
                        {"name": "api", "port": 8000},
                        {"port": 0},
                    ]
                },
            }
        )
        self.assertEqual(ports, {5173: "web", 8000: "api"})

    def test_preview_url_uses_configured_wildcard_origin(self):
        with patch.object(
            settings, "substrate_preview_base_url", "http://preview.localhost:8001"
        ):
            self.assertEqual(
                preview_proxy.preview_url("workspace-1", 5173),
                "http://5173--workspace-1.preview.localhost:8001",
            )

    def test_dynamic_shim_secret_uses_configured_prefix_convention(self):
        with patch.object(settings, "substrate_shim_secret_prefix", "test-shim"):
            self.assertEqual(
                preview_proxy._secret_name_for_workspace("fixture-space", "actor-1"),
                settings.shim_token_secret_name("fixture-space", "actor-1"),
            )

    def test_preview_touch_calls_lifecycle_service(self):
        async def exercise():
            with patch.object(
                preview_proxy.workspace_adapter,
                "touch_workspace",
                new=AsyncMock(),
            ) as touch:
                await preview_proxy._touch_preview("workspace-1")
            touch.assert_awaited_once_with("workspace-1", reason="preview")

        asyncio.run(exercise())

    def test_http_preview_requires_trusted_identity_and_ignores_forged_user_id(self):
        async def exercise():
            with patch.object(
                preview_proxy, "_resolve_target", new_callable=AsyncMock
            ) as resolve:
                anonymous = await preview_proxy._preview_http(
                    make_request("GET", {"host": _PREVIEW_HOST}),
                    preview_proxy.PreviewHost(5173, "workspace-1"),
                )
                forged = await preview_proxy._preview_http(
                    make_request(
                        "GET",
                        {"host": _PREVIEW_HOST, "x-user-id": "local-dev-user"},
                    ),
                    preview_proxy.PreviewHost(5173, "workspace-1"),
                )

            self.assertEqual(anonymous.status_code, 401)
            self.assertEqual(forged.status_code, 401)
            resolve.assert_not_awaited()

        with patch.dict(
            os.environ,
            {
                "SUBSTRATE_PREVIEW_LOCAL_DEV_MODE": "false",
                "SUBSTRATE_PREVIEW_TRUSTED_INGRESS": "false",
            },
        ):
            asyncio.run(exercise())

    def test_http_preview_uses_trusted_identity_for_workspace_ownership(self):
        async def exercise():
            with patch.object(
                preview_proxy, "_resolve_target", new=AsyncMock(return_value=None)
            ) as resolve:
                response = await preview_proxy._preview_http(
                    make_request(
                        "GET",
                        {
                            "host": _PREVIEW_HOST,
                            "x-user-id": "workspace-owner",
                            "cf-access-authenticated-user-email": "other-owner",
                        },
                    ),
                    preview_proxy.PreviewHost(5173, "workspace-1"),
                )

            self.assertEqual(response.status_code, 404)
            resolve.assert_awaited_once_with("workspace-1", "other-owner")

        with patch.dict(
            os.environ,
            {
                "SUBSTRATE_PREVIEW_LOCAL_DEV_MODE": "false",
                "SUBSTRATE_PREVIEW_TRUSTED_INGRESS": "true",
            },
        ):
            asyncio.run(exercise())

    def test_websocket_preview_requires_trusted_identity_and_checks_owner(self):
        async def exercise_anonymous():
            with patch.object(
                preview_proxy, "_resolve_target", new_callable=AsyncMock
            ) as resolve:
                for headers in (
                    {"host": _PREVIEW_HOST},
                    {"host": _PREVIEW_HOST, "x-user-id": "local-dev-user"},
                ):
                    websocket, sent = make_websocket(headers)
                    await preview_proxy._preview_websocket(
                        websocket, preview_proxy.PreviewHost(5173, "workspace-1")
                    )
                    self.assertEqual(sent[0]["type"], "websocket.close")
                    self.assertEqual(sent[0]["code"], 4401)
                resolve.assert_not_awaited()

        async def exercise_wrong_owner():
            with patch.object(
                preview_proxy, "_resolve_target", new=AsyncMock(return_value=None)
            ) as resolve:
                websocket, sent = make_websocket(
                    {
                        "host": _PREVIEW_HOST,
                        "x-user-id": "workspace-owner",
                        "cf-access-authenticated-user-email": "other-owner",
                    }
                )
                await preview_proxy._preview_websocket(
                    websocket, preview_proxy.PreviewHost(5173, "workspace-1")
                )
                self.assertEqual(sent[0]["type"], "websocket.close")
                self.assertEqual(sent[0]["code"], 4404)
                resolve.assert_awaited_once_with("workspace-1", "other-owner")

        with patch.dict(
            os.environ,
            {
                "SUBSTRATE_PREVIEW_LOCAL_DEV_MODE": "false",
                "SUBSTRATE_PREVIEW_TRUSTED_INGRESS": "false",
            },
        ):
            asyncio.run(exercise_anonymous())
        with patch.dict(
            os.environ,
            {
                "SUBSTRATE_PREVIEW_LOCAL_DEV_MODE": "false",
                "SUBSTRATE_PREVIEW_TRUSTED_INGRESS": "true",
            },
        ):
            asyncio.run(exercise_wrong_owner())

    def test_post_that_commits_then_disconnects_is_not_replayed(self):
        target = preview_proxy.PreviewTarget(
            workspace_id="workspace-1",
            user_id="local-dev-user",
            atespace="fixture-space",
            actor="fixture-actor",
            agent="claude",
            shim_secret_name="shim" + "-fixture",
            manifest={"dev": {"ports": [{"name": "web", "number": 5173}]}},
            observed_state="running",
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
                    preview_proxy, "_reported_ports", new=AsyncMock(return_value=())
                ),
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

        with patch.dict(os.environ, {"SUBSTRATE_PREVIEW_LOCAL_DEV_MODE": "true"}):
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
            agent="claude",
            shim_secret_name="shim" + "-fixture",
            manifest={"dev": {"ports": [{"name": "web", "number": 5173}]}},
            observed_state="running",
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
                patch.object(
                    preview_proxy, "_reported_ports", new=AsyncMock(return_value=())
                ),
                patch.object(preview_proxy, "_connect_router", side_effect=connect),
            ):
                response = await preview_proxy._preview_http(
                    make_request("GET", {"host": _PREVIEW_HOST}),
                    preview_proxy.PreviewHost(5173, "workspace-1"),
                )
                body = b"".join([chunk async for chunk in response.body_iterator])
            return response, body

        with patch.dict(os.environ, {"SUBSTRATE_PREVIEW_LOCAL_DEV_MODE": "true"}):
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
            agent="claude",
            shim_secret_name="shim" + "-fixture",
            manifest={},
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

    def test_workspace_port_list_combines_declared_and_reported_ports(self):
        async def exercise():
            shim_name = "shim" + "-fixture"
            target = preview_proxy.PreviewTarget(
                workspace_id="workspace-1",
                user_id="owner",
                atespace="fixture-space",
                actor="fixture-actor",
                agent="claude",
                shim_secret_name=shim_name,
                manifest={"dev": {"ports": [{"name": "web", "number": 5173}]}},
                observed_state="running",
            )
            with (
                patch.object(preview_proxy, "_resolve_target", return_value=target),
                patch.object(
                    preview_proxy, "_reported_ports", return_value=(5173, 8080)
                ),
                patch.object(
                    settings,
                    "substrate_preview_base_url",
                    "http://preview.localhost:8001",
                ),
            ):
                result = await preview_proxy.workspace_preview_ports(
                    "workspace-1", "owner"
                )
            self.assertEqual(
                result,
                [
                    {
                        "port": 5173,
                        "name": "web",
                        "url": "http://5173--workspace-1.preview.localhost:8001",
                    },
                    {
                        "port": 8080,
                        "name": "Port 8080",
                        "url": "http://8080--workspace-1.preview.localhost:8001",
                    },
                ],
            )

        asyncio.run(exercise())

    def test_workspace_port_list_does_not_wake_a_parked_actor(self):
        async def exercise():
            shim_name = "shim" + "-fixture"
            target = preview_proxy.PreviewTarget(
                workspace_id="workspace-1",
                user_id="owner",
                atespace="fixture-space",
                actor="fixture-actor",
                agent="claude",
                shim_secret_name=shim_name,
                manifest={"forwardPorts": [5173]},
                observed_state="suspended",
            )
            with (
                patch.object(preview_proxy, "_resolve_target", return_value=target),
                patch.object(
                    preview_proxy, "_reported_ports", new_callable=AsyncMock
                ) as report,
            ):
                ports = await preview_proxy.workspace_preview_ports(
                    "workspace-1", "owner"
                )
            report.assert_not_awaited()
            self.assertEqual([row["port"] for row in ports], [5173])

        asyncio.run(exercise())


if __name__ == "__main__":
    unittest.main()
