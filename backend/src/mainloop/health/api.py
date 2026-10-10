"""Owner-only reads of measured activity and existing report/approval claims."""

import json
from datetime import UTC, datetime, timedelta

from fastapi import APIRouter, Depends, HTTPException, Query
from mainloop.config import settings
from mainloop.db import db, health
from mainloop.health import collector
from mainloop.identity import current_user

router = APIRouter(tags=["orchestration health"])


async def authorize(
    conn, owner, project_id=None, task_id=None, session_id=None, attempt_id=None
):
    for value, query in (
        (project_id, "SELECT 1 FROM projects WHERE user_id=$1 AND id=$2"),
        (task_id, "SELECT 1 FROM tasks WHERE owner_id=$1 AND id=$2"),
        (
            session_id,
            "SELECT 1 FROM sessions WHERE user_id=$1 AND id=$2 UNION ALL SELECT 1 FROM orchestration_health_turns WHERE owner_id=$1 AND session_id=$2",
        ),
        (
            attempt_id,
            "SELECT 1 FROM task_attempts a JOIN tasks t ON t.id=a.task_id WHERE t.owner_id=$1 AND a.id=$2",
        ),
    ):
        if value and not await conn.fetchval(query, owner, value):
            raise HTTPException(404, "Not Found")


def totals(turns):
    known = [turn["summary"] for turn in turns]
    inputs = [row["input_tokens"] for row in known if row["input_tokens"] is not None]
    outputs = [
        row["output_tokens"] for row in known if row["output_tokens"] is not None
    ]
    return {
        "turns": len(known),
        "usage_observed_turns": sum(
            row["input_tokens"] is not None and row["output_tokens"] is not None
            for row in known
        ),
        "usage_complete_turns": sum(
            row["usage_coverage"] == "complete" for row in known
        ),
        "input_tokens": sum(inputs) if inputs else None,
        "output_tokens": sum(outputs) if outputs else None,
        "failures": sum(row["failures"] for row in known) if known else None,
        "repeated_failure_groups": (
            sum(row["repeated_failure_groups"] for row in known) if known else None
        ),
        "unresolved_calls": sum(row["unresolved_calls"] for row in known),
        "coverage": (
            "complete"
            if known
            and all(
                row["friction_coverage"] == "complete"
                and row["usage_coverage"] == "complete"
                for row in known
            )
            else "partial" if known else "unknown"
        ),
    }


