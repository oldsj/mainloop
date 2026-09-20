"""Sanitized, test-local contract examples. No SDK, DBOS, services, or provider calls."""

import unittest
from datetime import datetime, timedelta, timezone

from mainloop.runtime.contracts import (
    ContractError,
    ContractStore,
    StaleOwnership,
    capability_result,
)
from mainloop.runtime.projection import project_checkpoint
from pydantic import ValidationError

from models import CapabilityState, DeliveryState, NativeStatus

NOW = datetime(2026, 1, 1, tzinfo=timezone.utc)


def binding():
    return {
        "binding_id": "binding",
        "workspace_id": "workspace",
        "provider": "fixture",
        "runtime_type": "native",
        "native_session_id": "native-session",
        "herdr_session_id": "herdr-session",
        "herdr_agent_id": "agent",
        "creation_mode": "created",
        "ownership_generation": 1,
    }


def message():
    return {
        "logical_message_id": "message",
        "source_task_id": "task",
        "payload_ref": "fixture://payload",
        "authority_ref": "fixture://authority",
        "created_at": NOW.isoformat(),
        "desired_binding_id": "binding",
    }


def attempt(attempt_id="attempt", generation=1):
    return {
        "attempt_id": attempt_id,
        "logical_message_id": "message",
        "binding_id": "binding",
        "ownership_generation": generation,
        "created_at": NOW.isoformat(),
        "updated_at": NOW.isoformat(),
    }


def event(cursor=1, kind="activity", **extra):
    return {
        "binding_id": "binding",
        "ownership_generation": 1,
        "source_cursor": cursor,
        "native_type": f"fixture.{kind}",
        "normalized_type": kind,
        "source_at": NOW.isoformat(),
        "ingested_at": NOW.isoformat(),
        "raw_evidence_ref": f"fixture://events/{cursor}",
        **extra,
    }


def attention(cursor=1):
    return event(
        cursor,
        "attention",
        logical_message_id="message",
        attention={
            "deduplication_key": "question",
            "request_type": "question",
            "answer_shape": "text",
        },
    )


