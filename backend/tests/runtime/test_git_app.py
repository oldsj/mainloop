"""Production Git sidecar wiring with fake authority, GitHub and server I/O."""

import asyncio
import base64
import io
import json
import logging
import os
import socket
import stat
import tempfile
import unittest
from contextlib import asynccontextmanager
from datetime import UTC, datetime, timedelta
from pathlib import Path
from unittest.mock import AsyncMock, Mock, patch

import httpx
from httpcore._backends.mock import AsyncMockBackend, AsyncMockStream
from mainloop import git_app
from mainloop.config import settings
from mainloop.push_gate.protocol import Limits, TransportError, pkt
from mainloop.push_gate.transport import DispatchProof
from mainloop.push_gate.upstream import GitHubAppUpstream, LoopbackFixture
from mainloop.runtime.policy import PolicyError
from mainloop.services import github_auth
from pydantic import SecretStr
from tests.runtime.github_app_fake import ENCODED_KEY
from tests.runtime.test_push_transport_git import (
    PUSH,
    READ,
    FixtureAuthority,
    fixture_root,
)
from tests.runtime.test_push_transport_http import asgi


class Authority(FixtureAuthority):
    def seed_upstream(self, binding, upstream):
        return upstream


class AppUpstreamFixture:
    async def asyncSetUp(self):
        self.mints = []
        self.auth_requests = []
        self.git_requests = []
        self.auth_status = 201
        self.installation_status = 200
        self.git_error = None
        self.reflection = None
        self.auth = github_auth.GitHubAppAuth("123", SecretStr(ENCODED_KEY))
        self.logs = io.StringIO()
        self.handler = logging.StreamHandler(self.logs)
        self.logger = logging.getLogger()
        self.previous_level = self.logger.level
        self.logger.setLevel(logging.DEBUG)
        self.logger.addHandler(self.handler)
        client_factory = github_auth.http_client
        self.patch = patch(
            "mainloop.push_gate.upstream.http_client",
            side_effect=lambda: client_factory(httpx.MockTransport(self.auth_request)),
        )
        self.patch.start()

    async def asyncTearDown(self):
        self.patch.stop()
        self.logger.removeHandler(self.handler)
        self.logger.setLevel(self.previous_level)

    def auth_request(self, request):
        self.auth_requests.append(request.url.path)
        if request.url.path.endswith("/installation"):
            return httpx.Response(self.installation_status, json={"id": 456})
        payload = json.loads(request.content)
        self.mints.append(payload)
        return httpx.Response(
            self.auth_status,
            json={
                "token": f"fixture-app-{payload['permissions']['contents']}-{payload['repositories'][0]}",
                "expires_at": (datetime.now(UTC) + timedelta(hours=1)).isoformat(),
            },
        )

    def git_request(self, request):
        self.git_requests.append(request)
        if self.git_error:
            raise httpx.ConnectError(self.git_error, request=request)
        discovery = request.url.path.endswith("/info/refs")
        service = (
            request.url.params["service"]
            if discovery
            else request.url.path.rsplit("/", 1)[1]
        )
        data = self.reflection or (
            pkt(f"# service={service}\n".encode()) + b"00000000"
            if discovery
            else b"fixture-result"
        )
        return httpx.Response(
            200,
            stream=httpx.ByteStream(data),
            headers={
                "content-type": f"application/x-{service}-{'advertisement' if discovery else 'result'}"
            },
        )

    def upstream(self, repository="Owner/Repo"):
        return GitHubAppUpstream(
            repository,
            Limits(),
            auth=self.auth,
            fixture=LoopbackFixture(
                "http://127.0.0.1:19876",
                lambda: httpx.MockTransport(self.git_request),
            ),
        )

    def assert_no_token(self, value):
        for permission in ("read", "write"):
            token = f"fixture-app-{permission}-repo".encode()
            variants = (
                token,
                base64.b64encode(token),
                base64.b64encode(b"x-access-token:" + token),
            )
            for variant in variants:
                self.assertNotIn(variant.decode(), value)


