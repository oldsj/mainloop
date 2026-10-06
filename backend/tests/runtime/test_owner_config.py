"""The single configured owner is the identity of every request that reaches Mainloop."""

from __future__ import annotations

import os
import unittest
from unittest.mock import patch

from mainloop import api
from mainloop.config import Settings, settings
from mainloop.identity import current_user
from mainloop.runtime import workspace_api
from pydantic import ValidationError


class OwnerConfigTests(unittest.TestCase):
    def test_owner_defaults_to_the_local_development_user(self):
        with patch.dict(os.environ, clear=False) as env:
            env.pop("MAINLOOP_OWNER_ID", None)
            self.assertEqual(Settings(_env_file=None).owner_id, "local-dev-user")

    def test_owner_is_set_by_mainloop_owner_id(self):
        with patch.dict(os.environ, {"MAINLOOP_OWNER_ID": " james "}):
            self.assertEqual(Settings(_env_file=None).owner_id, "james")

    def test_dev_mode_is_a_development_environment_and_so_is_the_test_env(self):
        for env, expected in (
            ({}, False),
            ({"DEV_MODE": "true"}, True),
            ({"IS_TEST_ENV": "true"}, True),
        ):
            with self.subTest(env=env):
                with patch.dict(os.environ, env):
                    if not env:
                        os.environ.pop("DEV_MODE", None)
                        os.environ.pop("IS_TEST_ENV", None)
                    self.assertEqual(Settings(_env_file=None).is_dev, expected)

    def test_a_blank_owner_is_rejected(self):
        for blank in ("", "   "):
            with self.subTest(blank=blank):
                with patch.dict(os.environ, {"MAINLOOP_OWNER_ID": blank}):
                    with self.assertRaises(ValidationError):
                        Settings(_env_file=None)

    def test_the_retired_ingress_settings_are_not_read(self):
        env = {
            "SUBSTRATE_PREVIEW_TRUSTED_INGRESS": "true",
            "SUBSTRATE_PREVIEW_LOCAL_DEV_MODE": "true",
        }
        with patch.dict(os.environ, env):
            loaded = Settings(_env_file=None)
        self.assertFalse(hasattr(loaded, "substrate_preview_trusted_ingress"))
        self.assertFalse(hasattr(loaded, "substrate_preview_local_dev_mode"))
        self.assertEqual(loaded.owner_id, "local-dev-user")

    def test_the_current_user_is_the_configured_owner(self):
        with patch.object(settings, "owner_id", "the-owner"):
            self.assertEqual(current_user(), "the-owner")
        self.assertFalse(hasattr(api, "get_owner_id"))
        self.assertFalse(hasattr(workspace_api, "_user_id"))
        self.assertFalse(hasattr(api, "get_user_id_from_cf_header"))


if __name__ == "__main__":
    unittest.main()
