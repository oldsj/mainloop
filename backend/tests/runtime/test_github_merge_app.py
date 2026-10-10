"""Offline merge proofs with GitHub's permission-dependent repository payload."""

import json
import unittest
from datetime import UTC, datetime, timedelta
from types import SimpleNamespace
from unittest.mock import AsyncMock, MagicMock, patch

import httpx
from mainloop.runtime.policy import PolicyError
from mainloop.services import merge
from mainloop.services.github_auth import GitHubAppAuth
from mainloop.services.github_creation import GitHubCreationClient
from mainloop.services.github_merge import GitHubMergeClient
from pydantic import SecretStr, ValidationError
from tests.runtime.github_app_fake import ENCODED_KEY, PEM
from tests.runtime.test_merge import BASE, SHA, TEST_MERGE, GitHub


class ScopedAppServer:
    """Sanitized fixture, not a live capture. Metadata omits merge settings."""

    def __init__(self):
        self.product = GitHub()
        self.requests = []
        self.tokens = {}
        self.omit_settings = False
        self.override = None

    async def handle(self, request):
        self.requests.append(request)
        path = request.url.path
        if path == "/repos/owner/repo/installation":
            return httpx.Response(200, json={"id": 456})
        if path == "/app/installations/456/access_tokens":
            body = json.loads(request.content)
            if body["repositories"] != ["repo"]:
                raise AssertionError("token must be narrowed to the repository")
            token = f"permission-secret-{len(self.tokens)}"
            self.tokens[token] = body["permissions"]
            return httpx.Response(
                201,
                json={
                    "token": token,
                    "expires_at": (datetime.now(UTC) + timedelta(hours=1)).isoformat(),
                },
            )
        permissions = self.tokens[
            request.headers["Authorization"].removeprefix("Bearer ")
        ]
        if self.override:
            response = self.override(request)
            if response is not None:
                return response
        if path == "/repos/owner/repo":
            payload = dict(self.product.repo)
            if permissions != {"contents": "write"} or self.omit_settings:
                payload.pop("allow_squash_merge")
            return httpx.Response(200, json=payload)
        required = {
            "/repos/owner/repo/pulls/17": {"pull_requests": "read"},
            "/repos/owner/repo/pulls/17/files": {"pull_requests": "read"},
            "/repos/owner/repo/branches/main": {"contents": "read"},
            f"/repos/owner/repo/commits/{SHA}/check-suites": {"checks": "read"},
            f"/repos/owner/repo/commits/{SHA}/check-runs": {"checks": "read"},
            f"/repos/owner/repo/commits/{SHA}/statuses": {"statuses": "read"},
            "/repos/owner/repo/branches/main/protection": {"administration": "read"},
            "/repos/owner/repo/rules/branches/main": {"metadata": "read"},
            f"/repos/owner/repo/commits/{TEST_MERGE}": {"contents": "read"},
            f"/repos/owner/repo/compare/{BASE}...{TEST_MERGE}": {"contents": "read"},
        }[path]
        if permissions != required:
            raise AssertionError("evidence endpoint received the wrong permission set")
        response = await self.product.handle(request)
        if path == "/repos/owner/repo/pulls/17" and response.status_code == 200:
            payload = response.json()
            for ref in ("head", "base"):
                payload[ref]["repo"].pop("allow_squash_merge")
            return httpx.Response(200, json=payload)
        return response