class AppUpstreamTests(AppUpstreamFixture, unittest.IsolatedAsyncioTestCase):
    async def test_operation_permissions_repository_scope_and_shared_cache(self):
        upstream = self.upstream()
        self.assertFalse(self.auth_requests)
        await upstream.discovery("git-upload-pack")
        await upstream.upload_pack(b"fixture-body")
        await upstream.discovery("git-receive-pack")
        with tempfile.TemporaryDirectory() as directory:
            body = Path(directory) / "receive"
            body.write_bytes(b"fixture-pack")
            await upstream.receive_pack(body)
        # New request-local objects share the authenticator's cache, not headers.
        await self.upstream("OWNER/REPO").discovery("git-upload-pack")
        await self.upstream("Other/Second").discovery("git-upload-pack")
        self.assertEqual(
            self.mints,
            [
                {"repositories": ["repo"], "permissions": {"contents": "read"}},
                {"repositories": ["repo"], "permissions": {"contents": "write"}},
                {"repositories": ["second"], "permissions": {"contents": "read"}},
            ],
        )
        self.assertEqual(self.auth_requests.count("/repos/owner/repo/installation"), 2)
        for request, permission in zip(
            self.git_requests[:5],
            ("read", "read", "write", "write", "read"),
            strict=True,
        ):
            self.assertTrue(
                str(request.url).startswith("http://127.0.0.1:19876/owner/repo.git/")
            )
            expected = base64.b64encode(
                f"x-access-token:fixture-app-{permission}-repo".encode()
            ).decode()
            self.assertEqual(request.headers["Authorization"], "Basic " + expected)
        self.assert_no_token(self.logs.getvalue())

    async def test_missing_installation_preserves_existing_refusal_without_git_io(self):
        self.installation_status = 404
        with self.assertRaises(PolicyError) as error:
            await self.upstream().discovery("git-upload-pack")
        self.assertEqual(
            error.exception.message, "GitHub App not installed on owner/repo"
        )
        self.assertFalse(self.mints)
        self.assertFalse(self.git_requests)

    async def test_mint_failure_is_closed_and_has_no_credential_in_errors_or_logs(self):
        self.auth_status = 500
        with self.assertRaisesRegex(
            TransportError, "upstream_credential_unavailable"
        ) as error:
            await self.upstream().discovery("git-receive-pack")
        self.assertFalse(self.git_requests)
        self.assertEqual(len(self.mints), 1)  # No retries.
        self.assert_no_token(str(error.exception) + self.logs.getvalue())

    async def test_http_errors_are_opaque_and_reflected_tokens_are_refused(self):
        self.git_error = "fixture-app-read-repo"
        upstream = self.upstream()
        with self.assertRaisesRegex(TransportError, "upstream_unavailable") as error:
            await upstream.discovery("git-upload-pack")
        self.assert_no_token(str(error.exception) + self.logs.getvalue())
        self.git_error = None
        for variant in (
            b"fixture-app-read-repo",
            base64.b64encode(b"fixture-app-read-repo"),
            base64.b64encode(b"x-access-token:fixture-app-read-repo"),
        ):
            self.reflection = variant
            with self.assertRaisesRegex(TransportError, "upstream_reflection") as error:
                await upstream.discovery("git-receive-pack")
            self.assert_no_token(str(error.exception) + self.logs.getvalue())

    async def test_invalid_operation_never_mints(self):
        with self.assertRaisesRegex(TransportError, "upstream_service"):
            await self.upstream().discovery("arbitrary-service")
        self.assertFalse(self.auth_requests)

    async def test_real_httpcore_reflection_traces_are_redacted_before_handlers(self):
        token = b"fixture-app-write-repo"
        variants = (
            token,
            base64.b64encode(token),
            base64.b64encode(b"x-access-token:" + token),
            b"Basic " + base64.b64encode(b"x-access-token:" + token),
        )
        reflection = token
        connections = []
        transports = []
        sockets = []
        body = pkt(b"# service=git-receive-pack\n") + b"00000000"

        class Backend(AsyncMockBackend):
            async def connect_tcp(backend, host, port, **kwargs):
                self.assertEqual((host, port), ("github.com", 443))
                connections.append((host, port))
                return AsyncMockStream(
                    [
                        b"HTTP/1.1 200 " + reflection + b"\r\n"
                        b"Content-Type: application/x-git-receive-pack-advertisement\r\n"
                        b"Content-Length: " + str(len(body)).encode() + b"\r\n"
                        b"X-Reflection: " + reflection + b"\r\n\r\n" + body
                    ]
                )

        backend = Backend([])
        initialize = httpx.AsyncHTTPTransport.__init__

        def initialize_transport(transport, *args, **kwargs):
            initialize(transport, *args, **kwargs)
            transport._pool._network_backend = backend
            transports.append(transport)

        def refuse_socket(*args, **kwargs):
            sockets.append(True)
            raise AssertionError("in-memory HTTPcore fixture must not use sockets")

        with (
            patch.object(httpx.AsyncHTTPTransport, "__init__", initialize_transport),
            patch.object(socket.socket, "connect", refuse_socket),
            patch.object(socket.socket, "connect_ex", refuse_socket),
            patch.object(socket, "getaddrinfo", refuse_socket),
            patch.object(socket, "create_connection", refuse_socket),
        ):
            # Negative control proves the real HTTPcore parser emits the unsafe
            # DEBUG trace before the production guard, using a dummy token only.
            async with httpx.AsyncClient(trust_env=False) as client:
                await client.get("https://github.com/owner/repo.git/info/refs")
            self.assertIn("receive_response_headers.complete", self.logs.getvalue())
            self.assertIn(token.decode(), self.logs.getvalue())
            for reflection in variants:
                with self.subTest(reflection=reflection):
                    self.logs.seek(0)
                    self.logs.truncate(0)
                    upstream = GitHubAppUpstream("Owner/Repo", Limits(), auth=self.auth)
                    with self.assertRaisesRegex(
                        TransportError, "upstream_reflection"
                    ) as error:
                        await upstream.discovery("git-receive-pack")
                    self.assertIn("credentialed_http_transport", self.logs.getvalue())
                    self.assert_no_token(str(error.exception) + self.logs.getvalue())
                    self.assertNotIn("return_value=", self.logs.getvalue())
            # Context reset leaves another client's transport logging available.
            logging.getLogger("httpcore.http11").debug("unrelated_transport_message")
            self.assertIn("unrelated_transport_message", self.logs.getvalue())
        self.assertEqual(len(connections), 5)
        self.assertFalse(sockets)
        self.assertTrue(
            all(not transport._pool.connections for transport in transports)
        )

    async def test_transport_redaction_is_local_to_the_active_async_context(self):
        entered = asyncio.Event()
        release = asyncio.Event()

        async def slow_git(request):
            entered.set()
            await release.wait()
            logging.getLogger("httpx").debug("unsafe trace %s", "fixture-app-read-repo")
            return self.git_request(request)

        upstream = GitHubAppUpstream(
            "Owner/Repo",
            Limits(),
            auth=self.auth,
            fixture=LoopbackFixture(
                "http://127.0.0.1:19876", lambda: httpx.MockTransport(slow_git)
            ),
        )
        task = asyncio.create_task(upstream.discovery("git-upload-pack"))
        try:
            await asyncio.wait_for(entered.wait(), 3)
            logging.getLogger("httpcore.connection").debug("other_client_trace")
        finally:
            release.set()
            await task
        self.assertIn("other_client_trace", self.logs.getvalue())
        self.assert_no_token(self.logs.getvalue())

    async def test_delayed_descendant_logs_resume_after_upstream_operation_ends(self):
        async def exercise(outcome):
            self.logs.seek(0)
            self.logs.truncate(0)
            entered = asyncio.Event()
            protected = asyncio.Event()
            release = asyncio.Event()
            descendants = []
            message = f"unrelated_descendant_after_{outcome}"

            async def descendant():
                logging.getLogger("httpcore.http11").debug(
                    "active descendant %s", "fixture-app-read-repo"
                )
                protected.set()
                await release.wait()
                logging.getLogger("httpcore.http11").debug(message)
                logging.getLogger("httpx").debug(message)

            async def git(request):
                descendants.append(asyncio.create_task(descendant()))
                await protected.wait()
                entered.set()
                if outcome == "cancellation":
                    await asyncio.Event().wait()
                if outcome == "failure":
                    raise httpx.ConnectError("fixture-app-read-repo", request=request)
                return self.git_request(request)

            upstream = GitHubAppUpstream(
                "Owner/Repo",
                Limits(),
                auth=self.auth,
                fixture=LoopbackFixture(
                    "http://127.0.0.1:19876", lambda: httpx.MockTransport(git)
                ),
            )
            operation = asyncio.create_task(upstream.discovery("git-upload-pack"))
            try:
                await asyncio.wait_for(entered.wait(), 3)
                if outcome == "cancellation":
                    operation.cancel()
                    with self.assertRaises(asyncio.CancelledError):
                        await operation
                elif outcome == "failure":
                    with self.assertRaisesRegex(TransportError, "upstream_unavailable"):
                        await operation
                else:
                    await operation
            finally:
                if not operation.done():
                    operation.cancel()
                await asyncio.gather(operation, return_exceptions=True)
                release.set()
                await asyncio.gather(*descendants)
            logging.getLogger("httpcore.http11").debug("unrelated_parent_after_exit")
            self.assertIn("credentialed_http_transport", self.logs.getvalue())
            self.assertEqual(self.logs.getvalue().count(message), 2)
            self.assertIn("unrelated_parent_after_exit", self.logs.getvalue())
            self.assert_no_token(self.logs.getvalue())

        for outcome in ("success", "failure", "cancellation"):
            with self.subTest(outcome=outcome):
                await exercise(outcome)

    async def test_nested_upstream_exit_preserves_outer_redaction(self):
        async def git(request):
            logging.getLogger("httpcore.http11").debug(
                "outer before nested %s", "fixture-app-read-repo"
            )
            await self.upstream().discovery("git-receive-pack")
            logging.getLogger("httpcore.http11").debug(
                "outer after nested %s", "fixture-app-read-repo"
            )
            return self.git_request(request)

        upstream = GitHubAppUpstream(
            "Owner/Repo",
            Limits(),
            auth=self.auth,
            fixture=LoopbackFixture(
                "http://127.0.0.1:19876", lambda: httpx.MockTransport(git)
            ),
        )
        await upstream.discovery("git-upload-pack")
        logging.getLogger("httpcore.http11").debug("unrelated_parent_after_nested")
        self.assertIn("unrelated_parent_after_nested", self.logs.getvalue())
        self.assert_no_token(self.logs.getvalue())
        self.assertEqual(len(self.git_requests), 2)


