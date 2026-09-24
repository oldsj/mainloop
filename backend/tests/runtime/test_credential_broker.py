"""Credential broker tests use only synthetic fixture values and an in-memory Secret store."""

from __future__ import annotations

import asyncio
import base64
import json
import tempfile
import unittest
from datetime import UTC, datetime, timedelta
from pathlib import Path

from mainloop.runtime.credential_broker import (
    CredentialBroker,
    CredentialNeedsSignin,
)


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

    def read(self, namespace: str, name: str):
        value = self.values.get((namespace, name))
        return dict(value) if value is not None else None

    def publish(self, namespace: str, name: str, values):
        self.values[(namespace, name)] = dict(values)


class CredentialBrokerTests(unittest.TestCase):
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

            status = await broker.seed_configured("codex")
            self.assertTrue(status.available)
            self.assertFalse(status.needs_signin)
            self.assertEqual(status.expires_at.tzinfo, UTC)

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

            status = await broker.seed_configured("codex")
            self.assertFalse(status.available)
            self.assertTrue(status.needs_signin)
            values = store.read("mainloop-control", broker.secret_name("codex"))
            self.assertEqual(values["injection-value"], "")
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
            status = await broker.seed_configured("claude")
            self.assertTrue(status.available)
            self.assertFalse(status.needs_signin)
            values = store.read("mainloop-control", broker.secret_name("claude"))
            self.assertEqual(values["injection-value"], "synthetic-claude-token")

            missing = CredentialBroker(
                store=store, claude_token_path=str(root / "missing-token")
            )
            with self.assertRaises(CredentialNeedsSignin) as raised:
                await missing.seed_configured("claude")
            self.assertEqual(str(raised.exception), "Claude needs sign-in")

        with tempfile.TemporaryDirectory() as directory:
            asyncio.run(exercise(Path(directory)))

    def test_reauth_result_methods_publish_only_valid_synthetic_fixture_values(self):
        async def exercise():
            store = MemorySecretStore()
            broker = CredentialBroker(store=store)
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


if __name__ == "__main__":
    unittest.main()
