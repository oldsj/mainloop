"""Public GHCR config-only CDN boundary, with deterministic fake transports."""

import asyncio
import logging
import traceback
import unittest
from unittest.mock import patch

import httpx
from mainloop.environments import registry
from tests.test_environments import image

CDN = "https://pkg-containers.githubusercontent.com/config?signature=fake-secret"
CHALLENGE = 'Bearer realm="https://ghcr.io/token",service="ghcr.io"'
DIGEST = "sha256:" + "a" * 64


class ConfigTransport:
    def __init__(self, location=CDN, status=307, initial_status=401, token_status=200):
        self.location = location
        self.status = status
        self.initial_status = initial_status
        self.token_status = token_status
        self.calls = []
        self.cdn_response = lambda request: httpx.Response(200, content=b"{}")

    async def __call__(self, request):
        self.calls.append(request)
        if request.url.host == "pkg-containers.githubusercontent.com":
            return self.cdn_response(request)
        if request.url.path == "/token":
            return httpx.Response(
                self.token_status,
                json={"token": "anonymous-token"},
                headers={
                    "set-cookie": "token-cookie=secret; Domain=.githubusercontent.com"
                },
            )
        if "authorization" not in request.headers:
            return httpx.Response(
                self.initial_status,
                headers={"www-authenticate": CHALLENGE, "location": self.location},
            )
        return httpx.Response(
            self.status,
            headers={
                "location": self.location,
                "set-cookie": "registry-cookie=secret; Domain=.githubusercontent.com",
            },
        )

    async def read(self, host="ghcr.io", kind="blobs", reference=DIGEST):
        return await registry.AnonymousOCIRegistry(httpx.MockTransport(self)).read(
            host, "oldsj/dev", kind, reference
        )


