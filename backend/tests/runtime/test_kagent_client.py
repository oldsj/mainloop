"""kagent client against a fake gateway (fixture-backed; no network, no live kagent)."""

import hashlib
import json
import unittest
from dataclasses import fields as dataclass_fields
from dataclasses import replace
from pathlib import Path
from unittest.mock import AsyncMock, patch

import httpx
from mainloop.runtime.kagent_client import (
    AGENT_SETUP_DIGEST,
    CHILD_SETUP_DIGEST,
    SUPERVISOR_SETUP_DIGEST,
    A2AError,
    AgentRef,
    DevelopmentEnvironment,
    KagentClient,
    OutcomeUnknown,
    PreparationReceipt,
    PreparationRequest,
    PreparationTimestamp,
    RuntimeComposition,
    RuntimeOperation,
    RuntimeState,
    SendNotAccepted,
    SessionError,
    SessionWorkspace,
    StreamEvent,
    TaskNotFound,
    TaskProjection,
    Unreachable,
    _field_bytes,
    _field_str,
    _field_varint,
    assistant_message_id,
    decode_agent_response,
    decode_fields,
    decode_preparation_response,
    decode_session_response,
    grpc_web_frame,
    is_parked,
    is_terminal,
    normalise_state,
    parse_grpc_web,
)
from tests.runtime.kagent_fake import (
    CONTEXT_ID,
    TASK_ID,
    FakeKagent,
    grpc_response,
    stream_chunks,
)

AGENT = AgentRef("kagent", "claude-subscription")


class ExactSessionIdentityTests(unittest.IsolatedAsyncioTestCase):
    async def test_get_delete_resume_cannot_accept_another_session(self):
        client, _ = make_client(FakeKagent())
        live = await client.create_session(AGENT, request_id="identity-fixture")
        wrong = replace(
            live, id="other", context_id="other", state=RuntimeState.DELETED
        )
        for method in (
            client.get_session,
            client.delete_session,
            client.resume_session,
        ):
            with (
                self.subTest(method=method.__name__),
                patch.object(client, "_session_call", AsyncMock(return_value=wrong)),
            ):
                with self.assertRaises(OutcomeUnknown):
                    await method(live.id)

    async def test_create_checks_returned_agent_workspace_and_environment(self):
        client, _ = make_client(FakeKagent())
        live = await client.create_session(AGENT, request_id="identity-fixture")
        from mainloop.runtime.kagent_client import (
            DevelopmentEnvironment,
            SessionWorkspace,
        )

        for change in (
            {"agent": AgentRef("wrong", "wrong")},
            {
                "workspace": SessionWorkspace(
                    "https://github.com/example/wrong", "a" * 40, "wrong"
                )
            },
            {
                "development_environment": DevelopmentEnvironment(
                    "wrong", "linux/arm64", "wrong"
                )
            },
            {"context_id": "wrong"},
        ):
            with (
                self.subTest(change=change),
                patch.object(
                    client,
                    "_session_call",
                    AsyncMock(return_value=replace(live, **change)),
                ),
            ):
                with self.assertRaises(OutcomeUnknown):
                    await client.create_session(AGENT, request_id="identity-fixture")

    async def test_readiness_checks_known_create_contract_again(self):
        client, _ = make_client(FakeKagent())
        live = await client.create_session(AGENT, request_id="identity-fixture")
        starting = replace(
            live, state=RuntimeState.CREATING, operation=RuntimeOperation.CREATE
        )
        wrong = replace(live, agent=AgentRef("wrong", "wrong"))
        with patch.object(client, "get_session", AsyncMock(return_value=wrong)):
            with self.assertRaises(OutcomeUnknown):
                await client.ensure_ready(starting, interval=0)


def make_client(fake: FakeKagent, *, clock=None) -> tuple[KagentClient, list[float]]:
    """Build a client whose sleeps are recorded and, by default, advance a fake clock."""
    sleeps: list[float] = []
    now = [0.0]

    async def sleep(seconds: float) -> None:
        sleeps.append(seconds)
        now[0] += seconds

    http = httpx.AsyncClient(transport=fake.transport(), base_url="http://kagent.test")
    return (
        KagentClient(
            "http://kagent.test",
            user_id="mainloop",
            client=http,
            sleep=sleep,
            clock=clock or (lambda: now[0]),
        ),
        sleeps,
    )


async def collect(stream) -> list[StreamEvent]:
    return [event async for event in stream]


async def send(client: KagentClient, message_id: str = "m-1") -> list[StreamEvent]:
    return await collect(
        client.send_message(
            AGENT, text="hello", message_id=message_id, context_id=CONTEXT_ID
        )
    )


class RuntimeAssociationWireTests(unittest.TestCase):
    def test_optional_current_association_and_malformed_fields(self):
        from mainloop.runtime.kagent_client import (
            _field_bytes,
            _field_str,
            _field_varint,
            decode_session_response,
        )

        base = _field_str(1, "session") + _field_str(14, "session")
        self.assertIsNone(
            decode_session_response(_field_bytes(1, base)).runtime_association
        )
        association = b"".join(
            _field_str(n, value)
            for n, value in enumerate(
                ("generation", "space", "actor", "uid", "active"), 1
            )
        ) + _field_varint(6, 1)
        decoded = decode_session_response(
            _field_bytes(1, base + _field_bytes(20, association))
        )
        self.assertEqual(decoded.runtime_association.actor_uid, "uid")
        self.assertTrue(decoded.runtime_association.current_active)
        invalid = (
            _field_bytes(20, association) * 2,
            _field_varint(20, 1),
            _field_bytes(20, association + _field_str(1, "duplicate")),
            _field_bytes(20, association[:-2] + _field_varint(6, 2)),
            _field_bytes(20, _field_bytes(1, b"\xff") + association),
            _field_bytes(20, association[12:]),
            _field_bytes(20, b""),
            _field_bytes(20, _field_str(1, "x" * 257) + association),
        )
        for raw in invalid:
            with self.subTest(raw=raw[:20]), self.assertRaises(OutcomeUnknown):
                decode_session_response(_field_bytes(1, base + raw))


