"""The identity seam: every request is the configured owner, whatever ``X-User-ID`` says."""

from __future__ import annotations

import unittest
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch

import httpx
from mainloop import api
from mainloop.config import settings
from mainloop.runtime import preview_proxy

from models import Session

OWNER = "the-owner"
OTHER = "someone-else"

# (method, path, db function that finds the row, the status when the row is the owner's)
ROUTES = [
    ("GET", "/threads/x", "get_main_thread"),
    ("GET", "/conversations/x", "get_conversation"),
    ("GET", "/queue/x", "get_queue_item"),
    ("GET", "/projects/x", "get_project"),
    ("POST", "/projects/x/refresh", "get_project"),
    ("GET", "/projects/x/detail", "get_project"),
    ("GET", "/sessions/x", "get_session"),
    ("GET", "/sessions/x/conversation", "get_session"),
    ("GET", "/sessions/x/native", "get_session"),
    ("POST", "/sessions/x/stop-turn", "get_session"),
    ("POST", "/sessions/x/cancel", "get_session"),
    ("POST", "/sessions/x/archive", "get_session"),
    ("POST", "/queue/x/read", "get_queue_item"),
]


class IdentitySeamTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        for patcher in (
            patch.object(settings, "owner_id", OWNER),
            patch.object(settings, "api_hosts", "test"),
        ):
            patcher.start()
            self.addCleanup(patcher.stop)
        self.client = httpx.AsyncClient(
            transport=httpx.ASGITransport(app=api.app), base_url="http://test"
        )
        self.addAsyncCleanup(self.client.aclose)

    def found(self, function: str, user_id: str):
        row = SimpleNamespace(id="x", user_id=user_id)
        return patch.object(api.db, function, AsyncMock(return_value=row))

    async def test_another_users_row_is_404_on_every_listed_route(self):
        for method, path, function in ROUTES:
            with self.subTest(path=path), self.found(function, OTHER):
                response = await self.client.request(method, path)
                self.assertEqual(response.status_code, 404)

    async def test_x_user_id_does_not_unlock_another_users_row(self):
        for method, path, function in ROUTES:
            for header in (OTHER, OWNER):
                with self.subTest(path=path, header=header), self.found(function, OTHER):
                    response = await self.client.request(
                        method, path, headers={"X-User-ID": header}
                    )
                    self.assertEqual(response.status_code, 404)

    async def test_x_user_id_does_not_hide_the_owners_own_row(self):
        session = Session(
            id="x",
            user_id=OWNER,
            main_thread_id="mt",
            title="t",
            description="d",
            prompt="p",
            conversation_id="c",
        )
        with patch.object(api.db, "get_session", AsyncMock(return_value=session)):
            response = await self.client.get("/sessions/x", headers={"X-User-ID": OTHER})
        self.assertEqual(response.status_code, 200)

    async def test_dismiss_is_scoped_to_the_owner(self):
        dismiss = AsyncMock(return_value=False)
        with patch.object(api.db, "dismiss_session_notification", dismiss):
            response = await self.client.post(
                "/notifications/n1/dismiss", headers={"X-User-ID": OTHER}
            )
        self.assertEqual(response.status_code, 404)
        dismiss.assert_awaited_once_with("n1", OWNER)
        dismiss.return_value = True
        with patch.object(api.db, "dismiss_session_notification", dismiss):
            response = await self.client.post("/notifications/n1/dismiss")
        self.assertEqual(response.status_code, 200)

    async def test_lists_are_the_owners_whatever_the_header(self):
        seen = AsyncMock(return_value=[])
        with patch.object(api.db, "list_sessions", seen):
            await self.client.get("/sessions", headers={"X-User-ID": OTHER})
        self.assertEqual(seen.await_args.kwargs["user_id"], OWNER)

    async def test_the_preview_proxy_resolves_the_owner(self):
        self.assertIn("current_user", vars(preview_proxy))
        self.assertEqual(preview_proxy.current_user(), OWNER)


if __name__ == "__main__":
    unittest.main()
