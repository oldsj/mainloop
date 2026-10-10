"""Transactional health import and rebuildable, path-free turn summaries."""

import json
from datetime import UTC, datetime, timedelta

from mainloop.config import settings
from mainloop.health.aggregation import summarize
from pydantic import ValidationError

from models.orchestration_health import event_adapter, is_health_part

MAX_EVENTS = 2048
MAX_BYTES = 8192


async def attribution(conn, session_id, message_id, task_id):
    return await conn.fetchrow(
        """SELECT s.user_id AS owner_id,CASE WHEN a.id IS NULL THEN s.project_id ELSE t.project_id END AS project_id,s.id AS session_id,
                  b.kagent_session_id AS runtime_session_id,b.kind AS provider,
                  a.task_id,a.id AS attempt_id,a.configuration_revision AS profile_revision
           FROM sessions s JOIN native_bindings b ON b.session_id=s.id
           JOIN native_deliveries d ON d.session_id=s.id AND d.message_id=$2 AND d.task_id=$3
           LEFT JOIN task_attempts a ON a.session_id=s.id
           LEFT JOIN tasks t ON t.id=a.task_id
           WHERE s.id=$1 AND (a.id IS NULL OR t.owner_id=s.user_id)""",
        session_id,
        message_id,
        task_id,
    )


async def diagnostic(conn, attr, gateway, task_id, reason):
    await conn.execute(
        """INSERT INTO orchestration_health_collection
           (owner_id,gateway,session_id,a2a_task_id,project_id,reasons)
           VALUES($1,$2,$3,$4,$5,ARRAY[$6]::text[])
           ON CONFLICT(owner_id,gateway,session_id,a2a_task_id) DO UPDATE
           SET reasons=(SELECT ARRAY(SELECT DISTINCT unnest(orchestration_health_collection.reasons || EXCLUDED.reasons))),updated_at=now()""",
        attr["owner_id"],
        gateway,
        attr["session_id"],
        task_id,
        attr["project_id"],
        reason,
    )