@router.get("/orchestration-health")
async def read_health(
    project_id: str | None = None,
    task_id: str | None = None,
    session_id: str | None = None,
    attempt_id: str | None = None,
    provider: str | None = Query(None, pattern="^(claude|codex)$"),
    hours: int = Query(24, ge=1, le=8760),
    owner: str = Depends(current_user),
):
    async with db.connection() as conn:
        await authorize(conn, owner, project_id, task_id, session_id, attempt_id)
    recovery_limited = await collector.recover(owner, session_id=session_id)
    async with db.connection() as conn, conn.transaction():
        await health.retain(conn)
        since = datetime.now(UTC) - timedelta(hours=hours)
        turns = [
            dict(row)
            for row in await conn.fetch(
                """SELECT * FROM orchestration_health_turns WHERE owner_id=$1 AND occurred_at>$2
               AND ($3::text IS NULL OR project_id=$3) AND ($4::text IS NULL OR task_id=$4)
               AND ($5::text IS NULL OR session_id=$5) AND ($6::text IS NULL OR attempt_id=$6)
               AND ($7::text IS NULL OR provider=$7) ORDER BY occurred_at DESC LIMIT 2001""",
                owner,
                since,
                project_id,
                task_id,
                session_id,
                attempt_id,
                provider,
            )
        ]
        truncated = len(turns) > 2000
        turns = turns[:2000]
        for turn in turns:
            turn["summary"] = json.loads(turn["summary"])
        sessions = [
            dict(row)
            for row in await conn.fetch(
                """SELECT s.id AS session_id,s.title,b.kind AS provider,s.project_id,
                      a.task_id,a.id AS attempt_id,t.snapshot->>'title' AS task_title
               FROM sessions s JOIN native_bindings b ON b.session_id=s.id
               LEFT JOIN task_attempts a ON a.session_id=s.id
               LEFT JOIN tasks t ON t.id=a.task_id AND t.owner_id=s.user_id
               WHERE s.user_id=$1 AND ($2::text IS NULL OR s.project_id=$2)
                 AND ($3::text IS NULL OR t.id=$3) AND ($4::text IS NULL OR s.id=$4)
                 AND ($5::text IS NULL OR a.id=$5) AND ($6::text IS NULL OR b.kind=$6)
               ORDER BY s.created_at DESC LIMIT 501""",
                owner,
                project_id,
                task_id,
                session_id,
                attempt_id,
                provider,
            )
        ]
        truncated |= len(sessions) > 500
        sessions = sessions[:500]
        if session_id is None:
            sessions.extend(
                dict(row)
                for row in await conn.fetch(
                    """SELECT NULL::text AS session_id,t.snapshot->>'title' AS task_title,
                          t.id AS task_id,t.project_id,NULL::text AS attempt_id,'unknown'::text AS provider
                   FROM tasks t WHERE t.owner_id=$1 AND NOT EXISTS
                     (SELECT 1 FROM task_attempts a WHERE a.task_id=t.id AND a.session_id IS NOT NULL)
                     AND ($2::text IS NULL OR t.project_id=$2) AND ($3::text IS NULL OR t.id=$3)
                     AND $4::text IS NULL AND $5::text IS NULL LIMIT 500""",
                    owner,
                    project_id,
                    task_id,
                    attempt_id,
                    provider,
                )
            )
        reports = [
            dict(row)
            for row in await conn.fetch(
                """SELECT r.id,r.task_id,r.attempt_id,r.snapshot->>'outcome' AS outcome
               FROM task_reports r JOIN tasks t ON t.id=r.task_id
               WHERE t.owner_id=$1 AND r.snapshot->>'outcome' IN ('blocked','failed')
                 AND r.created_at>$2""",
                owner,
                since,
            )
        ]
        approvals = [
            dict(row)
            for row in await conn.fetch(
                """SELECT id,snapshot->'outer'->>'runtime_session_id' AS runtime_session_id,
                      snapshot->>'availability' AS state
               FROM native_hitl_requests WHERE owner_id=$1 AND NOT superseded""",
                owner,
            )
        ]
        gaps = [
            dict(row)
            for row in await conn.fetch(
                "SELECT * FROM orchestration_health_collection WHERE owner_id=$1", owner
            )
        ]
        receipts = [
            dict(row)
            for row in await conn.fetch(
                """SELECT d.session_id,count(*) AS receipts FROM native_deliveries d
               JOIN sessions s ON s.id=d.session_id WHERE s.user_id=$1
               AND d.created_at>$2 AND d.task_id IS NOT NULL GROUP BY d.session_id""",
                owner,
                since,
            )
        ]
        runtime_ids = {
            row["session_id"]: row["kagent_session_id"]
            for row in await conn.fetch(
                "SELECT session_id,kagent_session_id FROM native_bindings WHERE session_id=ANY($1::text[])",
                [row["session_id"] for row in sessions],
            )
        }
    groups = {}
    for seed in sessions + turns:
        key = seed["task_id"] or f"unassigned:{seed['session_id']}"
        group = groups.setdefault(
            key,
            {
                "id": key,
                "task_id": seed["task_id"],
                "project_id": seed["project_id"],
                "title": seed.get("task_title")
                or (seed.get("title") if seed["task_id"] else "Unassigned"),
                "sessions": [],
                "turns": [],
            },
        )
        if "summary" in seed:
            group["turns"].append(seed)
        elif not any(
            row["session_id"] == seed["session_id"] for row in group["sessions"]
        ):
            group["sessions"].append(seed)
    for group in groups.values():
        selected = group["turns"]
        group["totals"] = totals(selected)
        group["reports"] = [
            row
            for row in reports
            if row["task_id"] == group["task_id"]
            and (attempt_id is None or row["attempt_id"] == attempt_id)
        ]
        sids = {row["session_id"] for row in group["sessions"] + selected}
        group["approvals"] = [
            row
            for row in approvals
            if row["runtime_session_id"] in {runtime_ids.get(sid) for sid in sids}
        ]
        group["gaps"] = sorted(
            {
                reason
                for row in gaps
                if row["session_id"] in sids
                for reason in row["reasons"]
            }
        )
        group["expected_receipts"] = sum(
            row["receipts"] for row in receipts if row["session_id"] in sids
        )
        receipts_seen = {row["a2a_task_id"] for row in selected}
        if len(receipts_seen) < len(selected):
            group["gaps"] = sorted(set(group["gaps"]) | {"producer_restart"})
        if group["gaps"] or group["expected_receipts"] > len(receipts_seen):
            group["totals"]["coverage"] = "partial" if selected else "unknown"
        for session in group["sessions"]:
            session["totals"] = totals(
                [row for row in selected if row["session_id"] == session["session_id"]]
            )
    return {
        "rows": list(groups.values()),
        "hours": hours,
        "partial": recovery_limited or truncated,
        "unsupported": [
            "cache",
            "exact_approval_wait",
            "codex_usage",
            "native_child_attribution",
        ],
        "retention": {
            "events_days": settings.health_event_retention_days,
            "summaries_days": settings.health_summary_retention_days,
        },
    }


@router.get("/orchestration-health/events")
async def read_events(
    session_id: str,
    hours: int = Query(24, ge=1, le=8760),
    owner: str = Depends(current_user),
):
    async with db.connection() as conn, conn.transaction():
        await authorize(conn, owner, session_id=session_id)
        await health.retain(conn)
        rows = await conn.fetch(
            "SELECT event_id,task_id,attempt_id,received_at,payload FROM orchestration_health_events WHERE owner_id=$1 AND session_id=$2 AND occurred_at>$3 ORDER BY occurred_at,sequence LIMIT 201",
            owner,
            session_id,
            datetime.now(UTC) - timedelta(hours=hours),
        )
        return {
            "events": [
                {**dict(row), "payload": json.loads(row["payload"])}
                for row in rows[:200]
            ],
            "partial": len(rows) > 200,
        }