class MergeAppEvidenceTests(unittest.IsolatedAsyncioTestCase):
    def client(self, server, client_type=GitHubMergeClient):
        auth = GitHubAppAuth("123", SecretStr(ENCODED_KEY))
        with patch("mainloop.services.github_creation.app_auth", return_value=auth):
            return client_type(
                "owner/repo", transport=httpx.MockTransport(server.handle)
            )

    async def test_metadata_only_payload_reproduces_exact_three_request_failure(self):
        server = ScopedAppServer()
        # Reproduce the pre-repair permission selection without weakening validation.
        with patch.object(
            GitHubMergeClient, "_permissions", GitHubCreationClient._permissions
        ):
            async with self.client(server) as client:
                with self.assertRaises(ValidationError) as error:
                    await client.evidence("owner/repo", 17, SHA)
        self.assertEqual(error.exception.errors()[0]["loc"], ("allow_squash_merge",))
        self.assertEqual(
            [(r.method, r.url.path) for r in server.requests],
            [
                ("GET", "/repos/owner/repo/installation"),
                ("POST", "/app/installations/456/access_tokens"),
                ("GET", "/repos/owner/repo"),
            ],
        )
        self.assertEqual(list(server.tokens.values()), [{"metadata": "read"}])

    async def test_merge_settings_and_all_other_evidence_use_required_permissions(self):
        server = ScopedAppServer()
        async with self.client(server) as client:
            facts = await client.evidence("owner/repo", 17, SHA)
        self.assertTrue(facts["ci"]["complete"])
        self.assertTrue(facts["ci"]["green"])
        repo_reads = [r for r in server.requests if r.url.path == "/repos/owner/repo"]
        self.assertEqual(len(repo_reads), 2)
        for request in repo_reads:
            token = request.headers["Authorization"].removeprefix("Bearer ")
            self.assertEqual(server.tokens[token], {"contents": "write"})
        self.assertEqual(
            {tuple(p.items()) for p in server.tokens.values()},
            {
                (("contents", "write"),),
                (("contents", "read"),),
                (("pull_requests", "read"),),
                (("checks", "read"),),
                (("statuses", "read"),),
                (("administration", "read"),),
                (("metadata", "read"),),
            },
        )
        self.assertTrue(all(r.method in ("GET", "POST") for r in server.requests))
        self.assertFalse(server.product.puts)

    async def test_creation_repository_read_keeps_metadata_permission(self):
        server = ScopedAppServer()
        async with self.client(server, GitHubCreationClient) as client:
            repo = await client.repo("owner/repo")
        self.assertEqual(repo.id, 123)
        self.assertEqual(list(server.tokens.values()), [{"metadata": "read"}])

    async def read_evidence(self, server):
        client = self.client(server)
        connection = MagicMock()
        connection.__aenter__ = AsyncMock(return_value=None)
        connection.__aexit__ = AsyncMock(return_value=False)
        with (
            patch.object(merge.db, "connection", return_value=connection),
            patch.object(
                merge, "authority", AsyncMock(return_value=({}, "owner/repo"))
            ),
            patch.object(merge, "GitHubMergeClient", return_value=client),
        ):
            return await merge.read_evidence(
                {},
                SimpleNamespace(project_id="project", pr_number=17, expected_sha=SHA),
            )

    def assert_safe_refusal(self, logs, error, step, exception, server):
        self.assertEqual(error.code, "github")
        self.assertEqual(str(error), "complete GitHub merge evidence unavailable")
        self.assertEqual(
            [record.getMessage() for record in logs.records],
            [f"GitHub merge evidence unavailable: step={step} exception={exception}"],
        )
        text = str(error) + " ".join(logs.output)
        for secret in (ENCODED_KEY, PEM.decode(), "upstream-secret", *server.tokens):
            self.assertNotIn(secret, text)
        for request in server.requests:
            self.assertNotIn(request.headers.get("Authorization", "missing"), text)
        self.assertTrue(all(record.exc_info is None for record in logs.records))
        self.assertFalse(server.product.puts)

    async def test_missing_merge_setting_still_refuses_and_logs_validation_step(self):
        server = ScopedAppServer()
        server.omit_settings = True
        with (
            self.assertLogs("mainloop.services.merge", level="WARNING") as logs,
            self.assertRaises(PolicyError) as error,
        ):
            await self.read_evidence(server)
        self.assert_safe_refusal(
            logs, error.exception, "repository", "ValidationError", server
        )
        self.assertEqual(len(server.requests), 3)
        self.assertEqual(list(server.tokens.values()), [{"contents": "write"}])

    async def test_disabled_squash_merge_still_refuses(self):
        server = ScopedAppServer()
        server.product.repo["allow_squash_merge"] = False
        async with self.client(server) as client:
            with self.assertRaisesRegex(PolicyError, "squash enabled"):
                await client.evidence("owner/repo", 17, SHA)
        self.assertFalse(server.product.puts)

    async def test_each_unavailable_evidence_read_logs_its_step_without_body(self):
        for suffix, step in (
            ("/pulls/17", "pull_request"),
            ("/branches/main", "branch"),
            ("/pulls/17/files", "files"),
            (f"/commits/{SHA}/check-suites", "check_suites"),
            (f"/commits/{SHA}/check-runs", "check_runs"),
            (f"/commits/{SHA}/statuses", "statuses"),
            ("/branches/main/protection", "protection"),
            ("/rules/branches/main", "rules"),
            (f"/commits/{TEST_MERGE}", "merge_result"),
            (f"/compare/{BASE}...{TEST_MERGE}", "merge_result"),
        ):
            server = ScopedAppServer()
            server.product.errors[suffix] = (403, {"message": "upstream-secret"})
            with (
                self.subTest(step=step),
                self.assertLogs("mainloop.services.merge", level="WARNING") as logs,
                self.assertRaises(PolicyError) as error,
            ):
                await self.read_evidence(server)
            self.assert_safe_refusal(logs, error.exception, step, "GitHubError", server)

    async def test_refresh_validation_failure_logs_the_correct_read(self):
        for path, step, field in (
            ("/repos/owner/repo", "repository_refresh", "allow_squash_merge"),
            ("/repos/owner/repo/pulls/17", "pull_request_refresh", "mergeable"),
        ):
            server = ScopedAppServer()
            seen = []

            def override(request, path=path, field=field, server=server, seen=seen):
                if request.url.path == path:
                    seen.append(request)
                    if len(seen) == 2:
                        payload = dict(
                            server.product.repo
                            if field == "allow_squash_merge"
                            else server.product.pr
                        )
                        payload.pop(field)
                        return httpx.Response(200, json=payload)
                return None

            server.override = override
            with (
                self.subTest(step=step),
                self.assertLogs("mainloop.services.merge", level="WARNING") as logs,
                self.assertRaises(PolicyError) as error,
            ):
                await self.read_evidence(server)
            self.assert_safe_refusal(
                logs, error.exception, step, "ValidationError", server
            )

    async def test_client_setup_error_is_opaque_and_logged_without_exception_text(self):
        connection = MagicMock()
        connection.__aenter__ = AsyncMock(return_value=None)
        connection.__aexit__ = AsyncMock(return_value=False)
        with (
            patch.object(merge.db, "connection", return_value=connection),
            patch.object(
                merge, "authority", AsyncMock(return_value=({}, "owner/repo"))
            ),
            patch.object(
                merge, "GitHubMergeClient", side_effect=ValueError("upstream-secret")
            ),
            self.assertLogs("mainloop.services.merge", level="WARNING") as logs,
            self.assertRaises(PolicyError) as error,
        ):
            await merge.read_evidence(
                {},
                SimpleNamespace(project_id="project", pr_number=17, expected_sha=SHA),
            )
        self.assert_safe_refusal(
            logs, error.exception, "client", "ValueError", ScopedAppServer()
        )
