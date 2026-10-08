"""HTTP isolation, real-quarantine denial, and single-dispatch failure classification."""

import asyncio
import base64
import io
import os
import socket
import ssl
import unittest
from dataclasses import replace
from pathlib import Path
from unittest.mock import patch

import httpx
from httpcore._backends.mock import AsyncMockBackend, AsyncMockStream
from mainloop.push_gate.authorization import ZERO_OID
from mainloop.push_gate.protocol import Limits, pkt, receipt, receive_commands
from mainloop.push_gate.transport import create_git_applications
from mainloop.push_gate.upstream import FixedGitUpstream, LoopbackFixture
from tests.runtime.test_push_transport_git import (
    ASSOCIATION,
    GRANT,
    PAT,
    POLICY,
    PUSH,
    READ,
    FixtureAuthority,
    GitFixtureCase,
    command,
)

from models.push_gate import PublicationState


async def asgi(
    app,
    *,
    path=b"/Owner/Repo.git/git-receive-pack",
    query=b"",
    method="POST",
    body=b"",
    headers=None,
    chunks=None,
):
    raw_headers = (
        headers
        if headers is not None
        else [
            (b"host", b"testserver"),
            (b"authorization", f"Bearer {PUSH}".encode()),
            (b"content-type", b"application/x-git-receive-pack-request"),
            (b"content-length", str(len(body)).encode()),
        ]
    )
    scope = {
        "type": "http",
        "http_version": "1.1",
        "method": method,
        "scheme": "http",
        "path": path.decode("ascii", errors="replace"),
        "raw_path": path,
        "query_string": query,
        "headers": raw_headers,
    }
    parts = list(chunks) if chunks is not None else [body]
    output = []

    async def receive():
        part = parts.pop(0)
        return {"type": "http.request", "body": part, "more_body": bool(parts)}

    async def send(message):
        output.append(message)

    await app(scope, receive, send)
    return (
        output[0]["status"],
        b"".join(item.get("body", b"") for item in output),
        output,
    )


