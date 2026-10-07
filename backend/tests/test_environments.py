"""Static OCI validation with fixture bytes only."""

import hashlib
import json
import unittest

import httpx
from mainloop.environments.registry import (
    AnonymousOCIRegistry,
    FakeRegistry,
    RegistryError,
    refresh,
    validate,
)

from models.environment import (
    DevEnvironment,
    EnvironmentVersion,
    RegisterEnvironment,
    SelectEnvironment,
)


def blob(value):
    raw = json.dumps(value).encode()
    return raw, "sha256:" + hashlib.sha256(raw).hexdigest()


def image(user="65532:65532", architecture="amd64", index=True):
    config, config_digest = blob(
        {
            "architecture": architecture,
            "os": "linux",
            "config": {} if user is None else {"User": user},
        }
    )
    manifest, manifest_digest = blob(
        {"schemaVersion": 2, "config": {"digest": config_digest, "size": len(config)}}
    )
    root, root_digest = (
        blob(
            {
                "schemaVersion": 2,
                "manifests": [
                    {
                        "digest": manifest_digest,
                        "size": len(manifest),
                        "platform": {"os": "linux", "architecture": architecture},
                    }
                ],
            }
        )
        if index
        else (manifest, manifest_digest)
    )
    objects = {
        ("ghcr.io", "oldsj/dev", "manifests", root_digest): root,
        ("ghcr.io", "oldsj/dev", "manifests", manifest_digest): manifest,
        ("ghcr.io", "oldsj/dev", "blobs", config_digest): config,
        ("ghcr.io", "oldsj/dev", "manifests", "latest"): root,
    }
    return FakeRegistry(objects), f"ghcr.io/oldsj/dev@{root_digest}"


