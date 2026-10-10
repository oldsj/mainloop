"""Sanitized Claude artifacts, real PostgreSQL, fake gateway; no provider run."""

import copy
import json
from datetime import UTC, datetime, timedelta
from pathlib import Path
from unittest.mock import AsyncMock, patch

import httpx
from mainloop import api
from mainloop.config import settings
from mainloop.db import db, health
from mainloop.health import collector
from mainloop.health.api import totals
from mainloop.runtime import native_sessions as ns
from mainloop.runtime.kagent_client import ArtifactUpdate, StreamEvent, Task
from pydantic import ValidationError
from tests.runtime.test_postgres_ledger import PostgresTestCase

from models.orchestration_health import event_adapter

FIXTURE = Path(__file__).parent / "fixtures/kagent/health-tasks.json"


def fixtures(now=None):
    values = json.loads(FIXTURE.read_text())
    now = now or datetime.now(UTC)
    for task in values:
        for artifact in task["artifacts"]:
            artifact["parts"][0]["data"]["occurred_at"] = now.isoformat()
    return [Task.model_validate(value) for value in values]


class OrchestrationHealthTests(PostgresTestCase):
    reuse_database = True

    async def asyncSetUp(self):
        await super().asyncSetUp()
        self.sid, self.cid = await self.bound_session(role="main")
        self.tasks = fixtures()
        self.context = self.tasks[0].context_id
        await ns.ledger.update_binding(self.sid, kagent_session_id=self.context)
        self.messages = [
            await self.delivery(self.sid, self.cid, "completed") for _ in self.tasks
        ]
        for task, message in zip(self.tasks, self.messages, strict=False):
            await self.pool.execute(
                "UPDATE native_deliveries SET task_id=$2 WHERE message_id=$1",
                message,
                task.id,
            )
        self.tid, self.attempt = "product-" + self.user, "attempt-" + self.user
        await self.pool.execute(
            """INSERT INTO tasks(id,owner_id,root_task_id,mode,status,version,snapshot)
            VALUES($1,$2,$1,'coordination','running',1,'{"title":"Fixture task"}')""",
            self.tid,
            self.user,
        )
        await self.pool.execute(
            """INSERT INTO task_attempts(id,task_id,number,profile_id,native_provider,configuration_revision,
            agent_ref,role,depth,state,session_id,snapshot) VALUES($1,$2,1,'claude','claude','fixture-r1','{}','supervisor',1,'active',$3,'{}')""",
            self.attempt,
            self.tid,
            self.sid,
        )

    async def ingest(self, task=None, message=None, **options):
        task = task or self.tasks[0]
        message = message or self.messages[0]
        async with db.connection() as conn, conn.transaction():
            await health.ingest(
                conn,
                self.sid,
                message,
                task.id,
                task.context_id,
                task.artifacts,
                gateway=settings.kagent_gateway_url,
                **options,
            )

    async def summaries(self):
        return [
            {"summary": json.loads(row["summary"])}
            for row in await self.pool.fetch(
                "SELECT summary FROM orchestration_health_turns WHERE owner_id=$1",
                self.user,
            )
        ]

    async def test_stream_snapshot_restart_and_attribution(self):
        first = self.tasks[0]

        # A stream drops before the last usage/closure; recovery imports GetTask.
        async def dropped_stream():
            for artifact in first.artifacts[:4]:
                yield StreamEvent(
                    artifact_update=ArtifactUpdate(
                        task_id=first.id, context_id=first.context_id, artifact=artifact
                    )
                )

        await ns._consume(
            self.sid, self.messages[0], await ns.get_binding(self.sid), dropped_stream()
        )
        self.assertEqual(
            (await self.summaries())[0]["summary"]["friction_coverage"], "partial"
        )
        await self.ingest(first.model_copy(update={"artifacts": [first.artifacts[2]]}))
        await self.ingest(first)
        await self.pool.close()
        import asyncpg

        self.pool = await asyncpg.create_pool(self.url, min_size=1, max_size=3)
        db._pool = self.pool
        client = AsyncMock()
        client.get_task.side_effect = lambda _agent, task_id: next(
            task for task in self.tasks if task.id == task_id
        )
        with patch.object(ns, "get_client", return_value=client), patch.object(
            ns, "binding_agent_ref", new=AsyncMock(return_value="fixture")
        ):
            await collector.recover(self.user)
            await collector.recover(self.user)
        result = totals(await self.summaries())
        self.assertEqual((result["input_tokens"], result["output_tokens"]), (7, 3))
        self.assertEqual(
            (result["failures"], result["repeated_failure_groups"]), (3, 1)
        )
        self.assertEqual(result["usage_observed_turns"], 2)
        rows = await self.pool.fetch(
            "SELECT DISTINCT task_id,attempt_id,session_id,profile_revision FROM orchestration_health_events WHERE owner_id=$1",
            self.user,
        )
        self.assertEqual(
            tuple(rows[0]), (self.tid, self.attempt, self.sid, "fixture-r1")
        )
        self.assertEqual(await self.state_of(self.messages[0]), "completed")
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM orchestration_health_events WHERE owner_id=$1",
                self.user,
            ),
            12,
        )

    async def test_content_identity_conflicts_and_owner_api(self):
        await self.ingest()
        changed = copy.deepcopy(self.tasks[0])
        changed.artifacts[2].parts[0].data["tool"]["category"] = "permission_denied"
        await self.ingest(changed)
        bad = copy.deepcopy(self.tasks[1])
        bad.artifacts[0].parts[0].data["owner_id"] = "forged"
        await self.ingest(bad, self.messages[1])
        bad = copy.deepcopy(self.tasks[1])
        bad.artifacts[1].parts[0].data["usage"]["prompt"] = "PRIVATE"
        await self.ingest(bad, self.messages[1])
        bad = copy.deepcopy(self.tasks[1])
        bad.artifacts[0].parts[0].data["runtime_session_id"] = "other-owner"
        await self.ingest(bad, self.messages[1])
        reasons = await self.pool.fetchval(
            "SELECT reasons FROM orchestration_health_collection WHERE owner_id=$1 AND a2a_task_id=$2",
            self.user,
            self.tasks[0].id,
        )
        self.assertIn("identity_conflict", reasons)
        body = await self.pool.fetchval(
            "SELECT string_agg(payload::text,'') FROM orchestration_health_events WHERE owner_id=$1",
            self.user,
        )
        self.assertNotIn("PRIVATE", body)
        self.assertNotIn("forged", body)
        with patch.object(settings, "owner_id", self.user), patch.object(
            settings, "api_hosts", "localhost"
        ), patch.object(collector, "recover", new=AsyncMock(return_value=False)):
            async with httpx.AsyncClient(
                transport=httpx.ASGITransport(app=api.app), base_url="http://localhost"
            ) as client:
                response = await client.get(
                    "/orchestration-health", params={"task_id": self.tid}
                )
                self.assertEqual(response.status_code, 200, response.text)
                self.assertEqual(response.json()["rows"][0]["totals"]["failures"], 3)
                response = await client.get(
                    "/orchestration-health/events", params={"session_id": self.sid}
                )
                self.assertEqual(response.status_code, 200, response.text)
                events = response.json()["events"]
                self.assertEqual(len(events), 12)
                self.assertEqual(
                    sum(
                        event["payload"]["a2a_task_id"] == self.tasks[0].id
                        for event in events
                    ),
                    9,
                )
                other_sid, _ = await self.session(user="other")
                for route, filters in [
                    ("/orchestration-health", {"session_id": other_sid}),
                    ("/orchestration-health", {"task_id": "someone-else"}),
                    ("/orchestration-health/events", {"session_id": other_sid}),
                ]:
                    response = await client.get(route, params=filters)
                    self.assertEqual(response.status_code, 404, response.text)

    async def test_missing_usage_unassigned_claims_and_approval(self):
        await self.pool.execute("DELETE FROM task_attempts WHERE id=$1", self.attempt)
        task = copy.deepcopy(self.tasks[0])
        task.artifacts[7].parts[0].data["usage"]["input_tokens"] = None
        task.artifacts[7].parts[0].data["usage"]["output_tokens"] = None
        task.artifacts[7].parts[0].data["usage"]["coverage"] = "partial"
        await self.ingest(task)
        self.assertIsNone(totals(await self.summaries())["input_tokens"])
        await self.pool.execute(
            "INSERT INTO native_hitl_requests(id,owner_id,outer_key,snapshot) VALUES($1,$2,$1,$3::jsonb)",
            "approval-" + self.user,
            self.user,
            json.dumps(
                {
                    "outer": {"runtime_session_id": self.context},
                    "availability": "pending",
                }
            ),
        )
        with patch.object(collector, "recover", new=AsyncMock(return_value=False)):
            from mainloop.health.api import read_health

            view = await read_health(hours=24, provider=None, owner=self.user)
        row = next(row for row in view["rows"] if row["id"] == f"unassigned:{self.sid}")
        self.assertIsNone(row["task_id"])
        self.assertEqual(row["totals"]["coverage"], "partial")
        self.assertEqual(row["approvals"][0]["state"], "pending")

    async def test_retention_freezes_totals_and_rejects_replay(self):
        now = datetime.now(UTC)
        self.tasks = fixtures(now - timedelta(days=29))
        await self.ingest(now=now)
        original = await self.summaries()
        future = now + timedelta(days=2)
        async with db.connection() as conn, conn.transaction():
            await health.retain(conn, now=future)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM orchestration_health_events WHERE owner_id=$1",
                self.user,
            ),
            0,
        )
        await self.ingest(now=future)
        self.assertEqual(await self.summaries(), original)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM orchestration_health_events WHERE owner_id=$1",
                self.user,
            ),
            0,
        )
        async with db.connection() as conn, conn.transaction():
            await health.retain(conn, now=now + timedelta(days=370))
        self.assertEqual(await self.summaries(), [])

    async def test_failed_ingestion_does_not_change_delivery(self):
        with patch.object(
            health, "ingest", new=AsyncMock(side_effect=RuntimeError("PRIVATE"))
        ):
            await collector.collect(
                self.sid,
                self.messages[0],
                self.tasks[0].id,
                self.context,
                self.tasks[0].artifacts,
            )
        self.assertEqual(await self.state_of(self.messages[0]), "completed")
        reasons = await self.pool.fetchval(
            "SELECT reasons FROM orchestration_health_collection WHERE owner_id=$1",
            self.user,
        )
        self.assertEqual(reasons, ["source_unavailable"])

    async def test_business_artifacts_are_ignored_and_future_health_is_diagnostic(self):
        task = copy.deepcopy(self.tasks[0])
        task.artifacts = task.artifacts[:1]
        task.artifacts[0].name = "business-report"
        task.artifacts[0].parts[0].data = {"schema_version": 1, "value": "PRIVATE"}
        await self.ingest(task)
        with patch.object(health, "ingest", new=AsyncMock()) as ingest:
            await collector.collect(
                self.sid, self.messages[0], task.id, task.context_id, task.artifacts
            )
            ingest.assert_not_awaited()
        self.assertEqual(await self.summaries(), [])
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM orchestration_health_collection WHERE owner_id=$1",
                self.user,
            ),
            0,
        )
        task.artifacts[0].parts[0].data = {
            "schema_version": "kagent.dev/orchestration-health/v2"
        }
        await self.ingest(task)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT reasons FROM orchestration_health_collection WHERE owner_id=$1",
                self.user,
            ),
            ["invalid_event"],
        )

    async def test_lexical_paths_and_unknown_fields(self):
        base = copy.deepcopy(self.tasks[0].artifacts[1].parts[0].data)
        base["tool"]["path"] = "src/ok.ts"
        self.assertEqual(
            event_adapter.validate_json(json.dumps(base)).tool.path, "src/ok.ts"
        )
        for field in ["arguments", "result", "error_text", "command", "owner_id"]:
            bad = copy.deepcopy(base)
            bad["tool"][field] = "PRIVATE"
            with self.assertRaises(ValidationError):
                event_adapter.validate_json(json.dumps(bad))
        for path in [
            "../outside",
            ".env",
            ".ssh/key",
            "/absolute",
            "credentials.json",
            "src/../secrets",
        ]:
            bad = copy.deepcopy(base)
            bad["tool"]["path"] = path
            with self.assertRaises(ValidationError):
                event_adapter.validate_json(json.dumps(bad))

    async def test_existing_reports_approval_and_owner_deletion(self):
        await self.ingest()
        await self.pool.execute(
            "INSERT INTO task_reports(id,task_id,attempt_id,request_id,request_digest,snapshot) VALUES($1,$2,$3,'fixture','fixture',$4::jsonb)",
            "report-" + self.user,
            self.tid,
            self.attempt,
            json.dumps({"outcome": "blocked", "summary": "PRIVATE_REPORT"}),
        )
        await self.pool.execute(
            "INSERT INTO native_hitl_requests(id,owner_id,outer_key,snapshot) VALUES($1,$2,$1,$3::jsonb)",
            "approval-" + self.user,
            self.user,
            json.dumps(
                {
                    "outer": {"runtime_session_id": self.context},
                    "availability": "answered",
                }
            ),
        )
        with patch.object(collector, "recover", new=AsyncMock(return_value=False)):
            from mainloop.health.api import read_health

            view = await read_health(hours=24, provider=None, owner=self.user)
        row = next(row for row in view["rows"] if row["task_id"] == self.tid)
        self.assertEqual(row["reports"][0]["outcome"], "blocked")
        self.assertEqual(row["approvals"][0]["state"], "answered")
        self.assertNotIn("PRIVATE_REPORT", json.dumps(view, default=str))
        async with db.connection() as conn, conn.transaction():
            await health.delete_owner(conn, self.user)
        self.assertEqual(await self.summaries(), [])
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM task_reports WHERE task_id=$1", self.tid
            ),
            1,
        )

    async def test_collection_bounds_and_native_child_samples(self):
        with patch.object(health, "MAX_EVENTS", 3):
            await self.ingest()
        summary = (await self.summaries())[0]["summary"]
        self.assertEqual(summary["friction_coverage"], "partial")
        self.assertIn(
            "overflow",
            await self.pool.fetchval(
                "SELECT reasons FROM orchestration_health_collection WHERE owner_id=$1",
                self.user,
            ),
        )
        bad = copy.deepcopy(self.tasks[1])
        bad.artifacts[1].parts[0].data["usage"]["inclusion"] = "exclusive"
        await self.ingest(bad, self.messages[1])
        self.assertIsNone(totals(await self.summaries())["input_tokens"])
