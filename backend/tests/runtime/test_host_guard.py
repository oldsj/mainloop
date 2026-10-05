"""M8: the API answers only to its own Host names; any other Host is a 404."""

from __future__ import annotations

import unittest
from unittest.mock import AsyncMock, patch

import httpx
from mainloop import api
from mainloop.config import Settings, settings
from mainloop.runtime import preview_proxy

PREVIEW_BASE = "http://100-116-68-0.sslip.io:8001"
FRONTEND = "dev.husky-komodo.ts.net"


class HostGuardTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        for patcher in (
            patch.object(settings, "frontend_domain", FRONTEND),
            patch.object(settings, "api_hosts", "mainloop-backend"),
            patch.object(settings, "substrate_preview_base_url", PREVIEW_BASE),
        ):
            patcher.start()
            self.addCleanup(patcher.stop)
        self.transport = httpx.ASGITransport(app=api.app, raise_app_exceptions=False)
        self.client = httpx.AsyncClient(transport=self.transport, base_url="http://x")
        self.addAsyncCleanup(self.client.aclose)

    async def status(self, host: str, path: str = "/") -> int:
        return (await self.client.get(path, headers={"Host": host})).status_code

    async def test_the_api_host_is_served_with_or_without_a_port(self):
        for host in (FRONTEND, f"{FRONTEND}:8443", FRONTEND.upper(), "mainloop-backend:8000"):
            with self.subTest(host=host):
                self.assertEqual(await self.status(host), 200)

    async def test_loopback_is_served_for_development(self):
        for host in ("localhost:8000", "127.0.0.1:8000", "[::1]:8000"):
            with self.subTest(host=host), patch.object(settings, "dev_mode", True):
                self.assertEqual(await self.status(host), 200)

    async def test_loopback_is_not_implicitly_allowed_outside_development(self):
        with patch.object(settings, "dev_mode", False), patch.object(settings, "is_test_env", False):
            self.assertEqual(await self.status("localhost:8000"), 404)

    async def test_any_other_host_is_a_404(self):
        for host in (
            "evil.example",
            "evil.example:8443",
            f"{FRONTEND}.evil.example",
            f"evil.{FRONTEND}",
            "100-116-68-0.sslip.io:8001",
            "",
            "[bad",
        ):
            with self.subTest(host=host):
                self.assertEqual(await self.status(host), 404)

    async def test_a_preview_shaped_host_that_is_not_a_live_preview_is_not_the_api(self):
        host = "3000--ws--preview.100-116-68-0.sslip.io:8001"
        with patch.object(preview_proxy, "_resolve_target", AsyncMock(return_value=None)):
            self.assertEqual(await self.status(host), 404)  # a preview, workspace not found
        # Same name on another port is not a preview host at all, and not the API either.
        self.assertEqual(await self.status("3000--ws--preview.100-116-68-0.sslip.io"), 404)

    async def test_the_health_probe_is_exempt_because_kubelet_uses_the_pod_ip(self):
        self.assertEqual(await self.status("10.244.0.7:8000", "/health"), 200)
        self.assertEqual(await self.status("10.244.0.7:8000", "/sessions"), 404)

    def test_a_preview_host_with_the_base_port_is_parsed(self):
        parsed = preview_proxy.parse_preview_host(
            "3000--ws--preview.100-116-68-0.sslip.io:8001"
        )
        self.assertEqual(parsed, preview_proxy.PreviewHost(3000, "ws"))
        self.assertIsNone(
            preview_proxy.parse_preview_host("3000--ws--preview.100-116-68-0.sslip.io")
        )

    def test_the_frontend_origin_follows_the_scheme_setting(self):
        self.assertEqual(settings.frontend_origin, f"https://{FRONTEND}")
        with patch.object(settings, "frontend_scheme", "http"):
            self.assertEqual(settings.frontend_origin, f"http://{FRONTEND}")

    def test_api_hosts_are_normalised(self):
        loaded = Settings(
            _env_file=None, frontend_domain="A.example", MAINLOOP_API_HOSTS=" B.example. ,,c"
        )
        self.assertEqual(
            loaded.allowed_api_hosts, frozenset({"a.example", "b.example", "c"})
        )

    def test_configured_domains_with_ports_are_compared_as_hostnames(self):
        loaded = Settings(_env_file=None, frontend_domain="localhost:5173", api_domain="API.example:8443")
        self.assertEqual(loaded.frontend_origin, "https://localhost:5173")
        self.assertIn("api.example", loaded.allowed_api_hosts)
        self.assertIn("localhost", loaded.allowed_api_hosts)


if __name__ == "__main__":
    unittest.main()