class GitListenerTests(AppUpstreamFixture, unittest.IsolatedAsyncioTestCase):
    def applications(self):
        self.authority = Authority()
        return git_app.create_applications(
            self.authority,
            Path("unused-spool"),
            upstream_factory=lambda repository, limits: self.upstream(repository),
        )

    async def request(self, app, purpose, *, repository="Owner/Repo", token=None):
        return await asgi(
            app,
            path=f"/{repository}.git/info/refs".encode(),
            method="GET",
            query=(
                b"service=git-upload-pack"
                if purpose == "read"
                else b"service=git-receive-pack"
            ),
            headers=[
                (
                    b"host",
                    f"mainloop-git-{purpose}.mainloop.svc.cluster.local".encode(),
                ),
                (
                    b"authorization",
                    f"Bearer {token or (READ if purpose == 'read' else PUSH)}".encode(),
                ),
            ],
        )

    async def test_flags_off_refuse_all_requests_without_authentication_or_mint(self):
        read, push = self.applications()
        self.authority.authenticate = AsyncMock(
            side_effect=AssertionError("must not authenticate")
        )
        for enabled, push_enabled in ((False, False), (False, True)):
            with patch.multiple(
                settings, git_transport_enabled=enabled, push_gate_enabled=push_enabled
            ):
                for app, purpose in ((read, "read"), (push, "push")):
                    self.assertEqual((await self.request(app, purpose))[0], 403)
                    for method in ("GET", "POST", "PUT", "DELETE", "OPTIONS"):
                        status, body, _ = await asgi(
                            app, path=b"/health", method=method
                        )
                        self.assertEqual(status, 403)
                        self.assertEqual(body, b"git_transport_disabled\n")
        with patch.multiple(
            settings, git_transport_enabled=True, push_gate_enabled=False
        ):
            self.assertEqual((await self.request(push, "push"))[0], 403)
        self.authority.authenticate.assert_not_awaited()
        self.assertFalse(self.auth_requests)
        self.assertFalse(self.git_requests)

    async def test_enabled_listeners_use_correct_purpose_and_share_slot(self):
        read, push = self.applications()
        self.assertIs(read.slot, push.slot)
        self.assertEqual(read.slot._value, 1)
        self.assertEqual(
            read.common["seed_upstream_factory"], self.authority.seed_upstream
        )
        with patch.multiple(
            settings, git_transport_enabled=True, push_gate_enabled=True
        ):
            self.assertEqual((await self.request(read, "read"))[0], 200)
            self.assertEqual((await self.request(push, "push"))[0], 200)
        self.assertEqual(
            [mint["permissions"] for mint in self.mints],
            [{"contents": "read"}, {"contents": "write"}],
        )

    async def test_wrong_repository_or_capability_and_dispatch_denial_do_not_mint(self):
        read, push = self.applications()
        with patch.multiple(
            settings, git_transport_enabled=True, push_gate_enabled=True
        ):
            for app, purpose in ((read, "read"), (push, "push")):
                self.assertNotEqual(
                    (await self.request(app, purpose, repository="Other/Repo"))[0], 200
                )
                self.assertNotEqual(
                    (await self.request(app, purpose, token=READ + "_invalid"))[0],
                    200,
                )

            @asynccontextmanager
            async def denied(*args):
                raise TransportError("authority_changed")
                yield  # pragma: no cover - async context manager never admits dispatch

            self.authority.authorize_dispatch = denied
            self.assertNotEqual((await self.request(read, "read"))[0], 200)
        self.assertFalse(self.auth_requests)
        self.assertFalse(self.git_requests)

    async def test_mint_failure_is_safe_at_http_boundary(self):
        read, _ = self.applications()
        self.auth_status = 500
        with patch.multiple(
            settings, git_transport_enabled=True, push_gate_enabled=True
        ):
            status, body, _ = await self.request(read, "read")
        self.assertNotEqual(status, 200)
        self.assertIn(b"upstream_credential_unavailable", body)
        self.assertFalse(self.git_requests)
        self.assert_no_token(body.decode() + self.logs.getvalue())

    async def test_concurrent_repositories_keep_upstreams_and_credentials_separate(
        self,
    ):
        read, _ = self.applications()

        async def authenticate(capability, purpose):
            repository = (
                "Other/Second" if capability == READ + "_second" else "Owner/Repo"
            )
            return self.authority.binding(purpose).model_copy(
                update={"repository": repository}
            )

        @asynccontextmanager
        async def authorize_dispatch(binding, prepared):
            yield DispatchProof(binding)

        self.authority.authenticate = authenticate
        self.authority.authorize_dispatch = authorize_dispatch
        with patch.multiple(
            settings, git_transport_enabled=True, push_gate_enabled=True
        ):
            results = await asyncio.gather(
                self.request(read, "read"),
                self.request(
                    read, "read", repository="Other/Second", token=READ + "_second"
                ),
            )
        self.assertEqual([result[0] for result in results], [200, 200])
        for request in self.git_requests:
            name = request.url.path.split("/")[2].removesuffix(".git")
            expected = base64.b64encode(
                f"x-access-token:fixture-app-read-{name}".encode()
            ).decode()
            self.assertEqual(request.headers["Authorization"], "Basic " + expected)
        self.assertEqual(
            {tuple(mint["repositories"]) for mint in self.mints},
            {("repo",), ("second",)},
        )

    async def test_validation_slot_serializes_distinct_push_requests(self):
        entered = asyncio.Event()
        release = asyncio.Event()
        second_authenticated = asyncio.Event()
        validations = []
        self.authority = Authority()
        authenticate = self.authority.authenticate
        calls = 0

        async def observed_authenticate(*args):
            nonlocal calls
            binding = await authenticate(*args)
            calls += 1
            if calls == 2:
                second_authenticated.set()
            return binding

        self.authority.authenticate = observed_authenticate

        async def prepare(*args, **kwargs):
            validations.append(len(validations))
            entered.set()
            await release.wait()
            raise TransportError("fixture_validation_failed")

        body = (
            pkt(b"0" * 40 + b" " + b"1" * 40 + b" refs/heads/feature\0report-status\n")
            + b"0000PACK"
        )
        headers = [
            (b"host", b"mainloop-git-push.mainloop.svc.cluster.local"),
            (b"authorization", f"Bearer {PUSH}".encode()),
            (b"content-type", b"application/x-git-receive-pack-request"),
            (b"content-length", str(len(body)).encode()),
        ]
        with (
            tempfile.TemporaryDirectory(dir=fixture_root()) as directory,
            patch.multiple(
                settings, git_transport_enabled=True, push_gate_enabled=True
            ),
            patch("mainloop.push_gate.transport.prepare_receive", prepare),
        ):
            _, push = git_app.create_applications(
                self.authority,
                Path(directory),
                upstream_factory=lambda repository, limits: self.upstream(repository),
            )
            first = asyncio.create_task(asgi(push, body=body, headers=headers))
            second = None
            try:
                try:
                    await asyncio.wait_for(entered.wait(), 3)
                except TimeoutError:
                    self.fail(f"validation was not reached: {await first}")
                second = asyncio.create_task(asgi(push, body=body, headers=headers))
                await asyncio.wait_for(second_authenticated.wait(), 3)
                self.assertEqual(len(validations), 1)
                self.assertFalse(second.done())
            finally:
                release.set()
                results = await asyncio.gather(first, *([second] if second else []))
        self.assertEqual([result[0] for result in results], [403, 403])
        self.assertEqual(len(validations), 2)
        self.assertEqual(
            [mint["permissions"] for mint in self.mints], [{"contents": "read"}]
        )

    async def test_production_push_validates_using_private_child_of_group_writable_mount(
        self,
    ):
        self.authority = Authority()
        advertisement = pkt(b"# service=git-upload-pack\n") + b"00000000"
        body = (
            pkt(b"0" * 40 + b" " + b"1" * 40 + b" refs/heads/feature\0report-status\n")
            + b"0000PACK"
        )
        headers = [
            (b"host", b"mainloop-git-push.mainloop.svc.cluster.local"),
            (b"authorization", f"Bearer {PUSH}".encode()),
            (b"content-type", b"application/x-git-receive-pack-request"),
            (b"content-length", str(len(body)).encode()),
        ]
        with (
            tempfile.TemporaryDirectory(dir=fixture_root()) as directory,
            patch.multiple(
                settings, git_transport_enabled=True, push_gate_enabled=True
            ),
            patch.object(git_app.db, "connect", AsyncMock()),
            patch.object(git_app.db, "disconnect", AsyncMock()),
            patch.object(git_app, "get_client", Mock()),
            patch.object(git_app, "close_client", AsyncMock()),
            patch.object(git_app, "app_auth", Mock()),
            patch.object(git_app, "require_token_key", Mock()),
            patch.object(
                git_app, "PostgresTransportAuthority", return_value=self.authority
            ),
            patch.object(
                GitHubAppUpstream, "discovery", AsyncMock(return_value=advertisement)
            ) as discovery,
            patch(
                "mainloop.push_gate.transport.prepare_receive",
                AsyncMock(side_effect=TransportError("fixture_validation_reached")),
            ) as validation,
        ):
            mounted = Path(directory) / "mount"
            mounted.mkdir()
            mounted.chmod(0o2775)
            async with git_app.production_applications(mounted) as (read, push):
                private = push.common["spool_root"]
                self.assertEqual(private.parent, mounted)
                self.assertIs(read.common["spool_root"], private)
                self.assertEqual(stat.S_IMODE(private.stat().st_mode), 0o700)
                self.assertEqual(private.stat().st_uid, os.getuid())
                status, response, _ = await asgi(push, body=body, headers=headers)
                self.assertEqual(status, 403)
                self.assertIn(b"fixture_validation_reached", response)
                self.assertNotIn(b"private_spool_required", response)
                validation.assert_awaited_once()
                self.assertEqual(validation.await_args.args[0].parent, private)
                discovery.assert_awaited_once_with(
                    "git-upload-pack", secrets=unittest.mock.ANY
                )
            self.assertFalse(private.exists())
            self.assertEqual(stat.S_IMODE(mounted.stat().st_mode), 0o2775)