class ProductionTransportOwnershipTests(unittest.IsolatedAsyncioTestCase):
    async def ownership_case(self, second):
        """Exercise real HTTP pools; substitute only their network backend."""
        entered = [asyncio.Event(), asyncio.Event()]
        released = [asyncio.Event(), asyncio.Event()]
        bodies = [b"synthetic-upload-response", b"synthetic-discovery-response"]
        streams = []
        transports = []
        tasks = []
        socket_calls = []
        connections = []
        tls = []

        def reply(index):
            kind = b"result" if index == 0 else b"advertisement"
            return (
                b"HTTP/1.1 200 OK\r\nContent-Type: application/x-git-upload-pack-"
                + kind
                + b"\r\nContent-Length: "
                + str(len(bodies[index])).encode()
                + b"\r\n\r\n"
            )

        class Stream(AsyncMockStream):
            def __init__(stream, index):
                super().__init__([reply(index), bodies[index]])
                stream.index = index
                stream.reads = 0

            async def read(stream, max_bytes, timeout=None):
                stream.reads += 1
                if stream.reads == 2 and (stream.index == 0 or second == "cancelled"):
                    entered[stream.index].set()
                    await released[stream.index].wait()
                return await super().read(max_bytes, timeout)

            async def start_tls(
                stream, ssl_context, server_hostname=None, timeout=None
            ):
                self.assertEqual(ssl_context.verify_mode, ssl.CERT_REQUIRED)
                self.assertTrue(ssl_context.check_hostname)
                self.assertEqual(server_hostname, "github.com")
                tls.append(server_hostname)
                # No handshake occurs: this is a connection-ownership fixture.
                return await super().start_tls(ssl_context, server_hostname, timeout)

            async def aclose(stream):
                await super().aclose()
                released[stream.index].set()

        class Backend(AsyncMockBackend):
            async def connect_tcp(backend, host, port, **kwargs):
                self.assertEqual((host, port), ("github.com", 443))
                connections.append((host, port))
                stream = Stream(len(streams))
                streams.append(stream)
                return stream

        backend = Backend([])
        original_init = httpx.AsyncHTTPTransport.__init__

        def initialize(transport, *args, **kwargs):
            original_init(transport, *args, **kwargs)
            self.assertEqual(kwargs["retries"], 0)
            self.assertIs(kwargs["verify"], True)
            self.assertIs(kwargs["trust_env"], False)
            transport._pool._network_backend = backend
            transports.append(transport)

        def refuse_socket(*args, **kwargs):
            socket_calls.append("unexpected socket/DNS call")
            raise AssertionError("ownership fixture must not use sockets")

        with (
            patch.object(httpx.AsyncHTTPTransport, "__init__", initialize),
            patch.object(socket.socket, "connect", refuse_socket),
            patch.object(socket.socket, "connect_ex", refuse_socket),
            patch.object(socket, "getaddrinfo", refuse_socket),
            patch.object(socket, "create_connection", refuse_socket),
        ):
            upstream = FixedGitUpstream(
                "Owner/Repo", "synthetic-review-token", Limits()
            )
            slow = asyncio.create_task(upstream.upload_pack(b"synthetic-request"))
            tasks.append(slow)
            try:
                await asyncio.wait_for(entered[0].wait(), 3)
                if second is not None:
                    other = asyncio.create_task(upstream.discovery("git-upload-pack"))
                    tasks.append(other)
                    if second == "cancelled":
                        await asyncio.wait_for(entered[1].wait(), 3)
                        other.cancel()
                        with self.assertRaises(asyncio.CancelledError):
                            await other
                    else:
                        self.assertEqual(await asyncio.wait_for(other, 3), bodies[1])
                    self.assertFalse(
                        streams[0]._closed,
                        "finishing another operation closed the healthy upload connection",
                    )
                    self.assertFalse(slow.done())
                released[0].set()
                self.assertEqual(await asyncio.wait_for(slow, 3), bodies[0])
                self.assertTrue(all(stream._closed for stream in streams))
                self.assertTrue(
                    all(not transport._pool.connections for transport in transports)
                )
                self.assertEqual(len(transports), 1 if second is None else 2)
                self.assertEqual(len(tls), len(streams))
                self.assertEqual(socket_calls, [])
            finally:
                for event in released:
                    event.set()
                for task in tasks:
                    if not task.done():
                        task.cancel()
                await asyncio.gather(*tasks, return_exceptions=True)
                for transport in transports:
                    await transport.aclose()
                self.assertTrue(all(task.done() for task in tasks))
                self.assertTrue(all(stream._closed for stream in streams))
                self.assertTrue(
                    all(not transport._pool.connections for transport in transports)
                )
                self.ownership_evidence = {
                    "second_operation": second,
                    "actual_transports": len(transports),
                    "in_memory_connections": len(connections),
                    "socket_or_dns_calls": len(socket_calls),
                    "tasks_joined": len(tasks),
                    "streams_closed": all(stream._closed for stream in streams),
                    "pools_empty": all(
                        not transport._pool.connections for transport in transports
                    ),
                }

    async def test_single_production_request_control(self):
        await self.ownership_case(None)

    async def test_completed_discovery_preserves_overlapping_upload(self):
        await self.ownership_case("completed")

    async def test_cancelled_discovery_preserves_overlapping_upload(self):
        await self.ownership_case("cancelled")


