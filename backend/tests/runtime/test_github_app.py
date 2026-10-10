"""Offline credential-boundary proofs with generated RSA and fake GitHub HTTP."""

import asyncio
import base64
import gzip
import json
import logging
import os
import unittest
from datetime import UTC, datetime, timedelta
from unittest.mock import patch

import httpx
import jwt
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.hazmat.primitives.serialization import (
    BestAvailableEncryption,
    Encoding,
    NoEncryption,
    PrivateFormat,
    PublicFormat,
)
from mainloop.config import Settings
from mainloop.runtime.policy import PolicyError
from mainloop.services import github_pr
from mainloop.services.github_auth import (
    GitHubAppAuth,
    GitHubError,
    GitHubNotFound,
    bounded_response,
    http_client,
)
from mainloop.services.github_creation import GitHubCreationClient
from mainloop.services.github_merge import GitHubMergeClient
from mainloop.services.github_sdk import RepositoryGitHub
from pydantic import SecretStr, ValidationError
from tests.runtime.github_app_fake import ENCODED_KEY, KEY, PEM, app_settings


class AppServer:
    def __init__(self):
        self.requests = []
        self.now = datetime.now(UTC)
        self.installed = True
        self.delay = 0
        self.mints = 0
        self.token_body = None

    async def handle(self, request):
        self.requests.append(request)
        if request.url.path.endswith("/installation"):
            return httpx.Response(200 if self.installed else 404, json={"id": 456})
        if request.url.path == "/app/installations/456/access_tokens":
            self.mints += 1
            await asyncio.sleep(self.delay)
            return httpx.Response(
                201,
                json=self.token_body
                or {
                    "token": f"installation-secret-{self.mints}",
                    "expires_at": (self.now + timedelta(hours=1)).isoformat(),
                },
            )
        return httpx.Response(
            200, json={"id": 1, "full_name": "owner/repo", "default_branch": "main"}
        )


class WireStream(httpx.AsyncByteStream):
    def __init__(self, payload, *, delay=0):
        self.payload = payload
        self.delay = delay

    async def __aiter__(self):
        await asyncio.sleep(self.delay)
        for offset in range(0, len(self.payload), 7):
            yield self.payload[offset : offset + 7]


def gzip_response(status, payload, *, chunked=False, delay=0):
    wire = gzip.compress(json.dumps(payload).encode())
    headers = {"Content-Encoding": "gzip", "ETag": '"gzip-fixture"'}
    if chunked:
        headers["Transfer-Encoding"] = "chunked"
    else:
        headers["Content-Length"] = str(len(wire))
    return httpx.Response(status, headers=headers, stream=WireStream(wire, delay=delay))


