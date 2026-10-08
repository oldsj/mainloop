"""Trusted superseded-session policy; called by the existing dispatcher only."""

import calendar
import json
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from functools import lru_cache
from typing import Literal, Protocol

from asyncpg import PostgresError
from pydantic import ValidationError

from models.task import TaskArtifact, TaskAttempt
from models.task_handoff import RetentionReceipt

RECEIPT_KIND = "retention_receipt"


@dataclass(frozen=True)
class RetentionPolicy:
    archive_after_days: int = 21
    delete_after_months: int = 2

    def __post_init__(self):
        if type(self.archive_after_days) is not int or self.archive_after_days < 0:
            raise ValueError("invalid archive interval")
        if type(self.delete_after_months) is not int or self.delete_after_months < 1:
            raise ValueError("invalid deletion interval")
        if self.archive_after_days > minimum_calendar_days(self.delete_after_months):
            raise ValueError("deletion interval must follow archive")


def calendar_months(value: datetime, months: int) -> datetime:
    month_index = value.year * 12 + value.month - 1 + months
    year, month = divmod(month_index, 12)
    month += 1
    return value.replace(
        year=year, month=month, day=min(value.day, calendar.monthrange(year, month)[1])
    )


def due(
    attempt: TaskAttempt, now: datetime, policy: RetentionPolicy
) -> Literal["archive", "delete"] | None:
    if now.utcoffset() is None:
        raise ValueError("retention clock must be timezone aware")
    if (
        attempt.state != "superseded"
        or attempt.superseded_at is None
        or attempt.retention_hold
    ):
        return None
    if attempt.superseded_at.utcoffset() is None:
        raise ValueError("superseded timestamp must be timezone aware")
    if attempt.native_deleted_at is not None:
        return None
    if attempt.archived_at is None:
        if now >= attempt.superseded_at + timedelta(days=policy.archive_after_days):
            return "archive"
        return None
    if now >= calendar_months(attempt.superseded_at, policy.delete_after_months):
        return "delete"
    return None


class RetentionPort(Protocol):
    """Trusted adapter checks unresolved HITL/publication and reconciles exact native identity.

    Deletion receipt confirms only native/runtime data cleanup; durable task, attempt,
    mirrored conversation, artifacts, environment and publication audit remain.
    """

    async def safe_to_cleanup(self, conn, attempt: TaskAttempt) -> bool: ...
    async def delete_native(
        self, conn, attempt: TaskAttempt, action_id: str
    ) -> RetentionReceipt: ...


def receipt_payload(receipt: RetentionReceipt) -> dict:
    """Full typed artifact, using the existing 32 KiB canonical JSON storage."""
    payload = receipt.model_dump(mode="json")
    content = json.dumps(
        payload, sort_keys=True, separators=(",", ":"), ensure_ascii=False
    )
    if len(content.encode("utf-8")) > 32768:
        raise ValueError("retention receipt exceeds artifact contract")
    return payload


def validate_receipt_storage(attempt: TaskAttempt) -> None:
    # Every bounded string's worst JSON encoding is six bytes per character.
    # Check the maximum accepted contract before any external deletion.
    receipt_payload(
        RetentionReceipt(
            runtime_identity="\x00" * 100,
            attempt_id="\x00" * 100,
            session_id="\x00" * 100,
            action_id="\x00" * 100,
            qualification="qualified_live",
            confirmed=True,
            provenance="\x00" * 2048,
            observed_at=datetime.now(UTC),
        )
    )
    TaskAttempt.model_validate(
        {
            **attempt.model_dump(),
            "evidence_refs": (*attempt.evidence_refs, "retention-receipt:" + "0" * 32),
        }
    )


async def receipt_storage_ready(
    conn, attempt: TaskAttempt, operation_id: str, runtime_identity: str, action_id: str
) -> bool:
    """Exercise the real kind/size/model path before cleanup, rolling back the probe."""
    from mainloop.db import tasks as store
    from mainloop.tasks import lifecycle
    from mainloop.tasks.principal import TaskPrincipal

    try:
        validate_receipt_storage(attempt)
        # Validate the actual action/native scope too. This is an unconfirmed,
        # unsupported storage probe, never evidence that native deletion occurred.
        probe_receipt = RetentionReceipt(
            runtime_identity=runtime_identity,
            attempt_id=attempt.id,
            session_id=attempt.session_id,
            action_id=action_id,
            qualification="unsupported",
            confirmed=False,
            provenance="\x00" * 2048,
            observed_at=datetime.now(UTC),
        )
        async with conn.transaction():
            await store.admission_lock(conn)
            task = await lifecycle.load_task(conn, attempt.task_id, lock=True)
            current = await lifecycle.load_attempt(conn, attempt.id, lock=True)
            if current != attempt:
                return False
            probe = conn.transaction()
            await probe.start()
            try:
                artifact_id = await store.add_artifact(
                    conn, operation_id, RECEIPT_KIND, receipt_payload(probe_receipt)
                )
                artifact = TaskArtifact.model_validate(
                    await store.get_artifact(
                        conn, artifact_id, TaskPrincipal(task.owner_id)
                    )
                )
                if RetentionReceipt.model_validate(artifact.payload) != probe_receipt:
                    return False
                TaskAttempt.model_validate(
                    {
                        **current.model_dump(),
                        "evidence_refs": (
                            *current.evidence_refs,
                            "retention-receipt:" + artifact.id,
                        ),
                    }
                )
                return True
            finally:
                # Rollback removes the uncommitted row without touching immutable
                # artifacts, so the original receipt can own this operation/kind.
                await probe.rollback()
    except (PostgresError, store.TaskError, ValidationError, ValueError):
        return False