def preparation_request() -> PreparationRequest:
    return PreparationRequest(
        session_id="00000000-0000-4000-8000-000000000001",
        action_id="prep:fixture",
        create_request_id="create-fixture",
        generation_id="00000000-0000-4000-8000-000000000002",
        actor_uid="fixture-uid",
        prepared_revision="fixture-revision",
        workspace=SessionWorkspace(
            "https://github.com/example/repo", "a" * 40, "feature", 7
        ),
        development_environment=DevelopmentEnvironment(
            "example/dev@sha256:" + "b" * 64, "linux/amd64", "fixture-policy"
        ),
        runtime_composition=RuntimeComposition(
            "example/payload@sha256:" + "c" * 64, "codex", 1, "0.148.0"
        ),
        setup_profile="child",
        setup_digest=CHILD_SETUP_DIGEST,
    )


def preparation_receipt_message(
    *, classification="confirmed", historical=False, original=None
) -> bytes:
    """Synthetic receipt using all fields from sessions.proto at d6de0e40."""
    return (
        _field_bytes(1, (original or preparation_request()).encode())
        + b"".join(
            _field_str(n, value)
            for n, value in {
                2: "d" * 64,
                3: "execution-fixture",
                4: preparation_request().session_id,
                5: "fixture-space",
                6: "fixture-actor",
                7: classification,
                8: "challenge-fixture",
                10: "a" * 40,
                11: "feature",
                12: "e" * 64,
                13: "f" * 64,
                14: "0" * 64,
                15: "developer_instruction",
            }.items()
        )
        + _field_varint(9, 12)
        + _field_bytes(
            16, _field_varint(1, 1_700_000_000) + _field_varint(2, 123_456_789)
        )
        + _field_bytes(
            17, _field_varint(1, 1_700_000_001) + _field_varint(2, 987_654_321)
        )
        + (_field_varint(18, 1) if historical else b"")
    )


def replace_wire_field(raw: bytes, number: int, replacement: bytes) -> bytes:
    return (
        b"".join(
            (
                _field_bytes(n, value)
                if isinstance(value, bytes)
                else _field_varint(n, value)
            )
            for n, values in decode_fields(raw).items()
            if n != number
            for value in values
        )
        + replacement
    )