class ConfigCDNTests(unittest.IsolatedAsyncioTestCase):
    async def test_one_hop_and_fresh_client_without_registry_credentials(self):
        for target in (CDN, CDN.replace(".com/", ".com:443/")):
            with self.subTest(target=target):
                transport = ConfigTransport(target)
                # Simulate a populated jar, including a cookie valid for the CDN.
                # Merely deleting an Authorization header would still leak it.
                original = httpx.AsyncClient
                clients = []

                def client(*args, original=original, clients=clients, **kwargs):
                    result = original(*args, **kwargs)
                    clients.append(result)
                    if len(clients) == 1:
                        result.cookies.set(
                            "registry", "secret", domain=".githubusercontent.com"
                        )
                        result.cookies.set("host", "secret", domain="ghcr.io")
                    return result

                with patch.object(httpx, "AsyncClient", client):
                    self.assertEqual(await transport.read(), b"{}")
                self.assertEqual(len(clients), 2)
                self.assertEqual(len(transport.calls), 4)
                initial, token, authenticated, cdn = transport.calls
                self.assertNotIn("authorization", initial.headers)
                self.assertNotIn("authorization", token.headers)
                self.assertEqual(
                    authenticated.headers["authorization"], "Bearer anonymous-token"
                )
                self.assertIn("cookie", authenticated.headers)
                self.assertNotIn("authorization", cdn.headers)
                self.assertNotIn("cookie", cdn.headers)
                self.assertNotIn("proxy-authorization", cdn.headers)
                self.assertEqual(token.url.params["scope"], "repository:oldsj/dev:pull")
                for request in transport.calls:
                    self.assertEqual(request.headers["accept-encoding"], "identity")
                    self.assertEqual(request.extensions["timeout"]["read"], 20)

    async def test_denied_locations_never_requested(self):
        for target in (
            "",
            "https://evil.example/config",
            "http://pkg-containers.githubusercontent.com/config",
            "//pkg-containers.githubusercontent.com/config",
            "/config",
            "https://pkg-containers.githubusercontent.com:80/config",
            "https://pkg-containers.githubusercontent.com:444/config",
            "https://pkg-containers.githubusercontent.com:bad/config",
            "https://pkg-containers.githubusercontent.com:/config",
            "https://pkg-containers.githubusercontent.com.evil.example/config",
            "https://pkg-containers.githubusercontent.com./config",
            "https://user@pkg-containers.githubusercontent.com/config",
            "https://user:pass@pkg-containers.githubusercontent.com/config",
            "https://@pkg-containers.githubusercontent.com/config",
            CDN + "#fragment",
            CDN + "#",
            "https://127.0.0.1/config",
            "https://[::1]/config",
            "https://pkg-containers.githubusercontent.com\t/config",
        ):
            with self.subTest(target=target):
                transport = ConfigTransport(target)
                with self.assertRaisesRegex(
                    registry.RegistryError,
                    "Unsupported registry config redirect|unavailable or malformed",
                ):
                    await transport.read()
                self.assertEqual(len(transport.calls), 3)

    async def test_only_authenticated_ghcr_307_immutable_config(self):
        cases = (
            [
                ({"initial_status": code}, {})
                for code in (301, 302, 303, 307, 308, 403, 404)
            ]
            + [({"status": code}, {}) for code in (301, 302, 303, 308, 401, 403, 404)]
            + [
                ({}, {"kind": "manifests"}),
                ({}, {"kind": "manifests", "reference": "latest"}),
                ({}, {"reference": "latest"}),
                ({}, {"reference": "sha256:bad"}),
                ({"token_status": 307}, {}),
                ({"token_status": 401}, {}),
                ({"token_status": 403}, {}),
            ]
        )
        for options, read_options in cases:
            with self.subTest(options=options, read_options=read_options):
                transport = ConfigTransport(**options)
                with self.assertRaises(registry.RegistryError):
                    await transport.read(**read_options)
                self.assertTrue(all(r.url.host == "ghcr.io" for r in transport.calls))
        # A non-GHCR registry may complete its own anonymous auth but cannot use CDN.
        transport = ConfigTransport()
        original = transport.__call__

        async def non_ghcr(request):
            response = await original(request)
            if response.status_code == 401:
                response.headers["www-authenticate"] = CHALLENGE.replace(
                    "ghcr.io", "other.example"
                )
            return response

        client = registry.AnonymousOCIRegistry(httpx.MockTransport(non_ghcr))
        with self.assertRaisesRegex(registry.RegistryError, "HTTP 307"):
            await client.read("other.example", "oldsj/dev", "blobs", DIGEST)
        self.assertEqual(len(transport.calls), 3)

    async def test_auth_origin_and_token_redirects_never_followed(self):
        for realm in (
            CDN,
            "http://ghcr.io/token",
            "https://ghcr.io:444/token",
            "https://user@ghcr.io/token",
            "https://ghcr.io/token#fragment",
        ):
            calls = []

            def respond(request, calls=calls, realm=realm):
                calls.append(request)
                return httpx.Response(
                    401, headers={"www-authenticate": f'Bearer realm="{realm}"'}
                )

            with self.subTest(realm=realm), self.assertRaisesRegex(
                registry.RegistryError, "unsupported authentication origin"
            ):
                await registry.AnonymousOCIRegistry(httpx.MockTransport(respond)).read(
                    "ghcr.io", "oldsj/dev", "blobs", DIGEST
                )
            self.assertEqual(len(calls), 1)

    async def test_cdn_never_redirects_or_exchanges_auth(self):
        for code in (301, 302, 303, 307, 308, 401, 403, 404):
            with self.subTest(code=code):
                transport = ConfigTransport()
                transport.cdn_response = lambda request, code=code: httpx.Response(
                    code, headers={"location": CDN, "www-authenticate": CHALLENGE}
                )
                with self.assertRaises(registry.RegistryError):
                    await transport.read()
                self.assertEqual(len(transport.calls), 4)

    async def test_cdn_byte_encoding_and_read_budget(self):
        class Stream(httpx.AsyncByteStream):
            consumed = False
            closed = False

            async def __aiter__(self):
                self.consumed = True
                yield b"x" * 40
                yield b"x" * 30

            async def aclose(self):
                self.closed = True

        for encoding in ("identity", "gzip", "br"):
            with self.subTest(encoding=encoding):
                stream = Stream()
                transport = ConfigTransport()
                transport.cdn_response = (
                    lambda request, encoding=encoding, stream=stream: httpx.Response(
                        200, headers={"content-encoding": encoding}, stream=stream
                    )
                )
                appended = []

                class Buffer(bytearray):
                    def extend(self, chunk, appended=appended):
                        appended.append(len(chunk))
                        super().extend(chunk)

                with patch.object(registry, "MAX_METADATA_BYTES", 64), patch.object(
                    registry, "bytearray", Buffer, create=True
                ):
                    with self.assertRaisesRegex(
                        registry.RegistryError, "size limit|identity Content-Encoding"
                    ):
                        await transport.read()
                if encoding == "identity":
                    self.assertEqual(appended, [40])
                    self.assertTrue(stream.consumed)
                    self.assertTrue(stream.closed)
                if encoding != "identity":
                    self.assertFalse(stream.consumed)
                    self.assertTrue(stream.closed)

        transport = ConfigTransport()
        transport.cdn_response = lambda request: httpx.Response(200, content=b"x" * 65)
        with patch.object(registry, "MAX_METADATA_BYTES", 64):
            with self.assertRaisesRegex(registry.RegistryError, "size limit"):
                await transport.read()

        transport = ConfigTransport()
        original = transport.__call__

        async def delayed(request):
            await asyncio.sleep(0.02)
            return await original(request)

        with patch.object(registry, "REGISTRY_READ_BUDGET_SECONDS", 0.07):
            with self.assertRaisesRegex(
                registry.RegistryError, "read elapsed-time budget"
            ):
                await registry.AnonymousOCIRegistry(httpx.MockTransport(delayed)).read(
                    "ghcr.io", "oldsj/dev", "blobs", DIGEST
                )
        self.assertEqual(len(transport.calls), 3)

    async def test_full_index_chain_and_size_digest_user_platform_checks(self):
        for failure in (None, "size", "digest", "user", "platform"):
            with self.subTest(failure=failure):
                fake, reference = image(
                    architecture="arm64",
                    user="root" if failure == "user" else "65532:65532",
                )
                config_key = next(key for key in fake.objects if key[2] == "blobs")
                data = fake.objects[config_key]
                if failure == "size":
                    data += b" "
                if failure == "digest":
                    data = data.replace(b"65532", b"10000")
                if failure == "platform":
                    # A matching immutable manifest with a wrong config platform.
                    from tests.test_environments import blob

                    data, digest = blob(
                        {
                            "architecture": "amd64",
                            "os": "linux",
                            "config": {"User": "65532:65532"},
                        }
                    )
                    manifest, manifest_digest = blob(
                        {
                            "schemaVersion": 2,
                            "config": {"digest": digest, "size": len(data)},
                        }
                    )
                    fake.objects[
                        ("ghcr.io", "oldsj/dev", "manifests", manifest_digest)
                    ] = manifest
                    reference = "ghcr.io/oldsj/dev@" + manifest_digest
                    config_key = ("ghcr.io", "oldsj/dev", "blobs", digest)
                transport = ConfigTransport()
                original = transport.__call__

                async def respond(
                    request,
                    transport=transport,
                    data=data,
                    original=original,
                    fake=fake,
                ):
                    if request.url.host == "pkg-containers.githubusercontent.com":
                        transport.calls.append(request)
                        return httpx.Response(200, content=data)
                    if (
                        request.url.path == "/token"
                        or "authorization" not in request.headers
                        or "/blobs/" in request.url.path
                    ):
                        return await original(request)
                    transport.calls.append(request)
                    kind, ref = request.url.path.split("/")[-2:]
                    return httpx.Response(
                        200, content=fake.objects[("ghcr.io", "oldsj/dev", kind, ref)]
                    )

                client = registry.AnonymousOCIRegistry(httpx.MockTransport(respond))
                if failure:
                    with self.assertRaisesRegex(
                        registry.RegistryError,
                        {
                            "size": "Config size",
                            "digest": "digest mismatch",
                            "user": "Declared USER",
                            "platform": "Config platform",
                        }[failure],
                    ):
                        await registry.validate(
                            client, reference, "arm64", ["ghcr.io"], "env", "v"
                        )
                else:
                    version = await registry.validate(
                        client, reference, "arm64", ["ghcr.io"], "env", "v"
                    )
                    self.assertEqual(version.config_digest, config_key[3])
                    self.assertEqual(version.architecture, "arm64")
                    self.assertEqual(version.declared_user, "65532:65532")
                    self.assertEqual(version.validator_version, "oci-static-v2")
                    self.assertEqual(len(transport.calls), 10)

    async def test_signed_location_excluded_from_logs_and_error_tracebacks(self):
        for fail in (False, True):
            transport = ConfigTransport()

            def cdn(request, fail=fail):
                logging.getLogger("httpcore.http11").debug("signed Location: %s", CDN)
                if fail:
                    raise httpx.ReadError("failed " + CDN, request=request)
                return httpx.Response(200, content=b"{}")

            transport.cdn_response = cdn
            with self.assertLogs(level=logging.DEBUG) as captured:
                if fail:
                    with self.assertRaises(registry.RegistryError) as caught:
                        await transport.read()
                    trace = "".join(traceback.format_exception(caught.exception))
                    self.assertNotIn("fake-secret", trace)
                    self.assertNotIn(CDN, trace)
                else:
                    await transport.read()
                # Context was reset: unrelated HTTPX logs still work.
                logging.getLogger("httpx").info("unrelated request")
            self.assertEqual(captured.output, ["INFO:httpx:unrelated request"])

    async def test_log_suppression_is_local_and_cancellation_restores_it(self):
        entered = asyncio.Event()

        async def respond(request):
            logging.getLogger("httpcore.http11").debug("Location: %s", CDN)
            entered.set()
            await asyncio.Event().wait()

        with self.assertLogs(level=logging.DEBUG) as captured:
            task = asyncio.create_task(
                registry.AnonymousOCIRegistry(httpx.MockTransport(respond)).read(
                    "ghcr.io", "oldsj/dev", "blobs", DIGEST
                )
            )
            await entered.wait()
            logging.getLogger("httpx").info("concurrent request")
            task.cancel()
            with self.assertRaises(asyncio.CancelledError):
                await task
            logging.getLogger("httpx").info("after cancellation")
        self.assertEqual(
            captured.output,
            ["INFO:httpx:concurrent request", "INFO:httpx:after cancellation"],
        )
