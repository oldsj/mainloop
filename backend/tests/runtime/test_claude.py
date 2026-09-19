"""Sanitized native Claude boundary examples; no SDK, process, or credentials."""

import copy
import json
import unittest
from datetime import datetime, timedelta, timezone
from pathlib import Path

from mainloop.runtime.claude import (
    ClaudeSessionNormalizer,
    binding_from_init,
)
from mainloop.runtime.contracts import ContractStore
from pydantic import ValidationError

from models import CapabilityState, NativeStatus

NOW = datetime(2026, 1, 1, tzinfo=timezone.utc)
FIXTURES = Path(__file__).parent / "fixtures" / "claude"


def fixture(name: str) -> list[dict]:
    return json.loads((FIXTURES / name).read_text())


def adapter(records: list[dict], *, suffix: str = "stream") -> ClaudeSessionNormalizer:
    return ClaudeSessionNormalizer.from_init(
        records[0],
        binding_id=f"claude-binding-{suffix}",
        workspace_id=f"workspace-{suffix}",
        herdr_session_id=f"herdr-session-{suffix}",
        herdr_agent_id=f"herdr-agent-{suffix}",
    )


class ClaudeFixtureAdapterTests(unittest.TestCase):
    def test_binding_preserves_native_identity_and_observed_metadata(self):
        records = fixture("stream.json")
        binding = binding_from_init(
            records[0],
            binding_id="binding",
            workspace_id="workspace",
            herdr_session_id="herdr-session",
            herdr_agent_id="herdr-agent",
        )

        self.assertEqual(binding.provider, "claude")
        self.assertEqual(binding.runtime_type, "claude-native-cli")
        self.assertEqual(binding.native_session_id, "claude-native-session-fixture")
        self.assertEqual(binding.observed.model, "claude-sonnet-fixture")
        self.assertEqual(binding.observed.runtime_version, "claude-code-fixture-0.1")
        self.assertEqual(binding.observed.native_event_id, "claude-event-init-001")

    def test_stream_categories_preserve_cursor_evidence_and_optional_usage(self):
        records = fixture("stream.json")
        events = adapter(records).normalize_many(records, ingested_at=NOW)

        self.assertEqual(
            [event.normalized_type for event in events],
            [
                "activity",
                "output",
                "activity",
                "attention",
                "attention_resolved",
                "continuation",
                "completed",
                "unknown",
            ],
        )
        self.assertEqual([event.source_cursor for event in events], list(range(1, 9)))
        self.assertEqual(
            events[1].raw_evidence_ref,
            "fixture://claude/stream.json#cursor-2",
        )
        self.assertEqual(events[1].extension.input_tokens, 11)
        self.assertEqual(events[1].extension.output_tokens, 5)
        self.assertEqual(events[6].extension.input_tokens, 21)
        self.assertEqual(events[6].extension.output_tokens, 8)
        self.assertIsNone(events[6].extension.native_event_id)
        self.assertEqual(
            events[7].native_type,
            "claude.future_native_variant.not-yet-modeled",
        )

    def test_duplicate_ingestion_and_cursor_reconnect_do_not_duplicate_events(
        self,
    ):
        records = fixture("stream.json")
        normalizer = adapter(records)
        store = ContractStore(normalizer.binding)
        original_events = normalizer.normalize_many(records[:3], ingested_at=NOW)
        for event in original_events:
            store.ingest(event, 1)

        duplicate = normalizer.normalize(
            records[2], ingested_at=NOW + timedelta(seconds=10)
        )
        self.assertEqual(store.ingest(duplicate, 1), original_events[2])

        store.take_ownership(1)
        replay = normalizer.normalize(
            records[2],
            ingested_at=NOW + timedelta(seconds=20),
            ownership_generation=2,
        )
        self.assertEqual(store.ingest(replay, 2), original_events[2])
        self.assertEqual(store.events, original_events)
        self.assertEqual(store.checkpoint(2).evidence_cursor, 3)

    def test_source_gaps_are_retained_until_the_contiguous_prefix_is_complete(
        self,
    ):
        records = fixture("stream.json")
        normalizer = adapter(records)
        store = ContractStore(normalizer.binding)

        store.ingest(normalizer.normalize(records[2], ingested_at=NOW), 1)
        self.assertEqual(store.checkpoint(1).evidence_cursor, 0)
        store.ingest(normalizer.normalize(records[0], ingested_at=NOW), 1)
        self.assertEqual(store.checkpoint(1).evidence_cursor, 1)
        store.ingest(normalizer.normalize(records[1], ingested_at=NOW), 1)
        self.assertEqual(store.checkpoint(1).evidence_cursor, 3)
        self.assertEqual(store.checkpoint(1).native_status, NativeStatus.ACTIVE)

    def test_pending_attention_is_correlated_and_replay_is_idempotent(self):
        records = fixture("stream.json")
        normalizer = adapter(records)
        store = ContractStore(normalizer.binding)
        for record in records[:4]:
            store.ingest(normalizer.normalize(record, ingested_at=NOW), 1)

        checkpoint = store.checkpoint(1)
        self.assertEqual(checkpoint.native_status, NativeStatus.WAITING)
        self.assertEqual(len(checkpoint.attention), 1)
        self.assertEqual(checkpoint.attention[0].state, "pending")
        self.assertEqual(
            checkpoint.attention[0].request.deduplication_key,
            "claude-permission-request-001",
        )
        store.ingest(normalizer.normalize(records[3], ingested_at=NOW), 1)
        self.assertEqual(len(store.events), 4)
        self.assertEqual(store.checkpoint(1), checkpoint)

        store.ingest(normalizer.normalize(records[4], ingested_at=NOW), 1)
        self.assertEqual(store.checkpoint(1).attention[0].state, "resolved")
        self.assertEqual(store.checkpoint(1).native_status, NativeStatus.UNKNOWN)
        self.assertEqual(
            normalizer.normalize(records[4], ingested_at=NOW).attention_key,
            "claude-permission-request-001",
        )

    def test_non_permission_control_responses_remain_unknown_and_advance_cursor(self):
        records = fixture("control-operations.json")
        normalizer = adapter(records, suffix="controls")
        events = normalizer.normalize_many(records, ingested_at=NOW)

        self.assertEqual(
            [event.normalized_type for event in events],
            ["activity", "unknown", "unknown", "unknown", "unknown"],
        )
        self.assertEqual(
            events[2].raw_evidence_ref,
            "fixture://claude/control-operations.json#cursor-3",
        )
        self.assertIsNone(events[2].attention_key)

        store = ContractStore(normalizer.binding)
        for event in events:
            store.ingest(event, 1)
        checkpoint = store.checkpoint(1)
        self.assertEqual(checkpoint.evidence_cursor, 5)
        self.assertEqual(checkpoint.attention, ())

    def test_malformed_and_unknown_records_fail_safely(self):
        records = fixture("stream.json")
        normalizer = adapter(records)

        parsed = normalizer.normalize(records[0], ingested_at=NOW)
        self.assertEqual(parsed.source_at, NOW)

        naive_timestamp = copy.deepcopy(records[0])
        naive_timestamp["source_at"] = "2026-01-01T00:00:00"
        with self.assertRaises(ValidationError):
            normalizer.normalize(naive_timestamp, ingested_at=NOW)

        bad_cursor = copy.deepcopy(records[0])
        bad_cursor["source_cursor"] = "1"
        with self.assertRaises(ValidationError):
            normalizer.normalize(bad_cursor, ingested_at=NOW)

        missing_type = copy.deepcopy(records[0])
        del missing_type["event"]["type"]
        with self.assertRaises(ValidationError):
            normalizer.normalize(missing_type, ingested_at=NOW)

        malformed_attention = copy.deepcopy(records[3])
        del malformed_attention["event"]["request"]["tool_name"]
        with self.assertRaises(ValueError):
            normalizer.normalize(malformed_attention, ingested_at=NOW)

        unknown = normalizer.normalize(records[7], ingested_at=NOW)
        self.assertEqual(unknown.normalized_type, "unknown")
        self.assertEqual(
            unknown.raw_evidence_ref,
            "fixture://claude/stream.json#cursor-8",
        )
        self.assertEqual(unknown.extension.native_event_id, "claude-event-unknown-008")

    def test_missing_usage_and_completion_evidence_do_not_create_defaults(self):
        records = fixture("stream.json")
        normalizer = adapter(records)

        missing_usage = copy.deepcopy(records[6])
        del missing_usage["event"]["usage"]
        completed = normalizer.normalize(missing_usage, ingested_at=NOW)
        self.assertEqual(completed.normalized_type, "completed")
        self.assertIsNone(completed.extension.input_tokens)
        self.assertIsNone(completed.extension.output_tokens)
        self.assertIsNone(completed.extension.model)
        self.assertIsNone(completed.extension.effort)

        missing_error_flag = copy.deepcopy(records[6])
        del missing_error_flag["event"]["is_error"]
        not_proven_complete = normalizer.normalize(missing_error_flag, ingested_at=NOW)
        self.assertEqual(not_proven_complete.normalized_type, "unknown")

    def test_native_error_result_projects_to_interrupted(self):
        records = fixture("interruption.json")
        normalizer = adapter(records, suffix="interrupted")
        events = normalizer.normalize_many(records, ingested_at=NOW)
        self.assertEqual(events[-1].normalized_type, "interrupted")
        self.assertEqual(
            events[-1].raw_evidence_ref,
            "fixture://claude/interruption.json#cursor-3",
        )

        store = ContractStore(normalizer.binding)
        for event in events:
            store.ingest(event, 1)
        self.assertEqual(store.checkpoint(1).native_status, NativeStatus.INTERRUPTED)

    def test_native_completion_process_exit_quiet_and_transport_loss_are_distinct(
        self,
    ):
        stream = fixture("stream.json")
        completion_normalizer = adapter(stream)
        completion_store = ContractStore(completion_normalizer.binding)
        for event in completion_normalizer.normalize_many(stream, ingested_at=NOW):
            completion_store.ingest(event, 1)
        self.assertEqual(
            completion_store.checkpoint(1).native_status, NativeStatus.COMPLETED
        )

        quiet = fixture("quiet-output.json")
        quiet_normalizer = adapter(quiet, suffix="quiet")
        quiet_store = ContractStore(quiet_normalizer.binding)
        for event in quiet_normalizer.normalize_many(quiet[:2], ingested_at=NOW):
            quiet_store.ingest(event, 1)
        self.assertEqual(quiet_store.checkpoint(1).native_status, NativeStatus.ACTIVE)
        quiet_observation = quiet_normalizer.observe_runtime(quiet[2])
        self.assertEqual(quiet_observation.kind, "quiet")
        self.assertEqual(len(quiet_store.events), 2)

        process_exit = fixture("process-exit.json")
        exit_normalizer = adapter(process_exit, suffix="exit")
        exit_store = ContractStore(exit_normalizer.binding)
        for event in exit_normalizer.normalize_many(process_exit[:2], ingested_at=NOW):
            exit_store.ingest(event, 1)
        exit_observation = exit_normalizer.observe_runtime(process_exit[2])
        self.assertEqual(exit_observation.kind, "process_exit")
        self.assertEqual(exit_observation.exit_code, 0)
        self.assertEqual(exit_store.checkpoint(1).native_status, NativeStatus.ACTIVE)
        with self.assertRaises(ValueError):
            exit_normalizer.normalize(process_exit[2], ingested_at=NOW)

        transport = fixture("transport-loss.json")
        transport_normalizer = adapter(transport, suffix="transport")
        transport_store = ContractStore(transport_normalizer.binding)
        for event in transport_normalizer.normalize_many(transport, ingested_at=NOW):
            transport_store.ingest(event, 1)
        self.assertEqual(
            transport_store.checkpoint(1).native_status, NativeStatus.UNKNOWN
        )
        self.assertEqual(
            transport_normalizer.normalize(
                transport[2], ingested_at=NOW
            ).normalized_type,
            "transport_lost",
        )

    def test_capability_matrix_labels_live_gaps_and_unsupported_operations(self):
        records = fixture("stream.json")
        capabilities = {item.capability: item for item in adapter(records).capabilities}

        self.assertEqual(capabilities["native_completion"].scope, "fixture")
        self.assertEqual(
            capabilities["native_completion"].state, CapabilityState.PROVED
        )
        self.assertEqual(capabilities["interruption"].state, CapabilityState.PROVED)
        self.assertEqual(
            capabilities["delivery_receipt"].state, CapabilityState.UNSUPPORTED
        )
        self.assertEqual(capabilities["steering"].state, CapabilityState.UNSUPPORTED)
        self.assertEqual(
            capabilities["live_native_behavior"].state, CapabilityState.UNKNOWN
        )
        self.assertEqual(capabilities["live_native_behavior"].scope, "unverified")


if __name__ == "__main__":
    unittest.main()