class PreparationWireTests(unittest.TestCase):
    def test_preparation_workspace_depth_accepts_inclusive_boundaries(self):
        original = preparation_request()
        for depth in (0, 1000):
            request = replace(
                original, workspace=replace(original.workspace, depth=depth)
            )
            with self.subTest(depth=depth):
                self.assertEqual(PreparationRequest.decode(request.encode()), request)

    def test_setup_digests_match_verified_kagent_standing_fixture(self):
        fixture = json.loads(
            (
                Path(__file__).parent / "fixtures/kagent/preparation-standing.json"
            ).read_text()
        )
        self.assertEqual(
            fixture["revision"], "796e90b53f3e48bfd0353133641debb2c78344fd"
        )
        self.assertEqual(
            fixture["source"], "go/harness/runtime/workspace/preparation.go"
        )
        self.assertEqual(set(fixture["standing"]), {"supervisor", "child", "agent"})
        for profile, digest in (
            ("supervisor", SUPERVISOR_SETUP_DIGEST),
            ("child", CHILD_SETUP_DIGEST),
            ("agent", AGENT_SETUP_DIGEST),
        ):
            text = fixture["standing"][profile]
            self.assertTrue(
                text.startswith(f"# Mainloop standing context ({profile})\n\n")
            )
            self.assertTrue(
                text.endswith("Your tools come from the `mainloop` MCP server.\n")
            )
            self.assertEqual(hashlib.sha256(text.encode()).hexdigest(), digest)

    def test_preparation_profile_mapping_uses_binding_roles_only(self):
        from mainloop.push_gate.credentials import preparation_profile_for_binding_role

        for role, digest in (
            ("agent", AGENT_SETUP_DIGEST),
            ("supervisor", SUPERVISOR_SETUP_DIGEST),
            ("child", CHILD_SETUP_DIGEST),
        ):
            self.assertEqual(preparation_profile_for_binding_role(role), (role, digest))
        for role in ("main", "owner", "", "unknown"):
            with self.assertRaisesRegex(ValueError, "git_prepare_role_unsupported"):
                preparation_profile_for_binding_role(role)

    def test_composition_encoding_fields_defaults_and_uint32_bounds(self):
        value = preparation_request().runtime_composition
        self.assertEqual(
            decode_fields(value.encode()),
            {1: [value.payload_image.encode()], 2: [b"codex"], 3: [1], 4: [b"0.148.0"]},
        )
        for composition in (
            value,
            RuntimeComposition("", "", 0, ""),
            replace(value, schema=0xFFFFFFFF),
        ):
            self.assertEqual(
                RuntimeComposition.decode(composition.encode()), composition
            )
        self.assertEqual(RuntimeComposition("", "", 0, "").encode(), b"")
        for schema in (-1, 1 << 32):
            with self.subTest(schema=schema), self.assertRaises(ValueError):
                replace(value, schema=schema).encode()

    def test_prepare_request_fields_and_nested_round_trip(self):
        value = preparation_request()
        encoded = value.encode()
        self.assertEqual(PreparationRequest.decode(encoded), value)
        self.assertEqual(PreparationRequest.decode(encoded).encode(), encoded)
        self.assertEqual(
            decode_fields(encoded),
            {
                1: [value.session_id.encode()],
                2: [b"prep:fixture"],
                3: [b"create-fixture"],
                4: [value.generation_id.encode()],
                5: [b"fixture-uid"],
                6: [b"fixture-revision"],
                7: [value.workspace.encode()],
                8: [value.development_environment.encode()],
                9: [value.runtime_composition.encode()],
                10: [b"child"],
                11: [CHILD_SETUP_DIGEST.encode()],
            },
        )
        self.assertEqual(
            decode_fields(value.workspace.encode()),
            {
                1: [b"https://github.com/example/repo"],
                2: [b"a" * 40],
                3: [b"feature"],
                4: [7],
            },
        )
        self.assertEqual(
            decode_fields(value.development_environment.encode()),
            {
                1: [("example/dev@sha256:" + "b" * 64).encode()],
                2: [b"linux/amd64"],
                3: [b"fixture-policy"],
            },
        )

    def test_receipt_decodes_every_field_and_keeps_nanoseconds(self):
        receipt = PreparationReceipt.decode(
            preparation_receipt_message(historical=True)
        )
        self.assertEqual(
            receipt,
            PreparationReceipt(
                original=preparation_request(),
                request_digest="d" * 64,
                execution_id="execution-fixture",
                context_id=preparation_request().session_id,
                atespace="fixture-space",
                actor_name="fixture-actor",
                classification="confirmed",
                challenge_id="challenge-fixture",
                observation_sequence=12,
                head="a" * 40,
                branch="feature",
                transport_digest="e" * 64,
                config_digest="f" * 64,
                mcp_digest="0" * 64,
                native_hook="developer_instruction",
                effect_observed_at=PreparationTimestamp(1_700_000_000, 123_456_789),
                observed_at=PreparationTimestamp(1_700_000_001, 987_654_321),
                historical=True,
            ),
        )

    def test_pending_receipt_optional_scalars_and_unknown_fields(self):
        raw = (
            _field_bytes(1, preparation_request().encode())
            + _field_str(7, "pending")
            + _field_varint(99, 1)
        )
        receipt = PreparationReceipt.decode(raw)
        self.assertEqual(receipt.classification, "pending")
        self.assertEqual(receipt.observation_sequence, 0)
        self.assertFalse(receipt.historical)
        self.assertIsNone(receipt.effect_observed_at)
        self.assertIsNone(receipt.observed_at)
        for classification in ("pending", "uncertain", "confirmed", "definite-failure"):
            self.assertEqual(
                PreparationReceipt.decode(
                    preparation_receipt_message(classification=classification)
                ).classification,
                classification,
            )

    def test_session_receipt_field_21_and_absence(self):
        base = _field_str(1, preparation_request().session_id) + _field_str(
            14, preparation_request().session_id
        )
        self.assertIsNone(
            decode_session_response(_field_bytes(1, base)).workspace_preparation
        )
        raw = preparation_receipt_message(historical=True)
        session = decode_session_response(_field_bytes(1, base + _field_bytes(21, raw)))
        self.assertEqual(session.workspace_preparation, PreparationReceipt.decode(raw))
        for invalid in (
            _field_bytes(21, raw) * 2,
            _field_varint(21, 1),
            b"\xad\x01" + b"\x00" * 4,
            b"\xa9\x01" + b"\x00" * 8,
            _field_bytes(21, b""),
            _field_bytes(21, raw[:-1]),
        ):
            with self.subTest(raw=invalid[:20]), self.assertRaises(OutcomeUnknown):
                decode_session_response(_field_bytes(1, base + invalid))

    def test_receipt_rejects_duplicate_wrong_wire_and_invalid_utf8_fields(self):
        raw = preparation_receipt_message(historical=True)
        for number in range(1, 19):
            value = decode_fields(raw)[number][0]
            duplicate = (
                _field_bytes(number, value)
                if isinstance(value, bytes)
                else _field_varint(number, value)
            )
            wrong = (
                _field_varint(number, 1)
                if isinstance(value, bytes)
                else _field_str(number, "wrong")
            )
            for invalid in (raw + duplicate, replace_wire_field(raw, number, wrong)):
                with (
                    self.subTest(number=number, invalid=invalid[-20:]),
                    self.assertRaises(OutcomeUnknown),
                ):
                    PreparationReceipt.decode(invalid)
        for number in (*range(2, 9), *range(10, 16)):
            with self.subTest(number=number), self.assertRaises(OutcomeUnknown):
                PreparationReceipt.decode(
                    replace_wire_field(raw, number, _field_bytes(number, b"\xff"))
                )
        for number, value in ((9, 1 << 64), (18, 2)):
            with self.subTest(number=number), self.assertRaises(OutcomeUnknown):
                PreparationReceipt.decode(
                    replace_wire_field(raw, number, _field_varint(number, value))
                )

    def test_original_rejects_duplicate_wrong_wire_utf8_and_missing_selection(self):
        raw = preparation_request().encode()
        for number in range(1, 12):
            for invalid in (
                raw + _field_bytes(number, b"duplicate"),
                replace_wire_field(raw, number, _field_varint(number, 1)),
            ):
                with self.subTest(number=number), self.assertRaises(OutcomeUnknown):
                    PreparationRequest.decode(invalid)
        for number in (1, 2, 3, 4, 5, 6, 10, 11):
            with self.subTest(number=number), self.assertRaises(OutcomeUnknown):
                PreparationRequest.decode(
                    replace_wire_field(raw, number, _field_bytes(number, b"\xff"))
                )
        for number in (7, 8, 9):
            with self.subTest(number=number), self.assertRaises(OutcomeUnknown):
                PreparationRequest.decode(replace_wire_field(raw, number, b""))

    def test_original_rejects_malformed_nested_selection(self):
        raw = preparation_request().encode()
        for number, nested in (
            (7, _field_str(1, "duplicate") * 2),
            (7, _field_varint(1, 1)),
            (7, _field_bytes(1, b"\xff")),
            (7, _field_varint(4, 1 << 31)),
            (7, _field_str(4, "wrong")),
            (7, _field_varint(4, 1) * 2),
            (8, _field_str(1, "duplicate") * 2),
            (8, _field_varint(2, 1)),
            (8, _field_bytes(3, b"\xff")),
            (9, _field_str(1, "duplicate") * 2),
            (9, _field_str(3, "wrong")),
            (9, _field_varint(3, 1 << 32)),
            (9, _field_bytes(4, b"\xff")),
        ):
            with (
                self.subTest(number=number, nested=nested),
                self.assertRaises(OutcomeUnknown),
            ):
                PreparationRequest.decode(
                    replace_wire_field(raw, number, _field_bytes(number, nested))
                )

    def test_timestamp_presence_signed_seconds_and_invalid_fields(self):
        self.assertEqual(PreparationTimestamp.decode(b""), PreparationTimestamp(0, 0))
        self.assertEqual(
            PreparationTimestamp.decode(_field_varint(1, (1 << 64) - 1)),
            PreparationTimestamp(-1, 0),
        )
        for invalid in (
            _field_varint(1, 1) * 2,
            _field_varint(2, 1) * 2,
            _field_str(1, "wrong"),
            _field_str(2, "wrong"),
            _field_varint(2, 1_000_000_000),
            _field_varint(1, 1 << 64),
            _field_varint(1, 253402300800),
            b"\x08",
        ):
            with self.subTest(raw=invalid), self.assertRaises(OutcomeUnknown):
                PreparationTimestamp.decode(invalid)

    def test_prepare_response_uses_its_own_receipt_decoder(self):
        raw = preparation_receipt_message()
        self.assertEqual(
            decode_preparation_response(
                _field_bytes(1, raw) + _field_str(99, "future")
            ),
            PreparationReceipt.decode(raw),
        )
        for invalid in (
            b"",
            _field_str(99, "future"),
            _field_bytes(1, b""),
            _field_bytes(1, raw) * 2,
            _field_varint(1, 1),
            b"\x0d" + b"\x00" * 4,
            b"\x09" + b"\x00" * 8,
            _field_bytes(1, raw)[:-1],
            _field_bytes(1, _field_str(1, "session")),
            b"\x80",
        ):
            with self.subTest(raw=invalid[:20]), self.assertRaises(OutcomeUnknown):
                decode_preparation_response(invalid)


