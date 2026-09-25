"""Credential re-auth runner tests use fake state and sanitized challenge logs only."""

from __future__ import annotations

import asyncio
import json
import unittest
from unittest.mock import patch

from mainloop.config import settings
from mainloop.runtime import credential_reauth_api
from mainloop.runtime.credential_reauth import (
    FakeCredentialReauthRunner,
    KubernetesCredentialReauthRunner,
    _challenge_from_logs,
)
from starlette.requests import Request


async def _run_sync_in_test(function, *args, **kwargs):
    """Keep fake Kubernetes API calls deterministic in tests."""
    await asyncio.sleep(0)
    return function(*args, **kwargs)


class CredentialReauthTests(unittest.TestCase):
    def setUp(self):
        self._to_thread_patch = patch.object(
            asyncio, "to_thread", new=_run_sync_in_test
        )
        self._to_thread_patch.start()

    def tearDown(self):
        self._to_thread_patch.stop()

    def test_fake_runner_keeps_callback_token_private_and_completes(self):
        async def exercise():
            runner = FakeCredentialReauthRunner()
            job = await runner.start("codex", owner="fixture-user")
            self.assertEqual(job.provider, "codex")
            self.assertEqual(job.state, "running")
            self.assertIsNone(await runner.callback_provider(job.id, "wrong-token"))

            token = runner._jobs[job.id][1]
            self.assertEqual(await runner.callback_provider(job.id, token), "codex")
            await runner.complete(job.id, token)
            completed = await runner.status(job.id)
            self.assertEqual(completed.state, "completed")
            self.assertNotIn(token, repr(completed))

        asyncio.run(exercise())

    def test_kubernetes_runner_builds_a_bounded_non_root_job(self):
        class BatchApi:
            job = None

            def create_namespaced_job(self, namespace, body):
                self.job = body

        async def exercise():
            batch = BatchApi()
            runner = KubernetesCredentialReauthRunner(
                batch_api=batch, core_api=object()
            )
            with patch.object(settings, "substrate_reauth_job_image", "fixture-image"):
                job = await runner.start("codex")
            spec = batch.job.spec
            container = spec.template.spec.containers[0]
            self.assertEqual(job.state, "running")
            self.assertEqual(spec.backoff_limit, 0)
            self.assertGreater(spec.active_deadline_seconds, 0)
            self.assertTrue(spec.template.spec.automount_service_account_token is False)
            self.assertTrue(container.security_context.run_as_non_root)
            self.assertEqual(
                container.command, ["node", "/usr/local/bin/mainloop-reauth"]
            )
            token = next(
                env.value
                for env in container.env
                if env.name == "MAINLOOP_REAUTH_CALLBACK_TOKEN"
            )
            self.assertNotIn(token, repr(job))
            self.assertEqual(await runner.callback_provider(job.id, token), "codex")
            await runner.complete(job.id, token)
            self.assertIsNone(await runner.callback_provider(job.id, token))

        asyncio.run(exercise())

    def test_callback_authenticates_job_before_storing_credential(self):
        class MemoryBroker:
            stored = None

            async def store_codex_auth_document(self, raw):
                self.stored = raw

            async def store_claude_token(self, token):
                raise AssertionError("unexpected provider")

        async def exercise():
            runner = FakeCredentialReauthRunner()
            job = await runner.start("codex")
            token = runner._jobs[job.id][1]
            broker = MemoryBroker()
            payload = {"provider": "codex", "auth": {"fixture": "synthetic-auth"}}
            raw = json.dumps(payload).encode()
            delivered = False

            async def receive():
                nonlocal delivered
                if delivered:
                    return {"type": "http.request", "body": b"", "more_body": False}
                delivered = True
                return {"type": "http.request", "body": raw, "more_body": False}

            request = Request(
                {"type": "http", "method": "POST", "headers": []}, receive
            )
            with (
                patch.object(credential_reauth_api, "_reauth_runner", runner),
                patch.object(
                    credential_reauth_api, "CredentialBroker", return_value=broker
                ),
            ):
                response = await credential_reauth_api.receive_reauth_result(
                    job.id, request, token
                )
            self.assertEqual(response.status_code, 204)
            self.assertEqual(
                broker.stored,
                json.dumps(payload["auth"], separators=(",", ":")).encode(),
            )
            self.assertEqual((await runner.status(job.id)).state, "completed")

        asyncio.run(exercise())

    def test_only_https_challenges_from_allowed_provider_hosts_are_returned(self):
        good = _challenge_from_logs(
            "noise\nMAINLOOP_REAUTH_CHALLENGE "
            '{"url":"https://auth.openai.com/codex/device","code":"ABCD-EFGH"}\n'
        )
        self.assertEqual(good.url, "https://auth.openai.com/codex/device")
        self.assertEqual(good.code, "ABCD-EFGH")
        self.assertIsNone(
            _challenge_from_logs(
                "MAINLOOP_REAUTH_CHALLENGE "
                '{"url":"http://auth.openai.com/","code":"ABCD-EFGH"}'
            )
        )
        self.assertIsNone(
            _challenge_from_logs(
                "MAINLOOP_REAUTH_CHALLENGE "
                '{"url":"https://attacker.example/","code":"ABCD-EFGH"}'
            )
        )
        self.assertIsNone(
            _challenge_from_logs(
                "MAINLOOP_REAUTH_CHALLENGE "
                '{"url":"https://auth.openai.com/","code":"secret-token"}'
            )
        )


if __name__ == "__main__":
    unittest.main()
