"""Offline resolution, wire compatibility and pinned create recovery."""

import os
import unittest
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch

from mainloop.db import environments as store
from mainloop.environments.resolution import resolve
from mainloop.runtime import native_sessions as ns
from mainloop.runtime import workspaces
from mainloop.runtime.kagent_client import (
    AgentRef,
    DevelopmentEnvironment,
    KagentClient,
    KagentError,
    KagentSession,
    OutcomeUnknown,
    RuntimeComposition,
    RuntimeOperation,
    RuntimeState,
    _field_bytes,
    _field_str,
    _field_varint,
    _varint,
    decode_session_response,
)

from models.environment import EnvironmentVersion
from models.workspace import WorkspaceObservedState


def validated_version():
    return EnvironmentVersion(
        id="v1",
        environment_id="env",
        registry="ghcr.io",
        repository="example/dev",
        platform_manifest_digest="sha256:" + "a" * 64,
        config_digest="sha256:" + "b" * 64,
        architecture="arm64",
        declared_user="65532:65532",
        validation_status="static_validated",
        validator_version="oci-static-v2",
        provenance_kind="user_pushed",
    )


class ResolutionTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.env = SimpleNamespace(id="env", accepted_default_version_id="v1")
        self.selected = SimpleNamespace(
            environment_id="env", version_id=None, follow_default=True
        )
        self.version = validated_version()
        for name, value in (
            ("project", None),
            ("selection", self.selected),
            ("environment", self.env),
            ("access", True),
            ("version", self.version),
        ):
            mock = AsyncMock(return_value=value)
            setattr(self, name + "_mock", mock)
            patcher = patch.object(store, name, mock)
            patcher.start()
            self.addCleanup(patcher.stop)
        patcher = patch.dict(
            os.environ, {"WORKSPACE_DEVELOPMENT_PLATFORM": "linux/arm64"}
        )
        patcher.start()
        self.addCleanup(patcher.stop)

    async def test_follow_default_and_explicit(self):
        first = await resolve(None, "project", "owner")
        self.assertEqual(first.policy_identity, "v1:oci-static-v2")
        self.assertEqual(first.image, "ghcr.io/example/dev@sha256:" + "a" * 64)
        self.selected.follow_default = False
        self.selected.version_id = "v1"
        self.env.accepted_default_version_id = "v2"
        self.assertEqual(await resolve(None, "project", "owner"), first)

    async def test_no_selection(self):
        self.selection_mock.return_value = None
        self.assertIsNone(await resolve(None, "project", "owner"))
        self.environment_mock.assert_not_called()

    async def test_v1_validation_is_stale_for_default_and_explicit_selection(self):
        previous = self.version.model_copy(
            update={"validator_version": "oci-static-v1"}
        )
        self.version_mock.return_value = previous
        for follow_default in (True, False):
            with self.subTest(follow_default=follow_default):
                self.selected.follow_default = follow_default
                self.selected.version_id = None if follow_default else "v1"
                with self.assertRaisesRegex(store.EnvironmentError, "policy is stale"):
                    await resolve(None, "project", "owner")
                self.assertEqual(previous.validator_version, "oci-static-v1")
                self.assertEqual(previous.validation_status, "static_validated")

    async def test_revoked_pending_missing_default_and_platform(self):
        self.access_mock.return_value = False
        with self.assertRaisesRegex(store.EnvironmentError, "revoked"):
            await resolve(None, "project", "owner")
        self.version_mock.assert_not_called()
        self.access_mock.return_value = True
        self.version_mock.return_value = self.version.model_copy(
            update={"validation_status": "pending_build"}
        )
        with self.assertRaisesRegex(store.EnvironmentError, "validated"):
            await resolve(None, "project", "owner")
        self.version_mock.return_value = self.version.model_copy(
            update={"architecture": "amd64"}
        )
        with self.assertRaisesRegex(store.EnvironmentError, "platform"):
            await resolve(None, "project", "owner")
        self.version_mock.return_value = self.version.model_copy(
            update={"validator_version": "old-policy"}
        )
        with self.assertRaisesRegex(store.EnvironmentError, "stale"):
            await resolve(None, "project", "owner")
        self.env.accepted_default_version_id = None
        with self.assertRaisesRegex(store.EnvironmentError, "default"):
            await resolve(None, "project", "owner")