class HttpBoundaryTests(unittest.IsolatedAsyncioTestCase):
    def applications(self, handler=None):
        self.requests = []

        def upstream_request(request):
            self.requests.append(request)
            if handler:
                return handler(request)
            return httpx.Response(
                200,
                stream=httpx.ByteStream(b"fixture response"),
                headers={"content-type": "application/x-git-upload-pack-advertisement"},
            )

        self.upstream = FixedGitUpstream(
            "Owner/Repo",
            PAT,
            Limits(),
            fixture=LoopbackFixture(
                "http://127.0.0.1:1", lambda: httpx.MockTransport(upstream_request)
            ),
        )
        self.authority = FixtureAuthority()
        return create_git_applications(
            authority=self.authority,
            upstream=self.upstream,
            spool_root=Path.home() / ".cache" / "mainloop-p1-fixtures",
            read_hosts=("testserver",),
            push_hosts=("testserver",),
            response_secrets=(
                b"fixture_mcp_secret",
                b"fixture_runtime_secret",
                READ.encode(),
                PUSH.encode(),
            ),
        )

    async def test_absent_authority_rejects_startup_and_requests(self):
        read, push = create_git_applications()
        for app in (read, push):
            status, data, _ = await asgi(app)
            self.assertEqual(status, 503)
            self.assertIn(b"production_authority_absent", data)
            sent = []

            async def receive():
                return {"type": "lifespan.startup"}

            async def send(value, sent=sent):
                sent.append(value)

            await app({"type": "lifespan"}, receive, send)
            self.assertEqual(sent[0]["type"], "lifespan.startup.failed")

    async def test_route_raw_path_and_query_deny_before_any_upstream(self):
        read, push = self.applications()
        cases = [
            b"/Owner/Other.git/info/refs",
            b"/owner/repo.git/git-upload-pack",
            b"/Owner/Repo.git/objects/abc",
            b"/Owner/Repo.git/info/lfs",
            b"/graphql",
            b"/Owner/Repo.git/git-receive-pack/",
            b"/Owner/Repo.git/../info/refs",
            b"/Owner/Repo%2egit/info/refs",
            b"/Owner/Repo.git//info/refs",
            b"/Owner/Repo.git\\info/refs",
            b"/Owner/Repo.git/info/refs?service=git-receive-pack",
        ]
        headers = [
            (b"host", b"testserver"),
            (b"authorization", f"Bearer {PUSH}".encode()),
        ]
        for path in cases:
            status, _, _ = await asgi(
                push,
                method="GET",
                path=path,
                query=b"service=git-receive-pack",
                headers=headers,
            )
            self.assertNotEqual(status, 200, path)
        for query in (
            b"",
            b"service=git-upload-pack",
            b"service=git-receive-pack&service=git-receive-pack",
            b"service=git-receive-pack&x=1",
            b"service%3dgit-receive-pack",
        ):
            status, _, _ = await asgi(
                push,
                method="GET",
                path=b"/Owner/Repo.git/info/refs",
                query=query,
                headers=headers,
            )
            self.assertNotEqual(status, 200)
        for method in ("CONNECT", "PUT", "DELETE", "HEAD", "OPTIONS"):
            self.assertNotEqual((await asgi(push, method=method))[0], 200)
        self.assertNotEqual((await asgi(read))[0], 200)
        self.assertEqual(self.requests, [])

    async def test_duplicate_framing_identity_routing_and_wrong_purpose_denied(self):
        _, push = self.applications()
        headers = [
            (b"host", b"testserver"),
            (b"authorization", f"Bearer {PUSH}".encode()),
            (b"content-type", b"application/x-git-receive-pack-request"),
        ]
        extra = [
            [(b"authorization", f"Bearer {PUSH}".encode())],
            [(b"host", b"github.com")],
            [(b"content-length", b"0"), (b"content-length", b"0")],
            [(b"content-length", b"0"), (b"transfer-encoding", b"chunked")],
            [(b"transfer-encoding", b"gzip")],
            [(b"content-length", b"+0")],
            [(b"content-encoding", b"gzip")],
            [(b"upgrade", b"websocket")],
            [(b"x-actor-uid", b"uid")],
            [(b"x-session", b"native")],
            [(b"x-forwarded-host", b"github.com")],
            [(b"forwarded", b"host=github.com")],
            [(b"proxy-authorization", b"secret")],
            [(b"x-http-method-override", b"GET")],
            [(b"git-protocol", b"version=2")],
        ]
        for addition in extra:
            self.assertNotEqual((await asgi(push, headers=headers + addition))[0], 200)
        wrong = [
            (key, f"Bearer {READ}".encode() if key == b"authorization" else value)
            for key, value in headers
        ]
        self.assertNotEqual((await asgi(push, headers=wrong))[0], 200)
        self.assertEqual(self.requests, [])

    async def test_rebuilt_headers_fixed_target_redirects_and_full_secret_nonreflection(
        self,
    ):
        reflected = [
            base64.b64encode(f"x-access-token:{PAT}".encode()),
            base64.b64encode(READ.encode()),
            PAT.encode(),
            READ.encode(),
            PUSH.encode(),
            b"fixture_mcp_secret",
            b"fixture_runtime_secret",
        ]
        for secret in reflected:
            for location in ("body", "header"):

                def handler(request, location=location, secret=secret):
                    headers = {
                        "content-type": "application/x-git-upload-pack-advertisement"
                    }
                    if location == "header":
                        headers["x-reflect"] = secret.decode()
                    return httpx.Response(
                        200,
                        stream=httpx.ByteStream(
                            b"prefix" + secret + b"suffix"
                            if location == "body"
                            else b"safe"
                        ),
                        headers=headers,
                    )

                read, _ = self.applications(handler)
                headers = [
                    (b"host", b"testserver"),
                    (b"authorization", f"Bearer {READ}".encode()),
                    (b"cookie", b"actor_cookie"),
                    (b"git-protocol", b"version=2"),
                ]
                status, data, messages = await asgi(
                    read,
                    method="GET",
                    path=b"/OWNER/REPO.git/info/refs",
                    query=b"service=git-upload-pack",
                    headers=headers,
                )
                self.assertNotEqual(status, 200)
                self.assertIn(b"upstream_reflection", data)
                emitted = repr(messages).encode()
                for known in reflected:
                    self.assertNotIn(known, emitted)
                self.assertEqual(len(self.requests), 1)
                request = self.requests[0]
                self.assertEqual(
                    str(request.url),
                    "http://127.0.0.1:1/Owner/Repo.git/info/refs?service=git-upload-pack",
                )
                self.assertNotIn("cookie", request.headers)
                self.assertNotIn(PUSH, repr(request.headers))
                self.assertEqual(request.headers["git-protocol"], "version=2")
        read, _ = self.applications(
            lambda request: httpx.Response(
                307, headers={"location": "https://evil.invalid/path"}
            )
        )
        status, _, _ = await asgi(
            read,
            method="GET",
            path=b"/Owner/Repo.git/info/refs",
            query=b"service=git-upload-pack",
            headers=[
                (b"host", b"testserver"),
                (b"authorization", f"Bearer {READ}".encode()),
            ],
        )
        self.assertNotEqual(status, 200)
        self.assertEqual(len(self.requests), 1)
        for origin in (
            "https://evil.invalid",
            "http://localhost:1",
            "http://127.0.0.1:1/path",
            "http://user@127.0.0.1:1",
        ):
            with self.assertRaises(ValueError):
                LoopbackFixture(origin)

    async def test_upstream_auth_http_compression_duplicate_framing_and_limits(self):
        read, _ = self.applications()
        positive = await asgi(
            read,
            method="GET",
            path=b"/Owner/Repo.git/info/refs",
            query=b"service=git-upload-pack",
            headers=[
                (b"host", b"testserver"),
                (b"authorization", f"Bearer {READ}".encode()),
            ],
        )
        self.assertEqual(positive[:2], (200, b"fixture response"))
        responses = [
            httpx.Response(401, stream=httpx.ByteStream(b"denied")),
            httpx.Response(
                200,
                stream=httpx.ByteStream(b"bad"),
                headers={"content-type": "application/json"},
            ),
            httpx.Response(
                200,
                stream=httpx.ByteStream(b"bad"),
                headers={
                    "content-type": "application/x-git-upload-pack-advertisement",
                    "content-encoding": "gzip",
                },
            ),
            httpx.Response(
                200,
                stream=httpx.ByteStream(b"bad"),
                headers=[
                    ("content-type", "application/x-git-upload-pack-advertisement"),
                    ("content-length", "3"),
                    ("content-length", "3"),
                ],
            ),
            httpx.Response(
                200,
                stream=httpx.ByteStream(b"bad"),
                headers={
                    "content-type": "application/x-git-upload-pack-advertisement",
                    "content-length": "3",
                    "transfer-encoding": "chunked",
                },
            ),
        ]
        for response in responses:
            read, _ = self.applications(lambda request, response=response: response)
            result = await asgi(
                read,
                method="GET",
                path=b"/Owner/Repo.git/info/refs",
                query=b"service=git-upload-pack",
                headers=[
                    (b"host", b"testserver"),
                    (b"authorization", f"Bearer {READ}".encode()),
                ],
            )
            self.assertNotEqual(result[0], 200)
            self.assertEqual(len(self.requests), 1)
        read, _ = self.applications()
        self.upstream.limits = replace(Limits(), response_bytes=4)
        self.assertNotEqual(
            (
                await asgi(
                    read,
                    method="GET",
                    path=b"/Owner/Repo.git/info/refs",
                    query=b"service=git-upload-pack",
                    headers=[
                        (b"host", b"testserver"),
                        (b"authorization", f"Bearer {READ}".encode()),
                    ],
                )
            )[0],
            200,
        )


