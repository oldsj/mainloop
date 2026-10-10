"""Offline ref resolution through the actual repository-scoped App client."""

import json
import unittest
from functools import partial
from unittest.mock import patch

import httpx
from mainloop.services import github_checkout
from mainloop.services.github_creation import GitHubCreationClient
from tests.runtime.github_app_fake import app_settings
from tests.runtime.test_github_app import AppServer

SHA = "a" * 40


class CheckoutServer(AppServer):
    def __init__(self):
        super().__init__()
        self.sha, self.status = SHA, 200
        self.repository = "owner/repo"
        self.default_branch, self.repo_status = "trunk", 200

    async def handle(self, request):
        if "/commits/" in request.url.path:
            self.requests.append(request)
            return httpx.Response(self.status, json={"sha": self.sha})
        path = request.url.path
        if path.startswith("/repos/owner/") and path.count("/") == 3:
            self.requests.append(request)
            name = path.removeprefix("/repos/")
            return httpx.Response(
                self.repo_status,
                json={
                    "id": 1,
                    "full_name": self.repository if name == "owner/repo" else name,
                    "default_branch": self.default_branch,
                },
            )
        return await super().handle(request)


class CheckoutResolutionTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.server = CheckoutServer()
        for patcher in (
            app_settings(),
            patch.object(
                github_checkout,
                "GitHubCreationClient",
                partial(
                    GitHubCreationClient,
                    transport=httpx.MockTransport(self.server.handle),
                ),
            ),
        ):
            patcher.start()
            self.addCleanup(patcher.stop)

    async def test_default_branch_branch_tag_and_sha_resolve_through_contents_read(
        self,
    ):
        for ref in ("", "feature/topic", "v1.2.3", SHA, "HEAD"):
            with self.subTest(ref=ref):
                start = len(self.server.requests)
                self.assertEqual(
                    await github_checkout.resolve_checkout_ref("Owner/Repo", ref), SHA
                )
                commits = [
                    r for r in self.server.requests[start:] if "/commits/" in r.url.path
                ]
                self.assertEqual(len(commits), 1)
                self.assertEqual(
                    commits[0].url.path, f"/repos/owner/repo/commits/{ref or 'trunk'}"
                )
                if ref == "feature/topic":
                    self.assertTrue(
                        commits[0].url.raw_path.endswith(b"feature%2Ftopic")
                    )
                mint = [
                    r
                    for r in self.server.requests
                    if r.url.path.endswith("/access_tokens")
                ]
                self.assertIn(
                    {"contents": "read"},
                    [json.loads(r.content)["permissions"] for r in mint],
                )
                self.assertTrue(
                    all(r.method in ("GET", "POST") for r in self.server.requests)
                )
                self.assertTrue(
                    all(r.url.host == "api.github.com" for r in self.server.requests)
                )

    async def test_unresolvable_and_invalid_commit_responses_refuse(self):
        for status, sha in (
            (404, SHA),
            (403, SHA),
            (503, SHA),
            (200, "main"),
            (200, "b" * 39),
        ):
            with self.subTest(status=status, sha=sha):
                self.server.status, self.server.sha = status, sha
                with self.assertRaisesRegex(
                    github_checkout.CheckoutRefUnavailable, "could not be resolved"
                ):
                    await github_checkout.resolve_checkout_ref("owner/repo", "missing")

    async def test_default_branch_repository_mismatch_refuses_before_commit_lookup(
        self,
    ):
        self.server.repository = "other/repo"
        with self.assertRaises(github_checkout.CheckoutRefUnavailable):
            await github_checkout.resolve_checkout_ref("owner/repo", "")
        self.assertFalse(any("/commits/" in r.url.path for r in self.server.requests))

    async def test_missing_installation_is_an_owner_safe_refusal(self):
        self.server.installed = False
        with self.assertRaisesRegex(
            github_checkout.CheckoutRefUnavailable, "could not be resolved"
        ):
            await github_checkout.resolve_checkout_ref("owner/repo", SHA)

    async def test_endpoint_shaped_branch_suffixes_still_use_contents_read(self):
        for ref in ("feature/statuses", "feature/check-runs", "feature/check-suites"):
            self.assertEqual(
                await github_checkout.resolve_checkout_ref("owner/repo", ref), SHA
            )
        permissions = [
            json.loads(r.content)["permissions"]
            for r in self.server.requests
            if r.url.path.endswith("/access_tokens")
        ]
        self.assertEqual(permissions, [{"contents": "read"}])

    async def test_default_branch_reads_the_repository_through_the_app(self):
        self.assertEqual(
            await github_checkout.resolve_default_branch("Owner/Repo"), "trunk"
        )
        self.assertIn("/repos/owner/repo", [r.url.path for r in self.server.requests])
        self.assertFalse(any("/commits/" in r.url.path for r in self.server.requests))
        self.assertTrue(all(r.method in ("GET", "POST") for r in self.server.requests))

    async def test_unknown_default_branch_refuses_and_never_guesses(self):
        cases = (
            ("repository", "other/repo"),
            ("default_branch", ""),
            ("repo_status", 404),
            ("repo_status", 503),
        )
        for field, value in cases:
            with self.subTest(field=field, value=value):
                self.server = CheckoutServer()
                setattr(self.server, field, value)
                with (
                    patch.object(
                        github_checkout,
                        "GitHubCreationClient",
                        partial(
                            GitHubCreationClient,
                            transport=httpx.MockTransport(self.server.handle),
                        ),
                    ),
                    self.assertRaisesRegex(
                        github_checkout.DefaultBranchUnavailable, "could not be read"
                    ),
                ):
                    await github_checkout.resolve_default_branch("owner/repo")