class ConfigurationTests(unittest.TestCase):
    def test_surrounding_ascii_whitespace_accepts_key_and_normalizes_app_id(self):
        for key_format in (PrivateFormat.TraditionalOpenSSL, PrivateFormat.PKCS8):
            encoded = base64.b64encode(
                KEY.private_bytes(Encoding.PEM, key_format, NoEncryption())
            ).decode()
            for prefix, suffix in (
                ("", "\n"),
                ("", "\r\n"),
                ("  ", "  "),
                (" \t\n\r\v\f", " \t\n\r\v\f"),
            ):
                with self.subTest(format=key_format, prefix=prefix, suffix=suffix):
                    auth = GitHubAppAuth(
                        prefix + "123" + suffix, SecretStr(prefix + encoded + suffix)
                    )
                    claims = jwt.decode(
                        auth._jwt(),
                        KEY.public_key(),
                        algorithms=["RS256"],
                        issuer="123",
                    )
                    self.assertEqual(claims["iss"], "123")

    def test_first_use_rejects_missing_malformed_non_rsa_and_encrypted_keys(self):
        ec_key = ec.generate_private_key(ec.SECP256R1()).private_bytes(
            Encoding.PEM, PrivateFormat.PKCS8, NoEncryption()
        )
        for encoded in (
            "",
            "secret-invalid-base64",
            " \t\n\r\v\f",
            ENCODED_KEY[:64] + "\n" + ENCODED_KEY[64:],
            " " + ENCODED_KEY[:64] + "\r\n" + ENCODED_KEY[64:] + " ",
            ENCODED_KEY[:64] + " " + ENCODED_KEY[64:],
            ENCODED_KEY[:64] + "\t" + ENCODED_KEY[64:],
            "\u00a0" + ENCODED_KEY + "\u00a0",
            base64.b64encode(b"secret-invalid-pem").decode(),
            base64.b64encode(ec_key).decode(),
            base64.b64encode(
                KEY.private_bytes(
                    Encoding.PEM,
                    PrivateFormat.PKCS8,
                    BestAvailableEncryption(b"test-only"),
                )
            ).decode(),
            base64.b64encode(
                KEY.public_key().public_bytes(
                    Encoding.PEM, PublicFormat.SubjectPublicKeyInfo
                )
            ).decode(),
        ):
            with self.subTest(key="redacted"), self.assertRaises(PolicyError) as error:
                GitHubAppAuth("123", SecretStr(encoded))
            self.assertEqual(error.exception.code, "configuration")
            for secret in (encoded, PEM.decode(), ENCODED_KEY):
                if secret:
                    self.assertNotIn(secret, str(error.exception))
        for app_id in (
            "",
            " \t\n\r\v\f",
            "zero-secret",
            "0",
            "-1",
            "12\n3",
            " 12 3 ",
            "\u00a0123\u00a0",
        ):
            with self.subTest(app_id=app_id), self.assertRaises(PolicyError) as error:
                GitHubAppAuth(app_id, SecretStr(ENCODED_KEY))
            self.assertEqual(error.exception.code, "configuration")
            if app_id:
                self.assertNotIn(app_id, str(error.exception))
            self.assertNotIn(ENCODED_KEY, str(error.exception))

    def test_settings_hide_key_and_have_no_pat_setting(self):
        configured = Settings(
            _env_file=None, github_app_id="123", github_app_private_key=ENCODED_KEY
        )
        self.assertNotIn(ENCODED_KEY, repr(configured))
        self.assertNotIn(ENCODED_KEY, configured.model_dump_json())
        self.assertNotIn("github_token", Settings.model_fields)
        with patch.dict(os.environ, {"AGENT_TOKEN_KEY": ""}), self.assertRaises(
            ValidationError
        ) as error:
            Settings(
                _env_file=None,
                github_app_private_key=ENCODED_KEY,
                git_transport_enabled=True,
            )
        self.assertNotIn(ENCODED_KEY, str(error.exception))


class AuthTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.server = AppServer()
        self.client = http_client(httpx.MockTransport(self.server.handle))
        self.addAsyncCleanup(self.client.aclose)
        self.auth = GitHubAppAuth("123", SecretStr(ENCODED_KEY))

    async def token(self, repository="owner/repo", permissions=None):
        return await self.auth.token(
            self.client, repository, permissions or {"checks": "read"}
        )

    async def test_jwt_signature_claims_installation_and_single_repo_scope(self):
        now = self.server.now.timestamp()
        with patch("mainloop.services.github_auth.time.time", return_value=now):
            token = await self.token()
        self.assertEqual(token, "installation-secret-1")
        lookup, mint = self.server.requests
        self.assertEqual(lookup.url.path, "/repos/owner/repo/installation")
        encoded = lookup.headers["Authorization"].removeprefix("Bearer ")
        claims = jwt.decode(
            encoded, KEY.public_key(), algorithms=["RS256"], issuer="123"
        )
        self.assertEqual(claims["iat"], int(now) - 60)
        self.assertEqual(claims["exp"], int(now) + 540)
        self.assertEqual(jwt.get_unverified_header(encoded)["alg"], "RS256")
        self.assertEqual(mint.headers["Authorization"], lookup.headers["Authorization"])
        self.assertEqual(mint.method, "POST")
        self.assertEqual(
            json.loads(mint.content),
            {"repositories": ["repo"], "permissions": {"checks": "read"}},
        )
        self.assertTrue(
            all(r.url.host == "api.github.com" for r in self.server.requests)
        )

    async def test_cache_shared_by_case_but_separates_repo_and_permission_set(self):
        self.assertEqual(await self.token(), await self.token("Owner/Repo"))
        self.assertEqual(self.server.mints, 1)
        await self.token(permissions={"contents": "read", "pull_requests": "read"})
        await self.token(permissions={"pull_requests": "read", "contents": "read"})
        self.assertEqual(self.server.mints, 2)
        await self.token("owner/other")
        self.assertEqual(self.server.mints, 3)
        self.assertEqual(
            json.loads(self.server.requests[-1].content)["repositories"], ["other"]
        )
        self.assertNotIn("installation-secret", repr(self.auth))
        self.assertNotIn("installation-secret", repr(self.auth._loops))

    async def test_refresh_at_safety_margin_and_failed_refresh_never_returns_old_token(
        self,
    ):
        now = self.server.now.timestamp()
        with patch("mainloop.services.github_auth.time.time", return_value=now):
            old = await self.token()
        with patch("mainloop.services.github_auth.time.time", return_value=now + 3539):
            self.assertEqual(await self.token(), old)
        self.server.now += timedelta(hours=1)
        with patch("mainloop.services.github_auth.time.time", return_value=now + 3540):
            self.assertNotEqual(await self.token(), old)
        self.server.installed = False
        with patch("mainloop.services.github_auth.time.time", return_value=now + 7140):
            with self.assertRaisesRegex(
                PolicyError, "GitHub App not installed on owner/repo"
            ):
                await self.token()
        self.assertEqual(self.server.mints, 2)

    async def test_concurrent_clients_share_one_mint(self):
        self.server.delay = 0.02
        tokens = await asyncio.gather(*(self.token() for _ in range(25)))
        self.assertEqual(len(set(tokens)), 1)
        self.assertEqual(len(self.server.requests), 2)
        self.assertEqual(self.server.mints, 1)

    async def test_invalid_expiry_and_token_errors_are_opaque(self):
        for expiry in (
            "secret-invalid-date",
            datetime.now().isoformat(),
            (self.server.now + timedelta(seconds=59)).isoformat(),
        ):
            self.server.token_body = {"token": "upstream-secret", "expires_at": expiry}
            with self.assertRaises(GitHubError) as error:
                await self.token()
            self.assertNotIn("upstream-secret", str(error.exception))
            self.assertNotIn(expiry, str(error.exception))

    async def test_auth_refusals_redirects_size_deadline_and_transport_are_opaque(self):
        async def bad_transport(request):
            raise httpx.ConnectError("transport-secret", request=request)

        for handler in (
            lambda r: httpx.Response(
                302, headers={"Location": "https://evil.invalid/"}
            ),
            lambda r: httpx.Response(403, text="upstream-secret"),
            lambda r: httpx.Response(200, content=b"x" * 2_000_001),
            lambda r: httpx.Response(200, json={"id": "secret-invalid-id"}),
            bad_transport,
        ):
            async with http_client(httpx.MockTransport(handler)) as client:
                with self.assertRaises(GitHubError) as error:
                    await self.auth.token(client, "owner/other", {"checks": "read"})
                self.assertEqual(str(error.exception), "")
        self.server.delay = 0.05
        with patch("mainloop.services.github_auth.REQUEST_TIMEOUT_SECONDS", 0.01):
            with self.assertRaises(GitHubError):
                await self.token()


class ClientTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        patcher = app_settings()
        patcher.start()
        self.addCleanup(patcher.stop)
        self.server = AppServer()
        self.transport = httpx.MockTransport(self.server.handle)

    async def test_every_client_refuses_cross_repo_or_other_origin_before_http(self):
        for client_type in (GitHubCreationClient, GitHubMergeClient):
            async with client_type("owner/repo", transport=self.transport) as client:
                for path in (
                    "/repos/owner/other",
                    "https://evil.invalid/repos/owner/repo",
                    "//evil.invalid/repos/owner/repo",
                    "/repos/owner/repository",
                    "/repos/owner/repo/../../other/repo",
                    "/repos/owner/repo/%2e%2e/%2e%2e/other/repo",
                    "/repos/owner/repo/%5c..%5cother",
                    "/repos/owner/repo?x=1",
                ):
                    with self.assertRaises(PolicyError):
                        await client._request("GET", path)
        sdk = RepositoryGitHub("owner/repo", transport=self.transport)
        with self.assertRaises(PolicyError):
            await sdk.rest.repos.async_get(owner="owner", repo="other")
        self.assertEqual(self.server.requests, [])

    async def test_minimum_permissions_for_creation_merge_and_monitoring(self):
        sdk = RepositoryGitHub("owner/repo", transport=self.transport)
        for method, suffix, permissions in (
            ("GET", "", {"metadata": "read"}),
            ("GET", "/branches/main", {"contents": "read"}),
            ("GET", "/branches/main/protection", {"administration": "read"}),
            ("GET", "/rules/branches/main", {"metadata": "read"}),
            ("GET", "/pulls/17", {"pull_requests": "read"}),
            ("POST", "/pulls", {"pull_requests": "write", "contents": "read"}),
            ("GET", "/commits/abc/check-suites", {"checks": "read"}),
            ("GET", "/commits/abc/check-runs", {"checks": "read"}),
            ("GET", "/commits/abc/statuses", {"statuses": "read"}),
            ("GET", "/compare/abc...def", {"contents": "read"}),
            ("PUT", "/pulls/17/merge", {"contents": "write"}),
            ("GET", "/issues/17/comments", {"pull_requests": "read"}),
            ("POST", "/pulls/comments/17/reactions", {"pull_requests": "write"}),
            ("GET", "/pulls/comments/17/reactions", {"pull_requests": "read"}),
            ("GET", "/issues/17", {"issues": "read"}),
            ("POST", "/issues", {"issues": "write"}),
        ):
            await sdk.arequest(
                method, "/repos/owner/repo" + suffix, response_model=dict
            )
            mint = next(
                r
                for r in self.server.requests
                if r.url.path.endswith("/access_tokens")
                and json.loads(r.content)["permissions"] == permissions
            )
            self.assertEqual(
                json.loads(mint.content),
                {"repositories": ["repo"], "permissions": permissions},
            )
            self.assertTrue(
                self.server.requests[-1]
                .headers["Authorization"]
                .startswith("Bearer installation-secret-")
            )

    async def test_issue_comment_reactions_refuse_before_http(self):
        sdk = RepositoryGitHub("owner/repo", transport=self.transport)
        calls = (
            lambda: sdk.rest.reactions.async_list_for_issue_comment(
                owner="owner", repo="repo", comment_id=17
            ),
            lambda: sdk.rest.reactions.async_create_for_issue_comment(
                owner="owner", repo="repo", comment_id=17, content="eyes"
            ),
        )
        with patch.object(github_pr, "_get_github", lambda repo: sdk):
            calls += (
                lambda: github_pr.get_comment_reactions("owner/repo", 17),
                lambda: github_pr.add_reaction_to_comment("owner/repo", 17),
            )
            for call in calls:
                with self.assertRaisesRegex(
                    PolicyError, "issue-comment reactions require Issues permissions"
                ):
                    await call()
        self.assertEqual(self.server.requests, [])

    async def test_administration_mint_failure_never_proves_absent_protection(self):
        from tests.runtime.test_merge import SHA, GitHub

        for status in (201, 404, 401, 403, 429, 500):
            fake = GitHub()
            fake.protection = {
                "required_status_checks": {
                    "strict": False,
                    "contexts": ["required-extra"],
                    "checks": [{"context": "required-extra", "app_id": 4}],
                }
            }
            server = AppServer()

            async def handle(request, server=server, status=status, fake=fake):
                if request.url.path.endswith("/installation"):
                    return await server.handle(request)
                if request.url.path.endswith("/access_tokens"):
                    if (
                        json.loads(request.content)["permissions"]
                        == {"administration": "read"}
                        and status != 201
                    ):
                        return httpx.Response(
                            status, json={"message": "secret-auth-error"}
                        )
                    return await server.handle(request)
                return await fake.handle(request)

            auth = GitHubAppAuth("123", SecretStr(ENCODED_KEY))
            with self.subTest(mint_status=status), patch(
                "mainloop.services.github_creation.app_auth", return_value=auth
            ):
                async with GitHubMergeClient(
                    "owner/repo", transport=httpx.MockTransport(handle)
                ) as client:
                    if status == 201:
                        facts = await client.evidence("owner/repo", 17, SHA)
                        self.assertTrue(facts["ci"]["complete"])
                        self.assertFalse(facts["ci"]["green"])
                        self.assertTrue(facts["ci"]["required"])
                    else:
                        with self.assertRaises(GitHubError) as error:
                            await client.evidence("owner/repo", 17, SHA)
                        self.assertNotIsInstance(error.exception, GitHubNotFound)
                        self.assertEqual(str(error.exception), "")
                        self.assertFalse(fake.puts)
                protection_called = any(
                    r.url.path.endswith("/protection") for r in fake.calls
                )
                self.assertEqual(protection_called, status == 201)

    async def test_gzip_lookup_mint_creation_and_githubkit_reads(self):
        for phase in ("lookup", "mint", "creation", "githubkit"):
            requests = []

            def handle(request, requests=requests, phase=phase):
                requests.append(request)
                if request.url.path.endswith("/installation"):
                    status, payload, current = 200, {"id": 456}, "lookup"
                elif request.url.path.endswith("/access_tokens"):
                    status, payload, current = (
                        201,
                        {
                            "token": "gzip-token-secret",
                            "expires_at": (
                                datetime.now(UTC) + timedelta(hours=1)
                            ).isoformat(),
                        },
                        "mint",
                    )
                else:
                    status, current = 200, phase
                    payload = (
                        []
                        if phase == "githubkit"
                        else {
                            "id": 1,
                            "full_name": "owner/repo",
                            "default_branch": "main",
                        }
                    )
                return (
                    gzip_response(status, payload, chunked=phase == "githubkit")
                    if current == phase
                    else httpx.Response(status, json=payload)
                )

            auth = GitHubAppAuth("123", SecretStr(ENCODED_KEY))
            transport = httpx.MockTransport(handle)
            with self.subTest(phase=phase), patch(
                "mainloop.services.github_creation.app_auth", return_value=auth
            ):
                if phase == "githubkit":
                    sdk = RepositoryGitHub("owner/repo", transport=transport)
                    response = await sdk.rest.pulls.async_list(
                        owner="owner", repo="repo"
                    )
                    self.assertEqual(response.parsed_data, [])
                    self.assertNotIn("content-encoding", response.headers)
                    self.assertNotIn("transfer-encoding", response.headers)
                    self.assertEqual(
                        response.headers["content-length"], str(len(response.content))
                    )
                    self.assertEqual(response.headers["etag"], '"gzip-fixture"')
                    with self.assertRaises(RuntimeError):
                        _ = response.raw_request
                else:
                    async with GitHubCreationClient(
                        "owner/repo", transport=transport
                    ) as client:
                        self.assertEqual(
                            (await client.repo("owner/repo")).full_name, "owner/repo"
                        )
                self.assertEqual(len(requests), 3)
                self.assertEqual(
                    requests[-1].headers["Authorization"], "Bearer gzip-token-secret"
                )

    async def test_gzip_decoded_size_limit_and_deadline_remain_enforced(self):
        large = gzip_response(200, {"padding": "x" * 2_000_001})
        self.assertLess(len(large.stream.payload), 2_000_000)
        for response, deadline in (
            (large, 15),
            (gzip_response(200, {"id": 456}, delay=0.05), 0.01),
        ):
            async with http_client(
                httpx.MockTransport(lambda request, response=response: response)
            ) as client:
                with patch(
                    "mainloop.services.github_auth.REQUEST_TIMEOUT_SECONDS", deadline
                ):
                    with self.assertRaises(GitHubError):
                        await bounded_response(
                            client, "GET", "/repos/owner/repo/installation"
                        )

    async def test_decoded_response_has_fresh_framing_and_no_request(self):
        payload = {"padding": "gzip data " * 100}
        for chunked in (False, True):
            wire_response = gzip_response(200, payload, chunked=chunked)
            async with http_client(
                httpx.MockTransport(
                    lambda request, wire_response=wire_response: wire_response
                )
            ) as client:
                response = await bounded_response(
                    client,
                    "GET",
                    "/repos/owner/repo",
                    headers={"Authorization": "Bearer stream-test-secret"},
                )
            self.assertEqual(response.json(), payload)
            self.assertNotIn("content-encoding", response.headers)
            self.assertNotIn("transfer-encoding", response.headers)
            self.assertEqual(
                response.headers["content-length"], str(len(response.content))
            )
            self.assertEqual(response.headers["etag"], '"gzip-fixture"')
            if not chunked:
                self.assertNotEqual(
                    response.headers["content-length"],
                    wire_response.headers["content-length"],
                )
            with self.assertRaises(RuntimeError):
                _ = response.request

    async def test_concurrent_repository_clients_share_one_mint(self):
        self.server.delay = 0.02

        async def read():
            async with GitHubCreationClient(
                "owner/repo", transport=self.transport
            ) as client:
                return await client.repo("owner/repo")

        results = await asyncio.gather(*(read() for _ in range(20)))
        self.assertEqual(len(results), 20)
        self.assertEqual(self.server.mints, 1)
        self.assertEqual(
            sum(r.url.path.endswith("/installation") for r in self.server.requests), 1
        )

    async def test_not_installed_propagates_through_monitoring_and_no_product_write(
        self,
    ):
        self.server.installed = False
        with patch.object(
            github_pr,
            "_get_github",
            lambda repo: RepositoryGitHub(repo, transport=self.transport),
        ):
            for call in (
                github_pr.get_pr_status("owner/repo", 17),
                github_pr.add_reaction_to_comment(
                    "owner/repo", 17, is_review_comment=True
                ),
            ):
                with self.assertRaisesRegex(
                    PolicyError, "GitHub App not installed on owner/repo"
                ):
                    await call
        self.assertTrue(
            all(r.url.path.endswith("/installation") for r in self.server.requests)
        )

    async def test_upstream_secrets_never_enter_monitoring_logs(self):
        original = self.server.handle

        async def handle(request):
            if request.url.path.endswith("/reactions"):
                raise httpx.ReadTimeout(
                    "installation-secret-1 " + ENCODED_KEY, request=request
                )
            return await original(request)

        with patch.object(
            github_pr,
            "_get_github",
            lambda repo: RepositoryGitHub(repo, transport=httpx.MockTransport(handle)),
        ):
            with self.assertLogs(github_pr.logger, logging.WARNING) as logs:
                self.assertFalse(
                    await github_pr.add_reaction_to_comment(
                        "owner/repo", 17, is_review_comment=True
                    )
                )
        combined = "\n".join(logs.output)
        for secret in (ENCODED_KEY, PEM.decode(), "installation-secret-1"):
            self.assertNotIn(secret, combined)
