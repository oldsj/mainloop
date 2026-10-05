"""POST /sessions/{id}/stop-turn: owner check and the mapping of stop outcomes and errors."""

from __future__ import annotations

import unittest
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch

import httpx
from mainloop import api
from mainloop.config import settings
from mainloop.runtime import native_sessions
from mainloop.runtime.kagent_client import Unreachable

SESSION = "session-1"


class StopTurnApiTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.session = SimpleNamespace(id=SESSION, user_id="user-1")
        self.stop = AsyncMock(return_value="stopped")
        self.binding = AsyncMock(return_value={"session_id": SESSION})
        for patcher in (
            patch.object(settings, "owner_id", "user-1"),
            patch.object(settings, "api_hosts", "test"),
            patch.object(api.db, "get_session", AsyncMock(return_value=self.session)),
            patch.object(native_sessions, "get_binding", self.binding),
            patch.object(native_sessions, "stop_turn", self.stop),
        ):
            patcher.start()
            self.addCleanup(patcher.stop)
        self.client = httpx.AsyncClient(
            transport=httpx.ASGITransport(app=api.app), base_url="http://test"
        )
        self.addAsyncCleanup(self.client.aclose)

    async def post(self, user: str | None = None) -> httpx.Response:
        headers = {"X-User-ID": user} if user else {}
        return await self.client.post(f"/sessions/{SESSION}/stop-turn", headers=headers)

    async def test_owner_gets_the_outcome(self):
        for outcome in ("stopped", "finished", "no_open_turn"):
            self.stop.return_value = outcome
            response = await self.post()
            self.assertEqual(response.status_code, 200)
            self.assertEqual(response.json(), {"status": outcome})

    async def test_a_session_of_another_user_is_404_and_nothing_is_stopped(self):
        api.db.get_session.return_value = SimpleNamespace(id=SESSION, user_id="user-2")
        response = await self.post()
        self.assertEqual(response.status_code, 404)
        self.stop.assert_not_awaited()

    async def test_the_x_user_id_header_has_no_effect(self):
        api.db.get_session.return_value = SimpleNamespace(id=SESSION, user_id="user-2")
        self.assertEqual((await self.post("user-2")).status_code, 404)
        self.stop.assert_not_awaited()

    async def test_unknown_session_is_404(self):
        api.db.get_session.return_value = None
        self.assertEqual((await self.post()).status_code, 404)
        self.stop.assert_not_awaited()

    async def test_session_without_a_native_binding_is_409(self):
        self.binding.return_value = None
        self.assertEqual((await self.post()).status_code, 409)
        self.stop.assert_not_awaited()

    async def test_unconfirmed_stop_is_409_and_a_kagent_error_is_502(self):
        self.stop.side_effect = native_sessions.StopUnconfirmed("kagent shows no task")
        response = await self.post()
        self.assertEqual(response.status_code, 409)
        self.assertIn("kagent shows no task", response.json()["detail"])
        self.stop.side_effect = Unreachable("gateway down")
        response = await self.post()
        self.assertEqual(response.status_code, 502)
        self.assertIn("gateway down", response.json()["detail"])


if __name__ == "__main__":
    unittest.main()
