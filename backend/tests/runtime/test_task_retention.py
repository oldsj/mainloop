"""Calendar boundaries and retry keys without runtime calls."""

import unittest
from datetime import UTC, datetime, timedelta
from types import SimpleNamespace

from mainloop.tasks.retention import (
    RetentionPolicy,
    calendar_months,
    due,
    minimum_calendar_days,
)


class RetentionTests(unittest.IsolatedAsyncioTestCase):
    def attempt(self, **changes):
        return SimpleNamespace(
            id="attempt",
            state="superseded",
            superseded_at=datetime(2026, 1, 31, tzinfo=UTC),
            archived_at=changes.get("archived_at"),
            native_deleted_at=changes.get("native_deleted_at"),
            retention_hold=changes.get("retention_hold"),
        )

    def test_exact_boundaries_and_calendar_clamping(self):
        attempt = self.attempt()
        policy = RetentionPolicy()
        archive = attempt.superseded_at + timedelta(days=21)
        self.assertIsNone(due(attempt, archive - timedelta(microseconds=1), policy))
        self.assertEqual(due(attempt, archive, policy), "archive")
        attempt.archived_at = archive
        deletion = datetime(2026, 3, 31, tzinfo=UTC)
        self.assertIsNone(due(attempt, deletion - timedelta(microseconds=1), policy))
        self.assertEqual(due(attempt, deletion, policy), "delete")
        self.assertEqual(
            calendar_months(attempt.superseded_at, 1), datetime(2026, 2, 28, tzinfo=UTC)
        )
        self.assertEqual(
            calendar_months(datetime(2023, 12, 31, tzinfo=UTC), 2),
            datetime(2024, 2, 29, tzinfo=UTC),
        )

    def test_config_hold_and_non_superseded_history(self):
        now = datetime(2026, 8, 1, tzinfo=UTC)
        self.assertIsNone(
            due(self.attempt(retention_hold="audit"), now, RetentionPolicy())
        )
        self.assertIsNone(
            due(self.attempt(native_deleted_at=now), now, RetentionPolicy())
        )
        attempt = self.attempt()
        attempt.state = "active"
        self.assertIsNone(due(attempt, now, RetentionPolicy()))
        attempt.state = "superseded"
        self.assertEqual(
            due(attempt, attempt.superseded_at, RetentionPolicy(0, 1)), "archive"
        )
        for values in ((-1, 2), (21, 0), (True, 2), (60, 2)):
            with self.assertRaises(ValueError):
                RetentionPolicy(*values)

    def test_earliest_calendar_interval_accepts_59_days_for_two_months(self):
        self.assertEqual(minimum_calendar_days(1), 28)
        self.assertEqual(minimum_calendar_days(2), 59)
        RetentionPolicy(59, 2)
        with self.assertRaises(ValueError):
            RetentionPolicy(60, 2)

    async def test_archive_visibility_does_not_delete_and_failed_delete_keeps_timestamp(
        self,
    ):
        from contextlib import ExitStack, asynccontextmanager
        from unittest.mock import AsyncMock, patch

        from mainloop.tasks.retention import reconcile_retention

        from models.task_handoff import RetentionReceipt

        attempt = self.attempt()
        attempt.session_id = "old-session"
        attempt.binding_id = "old-session"
        attempt.task_id = "task"
        attempt.evidence_refs = ()

        @asynccontextmanager
        async def lock(*args):
            yield

        conn = AsyncMock()
        conn.transaction = lock
        conn.fetchval.side_effect = lambda query, *args: (
            "native-old" if "kagent_session_id" in query else True
        )
        database = AsyncMock()

        @asynccontextmanager
        async def connection():
            yield conn

        database.connection = connection
        port = AsyncMock()
        port.safe_to_cleanup.return_value = True
        port.delete_native.return_value = RetentionReceipt(
            runtime_identity="native-old",
            qualification="offline_fake",
            attempt_id="attempt",
            session_id="old-session",
            action_id="task-retention:attempt:delete",
            confirmed=False,
            provenance="fake:cleanup",
            observed_at=datetime.now(UTC),
        )
        with ExitStack() as stack:
            for target, replacement in (
                (
                    "mainloop.db.tasks.retention_candidates",
                    AsyncMock(return_value=[{"id": "attempt"}]),
                ),
                (
                    "mainloop.tasks.lifecycle.load_attempt",
                    AsyncMock(return_value=attempt),
                ),
                ("mainloop.tasks.lifecycle.save_attempt", AsyncMock()),
                ("mainloop.tasks.lifecycle.load_task", AsyncMock()),
                ("mainloop.db.tasks.admission_lock", AsyncMock()),
                (
                    "mainloop.tasks.retention.receipt_storage_ready",
                    AsyncMock(return_value=True),
                ),
                ("mainloop.tasks.lifecycle.locked", lock),
                ("mainloop.push_gate.lifecycle.locked", lock),
            ):
                stack.enter_context(patch(target, replacement))
            # Archive uses model_copy; supply only the behavior needed by this fake.
            attempt.model_copy = lambda update: self.attempt(**update)
            await reconcile_retention(database, RetentionPolicy(), port, live=False)
            port.delete_native.assert_not_called()
            self.assertIn(
                "UPDATE sessions SET archived_at", conn.execute.call_args.args[0]
            )
            attempt.archived_at = datetime(2026, 2, 21, tzinfo=UTC)
            for _ in range(2):
                await reconcile_retention(database, RetentionPolicy(), port, live=False)
            self.assertIsNone(attempt.native_deleted_at)
            self.assertEqual(port.delete_native.call_count, 2)
            port.delete_native.assert_called_with(
                conn, attempt, "task-retention:attempt:delete"
            )

    def test_maximum_typed_receipt_fits_full_artifact_with_all_json_encodings(self):
        from mainloop.tasks.retention import receipt_payload

        from models.task_handoff import RetentionReceipt

        for character in ("\x00", "😀", '"'):
            receipt = RetentionReceipt(
                runtime_identity=character * 100,
                attempt_id=character * 100,
                session_id=character * 100,
                action_id=character * 100,
                qualification="qualified_live",
                confirmed=True,
                provenance=character * 2048,
                observed_at=datetime.now(UTC),
            )
            self.assertEqual(
                RetentionReceipt.model_validate(receipt_payload(receipt)), receipt
            )