class WireTests(unittest.IsolatedAsyncioTestCase):
    async def test_legacy_bytes_and_selected_field(self):
        client = KagentClient("http://kagent.test", user_id="mainloop")
        agent = AgentRef("kagent", "dev")
        session = KagentSession(
            "wire-session",
            RuntimeState.READY,
            RuntimeOperation.NONE,
            "wire-session",
            agent=agent,
        )
        call = AsyncMock(return_value=session)
        client._session_call = call
        legacy = (
            _field_bytes(5, agent.encode())
            + _field_str(3, "request")
            + _field_str(4, "")
        )
        await client.create_session(agent, request_id="request")
        self.assertEqual(call.call_args.args[1], legacy)
        env = DevelopmentEnvironment(
            "ghcr.io/example/dev@sha256:" + "a" * 64, "linux/arm64", "v1:oci-static-v1"
        )
        call.return_value = KagentSession(
            "wire-session",
            RuntimeState.READY,
            RuntimeOperation.NONE,
            "wire-session",
            agent=agent,
            development_environment=env,
        )
        await client.create_session(
            agent, request_id="request", development_environment=env
        )
        self.assertEqual(call.call_args.args[1], legacy + _field_bytes(8, env.encode()))
        runtime = (
            _field_str(1, "payload@sha256:" + "b" * 64)
            + _field_str(2, "codex")
            + _field_varint(3, 1)
            + _field_str(4, "1.0")
        )
        session = decode_session_response(
            _field_bytes(
                1,
                _field_str(1, "session")
                + _field_bytes(18, env.encode())
                + _field_bytes(19, runtime),
            )
        )
        self.assertEqual(session.development_environment, env)
        self.assertEqual(session.runtime_composition.cli_version, "1.0")

    async def test_replacement_and_recovery_use_persisted_resolution(self):
        from tests.runtime.test_native_sessions import MemoryLedger

        ledger = MemoryLedger()
        ledger.development_environment = dict(
            image="ghcr.io/example/dev@sha256:" + "a" * 64,
            platform="linux/arm64",
            policy_identity="v1:oci-static-v1",
        )
        session = KagentSession(
            "session",
            RuntimeState.READY,
            RuntimeOperation.NONE,
            "session",
            agent=ns.agent_ref("claude", "agent"),
            development_environment=DevelopmentEnvironment(
                **ledger.development_environment
            ),
            runtime_composition=RuntimeComposition("payload", "codex", 1, "1.0"),
        )
        client = SimpleNamespace(create_session=AsyncMock(return_value=session))
        binding = dict(
            session_id="session",
            kind="claude",
            role="agent",
            kagent_request_id="original",
        )
        with patch.object(ns, "ledger", ledger), patch.object(
            ns, "get_client", return_value=client
        ):
            await ns._create_session_with_credentials(binding, ())
            await ns._create_session_with_credentials(binding, ())
            binding["kagent_request_id"] = "replacement"
            await ns._create_session_with_credentials(binding, ())
        calls = client.create_session.call_args_list
        self.assertEqual(calls[0], calls[1])
        self.assertEqual(
            calls[2].kwargs["development_environment"],
            calls[0].kwargs["development_environment"],
        )
        self.assertEqual(ledger.reported_composition, session)


def malformed_composition_fields():
    for number in (18, 19):
        yield f"field-{number}-varint", _field_varint(number, 1)
        for wire, length in ((1, 8), (5, 4)):
            yield f"field-{number}-fixed-{wire}", _varint(number << 3 | wire) + bytes(
                length
            )
        yield f"field-{number}-truncated-string", _field_bytes(number, b"\x0a\x02x")
        yield f"field-{number}-truncated-varint", _field_bytes(number, b"\x80")
        yield f"field-{number}-invalid-utf8", _field_bytes(
            number, _field_bytes(1, b"\xff")
        )
        yield f"field-{number}-wrong-nested-string", _field_bytes(
            number, _field_varint(1, 1)
        )
        yield f"field-{number}-nested-fixed-string", _field_bytes(
            number, b"\x0d" + bytes(4)
        )
        yield f"field-{number}-duplicate-string", _field_bytes(
            number, _field_str(1, "a") + _field_str(1, "b")
        )
    yield "runtime-wrong-schema-type", _field_bytes(19, _field_str(3, "1"))
    yield "runtime-schema-overflow", _field_bytes(19, _field_varint(3, 1 << 32))


def composition_reply(field):
    return _field_bytes(1, _field_str(1, "session") + field)


class MalformedCompositionTests(unittest.IsolatedAsyncioTestCase):
    async def test_malformed_fields_are_recoverable_without_rejection_status(self):
        for name, field in malformed_composition_fields():
            with self.subTest(name=name):
                with self.assertRaises(OutcomeUnknown) as caught:
                    decode_session_response(composition_reply(field))
                self.assertIsInstance(caught.exception, KagentError)
                self.assertIsNone(getattr(caught.exception, "grpc_status", None))

    async def test_observation_returns_unknown_for_malformed_composition(self):
        client = KagentClient("http://kagent.test", user_id="mainloop")
        self.addAsyncCleanup(client.aclose)
        with patch.object(ns, "get_client", return_value=client):
            for name, field in malformed_composition_fields():
                with self.subTest(name=name), patch.object(
                    client,
                    "_session_frames",
                    AsyncMock(return_value=[composition_reply(field)]),
                ):
                    state, detail = await workspaces._observe("session")
                    self.assertEqual(state, WorkspaceObservedState.UNKNOWN)
                    self.assertIn("Malformed", detail)

    async def test_absent_fields_and_unknown_nested_fields_stay_compatible(self):
        legacy = decode_session_response(composition_reply(b""))
        self.assertIsNone(legacy.development_environment)
        self.assertIsNone(legacy.runtime_composition)
        session = decode_session_response(
            composition_reply(
                _field_bytes(19, _field_varint(3, 1) + _field_str(100, "future"))
            )
        )
        self.assertEqual(session.runtime_composition.schema, 1)
