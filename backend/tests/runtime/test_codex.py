"""Sanitized Codex adapter examples; no Codex process or provider calls."""

import json
import unittest
from copy import deepcopy
from datetime import datetime, timedelta, timezone
from pathlib import Path

from mainloop.runtime.codex import (
    CodexAdapterError,
    CodexDeliverySignal,
    CodexEvidenceKind,
    CodexFixtureAdapter,
    codex_fixture_capabilities,
    normalize_codex_event,
)
from mainloop.runtime.contracts import ContractStore
from pydantic import ValidationError

from models import CapabilityResult, CapabilityState, NativeStatus

FIXTURES = Path(__file__).parent / "fixtures" / "codex"
NOW = datetime(2026, 1, 1, tzinfo=timezone.utc)
NATIVE_KEY = "codex-request:thread-codex-fixture-001"


def load_jsonl(name: str) -> list[dict]:
    return [
        json.loads(line)
        for line in (FIXTURES / name).read_text().splitlines()
        if line.strip()
    ]


def load_json(name: str) -> dict:
    return json.loads((FIXTURES / name).read_text())


def binding() -> dict:
    return load_json("session.json")["binding"]


def message() -> dict:
    return {
        "logical_message_id": "logical-approval-001",
        "source_task_id": "fixture-task",
        "payload_ref": "fixture://codex/payload/approval-001",
        "authority_ref": "fixture://codex/authority/approval-001",
        "created_at": "2026-01-01T00:01:00+00:00",
        "desired_binding_id": "codex-binding-fixture",
    }