class PreparationServiceTests(unittest.IsolatedAsyncioTestCase):
    async def prepare(self, client, *, request=None):
        request = request or preparation_request()
        kwargs = {
            field.name: getattr(request, field.name)
            for field in dataclass_fields(request)
            if field.name != "session_id"
        }
        return await client.prepare_session_workspace(request.session_id, **kwargs)

    async def test_invalid_workspace_depth_rejects_before_encoding_or_http(self):
        calls = []

        def handler(request):
            calls.append(request)
            return grpc_response(None)

        async with httpx.AsyncClient(
            transport=httpx.MockTransport(handler), base_url="http://k.test"
        ) as http:
            client = KagentClient("http://k.test", user_id="mainloop", client=http)
            original = preparation_request()
            for depth in (-1, -(1 << 31), 1001, (1 << 31) - 1):
                request = replace(
                    original, workspace=replace(original.workspace, depth=depth)
                )
                with (
                    self.subTest(depth=depth),
                    patch(
                        "mainloop.runtime.kagent_client._field_str",
                        side_effect=AssertionError(
                            "invalid preparation must not begin encoding"
                        ),
                    ) as encode_field,
                ):
                    with self.assertRaisesRegex(
                        ValueError, "depth must be between 0 and 1000"
                    ):
                        await self.prepare(client, request=request)
                    encode_field.assert_not_called()
                    self.assertEqual(calls, [])

    async def test_prepare_uses_session_grpc_web_path_and_exact_request(self):
        calls = []

        def handler(request):
            calls.append(request)
            return grpc_response(
                _field_bytes(1, preparation_receipt_message(classification="pending"))
            )

        async with httpx.AsyncClient(
            transport=httpx.MockTransport(handler), base_url="http://k.test"
        ) as http:
            result = await self.prepare(
                KagentClient("http://k.test", user_id="mainloop", client=http)
            )
        self.assertEqual(result.classification, "pending")
        self.assertEqual(len(calls), 1)
        self.assertEqual(
            calls[0].url.path,
            "/kagent.api.v1alpha1.SessionService/PrepareSessionWorkspace",
        )
        self.assertEqual(calls[0].headers["content-type"], "application/grpc-web+proto")
        self.assertEqual(calls[0].headers["x-user-id"], "mainloop")
        self.assertEqual(
            calls[0].content, grpc_web_frame(preparation_request().encode())
        )

    async def test_mismatched_original_or_context_is_an_unknown_outcome(self):
        receipt = preparation_receipt_message()
        wrong_context = replace_wire_field(receipt, 4, _field_str(4, "other"))
        for response in (
            wrong_context,
            *(
                preparation_receipt_message(
                    original=replace(preparation_request(), **change)
                )
                for change in (
                    {"session_id": "other"},
                    {"action_id": "other"},
                    {"generation_id": "other"},
                    {
                        "workspace": replace(
                            preparation_request().workspace, branch="other"
                        )
                    },
                    {
                        "runtime_composition": replace(
                            preparation_request().runtime_composition, provider="claude"
                        )
                    },
                )
            ),
        ):
            with self.subTest(response=response[-20:]):
                async with httpx.AsyncClient(
                    transport=httpx.MockTransport(
                        lambda request, response=response: grpc_response(
                            _field_bytes(1, response)
                        )
                    ),
                    base_url="http://k.test",
                ) as http:
                    with self.assertRaises(OutcomeUnknown):
                        await self.prepare(
                            KagentClient(
                                "http://k.test", user_id="mainloop", client=http
                            )
                        )

    async def test_errors_are_not_retried_and_keep_session_error_classification(self):
        cases = [
            (grpc_response(None, status=6), SessionError),
            (grpc_response(None, status=3), SessionError),
            *(
                (grpc_response(None, status=status), OutcomeUnknown)
                for status in (4, 10, 13, 14)
            ),
            (httpx.Response(500), OutcomeUnknown),
            (grpc_response(None), OutcomeUnknown),
            (grpc_response(b""), OutcomeUnknown),
            (httpx.ReadTimeout("lost reply"), OutcomeUnknown),
            (httpx.ConnectError("offline"), Unreachable),
            (
                httpx.Response(
                    200,
                    content=grpc_web_frame(
                        _field_bytes(1, preparation_receipt_message())
                    )
                    * 2,
                ),
                OutcomeUnknown,
            ),
        ]
        for response, error in cases:
            calls = []

            def handler(request, calls=calls, response=response):
                calls.append(request)
                if isinstance(response, Exception):
                    raise response
                return response

            with self.subTest(response=response, error=error):
                async with httpx.AsyncClient(
                    transport=httpx.MockTransport(handler), base_url="http://k.test"
                ) as http:
                    with self.assertRaises(error) as ctx:
                        await self.prepare(
                            KagentClient(
                                "http://k.test", user_id="mainloop", client=http
                            )
                        )
                    if isinstance(ctx.exception, SessionError):
                        self.assertIn(ctx.exception.grpc_status, (3, 6))
                self.assertEqual(len(calls), 1)


