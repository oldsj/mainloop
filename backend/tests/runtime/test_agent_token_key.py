"""``AGENT_TOKEN_KEY`` is required outside dev mode, and the app refuses to start without it."""

from __future__ import annotations

import unittest
from unittest.mock import AsyncMock, patch

from fastapi.testclient import TestClient
from mainloop import api
from mainloop.mcp_app import create_app
from mainloop.config import settings
from mainloop.runtime import agent_identity


def _settings(key: str, password: str, dev: bool = False, test_env: bool = False):
    return (
        patch.object(settings, "agent_token_key", key),
        patch.object(settings, "db_password", password),
        patch.object(settings, "dev_mode", dev),
        patch.object(settings, "is_test_env", test_env),
    )


class _Patched:
    def __init__(self, *patches):
        self.patches = patches

    def __enter__(self):
        for p in self.patches:
            p.start()

    def __exit__(self, *exc):
        for p in self.patches:
            p.stop()


class TokenKeyTests(unittest.TestCase):
    def test_outside_dev_mode_the_key_is_required_and_the_db_password_is_not_used(self):
        with _Patched(*_settings("", "db-password")):
            with self.assertRaises(RuntimeError):
                agent_identity.require_token_key()
            with self.assertRaises(RuntimeError):
                agent_identity.token_for("s1")

    def test_a_configured_key_passes_and_signs_with_the_key(self):
        with _Patched(*_settings("k1", "db-password")):
            agent_identity.require_token_key()
            first = agent_identity.token_for("s1")
        with _Patched(*_settings("k2", "db-password")):
            self.assertNotEqual(first, agent_identity.token_for("s1"))

    def test_dev_mode_and_the_test_env_fall_back_to_the_db_password(self):
        for kwargs in ({"dev": True}, {"test_env": True}):
            with self.subTest(kwargs=kwargs), _Patched(
                *_settings("", "db-password", **kwargs)
            ):
                agent_identity.require_token_key()
                self.assertTrue(agent_identity.token_for("s1").startswith("ml_"))

    def test_dev_mode_without_any_key_still_fails(self):
        with _Patched(*_settings("", "", dev=True)):
            with self.assertRaises(RuntimeError):
                agent_identity.require_token_key()


class StartupTests(unittest.IsolatedAsyncioTestCase):
    async def test_the_api_refuses_to_start_without_the_key_before_connecting(self):
        with (
            _Patched(*_settings("", "db-password")),
            patch.object(api.db, "connect", AsyncMock()) as connect,
        ):
            with self.assertRaises(RuntimeError):
                await api.startup_event()
        connect.assert_not_awaited()

    def test_the_mcp_listener_refuses_to_start_without_the_key_before_connecting(self):
        with (
            _Patched(*_settings("", "db-password")),
            patch.object(api.db, "connect", AsyncMock()) as connect,
        ):
            with self.assertRaises(RuntimeError), TestClient(create_app()):
                pass
        connect.assert_not_awaited()


if __name__ == "__main__":
    unittest.main()
