"""Fake-backed host, port policy, and proxy framing tests."""

from __future__ import annotations

import asyncio
import io
import unittest
from unittest.mock import AsyncMock, patch

from mainloop.config import settings
from mainloop.runtime import preview_proxy


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

    def test_chunked_response_body_is_decoded_for_streaming_response(self):
        import http.client

        headers = http.client.HTTPMessage()
        headers["Transfer-Encoding"] = "chunked"
        reader = io.BytesIO(b"4\r\nWiki\r\n5\r\npedia\r\n0\r\n\r\n")
        self.assertEqual(
            list(preview_proxy._response_body(reader, headers, no_body=False)),
            [b"Wiki", b"pedia"],
        )

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