class StateHelperTests(unittest.TestCase):
    def test_state_names_normalise(self):
        self.assertEqual(normalise_state("TASK_STATE_INPUT_REQUIRED"), "input_required")
        self.assertEqual(normalise_state("input-required"), "input_required")
        self.assertEqual(normalise_state(""), "unspecified")

    def test_terminal_and_parked_are_distinct(self):
        self.assertTrue(is_terminal("TASK_STATE_COMPLETED"))
        self.assertTrue(is_terminal("TASK_STATE_CANCELED"))
        self.assertFalse(is_terminal("TASK_STATE_WORKING"))
        self.assertTrue(is_parked("TASK_STATE_INPUT_REQUIRED"))
        self.assertFalse(is_terminal("TASK_STATE_INPUT_REQUIRED"))

    def test_reply_ids_are_deterministic(self):
        self.assertEqual(assistant_message_id("s", "t"), assistant_message_id("s", "t"))
        self.assertNotEqual(
            assistant_message_id("s", "t"), assistant_message_id("s", "u")
        )


class ProjectionTests(unittest.IsolatedAsyncioTestCase):
    async def test_stream_builds_reply_and_terminal_state(self):
        fake = FakeKagent()
        client, _ = make_client(fake)
        proj = TaskProjection()
        for event in await send(client):
            proj.apply(event)
        self.assertEqual(proj.task_id, TASK_ID)
        self.assertTrue(proj.terminal)
        self.assertEqual(proj.text, "ok")
        self.assertEqual(proj.history_message_ids, ["m-1"])

    async def test_replace_supersedes_accumulated_state(self):
        fake = FakeKagent()
        client, _ = make_client(fake)
        events = await send(client)
        proj = TaskProjection()
        for event in events[:3]:
            proj.apply(event)
        self.assertFalse(proj.terminal)
        stale = events[2].artifact_update.artifact
        stale.parts[0].text = "stale"
        proj.artifacts["junk"] = stale
        proj.replace(events[3].task)
        self.assertEqual(proj.text, "ok")
        self.assertNotIn("junk", proj.artifacts)

    async def test_append_extends_artifact(self):
        proj = TaskProjection()
        first = StreamEvent.model_validate(
            {
                "artifactUpdate": {
                    "taskId": "t",
                    "artifact": {"artifactId": "a", "parts": [{"text": "he"}]},
                }
            }
        )
        more = StreamEvent.model_validate(
            {
                "artifactUpdate": {
                    "taskId": "t",
                    "append": True,
                    "artifact": {"artifactId": "a", "parts": [{"text": "llo"}]},
                }
            }
        )
        proj.apply(first)
        proj.apply(more)
        self.assertEqual(proj.text, "hello")

    async def test_input_required_is_parked_not_terminal(self):
        proj = TaskProjection()
        proj.apply(
            StreamEvent.model_validate(
                {
                    "statusUpdate": {
                        "taskId": "t",
                        "status": {"state": "TASK_STATE_INPUT_REQUIRED"},
                    }
                }
            )
        )
        self.assertTrue(proj.parked)
        self.assertFalse(proj.terminal)


