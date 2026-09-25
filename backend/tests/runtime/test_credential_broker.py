"""Credential broker tests use only synthetic fixture values and an in-memory Secret store."""

from __future__ import annotations

import asyncio
import base64
import json
import tempfile
import unittest
from datetime import UTC, datetime, timedelta
from pathlib import Path
from unittest.mock import patch

from mainloop.runtime.credential_broker import (
    CredentialBroker,
    CredentialNeedsSignin,
    CredentialSecretMissing,
)


async def _run_sync_in_test(function, *args, **kwargs):
    """Keep fake Secret and temporary-file operations deterministic in tests."""
    await asyncio.sleep(0)
    return function(*args, **kwargs)


def jwt(expiry: int) -> str:
    payload = (
        base64.urlsafe_b64encode(
            json.dumps({"exp": expiry}, separators=(",", ":")).encode()
        )
        .decode()
        .rstrip("=")
    )
    return f"eyJhbGciOiJub25lIn0.{payload}.fixture"


class MemorySecretStore:
    def __init__(self):
        self.values: dict[tuple[str, str], dict[str, str]] = {}
        self.publish_count = 0

    def precreate(self, namespace: str, name: str):
        self.values[(namespace, name)] = {}

    def read(self, namespace: str, name: str):
        value = self.values.get((namespace, name))
        return dict(value) if value is not None else None

    def publish(self, namespace: str, name: str, values):
        if (namespace, name) not in self.values:
            raise AssertionError("credential Secret must be pre-created")
        self.values[(namespace, name)] = dict(values)
        self.publish_count += 1