async def ingest(
    conn, session_id, message_id, task_id, context_id, artifacts, *, gateway, now=None
):
    """Caller owns a transaction. Stream and snapshot import share this path."""
    now = now or datetime.now(UTC)
    await conn.execute(
        "SELECT pg_advisory_xact_lock_shared(hashtextextended('orchestration-health-retention',0))"
    )
    attr = await attribution(conn, session_id, message_id, task_id)
    if not attr:
        return
    # Serialize this receipt's event bound and turn rebuild; never held over I/O.
    await conn.execute(
        "SELECT pg_advisory_xact_lock(hashtextextended($1,0))",
        f"health:{attr['owner_id']}:{gateway}:{session_id}:{task_id}",
    )
    cutoff = now - timedelta(days=settings.health_event_retention_days)
    frozen = {
        row["turn_key"]
        for row in await conn.fetch(
            "SELECT turn_key FROM orchestration_health_turns WHERE owner_id=$1 AND gateway=$2 AND session_id=$3 AND a2a_task_id=$4 AND frozen",
            attr["owner_id"],
            gateway,
            session_id,
            task_id,
        )
    }
    changed = set()
    count = await conn.fetchval(
        "SELECT count(*) FROM orchestration_health_events WHERE owner_id=$1 AND gateway=$2 AND session_id=$3 AND a2a_task_id=$4",
        attr["owner_id"],
        gateway,
        session_id,
        task_id,
    )
    for artifact in artifacts:
        for part in artifact.parts:
            raw = part.data
            if not is_health_part(artifact.name, raw):
                continue
            encoded = json.dumps(raw, separators=(",", ":"))
            if len(encoded.encode()) > MAX_BYTES:
                await diagnostic(conn, attr, gateway, task_id, "overflow")
                continue
            try:
                event = event_adapter.validate_json(encoded)
            except ValidationError:
                await diagnostic(conn, attr, gateway, task_id, "invalid_event")
                continue
            if (
                event.a2a_task_id != task_id
                or event.runtime_session_id != context_id
                or context_id != attr["runtime_session_id"]
                or event.provider != attr["provider"]
                or artifact.artifact_id != event.event_id
            ):
                await diagnostic(conn, attr, gateway, task_id, "identity_mismatch")
                continue
            if event.occurred_at <= cutoff:
                continue  # Expired replay must not restore detail or increment frozen totals.
            if event.turn_key in frozen:
                continue
            if event.occurred_at > now + timedelta(minutes=5):
                await diagnostic(conn, attr, gateway, task_id, "invalid_timestamp")
                continue
            typed = json.loads(event.model_dump_json())
            # Preserve the source's nanoseconds: Python datetime stores only
            # microseconds, but identity conflicts compare the full typed data.
            typed["occurred_at"] = raw["occurred_at"]
            payload = json.dumps(typed)
            existing = await conn.fetchrow(
                "SELECT payload,session_id,a2a_task_id FROM orchestration_health_events WHERE owner_id=$1 AND gateway=$2 AND event_id=$3",
                attr["owner_id"],
                gateway,
                event.event_id,
            )
            if existing:
                if (
                    json.loads(existing["payload"]) != json.loads(payload)
                    or existing["session_id"] != session_id
                    or existing["a2a_task_id"] != task_id
                ):
                    await diagnostic(conn, attr, gateway, task_id, "identity_conflict")
                continue
            if count >= MAX_EVENTS:
                await diagnostic(conn, attr, gateway, task_id, "overflow")
                continue
            inserted = await conn.fetchval(
                """INSERT INTO orchestration_health_events
                   (owner_id,gateway,event_id,producer_epoch,sequence,session_id,runtime_session_id,a2a_task_id,
                    project_id,task_id,attempt_id,profile_revision,turn_key,occurred_at,payload)
                   VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15::jsonb)
                   ON CONFLICT DO NOTHING RETURNING event_id""",
                attr["owner_id"],
                gateway,
                event.event_id,
                event.producer_epoch,
                event.sequence,
                session_id,
                context_id,
                task_id,
                attr["project_id"],
                attr["task_id"],
                attr["attempt_id"],
                attr["profile_revision"],
                event.turn_key,
                event.occurred_at,
                payload,
            )
            if not inserted:
                await diagnostic(conn, attr, gateway, task_id, "identity_conflict")
                continue
            count += 1
            changed.add(event.turn_key)
    for turn_key in changed:
        rows = await conn.fetch(
            "SELECT payload FROM orchestration_health_events WHERE owner_id=$1 AND gateway=$2 AND session_id=$3 AND a2a_task_id=$4 AND turn_key=$5 ORDER BY sequence",
            attr["owner_id"],
            gateway,
            session_id,
            task_id,
            turn_key,
        )
        events = [event_adapter.validate_json(row["payload"]) for row in rows]
        await conn.execute(
            """INSERT INTO orchestration_health_turns
               (owner_id,gateway,session_id,runtime_session_id,a2a_task_id,turn_key,project_id,
                task_id,attempt_id,profile_revision,provider,occurred_at,summary)
               VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13::jsonb)
               ON CONFLICT(owner_id,gateway,session_id,a2a_task_id,turn_key) DO UPDATE
               SET summary=EXCLUDED.summary,occurred_at=EXCLUDED.occurred_at""",
            attr["owner_id"],
            gateway,
            session_id,
            context_id,
            task_id,
            turn_key,
            attr["project_id"],
            attr["task_id"],
            attr["attempt_id"],
            attr["profile_revision"],
            events[0].provider,
            max(event.occurred_at for event in events),
            json.dumps(summarize(events)),
        )


async def retain(conn, *, now=None):
    """Freeze aggregates before removing detail; never rebuild an expired turn."""
    now = now or datetime.now(UTC)
    await conn.execute(
        "SELECT pg_advisory_xact_lock(hashtextextended('orchestration-health-retention',0))"
    )
    cutoff = now - timedelta(days=settings.health_event_retention_days)
    await conn.execute(
        """UPDATE orchestration_health_turns t SET frozen=true WHERE EXISTS (
        SELECT 1 FROM orchestration_health_events e WHERE e.owner_id=t.owner_id AND e.gateway=t.gateway
        AND e.session_id=t.session_id AND e.a2a_task_id=t.a2a_task_id AND e.turn_key=t.turn_key AND e.occurred_at <= $1)""",
        cutoff,
    )
    await conn.execute(
        """DELETE FROM orchestration_health_events e WHERE occurred_at <= $1 OR EXISTS (
        SELECT 1 FROM orchestration_health_turns t WHERE t.owner_id=e.owner_id AND t.gateway=e.gateway
        AND t.session_id=e.session_id AND t.a2a_task_id=e.a2a_task_id AND t.turn_key=e.turn_key AND t.frozen)""",
        cutoff,
    )
    await conn.execute(
        "DELETE FROM orchestration_health_turns WHERE occurred_at <= $1",
        now - timedelta(days=settings.health_summary_retention_days),
    )
    await conn.execute(
        "DELETE FROM orchestration_health_collection WHERE updated_at <= $1",
        now - timedelta(days=settings.health_summary_retention_days),
    )


async def delete_owner(conn, owner_id):
    for query in (
        "DELETE FROM orchestration_health_events WHERE owner_id=$1",
        "DELETE FROM orchestration_health_turns WHERE owner_id=$1",
        "DELETE FROM orchestration_health_collection WHERE owner_id=$1",
    ):
        await conn.execute(query, owner_id)