class CodexAdapterTests(unittest.TestCase):
    def setUp(self):
        self.adapter = CodexFixtureAdapter(binding())
        self.core = load_jsonl("events.jsonl")

    def test_native_identity_cursor_and_raw_evidence_are_preserved(self):
        event = self.adapter.normalize(self.core[0])

        self.assertEqual(
            self.adapter.binding.native_session_id, "thread-codex-fixture-001"
        )
        self.assertEqual(event.source_cursor, 1)
        self.assertEqual(
            event.raw_evidence_ref, "fixture://codex/session-001/event-001"
        )
        self.assertEqual(event.native_type, "thread/started")
        self.assertEqual(event.extension.provider, "codex")
        self.assertEqual(event.extension.native_event_id, "evt-thread-001")

    def test_core_events_keep_source_order_and_normalize_supported_kinds(self):
        observations = tuple(self.adapter.observe(record) for record in self.core)

        self.assertEqual(
            [item.event.source_cursor for item in observations], list(range(1, 10))
        )
        self.assertEqual(
            [item.evidence_kind for item in observations],
            [
                CodexEvidenceKind.UNKNOWN,
                CodexEvidenceKind.DELIVERY,
                CodexEvidenceKind.ACTIVITY,
                CodexEvidenceKind.ACTIVITY,
                CodexEvidenceKind.OUTPUT,
                CodexEvidenceKind.USAGE,
                CodexEvidenceKind.CONTINUATION,
                CodexEvidenceKind.COMPLETION,
                CodexEvidenceKind.UNKNOWN,
            ],
        )
        self.assertEqual(observations[1].event.extension.model, "gpt-5-codex")
        self.assertEqual(observations[1].event.extension.effort, "medium")
        self.assertEqual(observations[5].event.extension.input_tokens, 128)
        self.assertEqual(observations[5].event.extension.output_tokens, 32)
        self.assertEqual(observations[6].event.normalized_type, "continuation")
        self.assertEqual(observations[7].event.normalized_type, "completed")

    def test_installed_camelcase_item_types_and_token_usage_are_normalized(self):
        observations = tuple(
            self.adapter.observe(record) for record in load_jsonl("native-wire.jsonl")
        )

        self.assertEqual(
            [item.event.normalized_type for item in observations],
            ["output", "activity", "activity", "activity", "activity", "usage"],
        )
        self.assertEqual(
            [item.evidence_kind for item in observations],
            [
                CodexEvidenceKind.OUTPUT,
                CodexEvidenceKind.ACTIVITY,
                CodexEvidenceKind.ACTIVITY,
                CodexEvidenceKind.ACTIVITY,
                CodexEvidenceKind.ACTIVITY,
                CodexEvidenceKind.USAGE,
            ],
        )
        self.assertEqual(
            observations[0].event.extension.native_event_id,
            "native-agent-message-001",
        )
        self.assertEqual(observations[5].event.native_type, "thread/tokenUsage/updated")
        self.assertEqual(observations[5].event.extension.input_tokens, 321)
        self.assertEqual(observations[5].event.extension.output_tokens, 45)
        self.assertEqual(
            observations[5].event.extension.native_event_id,
            "thread-codex-fixture-001",
        )

    def test_structured_thread_status_is_ingested_without_terminal_interpretation(
        self,
    ):
        observation = self.adapter.observe(load_jsonl("native-thread-status.jsonl")[0])
        store = ContractStore(binding())

        self.assertEqual(observation.event.normalized_type, "unknown")
        self.assertEqual(observation.evidence_kind, CodexEvidenceKind.UNKNOWN)
        self.assertIsNone(observation.delivery_signal)
        self.assertEqual(
            observation.event.extension.native_event_id,
            "thread-codex-fixture-001",
        )
        store.ingest(observation.event, 1)
        self.assertEqual(store.checkpoint(1).evidence_cursor, 1)
        self.assertEqual(store.checkpoint(1).native_status, NativeStatus.UNKNOWN)

    def test_native_model_metadata_preserves_observed_provider_model_and_effort(self):
        event = self.adapter.normalize(load_jsonl("native-metadata.jsonl")[0])

        self.assertEqual(event.normalized_type, "activity")
        self.assertEqual(event.extension.provider, "openai")
        self.assertEqual(event.extension.model, "gpt-5-codex")
        self.assertEqual(event.extension.effort, "high")

    def test_native_identifier_aliases_use_deterministic_precedence(self):
        events = self.adapter.normalize_many(load_jsonl("native-ids.jsonl"))

        self.assertEqual(
            [event.normalized_type for event in events],
            [
                "activity",
                "output",
                "completed",
                "unknown",
                "unknown",
                "activity",
                "activity",
            ],
        )
        self.assertEqual(
            [event.extension.native_event_id for event in events],
            [
                # itemId beats turnId and threadId.
                "item-native-alias-001",
                # item.id beats turnId and threadId.
                "item-native-002",
                # turnId beats threadId.
                "turn-native-alias-001",
                # threadId is kept when it is the only identifier.
                "thread-codex-fixture-001",
                # No identifier is invented.
                None,
                # An explicit native event ID beats every alias.
                "event-native-explicit-001",
                # A plain (non-JSON-RPC) event.id beats params.threadId/turnId.
                "event-native-plain-001",
            ],
        )

    def test_json_rpc_request_id_is_not_an_event_id(self):
        # The request id correlates attention; the event keeps its item ID.
        request = load_jsonl("native-attention.jsonl")[0]
        self.assertEqual(request["event"]["id"], 41)
        event = self.adapter.normalize(request)

        self.assertEqual(
            event.extension.native_event_id, "item-native-command-approval-001"
        )
        self.assertEqual(event.attention.deduplication_key, f"{NATIVE_KEY}:41")

    def test_permission_approval_follows_native_approval_rules(self):
        records = load_jsonl("native-permissions.jsonl")
        request = self.adapter.observe(records[0])
        resolution = self.adapter.observe(records[1])

        self.assertEqual(request.event.normalized_type, "attention")
        self.assertEqual(request.evidence_kind, CodexEvidenceKind.ATTENTION)
        self.assertEqual(
            (
                request.event.attention.deduplication_key,
                request.event.attention.request_type,
                request.event.attention.answer_shape,
            ),
            (f"{NATIVE_KEY}:61", "approval", "boolean"),
        )
        self.assertEqual(
            request.event.extension.native_event_id, "item-native-permissions-001"
        )
        self.assertEqual(resolution.event.normalized_type, "attention_resolved")
        self.assertEqual(resolution.event.attention_key, f"{NATIVE_KEY}:61")

        store = ContractStore(binding())
        store.ingest(request.event, 1)
        self.assertEqual(store.checkpoint(1).native_status, NativeStatus.WAITING)
        store.ingest(resolution.event, 1)
        checkpoint = store.checkpoint(1)
        self.assertEqual(checkpoint.attention[0].state, "resolved")
        self.assertEqual(checkpoint.native_status, NativeStatus.UNKNOWN)

    def test_permission_approval_replay_and_reconnect_do_not_duplicate_items(self):
        store = ContractStore(binding())
        request, resolution = load_jsonl("native-permissions.jsonl")[:2]
        original = self.adapter.normalize(request)
        store.ingest(original, 1)

        reingested = deepcopy(request)
        reingested["ingested_at"] = (NOW + timedelta(hours=1)).isoformat()
        self.assertEqual(store.ingest(self.adapter.normalize(reingested), 1), original)

        resent = deepcopy(request)
        resent["source_cursor"] = 2
        resent["raw_evidence_ref"] += "-replay"
        store.ingest(self.adapter.normalize(resent), 1)
        checkpoint = store.checkpoint(1)
        self.assertEqual(len(checkpoint.attention), 1)
        self.assertEqual(checkpoint.attention[0].state, "pending")

        resolved = deepcopy(resolution)
        resolved["source_cursor"] = 3
        resolved["raw_evidence_ref"] += "-replay"
        store.ingest(self.adapter.normalize(resolved), 1)
        checkpoint = store.checkpoint(1)
        self.assertEqual(len(checkpoint.attention), 1)
        self.assertEqual(checkpoint.attention[0].state, "resolved")

    def test_incomplete_permission_requests_stay_unknown(self):
        store = ContractStore(binding())
        records = load_jsonl("native-permissions.jsonl")[2:]
        self.assertEqual(len(records), 3)

        for cursor, record in enumerate(records, start=1):
            # Re-number from 1 so the store's contiguous cursor can advance.
            observation = self.adapter.observe(record, source_cursor=cursor)
            event = observation.event
            self.assertEqual(event.normalized_type, "unknown", record["event"])
            self.assertEqual(observation.evidence_kind, CodexEvidenceKind.UNKNOWN)
            self.assertIsNone(event.attention)
            self.assertEqual(event.native_type, "item/permissions/requestApproval")
            self.assertEqual(event.raw_evidence_ref, record["raw_evidence_ref"])
            store.ingest(event, 1)
        checkpoint = store.checkpoint(1)
        self.assertEqual(checkpoint.evidence_cursor, 3)
        self.assertEqual(checkpoint.attention, ())

    def test_receipt_delivery_and_completion_are_distinct_signals(self):
        receipt, delivery = load_jsonl("delivery.jsonl")
        receipt_observation = self.adapter.observe(receipt)
        delivery_observation = self.adapter.observe(delivery)
        completion_observation = self.adapter.observe(self.core[7])

        self.assertEqual(receipt_observation.evidence_kind, CodexEvidenceKind.RECEIPT)
        self.assertEqual(
            receipt_observation.delivery_signal, CodexDeliverySignal.RECEIPT
        )
        self.assertEqual(receipt_observation.event.normalized_type, "unknown")
        self.assertEqual(
            delivery_observation.delivery_signal, CodexDeliverySignal.DELIVERED
        )
        self.assertEqual(delivery_observation.event.normalized_type, "activity")
        self.assertEqual(
            completion_observation.delivery_signal, CodexDeliverySignal.COMPLETED
        )
        self.assertEqual(completion_observation.event.normalized_type, "completed")

    def test_duplicate_ingestion_and_reconnect_do_not_duplicate_events(self):
        store = ContractStore(binding())
        original = tuple(self.adapter.normalize(record) for record in self.core[:8])
        for event in original:
            store.ingest(event, 1)
        before = store.checkpoint(1)

        replay_records = []
        for record in self.core[:8]:
            replay = deepcopy(record)
            replay["ingested_at"] = (
                datetime.fromisoformat(record["ingested_at"]) + timedelta(minutes=1)
            ).isoformat()
            replay_records.append(replay)
        replayed = tuple(self.adapter.normalize(record) for record in replay_records)
        for event in replayed:
            self.assertEqual(store.ingest(event, 1), original[event.source_cursor - 1])

        self.assertEqual(store.events, original)
        self.assertEqual(store.checkpoint(1), before)
        self.assertEqual(store.checkpoint(1).evidence_cursor, 8)
        self.assertEqual(store.checkpoint(1).native_status, NativeStatus.COMPLETED)

    def test_reconnect_after_takeover_keeps_source_identity(self):
        store = ContractStore(binding())
        original = self.adapter.normalize(self.core[0])
        store.ingest(original, 1)
        store.take_ownership(1)

        replay = self.adapter.normalize(
            self.core[0],
            ownership_generation=2,
            ingested_at=NOW + timedelta(minutes=1),
        )
        self.assertEqual(store.ingest(replay, 2), original)
        self.assertEqual(store.events, (original,))
        self.assertEqual(store.checkpoint(2).evidence_cursor, 1)

    def test_source_gaps_wait_for_the_missing_cursor(self):
        store = ContractStore(binding())
        second = self.adapter.normalize(self.core[1])
        first = self.adapter.normalize(self.core[0])

        store.ingest(second, 1)
        self.assertEqual(store.checkpoint(1).evidence_cursor, 0)
        store.ingest(first, 1)
        self.assertEqual(store.checkpoint(1).evidence_cursor, 2)
        self.assertEqual(store.checkpoint(1).native_status, NativeStatus.ACTIVE)

    def test_attention_is_preserved_and_resolution_is_correlated(self):
        store = ContractStore(binding())
        store.record_message(message(), 1)
        attention, resolution = load_jsonl("attention.jsonl")

        normalized_attention = self.adapter.normalize(attention)
        self.assertEqual(normalized_attention.normalized_type, "attention")
        self.assertEqual(normalized_attention.attention.request_type, "approval")
        store.ingest(normalized_attention, 1)
        store.ingest(normalized_attention, 1)
        self.assertEqual(len(store.events), 1)
        self.assertEqual(store.checkpoint(1).native_status, NativeStatus.WAITING)

        store.ingest(self.adapter.normalize(resolution), 1)
        checkpoint = store.checkpoint(1)
        self.assertEqual(checkpoint.native_status, NativeStatus.UNKNOWN)
        self.assertEqual(checkpoint.attention[0].state, "resolved")

    def test_native_requests_map_to_attention_with_request_id_correlation(self):
        observations = tuple(
            self.adapter.observe(record)
            for record in load_jsonl("native-attention.jsonl")
        )

        self.assertEqual(
            [item.event.normalized_type for item in observations],
            ["attention", "attention_resolved"] * 4,
        )
        self.assertEqual(
            {item.evidence_kind for item in observations},
            {CodexEvidenceKind.ATTENTION},
        )
        requests = [item.event.attention for item in observations[0::2]]
        self.assertEqual(
            [
                (
                    request.deduplication_key,
                    request.request_type,
                    request.answer_shape,
                    request.choices,
                )
                for request in requests
            ],
            [
                (f"{NATIVE_KEY}:41", "approval", "boolean", ()),
                (f"{NATIVE_KEY}:request-file-002", "approval", "boolean", ()),
                (f"{NATIVE_KEY}:43", "question", "choice", ("fast", "safe")),
                (f"{NATIVE_KEY}:44", "question", "text", ()),
            ],
        )
        self.assertEqual(
            [item.event.attention_key for item in observations[1::2]],
            [request.deduplication_key for request in requests],
        )
        self.assertEqual(
            observations[0].event.extension.native_event_id,
            "item-native-command-approval-001",
        )
        self.assertEqual(
            observations[1].event.extension.native_event_id,
            "thread-codex-fixture-001",
        )

    def test_native_resolution_resolves_only_its_attention_item(self):
        store = ContractStore(binding())
        records = load_jsonl("native-attention.jsonl")

        store.ingest(self.adapter.normalize(records[0]), 1)
        checkpoint = store.checkpoint(1)
        self.assertEqual(checkpoint.native_status, NativeStatus.WAITING)
        self.assertEqual(
            [
                (item.request.deduplication_key, item.state)
                for item in checkpoint.attention
            ],
            [(f"{NATIVE_KEY}:41", "pending")],
        )

        store.ingest(self.adapter.normalize(records[1]), 1)
        checkpoint = store.checkpoint(1)
        self.assertEqual(checkpoint.native_status, NativeStatus.UNKNOWN)
        self.assertEqual(checkpoint.attention[0].state, "resolved")

        for record in records[2:]:
            store.ingest(self.adapter.normalize(record), 1)
        checkpoint = store.checkpoint(1)
        self.assertEqual(checkpoint.evidence_cursor, 8)
        self.assertEqual(len(checkpoint.attention), 4)
        self.assertEqual({item.state for item in checkpoint.attention}, {"resolved"})
        # Resolving attention is not native completion.
        self.assertNotEqual(checkpoint.native_status, NativeStatus.COMPLETED)

    def test_native_attention_replay_and_reconnect_do_not_duplicate_items(self):
        def replay(record: dict, cursor: int, minutes: int = 1) -> dict:
            copy = deepcopy(record)
            copy["source_cursor"] = cursor
            copy["raw_evidence_ref"] += "-replay"
            later = NOW + timedelta(hours=1, minutes=minutes)
            copy["ingested_at"] = later.isoformat()
            return copy

        store = ContractStore(binding())
        request, resolution = load_jsonl("native-attention.jsonl")[:2]
        original = self.adapter.normalize(request)
        store.ingest(original, 1)

        # The same source record redelivered after a reconnect.
        reingested = deepcopy(request)
        reingested["ingested_at"] = (NOW + timedelta(hours=1)).isoformat()
        self.assertEqual(store.ingest(self.adapter.normalize(reingested), 1), original)
        self.assertEqual(store.events, (original,))

        # The pending request re-announced at a later source cursor.
        store.ingest(self.adapter.normalize(replay(request, 2)), 1)
        checkpoint = store.checkpoint(1)
        self.assertEqual(len(checkpoint.attention), 1)
        self.assertEqual(checkpoint.attention[0].state, "pending")
        self.assertEqual(checkpoint.native_status, NativeStatus.WAITING)

        store.ingest(self.adapter.normalize(replay(resolution, 3)), 1)
        checkpoint = store.checkpoint(1)
        self.assertEqual(len(checkpoint.attention), 1)
        self.assertEqual(checkpoint.attention[0].state, "resolved")

        # A stale re-announcement of the resolved request neither duplicates
        # the item nor makes the binding wait again.
        store.ingest(self.adapter.normalize(replay(request, 4, 2)), 1)
        checkpoint = store.checkpoint(1)
        self.assertEqual(checkpoint.evidence_cursor, 4)
        self.assertEqual(len(checkpoint.attention), 1)
        self.assertEqual(checkpoint.attention[0].state, "resolved")
        self.assertEqual(checkpoint.native_status, NativeStatus.UNKNOWN)

    def test_incomplete_or_unsupported_native_requests_stay_unknown(self):
        store = ContractStore(binding())
        records = load_jsonl("native-attention-incomplete.jsonl")

        for record in records:
            observation = self.adapter.observe(record)
            event = observation.event
            self.assertEqual(event.normalized_type, "unknown", record["event"])
            self.assertEqual(observation.evidence_kind, CodexEvidenceKind.UNKNOWN)
            self.assertIsNone(event.attention)
            self.assertIsNone(event.attention_key)
            self.assertEqual(event.source_cursor, record["source_cursor"])
            self.assertEqual(event.raw_evidence_ref, record["raw_evidence_ref"])
            self.assertEqual(event.native_type, record["event"]["method"])
            store.ingest(event, 1)

        checkpoint = store.checkpoint(1)
        self.assertEqual(checkpoint.evidence_cursor, len(records))
        self.assertEqual(checkpoint.attention, ())
        self.assertEqual(checkpoint.native_status, NativeStatus.UNKNOWN)

    def test_resolution_of_an_unaccepted_request_stays_unknown_evidence(self):
        resolution = load_jsonl("native-attention.jsonl")[1]
        key = f"{NATIVE_KEY}:41"
        store = ContractStore(binding())

        # Stateless by default: the shared projection rejects the orphan.
        stateless = self.adapter.normalize(resolution, source_cursor=1)
        self.assertEqual(stateless.attention_key, key)
        with self.assertRaises(ValueError):
            store.ingest(stateless, 1)
        self.assertEqual(store.events, ())

        # Given the accepted keys, an unmatched resolution keeps its evidence
        # and lets the cursor advance without inventing attention state.
        unmatched = self.adapter.observe(resolution, source_cursor=1, attention_keys=())
        self.assertEqual(unmatched.event.normalized_type, "unknown")
        self.assertEqual(unmatched.evidence_kind, CodexEvidenceKind.UNKNOWN)
        self.assertIsNone(unmatched.event.attention_key)
        self.assertEqual(
            unmatched.event.raw_evidence_ref, resolution["raw_evidence_ref"]
        )
        store.ingest(unmatched.event, 1)
        self.assertEqual(store.checkpoint(1).evidence_cursor, 1)
        self.assertEqual(store.checkpoint(1).attention, ())

        matched = self.adapter.normalize(resolution, attention_keys={key})
        self.assertEqual(matched.normalized_type, "attention_resolved")
        self.assertEqual(matched.attention_key, key)

    def test_interruption_is_not_completion(self):
        store = ContractStore(binding())
        for record in load_jsonl("interrupted.jsonl"):
            store.ingest(self.adapter.normalize(record), 1)

        checkpoint = store.checkpoint(1)
        self.assertEqual(checkpoint.native_status, NativeStatus.INTERRUPTED)
        self.assertNotEqual(checkpoint.native_status, NativeStatus.COMPLETED)

    def test_nested_terminal_status_does_not_claim_completion(self):
        observations = tuple(
            self.adapter.observe(record)
            for record in load_jsonl("terminal-status.jsonl")
        )

        self.assertEqual(
            [item.event.normalized_type for item in observations],
            [
                "interrupted",
                "interrupted",
                "interrupted",
                "unknown",
                "unknown",
            ],
        )
        self.assertEqual(
            [item.delivery_signal for item in observations[:3]],
            [
                CodexDeliverySignal.INTERRUPTED,
                CodexDeliverySignal.INTERRUPTED,
                CodexDeliverySignal.INTERRUPTED,
            ],
        )
        self.assertIsNone(observations[3].delivery_signal)
        self.assertIsNone(observations[4].delivery_signal)

    def test_foreign_thread_events_are_unknown_and_do_not_change_checkpoint(self):
        observations = tuple(
            self.adapter.observe(record)
            for record in load_jsonl("foreign-thread.jsonl")
        )
        store = ContractStore(binding())

        self.assertEqual(
            [item.event.normalized_type for item in observations],
            ["unknown", "unknown", "unknown"],
        )
        self.assertEqual(
            [item.evidence_kind for item in observations],
            [CodexEvidenceKind.UNKNOWN] * 3,
        )
        self.assertEqual([item.delivery_signal for item in observations], [None] * 3)
        self.assertEqual(
            [item.event.extension.native_event_id for item in observations],
            [
                "turn-foreign-delivery-001",
                "item-foreign-activity-001",
                "turn-foreign-completion-001",
            ],
        )
        for observation in observations:
            store.ingest(observation.event, 1)
        checkpoint = store.checkpoint(1)
        self.assertEqual(checkpoint.evidence_cursor, 3)
        self.assertEqual(checkpoint.native_status, NativeStatus.UNKNOWN)

        active_store = ContractStore(binding())
        active_store.ingest(self.adapter.normalize(self.core[1], source_cursor=1), 1)
        active_store.ingest(
            self.adapter.normalize(
                load_jsonl("foreign-thread.jsonl")[1], source_cursor=2
            ),
            1,
        )
        self.assertEqual(active_store.checkpoint(1).native_status, NativeStatus.ACTIVE)

        completed_store = ContractStore(binding())
        completed_store.ingest(self.adapter.normalize(self.core[7], source_cursor=1), 1)
        completed_store.ingest(
            self.adapter.normalize(
                load_jsonl("foreign-thread.jsonl")[2], source_cursor=2
            ),
            1,
        )
        self.assertEqual(
            completed_store.checkpoint(1).native_status, NativeStatus.COMPLETED
        )

    def test_unknown_terminal_status_is_not_completion(self):
        observations = tuple(
            self.adapter.observe(record)
            for record in load_jsonl("terminal-unknown-status.jsonl")
        )

        self.assertEqual(
            [item.event.normalized_type for item in observations],
            ["unknown", "unknown"],
        )
        self.assertEqual(
            [item.evidence_kind for item in observations],
            [CodexEvidenceKind.UNKNOWN, CodexEvidenceKind.UNKNOWN],
        )
        self.assertEqual([item.delivery_signal for item in observations], [None, None])

    def test_quiet_terminal_output_is_not_completion(self):
        store = ContractStore(binding())
        quiet = tuple(
            self.adapter.observe(record) for record in load_jsonl("quiet.jsonl")
        )

        self.assertEqual(
            [item.evidence_kind for item in quiet],
            [CodexEvidenceKind.QUIET, CodexEvidenceKind.QUIET],
        )
        for item in quiet:
            self.assertEqual(item.event.normalized_type, "unknown")
            store.ingest(item.event, 1)
        self.assertEqual(store.checkpoint(1).native_status, NativeStatus.UNKNOWN)

    def test_missing_model_effort_and_usage_remain_unavailable(self):
        events = tuple(
            self.adapter.normalize(record)
            for record in load_jsonl("missing-metadata.jsonl")
        )

        self.assertIsNone(events[0].extension.model)
        self.assertIsNone(events[0].extension.effort)
        self.assertIsNone(events[1].extension.input_tokens)
        self.assertIsNone(events[1].extension.output_tokens)

    def test_unknown_event_keeps_type_and_raw_evidence(self):
        event = self.adapter.normalize(self.core[-1])

        self.assertEqual(event.normalized_type, "unknown")
        self.assertEqual(event.native_type, "turn/metadata_changed")
        self.assertEqual(
            event.raw_evidence_ref, "fixture://codex/session-001/event-009"
        )

    def test_malformed_external_data_is_rejected_before_contract_ingestion(self):
        store = ContractStore(binding())
        malformed = load_json("malformed.json")

        with self.assertRaises((CodexAdapterError, ValidationError)):
            self.adapter.normalize(malformed)
        self.assertEqual(store.events, ())

        missing_cursor = deepcopy(self.core[0])
        missing_cursor.pop("source_cursor")
        with self.assertRaises(CodexAdapterError):
            self.adapter.normalize(missing_cursor)

        missing_evidence = deepcopy(self.core[0])
        missing_evidence.pop("raw_evidence_ref")
        with self.assertRaises(CodexAdapterError):
            self.adapter.normalize(missing_evidence)

    def test_adapter_does_not_invent_cursor_or_ingestion_time(self):
        no_cursor = deepcopy(self.core[0])
        no_cursor.pop("source_cursor")
        with self.assertRaises(CodexAdapterError):
            normalize_codex_event(no_cursor, binding(), ingested_at=NOW)

        no_ingestion_time = deepcopy(self.core[0])
        no_ingestion_time.pop("ingested_at")
        with self.assertRaises(CodexAdapterError):
            normalize_codex_event(no_ingestion_time, binding())

    def test_normalized_batch_preserves_input_order(self):
        records = [self.core[3], self.core[1], self.core[0]]
        normalized = self.adapter.normalize_many(records)

        self.assertEqual([event.source_cursor for event in normalized], [4, 2, 1])

    def test_agreeing_logical_message_id_is_preserved_and_nulls_are_ignored(self):
        agreed, conflicting = load_jsonl("conflicting-logical-message.jsonl")[:2]
        self.assertEqual(
            self.adapter.normalize(agreed).logical_message_id, "logical-approval-001"
        )

        outer_null = deepcopy(conflicting)
        outer_null["logical_message_id"] = None
        self.assertEqual(
            self.adapter.normalize(outer_null).logical_message_id,
            "logical-other-002",
        )

        empty = deepcopy(conflicting)
        empty["logical_message_id"] = ""
        with self.assertRaises(CodexAdapterError):
            self.adapter.normalize(empty)

    def test_conflicting_logical_message_ids_are_rejected_without_ingestion(self):
        agreed, *conflicts = load_jsonl("conflicting-logical-message.jsonl")
        self.assertEqual(len(conflicts), 3)
        store = ContractStore(binding())
        store.record_message(message(), 1)
        store.ingest(self.adapter.normalize(agreed), 1)
        before = store.events
        checkpoint = store.checkpoint(1)

        # Envelope vs event, event vs params, and the snake/camel aliases in
        # one envelope must each fail; no precedence picks a winner.
        for record in conflicts:
            with self.assertRaisesRegex(CodexAdapterError, "disagree"):
                self.adapter.observe(record)
            with self.assertRaisesRegex(CodexAdapterError, "disagree"):
                normalize_codex_event(record, binding())
        with self.assertRaisesRegex(CodexAdapterError, "disagree"):
            self.adapter.normalize_many([agreed, *conflicts])

        self.assertEqual(store.events, before)
        self.assertEqual(store.checkpoint(1), checkpoint)
        self.assertEqual(checkpoint.evidence_cursor, 1)

    def test_capabilities_are_typed_and_scoped_to_fixtures(self):
        capabilities = self.adapter.capabilities
        by_name = {item.capability: item for item in capabilities}

        self.assertEqual(capabilities, codex_fixture_capabilities())
        self.assertEqual(len(by_name), len(capabilities))
        self.assertTrue(
            all(isinstance(item, CapabilityResult) for item in capabilities)
        )
        self.assertNotIn("live", {item.scope for item in capabilities})
        for name in ("session_identity", "thread_isolation", "cursor_reconnect"):
            self.assertEqual(by_name[name].state, CapabilityState.PROVED)
            self.assertEqual(by_name[name].scope, "fixture")
        for name in (
            "model_metadata",
            "evidence_distinction",
            "attention_request",
            "usage",
            "continuation_observation",
        ):
            self.assertEqual(by_name[name].state, CapabilityState.PARTIAL)
        for name in (
            "attention_response",
            "discovery",
            "session_creation",
            "transport_ownership",
            "steering",
            "process_lifecycle",
        ):
            self.assertEqual(by_name[name].state, CapabilityState.UNSUPPORTED)
            self.assertIsNone(by_name[name].evidence_ref)
        live = by_name["live_native_behavior"]
        self.assertEqual(live.state, CapabilityState.UNKNOWN)
        self.assertEqual(live.scope, "unverified")

    def test_proved_and_partial_capabilities_cite_existing_fixture_evidence(self):
        refs = {
            record["raw_evidence_ref"]
            for path in FIXTURES.glob("*.jsonl")
            for record in load_jsonl(path.name)
        }
        for item in self.adapter.capabilities:
            if item.state in (CapabilityState.PROVED, CapabilityState.PARTIAL):
                self.assertIn(item.evidence_ref, refs, item.capability)

    def test_non_codex_binding_is_rejected(self):
        other = deepcopy(binding())
        other["provider"] = "claude"
        with self.assertRaises(CodexAdapterError):
            CodexFixtureAdapter(other)


if __name__ == "__main__":
    unittest.main()
