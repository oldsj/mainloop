"""Browser writes from another origin are refused; the preview listener and non-browser clients aren't."""

import unittest
from unittest.mock import AsyncMock, patch

import httpx
from fastapi.responses import PlainTextResponse
from mainloop import api
from mainloop.config import settings
from mainloop.runtime import preview_proxy

WRITES = (
    "/sessions/s1/cancel",
    "/sessions/s1/stop-turn",
    "/sessions/s1/archive",
    "/workspaces/s1/suspend",
    "/workspaces/s1/resume",
    "/workspaces/s1/refresh",
)


class OriginGuardTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        hosts = patch.object(settings, "api_hosts", "api.test")
        hosts.start()
        self.addCleanup(hosts.stop)
        self.client = httpx.AsyncClient(
            # A request the guard lets through reaches a handler with no database here and may
            # fail with a 5xx; only a guard refusal (403) matters to these tests.
            transport=httpx.ASGITransport(app=api.app, raise_app_exceptions=False),
            base_url="http://api.test",
        )
        self.addAsyncCleanup(self.client.aclose)

    async def post(self, path, origin=None, **headers):
        if origin is not None:
            headers["Origin"] = origin
        return await self.client.post(path, headers=headers)

    async def test_a_write_from_another_origin_is_refused(self):
        previews = "https://5173--workspace--preview.mainloop.example"
        for origin in (previews, "https://evil.example", "http://localhost", "null"):
            for path in WRITES:
                with self.subTest(origin=origin, path=path):
                    response = await self.post(path, origin)
                    self.assertEqual(response.status_code, 403)
                    self.assertEqual(
                        response.json(), {"detail": "Cross-origin request refused."}
                    )

    async def test_the_frontend_origin_is_allowed(self):
        for path in WRITES:
            with self.subTest(path=path):
                response = await self.post(path, settings.frontend_origin)
                self.assertNotEqual(response.status_code, 403)

    async def test_a_localhost_origin_is_allowed_only_in_development(self):
        for path in WRITES:
            with self.subTest(mode="dev", path=path), patch.object(
                settings, "dev_mode", True
            ):
                response = await self.post(path, "http://localhost:5173")
                self.assertNotEqual(response.status_code, 403)
            with self.subTest(mode="prod", path=path), patch.object(
                settings, "dev_mode", False
            ), patch.object(settings, "is_test_env", False):
                response = await self.post(path, "http://localhost:5173")
                self.assertEqual(response.status_code, 403)

    async def test_the_apis_own_origin_is_allowed(self):
        self.assertNotEqual(
            (await self.post(WRITES[0], "http://api.test")).status_code, 403
        )
        # Same host on another port is another origin.
        self.assertEqual(
            (await self.post(WRITES[0], "http://api.test:9999")).status_code, 403
        )

    async def test_a_client_that_sends_no_origin_is_not_affected(self):
        for path in WRITES:
            with self.subTest(path=path):
                self.assertNotEqual((await self.post(path)).status_code, 403)

    async def test_reads_and_preflights_are_not_checked(self):
        evil = {"Origin": "https://evil.example"}
        self.assertNotEqual(
            (await self.client.get("/sessions/s1", headers=evil)).status_code, 403
        )
        preflight = await self.client.options(
            WRITES[0],
            headers={**evil, "Access-Control-Request-Method": "POST"},
        )
        self.assertNotEqual(preflight.status_code, 403)
        self.assertNotIn("access-control-allow-origin", preflight.headers)

    async def test_the_preview_listener_is_not_subject_to_the_guard(self):
        # A preview page posts to its own dev server with its own Origin.
        host = "5173--workspace--preview.mainloop.example"
        served = AsyncMock(return_value=PlainTextResponse("dev server"))
        with (
            patch.object(
                settings,
                "substrate_preview_base_url",
                "https://mainloop.example",
            ),
            patch.object(preview_proxy, "_preview_http", served),
        ):
            response = await self.client.post(
                "/api/login",
                headers={"Host": host, "Origin": f"https://{host}"},
            )
        self.assertEqual(response.status_code, 200)
        self.assertEqual(response.text, "dev server")
        served.assert_awaited_once()


if __name__ == "__main__":
    unittest.main()