class QuarantineHttpTests(GitFixtureCase):
    async def request_body(self, app, body, **kwargs):
        return await asgi(
            app,
            body=body,
            headers=[
                (b"host", app.hosts[0].encode()),
                (b"authorization", f"Bearer {PUSH}".encode()),
                (b"content-type", b"application/x-git-receive-pack-request"),
                (b"content-length", str(len(body)).encode()),
            ],
            **kwargs,
        )

    async def test_split_packets_chunked_body_and_thin_fast_forward(self):
        import hashlib

        (self.client / "file").write_text(
            "".join(hashlib.sha256(str(i).encode()).hexdigest() for i in range(2048))
        )
        async with self.transport() as (_, url, app):
            first = await self.commit("first")
            await self.push(url)
            second = await self.commit("second")
            body = await self.raw_body(first, second, thin=True)
            parsed = receive_commands(io.BytesIO(body), self.limits)
            empty = self.root / "empty.git"
            await command("/usr/bin/git", "init", "--bare", str(empty))
            rc, _, error = await command(
                "/usr/bin/git",
                "--git-dir=" + str(empty),
                "index-pack",
                "--stdin",
                "--fix-thin",
                "--strict",
                data=body[parsed.pack_offset :],
                success=False,
            )
            self.assertNotEqual(rc, 0)
            self.assertIn(b"unresolved delta", error)
            headers = [
                (b"host", app.hosts[0].encode()),
                (b"authorization", f"Bearer {PUSH}".encode()),
                (b"content-type", b"application/x-git-receive-pack-request"),
                (b"transfer-encoding", b"chunked"),
            ]
            result = await asgi(
                app,
                body=body,
                headers=headers,
                chunks=[body[index : index + 1] for index in range(len(body))],
            )
            self.assertEqual(result[0], 200, result[1])
            self.assertTrue(receipt(result[1], "refs/heads/feature", sideband=True))
            self.assertEqual(self.fake.receives[-1], body)
            # A thin base from an unadvertised, private client-only lineage is unavailable.
            (self.client / "file").write_text(
                "".join(
                    hashlib.sha256(f"foreign{i}".encode()).hexdigest()
                    for i in range(2048)
                )
            )
            foreign = await self.commit("foreign")
            child = await self.commit("foreign-child")
            invalid = await self.raw_body(foreign, child, thin=True)
            offset = receive_commands(io.BytesIO(invalid), self.limits).pack_offset
            missing = (
                pkt(
                    f"{second} {child} refs/heads/feature".encode()
                    + b"\0report-status side-band-64k ofs-delta\n"
                )
                + b"0000"
                + invalid[offset:]
            )
            count = len(self.fake.receives)
            self.assertNotEqual((await self.request_body(app, missing))[0], 200)
            self.assertEqual(len(self.fake.receives), count)

    async def test_inherited_proxy_cannot_reroute_fixed_upstream_and_bad_http_eof_denies(
        self,
    ):
        new = await self.commit("first")
        body = await self.raw_body(ZERO_OID, new)
        poison = {
            name: "http://127.0.0.1:1"
            for name in (
                "HTTP_PROXY",
                "HTTPS_PROXY",
                "ALL_PROXY",
                "http_proxy",
                "https_proxy",
                "all_proxy",
            )
        }
        poison.update(
            {
                "NO_PROXY": "",
                "no_proxy": "",
                "SSL_CERT_FILE": str(self.root / "absent-ca-file"),
                "SSL_CERT_DIR": str(self.root / "absent-ca-dir"),
            }
        )
        with patch.dict(os.environ, poison):
            async with self.transport() as (_, _, app):
                headers = [
                    (b"host", app.hosts[0].encode()),
                    (b"authorization", f"Bearer {PUSH}".encode()),
                    (b"content-type", b"application/x-git-receive-pack-request"),
                    (b"content-length", str(len(body) + 1).encode()),
                ]
                self.assertNotEqual(
                    (await asgi(app, body=body, headers=headers))[0], 200
                )
                self.assertEqual(self.fake.receives, [])
                result = await self.request_body(app, body)
                self.assertEqual(result[0], 200, result[1])
                self.assertEqual(len(self.fake.receives), 1)

    async def test_corrupt_truncated_trailing_checksum_and_noncommit_zero_writes(self):
        new = await self.commit("first")
        body = await self.raw_body(ZERO_OID, new)
        commands = receive_commands(io.BytesIO(body), self.limits)
        corrupt = bytearray(body)
        corrupt[-1] ^= 1
        cases = [
            body[:-1],
            body + b"trailing",
            bytes(corrupt),
            body[: commands.pack_offset],
            body[: commands.pack_offset + 10],
        ]
        tree = (
            (
                await command(
                    "/usr/bin/git", "rev-parse", "HEAD^{tree}", cwd=self.client
                )
            )[1]
            .strip()
            .decode()
        )
        cases.append(await self.raw_body(ZERO_OID, tree))
        async with self.transport() as (_, _, app):
            for candidate in cases:
                status, _, _ = await self.request_body(app, candidate)
                self.assertNotEqual(status, 200)
                self.assertEqual(self.fake.receives, [])
            self.assertEqual(self.authority.records, [])

    async def test_raw_mixed_tags_notes_delete_foreign_and_non_ff_zero_writes(self):
        new = await self.commit("first")
        good = await self.raw_body(ZERO_OID, new)
        parsed = receive_commands(io.BytesIO(good), self.limits)
        pack = good[parsed.pack_offset :]
        async with self.transport() as (_, url, app):
            for ref, old, target in [
                ("refs/tags/v1", ZERO_OID, new),
                ("refs/notes/x", ZERO_OID, new),
                ("refs/heads/main", ZERO_OID, new),
                ("refs/heads/release/x", ZERO_OID, new),
                ("refs/heads/old-default", ZERO_OID, new),
                ("refs/heads/Feature", ZERO_OID, new),
                ("refs/heads/feature", new, ZERO_OID),
            ]:
                body = (
                    pkt(
                        f"{old} {target} {ref}".encode()
                        + b"\0report-status side-band-64k\n"
                    )
                    + b"0000"
                    + pack
                )
                self.assertNotEqual((await self.request_body(app, body))[0], 200)
            mixed = (
                good[: parsed.pack_offset - 4]
                + pkt(f"{ZERO_OID} {new} refs/heads/other\n".encode())
                + b"0000"
                + pack
            )
            self.assertNotEqual((await self.request_body(app, mixed))[0], 200)
            self.assertNotEqual(
                (
                    await self.request_body(
                        app, good, path=b"/Owner/Foreign.git/git-receive-pack"
                    )
                )[0],
                200,
            )
            self.assertEqual(self.fake.receives, [])
            await self.push(url)
            second = await self.commit("second")
            await self.push(url)
            rewind = await self.raw_body(second, new)
            self.assertNotEqual((await self.request_body(app, rewind))[0], 200)
            self.assertEqual(len(self.fake.receives), 2)

    async def test_missing_commit_ancestry_denied(self):
        tree = (
            (
                await command(
                    "/usr/bin/git", "rev-parse", "HEAD^{tree}", cwd=self.client
                )
            )[1]
            .strip()
            .decode()
        )
        commit = f"tree {tree}\nparent {'a' * 40}\nauthor Fixture <fixture@example.invalid> 1 +0000\ncommitter Fixture <fixture@example.invalid> 1 +0000\n\nmissing parent\n".encode()
        oid = (
            await command(
                "/usr/bin/git",
                "hash-object",
                "-t",
                "commit",
                "-w",
                "--stdin",
                cwd=self.client,
                data=commit,
            )
        )[1].strip()
        objects = (
            await command(
                "/usr/bin/git", "rev-list", "--objects", self.base, cwd=self.client
            )
        )[1].splitlines()
        data = (
            oid
            + b"\n"
            + b"\n".join(
                line.split()[0]
                for line in objects
                if not line.startswith(self.base.encode())
            )
            + b"\n"
        )
        pack = (
            await command(
                "/usr/bin/git", "pack-objects", "--stdout", cwd=self.client, data=data
            )
        )[1]
        body = (
            pkt(
                f"{ZERO_OID} {oid.decode()} refs/heads/feature".encode()
                + b"\0report-status side-band-64k\n"
            )
            + b"0000"
            + pack
        )
        async with self.transport() as (_, _, app):
            self.assertNotEqual((await self.request_body(app, body))[0], 200)
            self.assertEqual(self.fake.receives, [])

    async def test_body_object_expansion_disk_memory_and_deadline_limits(self):
        new = await self.commit("first")
        body = await self.raw_body(ZERO_OID, new)
        original = self.limits
        for overrides in (
            {"body_bytes": len(body)},
            {"objects": 1},
            {"expanded_bytes": 10},
            {"disk_bytes": 1},
            {"memory_bytes": 1},
            {"validation_seconds": 0.000001},
        ):
            with self.subTest(overrides=overrides):
                self.limits = replace(original, **overrides)
                async with self.transport() as (_, _, app):
                    self.assertNotEqual((await self.request_body(app, body))[0], 200)
                    self.assertEqual(self.fake.receives, [])
        self.limits = original

    async def test_final_policy_runtime_revocation_and_actual_remote_oid_changes(self):
        new = await self.commit("first")
        body = await self.raw_body(ZERO_OID, new)
        changes = [
            lambda authority: setattr(authority, "revoked", True),
            lambda authority: setattr(
                authority,
                "association",
                ASSOCIATION.model_copy(update={"actor_uid": "replacement"}),
            ),
            lambda authority: setattr(
                authority,
                "policy",
                POLICY.model_copy(update={"version": 2, "default_branch": "feature"}),
            ),
            lambda authority: setattr(
                authority,
                "grant",
                GRANT.model_copy(
                    update={
                        "writer_generation": 2,
                        "attempt_id": "new",
                        "role": "child",
                    }
                ),
            ),
        ]
        for change in changes:
            (
                self.authority.revoked,
                self.authority.association,
                self.authority.policy,
                self.authority.grant,
            ) = (False, ASSOCIATION, POLICY, GRANT)
            self.authority.final_change = change
            async with self.transport() as (_, _, app):
                self.assertNotEqual((await self.request_body(app, body))[0], 200)
                self.assertEqual(self.fake.receives, [])
        self.authority.final_change = None
        (
            self.authority.revoked,
            self.authority.association,
            self.authority.policy,
            self.authority.grant,
        ) = (False, ASSOCIATION, POLICY, GRANT)
        async with self.transport() as (_, _, app):

            async def changed(prepared):
                await command(
                    "/usr/bin/git",
                    "--git-dir=" + str(self.repo),
                    "update-ref",
                    "refs/heads/feature",
                    self.base,
                )

            self.authority.before_dispatch = changed
            self.assertNotEqual((await self.request_body(app, body))[0], 200)
            self.assertEqual(self.fake.receives, [])

    async def test_immutable_body_identity_and_failed_dispatch_record_zero_writes(self):
        new = await self.commit("first")
        body = await self.raw_body(ZERO_OID, new)
        async with self.transport() as (_, _, app):

            async def changed(prepared):
                prepared.body.chmod(0o600)
                with prepared.body.open("ab") as target:
                    target.write(b"tamper")

            self.authority.before_dispatch = changed
            status, data, _ = await self.request_body(app, body)
            self.assertNotEqual(status, 200)
            self.assertIn(b"body_identity_changed", data)
            self.assertEqual(self.fake.receives, [])
            self.authority.before_dispatch = None
            self.authority.fail_state = PublicationState.DISPATCHING
            self.assertNotEqual((await self.request_body(app, body))[0], 200)
            self.assertEqual(self.fake.receives, [])

    async def test_loss_extra_reflection_and_durable_outcome_failure_unknown_no_replay(
        self,
    ):
        for mode in ("partial", "before", "loss", "extra", "reflect", "durability"):
            if mode == "partial":
                import hashlib

                (self.client / "file").write_text(
                    "".join(
                        hashlib.sha256(str(i).encode()).hexdigest() for i in range(8192)
                    )
                )
            self.authority.states.clear()
            self.authority.history.clear()
            self.fake.mode = "normal" if mode == "durability" else mode
            self.authority.fail_state = (
                PublicationState.CONFIRMED if mode == "durability" else None
            )
            new = await self.commit(mode)
            remote = await command(
                "/usr/bin/git",
                "--git-dir=" + str(self.repo),
                "rev-parse",
                "--verify",
                "refs/heads/feature",
                success=False,
            )
            old = remote[1].strip().decode() if remote[0] == 0 else ZERO_OID
            body = await self.raw_body(old, new)
            async with self.transport() as (_, _, app):
                count = len(self.fake.receives)
                status, data, _ = await self.request_body(app, body)
                self.assertEqual(status, 503, data)
                self.assertEqual(
                    set(self.authority.states.values()), {PublicationState.UNKNOWN}
                )
                self.assertEqual(len(self.fake.receives), count + 1)
                if mode == "partial":
                    self.assertLess(len(self.fake.receives[-1]), len(body))
                    self.assertTrue(body.startswith(self.fake.receives[-1]))
                # A rotated capability cannot clear the unresolved fixture ledger.
                self.authority.grant = self.authority.grant.model_copy(
                    update={"version": self.authority.grant.version + 1}
                )
                remote = await command(
                    "/usr/bin/git",
                    "--git-dir=" + str(self.repo),
                    "rev-parse",
                    "--verify",
                    "refs/heads/feature",
                    success=False,
                )
                current = remote[1].strip().decode() if remote[0] == 0 else ZERO_OID
                retry_tip = await self.commit("retry-" + mode)
                fresh_body = await self.raw_body(current, retry_tip)
                status, data, _ = await self.request_body(app, fresh_body)
                self.assertNotEqual(status, 200)
                self.assertIn(b"publication_unresolved", data)
                self.assertEqual(len(self.fake.receives), count + 1)

    async def test_200_ng_is_rejected_and_local_validation_never_confirms(self):
        self.fake.mode = "ng"
        new = await self.commit("first")
        body = await self.raw_body(ZERO_OID, new)
        async with self.transport() as (_, _, app):
            status, data, _ = await self.request_body(app, body)
            self.assertEqual(status, 200, data)
            self.assertFalse(receipt(data, "refs/heads/feature", sideband=True))
            self.assertEqual(
                set(self.authority.states.values()), {PublicationState.REJECTED}
            )
            self.assertEqual(len(self.fake.receives), 1)
            rc, _, _ = await command(
                "/usr/bin/git",
                "--git-dir=" + str(self.repo),
                "rev-parse",
                "--verify",
                "refs/heads/feature",
                success=False,
            )
            self.assertNotEqual(rc, 0)

    async def test_cancellation_waits_for_unknown_record_under_lock_without_resend(
        self,
    ):
        self.fake.mode = "wait"
        new = await self.commit("first")
        body = await self.raw_body(ZERO_OID, new)
        async with self.transport() as (_, _, app):
            task = asyncio.create_task(self.request_body(app, body))
            await asyncio.wait_for(self.fake.dispatched.wait(), 10)
            task.cancel()
            await asyncio.sleep(0)
            self.assertTrue(self.authority.lock.locked())
            self.fake.release.set()
            with self.assertRaises(asyncio.CancelledError):
                await task
            self.assertEqual(
                set(self.authority.states.values()), {PublicationState.UNKNOWN}
            )
            self.assertEqual(len(self.fake.receives), 1)
            self.assertFalse(self.authority.lock.locked())