class ContractTests(unittest.TestCase):
    def setUp(self):
        self.store = ContractStore(binding())
        self.store.record_message(message(), 1)

    def sending(self):
        self.store.create_attempt(attempt(), 1)
        self.store.transition("attempt", 1, "queued", NOW)
        self.store.transition("attempt", 1, "sending", NOW)

    def evidence(self, outcome):
        return {
            "attempt_id": "attempt",
            "binding_id": "binding",
            "evidence_ref": "fixture://reconciliation",
            "observed_at": NOW,
            "outcome": outcome,
        }

    def test_logical_id_is_idempotent_and_conflicts_are_rejected(self):
        original = self.store.record_message(message(), 1)
        self.assertEqual(original, self.store.record_message(message(), 1))
        self.assertEqual(len(self.store.messages), 1)
        with self.assertRaises(ContractError):
            self.store.record_message(
                {**message(), "payload_ref": "fixture://different"}, 1
            )
        self.assertEqual(self.store.messages, (original,))

    def test_attempt_id_is_idempotent_and_duplicate_send_is_blocked(self):
        self.sending()
        self.assertEqual(
            self.store.create_attempt(attempt(), 1).state, DeliveryState.SENDING
        )
        with self.assertRaises(ContractError):
            self.store.create_attempt(attempt("duplicate"), 1)
        self.assertEqual(len(self.store.attempts), 1)

    def test_attempt_requires_recorded_message_and_initial_state(self):
        with self.assertRaises(ContractError):
            ContractStore(binding()).create_attempt(attempt(), 1)
        with self.assertRaises(ContractError):
            self.store.create_attempt({**attempt(), "state": "delivered"}, 1)
        self.assertEqual(self.store.attempts, ())

    def test_disconnect_after_send_stays_uncertain_until_reconciled(self):
        self.sending()
        self.store.transition("attempt", 1, "uncertain", NOW)
        for state in ("failed", "queued", "delivered"):
            with self.assertRaises(ContractError):
                self.store.transition("attempt", 1, state, NOW)
        with self.assertRaises(ContractError):
            self.store.create_attempt(attempt("retry"), 1)
        self.assertEqual(
            self.store.checkpoint(1).pending_delivery[0].state, DeliveryState.UNCERTAIN
        )
        resolved = self.store.reconcile(self.evidence("delivered"), 1)
        self.assertEqual(resolved.state, DeliveryState.DELIVERED)
        self.store.transition(
            "attempt", 1, "completed", NOW, evidence_ref="fixture://completion"
        )
        self.assertEqual(self.store.checkpoint(1).pending_delivery, ())
        with self.assertRaises(ContractError):
            self.store.create_attempt(attempt("replay"), 1)

    def test_proved_non_delivery_allows_traceable_retry(self):
        self.sending()
        self.store.transition("attempt", 1, "uncertain", NOW)
        self.store.reconcile(self.evidence("not_delivered"), 1)
        self.store.create_attempt(attempt("retry"), 1)
        self.assertEqual(len(self.store.messages), 1)
        self.assertEqual(
            [a.state for a in self.store.attempts],
            [DeliveryState.FAILED, DeliveryState.RECORDED],
        )
        self.assertEqual(
            self.store.attempts[0].evidence_ref, "fixture://reconciliation"
        )

    def test_delivery_needs_evidence_and_does_not_imply_completion(self):
        self.sending()
        with self.assertRaises(ContractError):
            self.store.transition("attempt", 1, "delivered", NOW)
        self.store.transition(
            "attempt", 1, "delivered", NOW, evidence_ref="fixture://receipt"
        )
        self.assertEqual(self.store.checkpoint(1).native_status, NativeStatus.UNKNOWN)
        self.assertEqual(len(self.store.checkpoint(1).pending_delivery), 1)
        with self.assertRaises(ContractError):
            self.store.reconcile(self.evidence("not_delivered"), 1)
        with self.assertRaises(ContractError):
            self.store.transition("attempt", 1, "uncertain", NOW)

    def test_takeover_fences_all_writes_and_checkpoint_access(self):
        self.sending()
        self.store.take_ownership(1)
        operations = (
            lambda: self.store.record_message(message(), 1),
            lambda: self.store.create_attempt(attempt("new"), 1),
            lambda: self.store.transition("attempt", 1, "delivered", NOW),
            lambda: self.store.ingest(event(), 1),
            lambda: self.store.checkpoint(1),
            lambda: self.store.take_ownership(1),
            lambda: self.store.reconcile(self.evidence("delivered"), 1),
            lambda: self.store.ingest(event(), 2),
        )
        before = self.store.checkpoint(2)
        for operation in operations:
            with self.assertRaises(StaleOwnership):
                operation()
            self.assertEqual(self.store.checkpoint(2), before)
        self.assertEqual(before.pending_delivery[0].state, DeliveryState.UNCERTAIN)
        self.store.reconcile(self.evidence("completed"), 2)
        self.assertEqual(self.store.attempts[0].ownership_generation, 1)

    def test_out_of_order_events_wait_for_gap_and_duplicates_do_not_regress(self):
        self.store.ingest(event(2, "completed"), 1)
        self.assertEqual(self.store.checkpoint(1).evidence_cursor, 0)
        self.store.ingest(event(), 1)
        checkpoint = self.store.checkpoint(1)
        self.assertEqual(checkpoint.evidence_cursor, 2)
        self.assertEqual(checkpoint.native_status, NativeStatus.COMPLETED)
        self.store.ingest(
            event(ingested_at=(NOW + timedelta(seconds=1)).isoformat()), 1
        )
        self.assertEqual(self.store.checkpoint(1), checkpoint)

        self.assertEqual(len(self.store.events), 2)
        with self.assertRaises(ContractError):
            self.store.ingest(event(1, "interrupted"), 1)
        self.assertEqual(self.store.checkpoint(1), checkpoint)

    def test_takeover_can_retire_unsent_attempt_with_evidence(self):
        self.store.create_attempt(attempt(), 1)
        self.store.take_ownership(1)
        with self.assertRaises(ContractError):
            self.store.reconcile(self.evidence("delivered"), 2)
        self.store.reconcile(self.evidence("not_delivered"), 2)
        self.store.create_attempt(attempt("retry", generation=2), 2)
        self.assertEqual(len(self.store.attempts), 2)

    def test_takeover_reconnect_deduplicates_source_event(self):
        original = self.store.ingest(attention(), 1)
        self.store.take_ownership(1)
        before = self.store.checkpoint(2)
        replay = {
            **attention(),
            "ownership_generation": 2,
            "ingested_at": (NOW + timedelta(seconds=1)).isoformat(),
        }
        self.assertEqual(self.store.ingest(replay, 2), original)
        self.assertEqual(self.store.events, (original,))
        self.assertEqual(self.store.checkpoint(2), before)
        self.assertEqual(len(before.attention), 1)
        for raw, generation in ((attention(), 1), (attention(), 2), (replay, 1)):
            with self.assertRaises(StaleOwnership):
                self.store.ingest(raw, generation)
        with self.assertRaises(ContractError):
            self.store.ingest({**replay, "raw_evidence_ref": "fixture://other"}, 2)
        self.assertEqual(self.store.events, (original,))
        self.assertEqual(self.store.checkpoint(2), before)
        self.store.ingest(event(2, "completed", ownership_generation=2), 2)
        self.assertEqual(self.store.checkpoint(2).evidence_cursor, 2)

    def test_projection_deduplicates_reconnect_observations_in_any_order(self):
        original = self.store.ingest(attention(), 1)
        second = self.store.ingest(event(2), 1)
        self.store.take_ownership(1)
        replay = {
            **original.model_dump(mode="json"),
            "ownership_generation": 2,
            "ingested_at": (NOW + timedelta(seconds=1)).isoformat(),
        }
        expected = self.store.checkpoint(2)
        for records in (
            [original, second, replay],
            [replay, second, original],
            [second, original, replay],
        ):
            with self.subTest(records=records):
                self.assertEqual(
                    project_checkpoint(self.store.binding, records), expected
                )
        with self.assertRaises(ValueError):
            project_checkpoint(
                self.store.binding,
                [original, {**replay, "raw_evidence_ref": "fixture://other"}],
            )

    def test_takeover_can_fill_gap_before_historical_observation(self):
        buffered = self.store.ingest(event(2, "completed"), 1)
        self.assertEqual(self.store.checkpoint(1).evidence_cursor, 0)
        self.store.take_ownership(1)
        before = self.store.checkpoint(2)
        missing = event(1, ownership_generation=2)
        for raw, generation in ((event(1), 1), (event(1), 2), (missing, 1)):
            with self.assertRaises(StaleOwnership):
                self.store.ingest(raw, generation)
            self.assertEqual(self.store.checkpoint(2), before)
        replay = event(2, "completed", ownership_generation=2)
        self.assertEqual(self.store.ingest(replay, 2), buffered)
        self.assertEqual(self.store.checkpoint(2), before)
        self.store.ingest(missing, 2)
        checkpoint = self.store.checkpoint(2)
        self.assertEqual(checkpoint.evidence_cursor, 2)
        self.assertEqual(checkpoint.native_status, NativeStatus.COMPLETED)
        self.assertEqual(
            [
                (item.source_cursor, item.ownership_generation)
                for item in self.store.events
            ],
            [(1, 2), (2, 1)],
        )
        self.store.ingest(missing, 2)
        self.assertEqual(self.store.ingest(replay, 2), buffered)
        self.assertEqual(len(self.store.events), 2)
        self.assertEqual(self.store.checkpoint(2), checkpoint)
        self.assertEqual(
            project_checkpoint(
                self.store.binding,
                [item.model_dump(mode="json") for item in reversed(self.store.events)],
            ),
            checkpoint,
        )
        for invalid in (
            event(3, ownership_generation=3),
            event(3, binding_id="another-binding"),
        ):
            with self.assertRaises(ValueError):
                project_checkpoint(self.store.binding, [*self.store.events, invalid])

    def test_attention_correlation_deduplication_and_resolution(self):
        self.store.ingest(attention(), 1)
        self.store.ingest(attention(), 1)
        self.store.ingest(attention(2), 1)
        (item,) = self.store.checkpoint(1).attention
        self.assertEqual(item.logical_message_id, "message")
        self.assertEqual(item.source_cursor, 1)
        self.store.ingest(event(3, "attention_resolved", attention_key="question"), 1)
        (item,) = self.store.checkpoint(1).attention
        self.assertEqual(item.state, "resolved")
        self.store.ingest(attention(4), 1)
        self.assertEqual(self.store.checkpoint(1).attention[0].state, "resolved")
        self.assertEqual(self.store.checkpoint(1).native_status, NativeStatus.UNKNOWN)

    def test_resolving_one_attention_request_keeps_other_requests_waiting(self):
        self.store.ingest(attention(), 1)
        second = attention(2)
        second["attention"]["deduplication_key"] = "second-question"
        self.store.ingest(second, 1)
        resolution = event(3, "attention_resolved", attention_key="question")
        self.store.ingest(resolution, 1)
        checkpoint = self.store.checkpoint(1)
        self.assertEqual(checkpoint.native_status, NativeStatus.WAITING)
        self.assertEqual(
            {
                item.request.deduplication_key: item.state
                for item in checkpoint.attention
            },
            {"question": "resolved", "second-question": "pending"},
        )
        self.store.ingest(resolution, 1)
        self.assertEqual(self.store.checkpoint(1), checkpoint)
        self.assertEqual(
            project_checkpoint(self.store.binding, reversed(self.store.events)),
            checkpoint,
        )
        self.store.ingest(
            event(4, "attention_resolved", attention_key="second-question"), 1
        )
        final = self.store.checkpoint(1)
        self.assertEqual(final.native_status, NativeStatus.UNKNOWN)
        self.assertTrue(all(item.state == "resolved" for item in final.attention))

    def test_bad_attention_or_message_correlation_is_atomic(self):
        self.store.ingest(attention(), 1)
        before = self.store.events
        bad = attention(2)
        bad["attention"]["answer_shape"] = "boolean"
        with self.assertRaises(ValueError):
            self.store.ingest(bad, 1)
        with self.assertRaises(ContractError):
            self.store.ingest(event(2, logical_message_id="absent"), 1)
        with self.assertRaises(ValueError):
            self.store.ingest(event(2, "attention_resolved", attention_key="absent"), 1)
        self.assertEqual(self.store.events, before)

    def test_checkpoint_reconstruction_from_serialized_records(self):
        self.sending()
        self.store.transition("attempt", 1, "uncertain", NOW)
        self.store.ingest(attention(), 1)
        self.store.ingest(event(2, "transport_lost"), 1)
        checkpoint = self.store.checkpoint(
            1, repository_ref="fixture://repo", candidate_ref="abc123"
        )
        rebuilt = project_checkpoint(
            self.store.binding,
            [e.model_dump(mode="json") for e in reversed(self.store.events)],
            [a.model_dump(mode="json") for a in self.store.attempts],
            repository_ref="fixture://repo",
            candidate_ref="abc123",
        )
        self.assertEqual(rebuilt, checkpoint)
        self.assertEqual(rebuilt.native_status, NativeStatus.UNKNOWN)
        self.assertEqual(rebuilt.pending_delivery[0].state, DeliveryState.UNCERTAIN)

    def test_explicit_unsupported_and_unknown_capabilities(self):
        workspace = {
            "workspace_id": "workspace",
            "runtime_endpoint": "fixture://runtime",
            "observed_at": NOW,
            "capabilities": [
                {
                    "capability": "steering",
                    "state": "unsupported",
                    "detail": "Fixture interface does not expose steering",
                }
            ],
        }
        self.assertEqual(
            capability_result(workspace, "steering").state, CapabilityState.UNSUPPORTED
        )
        self.assertEqual(
            capability_result(workspace, "usage").state, CapabilityState.UNKNOWN
        )
        workspace["capabilities"][0]["state"] = "proved"
        with self.assertRaises(ValidationError):
            capability_result(workspace, "steering")
        workspace["capabilities"][0].update(
            scope="fixture", evidence_ref="fixture://proof"
        )
        self.assertEqual(capability_result(workspace, "steering").scope, "fixture")

    def test_malformed_external_data_is_rejected_before_mutation(self):
        for bad in (
            event(source_cursor="1"),
            event(source_cursor=True),
            event(source_cursor=0),
            event(normalized_type="guessed_completion"),
            event(extra_field="ignored?"),
            event(ingested_at="not-a-date"),
            event(ingested_at="2026-01-01T00:00:00"),
            event(1, "attention"),
            event(extension={"provider": "fixture", "output_tokens": -1}),
        ):
            with self.assertRaises(ValidationError):
                self.store.ingest(bad, 1)
        self.assertEqual(self.store.events, ())

    def test_reconciliation_is_correlated_and_time_cannot_regress(self):
        self.sending()
        self.store.transition("attempt", 1, "uncertain", NOW + timedelta(seconds=2))
        for bad in (
            self.evidence("delivered"),
            {**self.evidence("delivered"), "binding_id": "other"},
            {**self.evidence("delivered"), "evidence_ref": ""},
        ):
            with self.assertRaises(ValueError):
                self.store.reconcile(bad, 1)
        self.assertEqual(self.store.attempts[0].state, DeliveryState.UNCERTAIN)

    def test_unknown_usage_and_quiet_events_do_not_claim_completion(self):
        self.store.ingest(event(1, "unknown"), 1)
        self.store.ingest(event(2, "usage", extension={"provider": "fixture"}), 1)
        self.assertEqual(self.store.checkpoint(1).native_status, NativeStatus.UNKNOWN)
        self.assertIsNone(self.store.events[1].extension.output_tokens)


if __name__ == "__main__":
    unittest.main()