class ProductionWiringTests(unittest.IsolatedAsyncioTestCase):
    async def test_disabled_startup_requires_no_database_kagent_key_or_app(self):
        with (
            patch.multiple(
                settings, git_transport_enabled=False, push_gate_enabled=False
            ),
            patch.object(git_app.db, "connect", AsyncMock()) as connect,
            patch.object(git_app.db, "disconnect", AsyncMock()) as disconnect,
            patch.object(git_app, "get_client") as client,
            patch.object(git_app, "app_auth") as auth,
            patch.object(git_app, "require_token_key") as key,
        ):
            async with git_app.production_applications() as (read, push):
                self.assertEqual((await asgi(read))[0], 403)
                self.assertEqual((await asgi(push))[0], 403)
            connect.assert_not_awaited()
            disconnect.assert_not_awaited()
            client.assert_not_called()
            auth.assert_not_called()
            key.assert_not_called()

    async def test_enabled_startup_constructs_production_authority_and_closes_once(
        self,
    ):
        with (
            tempfile.TemporaryDirectory() as directory,
            patch.multiple(
                settings, git_transport_enabled=True, push_gate_enabled=True
            ),
            patch.object(git_app.db, "connect", AsyncMock()) as connect,
            patch.object(git_app.db, "disconnect", AsyncMock()) as disconnect,
            patch.object(git_app, "get_client", return_value=Mock()) as client,
            patch.object(git_app, "close_client", AsyncMock()) as close,
            patch.object(git_app, "app_auth") as auth,
            patch.object(git_app, "require_token_key") as key,
        ):
            root = Path(directory) / "spool"
            async with git_app.production_applications(root) as (read, push):
                authority = read.common["authority"]
                self.assertIsInstance(authority, git_app.PostgresTransportAuthority)
                self.assertIs(authority.database, git_app.db)
                self.assertIs(authority.client, client.return_value)
                self.assertIs(authority.metadata, git_app.get_repo_metadata)
                self.assertIs(authority, push.common["authority"])
                self.assertIs(read.upstream_factory, GitHubAppUpstream)
                self.assertEqual(
                    read.common["seed_upstream_factory"], authority.seed_upstream
                )
                self.assertTrue(root.is_dir())
                private = read.common["spool_root"]
                self.assertEqual(private.parent, root)
                self.assertEqual(stat.S_IMODE(private.stat().st_mode), 0o700)
                self.assertEqual(private.stat().st_uid, os.getuid())
            self.assertFalse(private.exists())
            connect.assert_awaited_once_with()
            disconnect.assert_awaited_once_with()
            close.assert_awaited_once_with()
            auth.assert_called_once_with()
            key.assert_called_once_with()

    async def test_paired_servers_have_private_ports_and_stop_together(self):
        servers = []

        class Server:
            def __init__(self, config):
                self.config = config
                self.should_exit = False
                servers.append(self)

            async def serve(self):
                if self.config.port == git_app.READ_PORT:
                    return
                while not self.should_exit:
                    await asyncio.sleep(0)

        @asynccontextmanager
        async def applications():
            yield (Mock(), Mock())

        with (
            patch.object(git_app, "production_applications", applications),
            patch.object(git_app, "ListenerServer", Server),
        ):
            await asyncio.wait_for(git_app.serve(), 3)
        self.assertEqual([server.config.port for server in servers], [8003, 8004])
        for server in servers:
            self.assertTrue(server.should_exit)
            self.assertFalse(server.config.proxy_headers)
            self.assertFalse(server.config.access_log)