class SessionServiceTests(unittest.IsolatedAsyncioTestCase):
    async def test_create_is_idempotent_by_request_id(self):
        fake = FakeKagent()
        client, _ = make_client(fake)
        first = await client.create_session(AGENT, request_id="req-1", name="n")
        second = await client.create_session(AGENT, request_id="req-1", name="n")
        self.assertEqual(first.id, second.id)
        self.assertEqual(first.context_id, first.id)
        self.assertEqual(first.state, RuntimeState.READY)
        fields = decode_fields(fake.session_calls("CreateSession")[0])
        self.assertEqual(fields[3][0], b"req-1")
        agent = decode_fields(fields[5][0])
        self.assertEqual(
            (agent[1][0], agent[2][0]), (b"kagent", b"claude-subscription")
        )

    async def test_identity_header_is_sent(self):
        seen: list[str] = []

        def handler(request: httpx.Request) -> httpx.Response:
            seen.append(request.headers["x-user-id"])
            return httpx.Response(
                200, json={"jsonrpc": "2.0", "id": "x", "result": {"tasks": []}}
            )

        http = httpx.AsyncClient(
            transport=httpx.MockTransport(handler), base_url="http://k.test"
        )
        client = KagentClient("http://k.test", user_id="mainloop", client=http)
        await client.list_tasks(AGENT, CONTEXT_ID)
        self.assertEqual(seen, ["mainloop"])

    async def test_suspend_then_resume(self):
        fake = FakeKagent()
        client, _ = make_client(fake)
        session = await client.create_session(AGENT, request_id="r")
        suspended = await client.suspend_session(session.id)
        self.assertEqual(suspended.state, RuntimeState.SUSPENDED)
        resumed = await client.ensure_ready(suspended)
        self.assertEqual(resumed.state, RuntimeState.READY)
        self.assertEqual(len(fake.session_calls("ResumeSession")), 1)

    async def test_ready_session_is_not_resumed(self):
        fake = FakeKagent()
        client, _ = make_client(fake)
        session = await client.create_session(AGENT, request_id="r")
        await client.ensure_ready(session)
        self.assertEqual(fake.session_calls("ResumeSession"), [])

    async def test_waits_for_busy_session(self):
        fake = FakeKagent()
        client, sleeps = make_client(fake)
        session = await client.create_session(AGENT, request_id="r")
        busy = type(session)(
            id=session.id,
            state=RuntimeState.CREATING,
            operation=RuntimeOperation.CREATE,
            context_id=session.context_id,
            agent=session.agent,
            workspace=session.workspace,
            development_environment=session.development_environment,
        )
        ready = await client.ensure_ready(busy, interval=0.5)
        self.assertEqual(ready.state, RuntimeState.READY)
        self.assertEqual(sleeps, [0.5])

    async def test_failed_session_raises(self):
        fake = FakeKagent()
        client, _ = make_client(fake)
        session = await client.create_session(AGENT, request_id="r")
        failed = type(session)(
            id=session.id,
            state=RuntimeState.FAILED,
            operation=RuntimeOperation.NONE,
            context_id=session.context_id,
            failure_reason="Boom",
        )
        with self.assertRaises(SessionError):
            await client.ensure_ready(failed)

    async def test_grpc_error_status(self):
        fake = FakeKagent()
        client, _ = make_client(fake)
        with self.assertRaises(SessionError) as ctx:
            await client.get_session("00000000-0000-4000-8000-0000000000ff")
        self.assertEqual(ctx.exception.grpc_status, 5)

    async def test_get_agent_reads_named_template_ref(self):
        def field(number, value):
            return _field_bytes(number, value)

        def text(number, value):
            return _field_str(number, value)

        def struct_value(value):
            return field(5, value)

        def struct(items):
            return b"".join(
                field(1, text(1, key) + field(2, value)) for key, value in items
            )

        template_ref = struct([("name", text(3, "claude-workspace"))])
        spec = struct([("templateRef", struct_value(template_ref))])
        resource = struct([("spec", struct_value(spec))])
        ref = text(1, "kagent") + text(2, "claude-subscription")
        structured = (
            text(1, "kagent.dev/v1alpha3") + text(2, "Agent") + field(3, resource)
        )
        agent = field(1, ref) + field(2, structured)

        def handler(request):
            self.assertTrue(
                request.url.path.endswith("AgentService/GetAgent"),
                request.url.path,
            )
            return grpc_response(field(1, agent))

        client = KagentClient(
            "http://k.test",
            user_id="mainloop",
            client=httpx.AsyncClient(
                transport=httpx.MockTransport(handler), base_url="http://k.test"
            ),
        )
        found = await client.get_agent(AGENT)
        self.assertEqual(found.ref, AGENT)
        self.assertEqual(found.template_name, "claude-workspace")
        self.assertFalse(found.inline_template)

    async def test_agent_lookup_rejects_inline_template(self):
        def field(number, value):
            return _field_bytes(number, value)

        def text(number, value):
            return _field_str(number, value)

        def struct_value(value):
            return field(5, value)

        inline = b""
        spec = field(1, text(1, "template") + field(2, struct_value(inline)))
        resource = field(1, text(1, "spec") + field(2, struct_value(spec)))
        ref = text(1, "kagent") + text(2, "claude-subscription")
        structured = (
            text(1, "kagent.dev/v1alpha3") + text(2, "Agent") + field(3, resource)
        )
        response = field(1, field(1, ref) + field(2, structured))
        decoded = decode_agent_response(response)
        self.assertTrue(decoded.inline_template)
        self.assertIsNone(decoded.template_name)

    async def test_ambiguous_session_errors_are_not_definitive_rejections(self):
        from tests.runtime.kagent_fake import grpc_response

        for response in [
            httpx.Response(500),
            *(grpc_response(None, status=status) for status in (4, 10, 13, 14)),
            grpc_response(None),
        ]:
            with self.subTest(status=response.status_code, body=response.content):
                http = httpx.AsyncClient(
                    transport=httpx.MockTransport(
                        lambda request, response=response: response
                    ),
                    base_url="http://k.test",
                )
                client = KagentClient("http://k.test", user_id="fixture", client=http)
                with self.assertRaises(OutcomeUnknown):
                    await client.create_session(AGENT, request_id="fixture-request")
                await http.aclose()

    async def test_grpc_web_frames_round_trip(self):
        from tests.runtime.kagent_fake import grpc_response, session_message

        message = session_message("abc")
        messages, trailers = parse_grpc_web(grpc_response(message).content)
        self.assertEqual(messages, [message])
        self.assertEqual(trailers["grpc-status"], "0")
        with self.assertRaises(ValueError):
            parse_grpc_web(grpc_response(message).content[:-9])