class CredentialBrokerTests(unittest.TestCase):
    def setUp(self):
        self._to_thread_patch = patch.object(
            asyncio, "to_thread", new=_run_sync_in_test
        )
        self._to_thread_patch.start()

    def tearDown(self):
        self._to_thread_patch.stop()

    def test_codex_seed_publishes_access_token_and_delivers_only_synthetic_auth(self):
        async def exercise(root: Path):
            store = MemorySecretStore()
            source_access = jwt(
                int((datetime.now(UTC) + timedelta(hours=1)).timestamp())
            )
            source_id = jwt(int((datetime.now(UTC) + timedelta(hours=1)).timestamp()))
            source = {
                "auth_mode": "chatgpt",
                "tokens": {
                    "id_token": source_id,
                    "access_token": source_access,
                    "refresh_token": "synthetic-refresh-fixture",
                    "account_id": "fixture-account-42",
                },
                "last_refresh": datetime.now(UTC).isoformat(),
            }
            auth_path = root / "auth.json"
            auth_path.write_text(json.dumps(source), encoding="utf-8")
            broker = CredentialBroker(
                store=store,
                codex_auth_path=str(auth_path),
                namespace="test-control",
                account="fixture-owner",
            )
            store.precreate("test-control", broker.secret_name("codex"))

            statuses = await asyncio.gather(
                broker.status("codex"), broker.status("codex")
            )
            status = statuses[0]
            self.assertTrue(status.available)
            self.assertFalse(status.needs_signin)
            self.assertEqual(status.expires_at.tzinfo, UTC)
            self.assertEqual(status.state, "available")
            self.assertEqual(store.publish_count, 1)

            secret = store.read("test-control", broker.secret_name("codex"))
            self.assertEqual(secret["injection-value"], source_access)
            placeholder = json.loads(await broker.codex_placeholder_auth())
            self.assertEqual(placeholder["tokens"]["account_id"], "fixture-account-42")
            self.assertEqual(placeholder["tokens"]["refresh_token"], "")
            self.assertNotEqual(placeholder["tokens"]["access_token"], source_access)
            self.assertNotEqual(placeholder["tokens"]["id_token"], source_id)
            self.assertNotIn(source_access, json.dumps(placeholder))
            self.assertNotIn("synthetic-refresh-fixture", json.dumps(placeholder))
            self.assertEqual(len(placeholder["tokens"]["access_token"].split(".")), 3)

        with tempfile.TemporaryDirectory() as directory:
            asyncio.run(exercise(Path(directory)))

    def test_expired_codex_token_is_not_published_and_requires_signin(self):
        async def exercise(root: Path):
            store = MemorySecretStore()
            source = {
                "tokens": {
                    "id_token": jwt(2_000_000_000),
                    "access_token": jwt(1),
                    "refresh_token": "synthetic-refresh-fixture",
                    "account_id": "fixture-account-expired",
                },
                "last_refresh": datetime.now(UTC).isoformat(),
            }
            path = root / "auth.json"
            path.write_text(json.dumps(source), encoding="utf-8")
            broker = CredentialBroker(store=store, codex_auth_path=str(path))
            store.precreate("mainloop-control", broker.secret_name("codex"))

            status = await broker.seed_configured("codex")
            self.assertFalse(status.available)
            self.assertTrue(status.needs_signin)
            self.assertEqual(status.state, "expired")
            values = store.read("mainloop-control", broker.secret_name("codex"))
            self.assertEqual(values["injection-value"], "")

            valid_expiry = int((datetime.now(UTC) + timedelta(hours=1)).timestamp())
            path.write_text(
                json.dumps(
                    {
                        "tokens": {
                            "id_token": jwt(valid_expiry),
                            "access_token": jwt(valid_expiry),
                            "refresh_token": "new-synthetic-refresh",
                            "account_id": "fixture-account-expired",
                        },
                        "last_refresh": datetime.now(UTC).isoformat(),
                    }
                ),
                encoding="utf-8",
            )
            status = await broker.status("codex")
            self.assertEqual(status.state, "expired")
            self.assertEqual(store.publish_count, 1)
            with self.assertRaises(CredentialNeedsSignin) as raised:
                await broker.codex_placeholder_auth()
            self.assertEqual(str(raised.exception), "Codex needs sign-in")
            self.assertNotIn(str(path), str(raised.exception))

        with tempfile.TemporaryDirectory() as directory:
            asyncio.run(exercise(Path(directory)))

    def test_expiring_stored_token_is_removed_from_injection_before_signin(self):
        async def exercise(root: Path):
            store = MemorySecretStore()
            expiry = int((datetime.now(UTC) + timedelta(hours=1)).timestamp())
            source = {
                "tokens": {
                    "id_token": jwt(expiry),
                    "access_token": jwt(expiry),
                    "refresh_token": "synthetic-refresh-fixture",
                    "account_id": "fixture-account-near-expiry",
                },
                "last_refresh": datetime.now(UTC).isoformat(),
            }
            path = root / "auth.json"
            path.write_text(json.dumps(source), encoding="utf-8")
            broker = CredentialBroker(store=store, codex_auth_path=str(path))
            store.precreate("mainloop-control", broker.secret_name("codex"))
            await broker.seed_configured("codex")
            name = broker.secret_name("codex")
            values = store.read("mainloop-control", name)
            values["expires-at"] = str(
                int((datetime.now(UTC) + timedelta(seconds=30)).timestamp())
            )
            store.publish("mainloop-control", name, values)

            status = await broker.status("codex")
            self.assertTrue(status.needs_signin)
            values = store.read("mainloop-control", name)
            self.assertEqual(values["injection-value"], "")
            self.assertEqual(values["needs-signin"], "true")
            with self.assertRaises(CredentialNeedsSignin):
                await broker.codex_placeholder_auth()

        with tempfile.TemporaryDirectory() as directory:
            asyncio.run(exercise(Path(directory)))

    def test_claude_file_is_seeded_by_path_and_missing_source_is_safe(self):
        async def exercise(root: Path):
            store = MemorySecretStore()
            path = root / "claude-token"
            path.write_text("synthetic-claude-token\n", encoding="utf-8")
            broker = CredentialBroker(store=store, claude_token_path=str(path))
            store.precreate("mainloop-control", broker.secret_name("claude"))
            status = await broker.status("claude")
            self.assertTrue(status.available)
            self.assertFalse(status.needs_signin)
            values = store.read("mainloop-control", broker.secret_name("claude"))
            self.assertEqual(values["injection-value"], "synthetic-claude-token")

            missing_store = MemorySecretStore()
            missing = CredentialBroker(
                store=missing_store, claude_token_path=str(root / "missing-token")
            )
            missing_store.precreate("mainloop-control", missing.secret_name("claude"))
            with self.assertRaises(CredentialNeedsSignin) as raised:
                await missing.seed_configured("claude")
            self.assertEqual(str(raised.exception), "Claude needs sign-in")

        with tempfile.TemporaryDirectory() as directory:
            asyncio.run(exercise(Path(directory)))

    def test_reauth_result_methods_publish_only_valid_synthetic_fixture_values(self):
        async def exercise():
            store = MemorySecretStore()
            broker = CredentialBroker(store=store)
            store.precreate("mainloop-control", broker.secret_name("codex"))
            store.precreate("mainloop-control", broker.secret_name("claude"))
            access = jwt(int((datetime.now(UTC) + timedelta(hours=1)).timestamp()))
            document = {
                "auth_mode": "chatgpt",
                "tokens": {
                    "id_token": access,
                    "access_token": access,
                    "refresh_token": "synthetic-refresh-fixture",
                    "account_id": "fixture-account-re-auth",
                },
                "last_refresh": datetime.now(UTC).isoformat(),
            }
            status = await broker.store_codex_auth_document(
                json.dumps(document).encode("utf-8")
            )
            self.assertTrue(status.available)
            self.assertEqual(
                store.read("mainloop-control", broker.secret_name("codex"))[
                    "injection-value"
                ],
                access,
            )
            claude = await broker.store_claude_token("synthetic-claude-token")
            self.assertTrue(claude.available)
            self.assertEqual(
                store.read("mainloop-control", broker.secret_name("claude"))[
                    "injection-value"
                ],
                "synthetic-claude-token",
            )
            placeholder = await broker.claude_placeholder_token()
            self.assertNotEqual(placeholder, "synthetic-claude-token")
            self.assertIn("egress-placeholder", placeholder)

        asyncio.run(exercise())

    def test_missing_secret_does_not_attempt_file_seeding(self):
        async def exercise(root: Path):
            store = MemorySecretStore()
            path = root / "auth.json"
            expiry = int((datetime.now(UTC) + timedelta(hours=1)).timestamp())
            path.write_text(
                json.dumps(
                    {
                        "tokens": {
                            "id_token": jwt(expiry),
                            "access_token": jwt(expiry),
                            "refresh_token": "synthetic-refresh-fixture",
                            "account_id": "fixture-account-missing-secret",
                        },
                        "last_refresh": datetime.now(UTC).isoformat(),
                    }
                ),
                encoding="utf-8",
            )
            broker = CredentialBroker(store=store, codex_auth_path=str(path))

            status = await broker.status("codex")
            self.assertEqual(status.state, "missing")
            self.assertFalse(status.available)
            self.assertEqual(store.values, {})
            self.assertEqual(store.publish_count, 0)

            with self.assertRaises(CredentialSecretMissing):
                await broker.seed_configured("codex")
            self.assertEqual(store.publish_count, 0)

        with tempfile.TemporaryDirectory() as directory:
            asyncio.run(exercise(Path(directory)))

    def test_rejected_secret_is_not_replaced_from_a_configured_file(self):
        async def exercise(root: Path):
            store = MemorySecretStore()
            path = root / "claude-token"
            path.write_text("synthetic-new-claude-token", encoding="utf-8")
            broker = CredentialBroker(store=store, claude_token_path=str(path))
            secret_name = broker.secret_name("claude")
            store.values[("mainloop-control", secret_name)] = {
                "oauth-token": "synthetic-rejected-token",
                "injection-value": "",
                "needs-signin": "true",
            }

            status = await broker.status("claude")
            self.assertEqual(status.state, "rejected")
            self.assertFalse(status.available)
            self.assertEqual(
                store.read("mainloop-control", secret_name)["oauth-token"],
                "synthetic-rejected-token",
            )
            self.assertEqual(store.publish_count, 0)

        with tempfile.TemporaryDirectory() as directory:
            asyncio.run(exercise(Path(directory)))


if __name__ == "__main__":
    unittest.main()