class ValidationTests(unittest.IsolatedAsyncioTestCase):
    async def test_platforms_and_single_manifests(self):
        for architecture in ("arm64", "amd64"):
            for index in (True, False):
                client, reference = image(architecture=architecture, index=index)
                value = await validate(
                    client, reference, architecture, ["ghcr.io"], "env", "v1"
                )
                self.assertEqual(value.validation_status, "static_validated")
                self.assertEqual(value.declared_user, "65532:65532")
                self.assertEqual(value.architecture, architecture)
                self.assertEqual(bool(value.index_digest), index)
                self.assertEqual(value.capability_profile, {})
                with self.assertRaises(ValueError):
                    value.declared_user = "root"

    async def test_reject_users(self):
        for user in (
            "root",
            "0:0",
            "1000:1000",
            "nonroot",
            None,
            "65532",
            "65532:65532 ",
        ):
            client, reference = image(user)
            with self.assertRaisesRegex(RegistryError, "Declared USER must be exactly"):
                await validate(client, reference, "amd64", ["ghcr.io"], "env", "v")

    async def test_reject_before_registry_call(self):
        client, reference = image()
        for invalid in (
            reference.replace("ghcr.io", "docker.io"),
            "ghcr.io/oldsj/dev:latest",
            reference.replace("oldsj/dev", "../dev"),
        ):
            with self.assertRaises(RegistryError):
                await validate(client, invalid, "amd64", ["ghcr.io"], "env", "v")
        self.assertEqual(client.calls, [])

    async def test_tampered_config_and_wrong_architecture(self):
        client, reference = image()
        key = next(k for k in client.objects if k[2] == "blobs")
        client.objects[key] = client.objects[key].replace(b"65532", b"10000")
        with self.assertRaisesRegex(RegistryError, "digest mismatch"):
            await validate(client, reference, "amd64", ["ghcr.io"], "env", "v")
        client, reference = image(index=False)
        with self.assertRaisesRegex(RegistryError, "platform"):
            await validate(client, reference, "arm64", ["ghcr.io"], "env", "v")

    async def test_refresh_candidate(self):
        client, reference = image()
        old = await validate(client, reference, "amd64", ["ghcr.io"], "env", "old")
        changed, _ = image(index=False)
        client.objects.update(changed.objects)
        env = DevEnvironment(
            id="env",
            owner_id="owner",
            name="dev",
            source_kind="prebuilt_image",
            watched_tag="latest",
            accepted_default_version_id="old",
        )
        candidate = await refresh(client, env, old, ["ghcr.io"], "new")
        self.assertNotEqual(candidate.platform_manifest_digest, old.index_digest)
        self.assertEqual(env.accepted_default_version_id, "old")
        self.assertEqual(old.id, "old")

    async def test_anonymous_token_and_no_layer_reads(self):
        client, reference = image(index=False)
        calls = []

        def respond(request):
            calls.append(request)
            if request.url.path == "/token":
                self.assertEqual(
                    request.url.params["scope"], "repository:oldsj/dev:pull"
                )
                self.assertNotIn("authorization", request.headers)
                return httpx.Response(200, json={"token": "anonymous-token"})
            if "authorization" not in request.headers:
                return httpx.Response(
                    401,
                    headers={
                        "www-authenticate": 'Bearer realm="https://ghcr.io/token",service="ghcr.io"'
                    },
                )
            kind, ref = request.url.path.split("/")[-2:]
            return httpx.Response(
                200, content=client.objects[("ghcr.io", "oldsj/dev", kind, ref)]
            )

        value = await validate(
            AnonymousOCIRegistry(httpx.MockTransport(respond)),
            reference,
            "amd64",
            ["ghcr.io"],
            "env",
            "v",
        )
        self.assertEqual(value.validation_status, "static_validated")
        self.assertEqual(len(calls), 6)

    async def test_private_and_foreign_auth_origin(self):
        for challenge in (
            'Basic realm="private"',
            'Bearer realm="https://evil.example/token"',
        ):
            calls = []

            def respond(request, calls=calls, challenge=challenge):
                calls.append(request)
                return httpx.Response(401, headers={"www-authenticate": challenge})

            with self.assertRaisesRegex(
                RegistryError, "private images not supported yet"
            ):
                await AnonymousOCIRegistry(httpx.MockTransport(respond)).read(
                    "ghcr.io", "oldsj/dev", "manifests", "latest"
                )
            self.assertEqual(len(calls), 1)

    def test_source_and_selection_models(self):
        request = RegisterEnvironment(
            name="definition",
            source_kind="definition_repo",
            definition={
                "repository": "https://github.com/oldsj/env",
                "commit_sha": "a" * 40,
                "path": ".devcontainer/devcontainer.json",
            },
        )
        self.assertIsNone(request.image)
        for values in ({}, {"follow_default": True, "version_id": "v"}):
            with self.assertRaises(ValueError):
                SelectEnvironment(environment_id="env", expected_version=0, **values)
        with self.assertRaises(ValueError):
            EnvironmentVersion(
                id="v",
                environment_id="env",
                validation_status="static_validated",
                validator_version="v",
                provenance_kind="user_pushed",
            )

    async def test_metadata_limits_and_redirects(self):
        from mainloop.environments.registry import MAX_METADATA_BYTES

        for response in (
            httpx.Response(302, headers={"location": "https://evil.example/config"}),
            httpx.Response(200, content=b"x" * (MAX_METADATA_BYTES + 1)),
        ):
            calls = []

            def respond(request, response=response, calls=calls):
                calls.append(request)
                return response

            with self.assertRaises(RegistryError):
                await AnonymousOCIRegistry(httpx.MockTransport(respond)).read(
                    "ghcr.io", "oldsj/dev", "blobs", "sha256:" + "a" * 64
                )
            self.assertEqual(len(calls), 1)

    async def test_bad_anonymous_token_is_clear_error(self):
        def respond(request):
            if request.url.path == "/token":
                return httpx.Response(200, json=["not a token object"])
            return httpx.Response(
                401,
                headers={"www-authenticate": 'Bearer realm="https://ghcr.io/token"'},
            )

        with self.assertRaisesRegex(RegistryError, "malformed"):
            await AnonymousOCIRegistry(httpx.MockTransport(respond)).read(
                "ghcr.io", "oldsj/dev", "manifests", "latest"
            )

    def test_derived_package_data(self):
        from models import EnvironmentVersion, PackageDeclaration

        declaration = PackageDeclaration(
            manager="apt",
            packages=[{"name": "ripgrep", "requested_version": "14.1.0-1"}],
            policy_id="debian",
            policy_version=1,
        )
        version = EnvironmentVersion(
            id="derived",
            environment_id="env",
            parent_version_id="parent",
            package_declaration=declaration,
            validation_status="pending_build",
            validator_version="v1",
            provenance_kind="mainloop_built",
        )
        self.assertEqual(version.package_declaration.packages[0].name, "ripgrep")
        with self.assertRaises(ValueError):
            PackageDeclaration(
                manager="apt",
                packages=[{"name": "rg; touch /data/pwn"}],
                policy_id="debian",
                policy_version=1,
            )