class SendTests(unittest.IsolatedAsyncioTestCase):
    async def test_send_streams_task_events(self):
        fake = FakeKagent()
        client, _ = make_client(fake)
        events = await send(client)
        self.assertEqual([bool(e.task) for e in events], [True, False, False, True])
        body = fake.rpc_calls("SendStreamingMessage")[0]["params"]["message"]
        self.assertEqual(body["messageId"], "m-1")
        self.assertEqual(body["contextId"], CONTEXT_ID)
        self.assertEqual(body["role"], "ROLE_USER")
        self.assertNotIn("taskId", body)

    async def test_not_accepted_sse_is_retried_with_the_same_message(self):
        fake = FakeKagent()
        fake.send_script = ["not-accepted", "not-accepted", "ok"]
        client, sleeps = make_client(fake)
        events = await send(client)
        self.assertEqual(len(events), 4)
        calls = [c["params"]["message"] for c in fake.rpc_calls("SendStreamingMessage")]
        self.assertEqual(len(calls), 3)
        self.assertEqual({c["messageId"] for c in calls}, {"m-1"})
        # kagent's retryAfterMs, backed off.
        self.assertEqual(sleeps, [0.1, 0.2])

    async def test_not_accepted_plain_json_is_retried(self):
        fake = FakeKagent()
        fake.send_script = ["not-accepted-json", "ok"]
        client, _ = make_client(fake)
        await send(client)
        self.assertEqual(len(fake.rpc_calls("SendStreamingMessage")), 2)

    async def test_not_accepted_gives_up_when_the_30s_budget_runs_out(self):
        fake = FakeKagent()
        fake.send_script = ["not-accepted"] * 100
        client, sleeps = make_client(fake)
        with self.assertRaises(SendNotAccepted):
            await send(client)
        calls = fake.rpc_calls("SendStreamingMessage")
        self.assertEqual(len(calls), len(sleeps) + 1)
        self.assertLessEqual(sum(sleeps), 30.0)
        self.assertGreater(sum(sleeps), 25.0)
        self.assertLessEqual(max(sleeps), 2.0)  # backoff is capped
        self.assertEqual({c["params"]["message"]["messageId"] for c in calls}, {"m-1"})
        self.assertEqual(fake.accepted_message_ids, [])

    async def test_kagents_own_wait_per_attempt_counts_against_the_budget(self):
        # kagent holds each attempt up to 10s while the Session is busy before it says
        # "not accepted"; the client measures wall time, not attempts.
        fake = FakeKagent()
        fake.send_script = ["not-accepted"] * 10
        ticks = iter(range(0, 1000, 10))
        client, _ = make_client(fake, clock=lambda: float(next(ticks)))
        with self.assertRaises(SendNotAccepted):
            await send(client)
        self.assertEqual(len(fake.rpc_calls("SendStreamingMessage")), 3)

    async def test_other_errors_are_not_retried(self):
        fake = FakeKagent()
        fake.send_script = ["other-error", "ok"]
        client, _ = make_client(fake)
        with self.assertRaises(A2AError) as ctx:
            await send(client)
        self.assertNotIsInstance(ctx.exception, SendNotAccepted)
        self.assertEqual(len(fake.rpc_calls("SendStreamingMessage")), 1)

    async def test_not_accepted_needs_the_a2a_domain(self):
        from mainloop.runtime.kagent_client import a2a_error_from_json

        error = a2a_error_from_json(
            {
                "code": -32603,
                "message": "x",
                "data": [
                    {
                        "@type": "type.googleapis.com/google.rpc.ErrorInfo",
                        "reason": "UNSUPPORTED_OPERATION",
                        "domain": "other.example",
                        "metadata": {"reason": "KAGENT_SEND_NOT_ACCEPTED"},
                    }
                ],
            }
        )
        self.assertNotIsInstance(error, SendNotAccepted)

    async def test_not_accepted_is_read_from_the_metadata_reason_not_the_top_level_one(
        self,
    ):
        from mainloop.runtime.kagent_client import a2a_error_from_json

        info = {
            "@type": "type.googleapis.com/google.rpc.ErrorInfo",
            "reason": "KAGENT_SEND_NOT_ACCEPTED",
            "domain": "a2a-protocol.org",
        }
        error = a2a_error_from_json({"code": -32004, "message": "x", "data": [info]})
        self.assertNotIsInstance(error, SendNotAccepted)
        info = {
            **info,
            "reason": "UNSUPPORTED_OPERATION",
            "metadata": {"reason": info["reason"]},
        }
        error = a2a_error_from_json({"code": -32004, "message": "x", "data": [info]})
        self.assertIsInstance(error, SendNotAccepted)

    async def test_unreachable_is_a_definite_non_delivery(self):
        fake = FakeKagent()
        fake.send_script = ["unreachable"]
        client, _ = make_client(fake)
        with self.assertRaises(Unreachable):
            await send(client)

    async def test_lost_request_is_outcome_unknown_and_not_resent(self):
        fake = FakeKagent()
        fake.send_script = ["drop", "ok"]
        client, _ = make_client(fake)
        with self.assertRaises(OutcomeUnknown):
            await send(client)
        self.assertEqual(len(fake.rpc_calls("SendStreamingMessage")), 1)

    async def test_stream_cut_is_outcome_unknown_and_resolvable_without_resend(self):
        fake = FakeKagent()
        fake.send_script = ["cut"]
        client, _ = make_client(fake)
        seen: list[StreamEvent] = []
        with self.assertRaises(OutcomeUnknown):
            async for event in client.send_message(
                AGENT, text="hello", message_id="m-1", context_id=CONTEXT_ID
            ):
                seen.append(event)
        self.assertEqual(len(seen), 2)
        task = await client.find_task_for_message(AGENT, CONTEXT_ID, "m-1")
        self.assertIsNotNone(task)
        self.assertEqual(task.id, TASK_ID)
        self.assertEqual(len(fake.rpc_calls("SendStreamingMessage")), 1)

    async def test_list_tasks_follows_pages(self):
        fake = FakeKagent()
        fake.default_page_size = 50
        for i in range(130):
            fake.tasks[f"t-{i}"] = {
                "id": f"t-{i}",
                "contextId": CONTEXT_ID,
                "status": {"state": "TASK_STATE_COMPLETED"},
                "history": [{"messageId": f"m-{i}", "parts": [{"text": "x"}]}],
            }
        client, _ = make_client(fake)
        tasks = await client.list_tasks(AGENT, CONTEXT_ID)
        self.assertEqual([t.id for t in tasks], [f"t-{i}" for i in range(130)])
        params = [c["params"] for c in fake.rpc_calls("ListTasks")]
        self.assertEqual(len(params), 2)
        self.assertEqual(params[0]["pageSize"], 100)
        self.assertNotIn("pageToken", params[0])
        self.assertEqual(params[1]["pageToken"], "100")

    async def test_found_task_is_reread_because_listed_tasks_have_no_artifacts(self):
        # A long-lived main thread: the message is in a task past the first page, and the
        # reply text only comes back from GetTask.
        fake = FakeKagent()
        for i in range(120):
            fake.tasks[f"t-{i}"] = {
                "id": f"t-{i}",
                "contextId": CONTEXT_ID,
                "status": {"state": "TASK_STATE_COMPLETED"},
                "history": [{"messageId": f"m-{i}", "parts": [{"text": "x"}]}],
                "artifacts": [
                    {"artifactId": f"a-{i}", "parts": [{"text": f"reply {i}"}]}
                ],
            }
        client, _ = make_client(fake)
        listed = await client.list_tasks(AGENT, CONTEXT_ID)
        self.assertTrue(all(t.artifacts == [] for t in listed))
        task = await client.find_task_for_message(AGENT, CONTEXT_ID, "m-117")
        self.assertEqual(task.id, "t-117")
        self.assertEqual(task.artifacts[0].text, "reply 117")
        self.assertEqual(fake.rpc_calls("GetTask")[-1]["params"], {"id": "t-117"})

    async def test_find_task_for_unknown_message_is_none(self):
        fake = FakeKagent()
        client, _ = make_client(fake)
        await send(client)
        self.assertIsNone(
            await client.find_task_for_message(AGENT, CONTEXT_ID, "other")
        )