async def reconcile_retention(
    database, policy: RetentionPolicy, port: RetentionPort, *, live=True
):
    """One existing-dispatcher pass. Native cleanup remains adapter-owned and opt-in."""
    from mainloop.db import tasks as store
    from mainloop.push_gate import lifecycle as push_lifecycle
    from mainloop.tasks import lifecycle

    async with database.connection() as conn:
        rows = await store.retention_candidates(conn)
        for row in rows:
            attempt = await lifecycle.load_attempt(conn, row["id"])
            key = f"task-retention:{attempt.id}"
            if not await conn.fetchval(
                "SELECT pg_try_advisory_lock(hashtextextended($1,0))", key
            ):
                continue
            try:
                async with push_lifecycle.locked(
                    conn, attempt.session_id
                ), lifecycle.locked(conn, attempt.session_id):
                    attempt = await lifecycle.load_attempt(conn, attempt.id)
                    action = due(attempt, datetime.now(UTC), policy)
                    if action is None or not await port.safe_to_cleanup(conn, attempt):
                        continue
                    action_id = f"task-retention:{attempt.id}:{action}"
                    if action == "archive":
                        # Visibility only; never invokes immediate native deletion.
                        async with conn.transaction():
                            await store.admission_lock(conn)
                            await lifecycle.load_task(conn, attempt.task_id, lock=True)
                            current = await lifecycle.load_attempt(
                                conn, attempt.id, lock=True
                            )
                            if current != attempt:
                                continue
                            now = datetime.now(UTC)
                            await conn.execute(
                                "UPDATE sessions SET archived_at=$2 WHERE id=$1",
                                attempt.session_id,
                                now,
                            )
                            await lifecycle.save_attempt(
                                conn, current.model_copy(update={"archived_at": now})
                            )
                        continue
                    runtime_identity = await conn.fetchval(
                        "SELECT kagent_session_id FROM native_bindings WHERE session_id=$1",
                        attempt.binding_id,
                    )
                    if len(attempt.evidence_refs) >= 64:
                        continue  # Preserve space for the confirmed cleanup audit.
                    # Associate audit with the existing operation that superseded
                    # this exact attempt; never invent another audit operation.
                    operation_id = await conn.fetchval(
                        "SELECT id FROM task_operations WHERE task_id=$1 "
                        "AND snapshot->>'source_attempt_id'=$2 "
                        "AND kind IN ('retry','reassign') ORDER BY created_at,id LIMIT 1",
                        attempt.task_id,
                        attempt.id,
                    )
                    if operation_id is None:
                        continue
                    if not await receipt_storage_ready(
                        conn, attempt, operation_id, runtime_identity, action_id
                    ):
                        continue
                    receipt = await port.delete_native(conn, attempt, action_id)
                    if (
                        receipt.runtime_identity != runtime_identity
                        or receipt.qualification
                        != ("qualified_live" if live else "offline_fake")
                        or receipt.observed_at > datetime.now(UTC)
                    ):
                        continue
                    if not receipt.confirmed or (
                        receipt.attempt_id,
                        receipt.session_id,
                        receipt.action_id,
                    ) != (attempt.id, attempt.session_id, action_id):
                        continue
                    async with conn.transaction():
                        await store.admission_lock(conn)
                        await lifecycle.load_task(conn, attempt.task_id, lock=True)
                        current = await lifecycle.load_attempt(
                            conn, attempt.id, lock=True
                        )
                        if current != attempt:
                            continue
                        artifact_id = await store.add_artifact(
                            conn,
                            operation_id,
                            RECEIPT_KIND,
                            receipt_payload(receipt),
                        )
                        await lifecycle.save_attempt(
                            conn,
                            TaskAttempt.model_validate(
                                {
                                    **current.model_dump(),
                                    "native_deleted_at": receipt.observed_at,
                                    "evidence_refs": (
                                        *current.evidence_refs,
                                        "retention-receipt:" + artifact_id,
                                    ),
                                }
                            ),
                        )
            finally:
                await conn.fetchval(
                    "SELECT pg_advisory_unlock(hashtextextended($1,0))", key
                )


@lru_cache(maxsize=128)
def minimum_calendar_days(months: int) -> int:
    """Earliest clamped deletion boundary across the Gregorian 400-year cycle.

    For each starting month the final day minimizes the elapsed interval; any
    earlier unclamped day has the same or a longer interval. Full cycles repeat.
    """
    cycles, remainder = divmod(months, 4800)
    minimum = min(
        (calendar_months(start, remainder) - start).days
        for year in range(2000, 2400)
        for month in range(1, 13)
        for start in (
            datetime(year, month, calendar.monthrange(year, month)[1], tzinfo=UTC),
        )
    )
    return cycles * 146097 + minimum