class RegistryBoundTests(unittest.IsolatedAsyncioTestCase):
    async def test_compression_rejected_before_body_at_every_auth_stage(self):
        import gzip
        from unittest.mock import patch

        class CompressedStream(httpx.AsyncByteStream):
            consumed = False
            closed = False

            async def __aiter__(self):
                self.consumed = True
                yield gzip.compress(b"x" * (16 * 1024 * 1024))

            async def aclose(self):
                self.closed = True

        for stage in ("manifest", "token", "retry"):
            stream = CompressedStream()
            calls = []

            def respond(request, stage=stage, stream=stream, calls=calls):
                self.assertEqual(request.headers["accept-encoding"], "identity")
                calls.append(request)
                current = (
                    "token"
                    if request.url.path == "/token"
                    else "retry" if "authorization" in request.headers else "manifest"
                )
                if current == stage:
                    return httpx.Response(
                        200, headers={"content-encoding": "gzip"}, stream=stream
                    )
                if current == "manifest":
                    return httpx.Response(
                        401,
                        headers={
                            "www-authenticate": 'Bearer realm="https://ghcr.io/token"'
                        },
                    )
                return httpx.Response(200, json={"token": "anonymous-token"})

            with patch.object(
                httpx.Response,
                "aiter_bytes",
                side_effect=AssertionError("Decoder must not run"),
            ):
                with self.assertRaisesRegex(RegistryError, "identity Content-Encoding"):
                    await AnonymousOCIRegistry(httpx.MockTransport(respond)).read(
                        "ghcr.io", "oldsj/dev", "manifests", "latest"
                    )
            self.assertFalse(stream.consumed)
            self.assertTrue(stream.closed)
            self.assertEqual(len(calls), {"manifest": 1, "token": 2, "retry": 3}[stage])

    async def test_buffer_capacity_checked_before_append(self):
        from unittest.mock import patch

        from mainloop.environments import registry

        appended = []

        class ObservedBuffer(bytearray):
            def extend(self, chunk):
                appended.append(len(chunk))
                super().extend(chunk)

        class Stream(httpx.AsyncByteStream):
            async def __aiter__(self):
                yield b"123"
                yield b"456789"

        def respond(request):
            return httpx.Response(200, stream=Stream())

        with patch.object(registry, "MAX_METADATA_BYTES", 8), patch.object(
            registry, "bytearray", ObservedBuffer, create=True
        ):
            with self.assertRaisesRegex(RegistryError, "size limit"):
                await AnonymousOCIRegistry(httpx.MockTransport(respond)).read(
                    "ghcr.io", "oldsj/dev", "manifests", "latest"
                )
        self.assertEqual(appended, [3])

    async def test_paced_stream_hits_elapsed_deadline_and_closes(self):
        import asyncio
        from unittest.mock import patch

        from mainloop.environments import registry

        class PacedStream(httpx.AsyncByteStream):
            chunks = 0
            closed = False

            async def __aiter__(self):
                for _ in range(100):
                    await asyncio.sleep(0.01)
                    self.chunks += 1
                    yield b"x"

            async def aclose(self):
                self.closed = True

        stream = PacedStream()

        def respond(request):
            self.assertEqual(request.extensions["timeout"]["read"], 20)
            return httpx.Response(200, stream=stream)

        with patch.object(registry, "REGISTRY_READ_BUDGET_SECONDS", 0.055):
            with self.assertRaisesRegex(
                RegistryError, "read elapsed-time budget exceeded"
            ):
                await AnonymousOCIRegistry(httpx.MockTransport(respond)).read(
                    "ghcr.io", "oldsj/dev", "manifests", "latest"
                )
        self.assertGreater(stream.chunks, 0)
        self.assertLess(stream.chunks, 100)
        self.assertTrue(stream.closed)

    async def test_token_and_retry_share_read_deadline(self):
        import asyncio
        from unittest.mock import patch

        from mainloop.environments import registry

        calls = []

        async def respond(request):
            calls.append(request.url.path)
            await asyncio.sleep(0.03)
            if request.url.path == "/token":
                return httpx.Response(200, json={"token": "anonymous-token"})
            if "authorization" not in request.headers:
                return httpx.Response(
                    401,
                    headers={
                        "www-authenticate": 'Bearer realm="https://ghcr.io/token"'
                    },
                )
            return httpx.Response(200, content=b"{}")

        with patch.object(registry, "REGISTRY_READ_BUDGET_SECONDS", 0.08):
            with self.assertRaisesRegex(
                RegistryError, "read elapsed-time budget exceeded"
            ):
                await AnonymousOCIRegistry(httpx.MockTransport(respond)).read(
                    "ghcr.io", "oldsj/dev", "manifests", "latest"
                )
        self.assertEqual(len(calls), 3)

    async def test_whole_validation_and_refresh_deadlines(self):
        import asyncio
        from unittest.mock import patch

        from mainloop.environments import registry

        fake, reference = image()
        previous = await validate(fake, reference, "amd64", ["ghcr.io"], "env", "v")
        env = DevEnvironment(
            id="env",
            owner_id="owner",
            name="dev",
            source_kind="prebuilt_image",
            watched_tag="latest",
        )

        class SlowFake(FakeRegistry):
            async def read(self, *args):
                await asyncio.sleep(0.03)
                return await super().read(*args)

        slow = SlowFake(fake.objects)
        with patch.object(registry, "VALIDATION_BUDGET_SECONDS", 0.08):
            with self.assertRaisesRegex(
                RegistryError, "validation elapsed-time budget exceeded"
            ):
                await validate(slow, reference, "amd64", ["ghcr.io"], "env", "v")
        self.assertEqual(len(slow.calls), 2)
        slow.calls.clear()
        # Discovery plus validation exceeds the refresh budget, although validation
        # alone and each individual fake read fit their own limits.
        with patch.object(registry, "REFRESH_BUDGET_SECONDS", 0.11), patch.object(
            registry, "VALIDATION_BUDGET_SECONDS", 1
        ):
            with self.assertRaisesRegex(
                RegistryError, "refresh elapsed-time budget exceeded"
            ):
                await refresh(slow, env, previous, ["ghcr.io"], "new")
        self.assertEqual(len(slow.calls), 3)

    async def test_external_cancellation_is_preserved(self):
        import asyncio
        from unittest.mock import patch

        from mainloop.environments import registry

        entered = asyncio.Event()

        async def respond(request):
            entered.set()
            await asyncio.Event().wait()

        with patch.object(registry, "REGISTRY_READ_BUDGET_SECONDS", 1):
            task = asyncio.create_task(
                AnonymousOCIRegistry(httpx.MockTransport(respond)).read(
                    "ghcr.io", "oldsj/dev", "manifests", "latest"
                )
            )
            await entered.wait()
            task.cancel()
            with self.assertRaises(asyncio.CancelledError):
                await task