class ReconnectAndCancelTests(unittest.IsolatedAsyncioTestCase):
    async def test_subscribe_first_event_replaces_the_projection(self):
        fake = FakeKagent()
        fake.subscribe_events = stream_chunks("m-1")[-1:]
        client, _ = make_client(fake)
        proj = TaskProjection(task_id=TASK_ID, state="TASK_STATE_WORKING")
        events = await collect(client.subscribe_to_task(AGENT, TASK_ID))
        proj.replace(events[0].task)
        self.assertTrue(proj.terminal)
        self.assertEqual(proj.text, "ok")
        self.assertEqual(
            fake.rpc_calls("SubscribeToTask")[0]["params"], {"id": TASK_ID}
        )

    async def test_get_task_and_missing_task(self):
        fake = FakeKagent()
        client, _ = make_client(fake)
        await send(client)
        self.assertEqual((await client.get_task(AGENT, TASK_ID)).id, TASK_ID)
        with self.assertRaises(TaskNotFound):
            await client.get_task(AGENT, "nope")

    async def test_cancel_running_task(self):
        fake = FakeKagent()
        fake.send_script = ["cut"]
        client, _ = make_client(fake)
        with self.assertRaises(OutcomeUnknown):
            await send(client)
        task = await client.cancel_task(AGENT, TASK_ID)
        self.assertEqual(normalise_state(task.status.state), "canceled")

    async def test_cancel_completed_task_returns_it_unchanged(self):
        fake = FakeKagent()
        client, _ = make_client(fake)
        await send(client)
        task = await client.cancel_task(AGENT, TASK_ID)
        self.assertEqual(normalise_state(task.status.state), "completed")


if __name__ == "__main__":
    unittest.main()


class HITLWireTests(unittest.IsolatedAsyncioTestCase):
    async def test_identity_decoder_and_message_projection_preserve_extension(self):
        from mainloop.runtime.kagent_client import (
            Message,
            Task,
            TaskStatus,
            _field_bytes,
            _field_str,
            decode_session_response,
        )

        from models.hitl import HITL_EXTENSION

        raw = (
            _field_str(1, "session")
            + _field_str(2, "creator")
            + _field_str(5, "revision")
            + _field_str(6, "session-session.team.actors.resources.substrate.ate.dev")
            + _field_str(14, "context")
            + _field_bytes(15, AgentRef("team", "agent").encode())
        )
        decoded = decode_session_response(_field_bytes(1, raw))
        self.assertEqual(
            (
                decoded.creator,
                decoded.prepared_revision,
                decoded.agent,
                decoded.context_id,
            ),
            ("creator", "revision", AgentRef("team", "agent"), "context"),
        )
        projection = TaskProjection()
        message = Message(
            message_id="one",
            extensions=[HITL_EXTENSION],
            metadata={
                "unknown": {"keep": True},
                HITL_EXTENSION: {"type": "ask_user_request", "id": "a"},
            },
        )
        task = Task(
            id="task",
            context_id="context",
            status=TaskStatus(state="input-required", message=message),
        )
        self.assertTrue(projection.apply(StreamEvent(task=task)))
        self.assertEqual(projection.status_message.metadata["unknown"], {"keep": True})
        changed = task.model_copy(deep=True)
        changed.status.message.metadata[HITL_EXTENSION]["id"] = "b"
        self.assertTrue(projection.apply(StreamEvent(task=changed)))
        self.assertFalse(projection.apply(StreamEvent(task=changed)))

    async def test_structured_continuation_uses_exact_task_and_activates_extension(
        self,
    ):
        import json

        from models.hitl import HITL_EXTENSION, AskUserAnswer, AskUserResponse

        requests = []

        async def handler(request):
            requests.append(request)
            body = json.loads(request.content)
            if body["method"] == "GetExtendedAgentCard":
                result = {"capabilities": {"extensions": [{"uri": HITL_EXTENSION}]}}
            elif body["method"] == "ListTasks":
                result = {"tasks": [], "nextPageToken": "next"}
            else:
                result = {
                    "task": {
                        "id": "original-task",
                        "contextId": "original-context",
                        "status": {"state": "working"},
                    }
                }
            return httpx.Response(200, json={"jsonrpc": "2.0", "result": result})

        async with httpx.AsyncClient(
            transport=httpx.MockTransport(handler), base_url="http://fake"
        ) as http:
            client = KagentClient("http://fake", user_id="configured", client=http)
            self.assertTrue(await client.supports_hitl(AGENT))
            self.assertEqual(
                await client.list_tasks_page(AGENT, "original-context", "cursor", 7),
                ([], "next"),
            )
            response = AskUserResponse(
                type="ask_user_response",
                id="child-question",
                answers=(AskUserAnswer(answer=("answer",)),),
            )
            events = [
                e
                async for e in client.send_hitl_response(
                    AGENT,
                    response=response,
                    message_id="stable-message",
                    task_id="original-task",
                    context_id="original-context",
                )
            ]
            self.assertEqual(events[0].task.id, "original-task")
        for request in requests:
            self.assertEqual(request.headers["A2A-Extensions"], HITL_EXTENSION)
            self.assertEqual(request.headers["x-user-id"], "configured")
            self.assertEqual(request.url.path, AGENT.path)
        wire = json.loads(requests[-1].content)["params"]["message"]
        self.assertEqual(
            (wire["taskId"], wire["contextId"], wire["messageId"]),
            ("original-task", "original-context", "stable-message"),
        )
        self.assertEqual(wire["metadata"][HITL_EXTENSION]["id"], "child-question")
        self.assertEqual(wire["parts"], [])
        self.assertEqual(
            json.loads(requests[1].content)["params"]["pageToken"], "cursor"
        )
