"""Real isolated PostgreSQL with fake kagent: task MCP and crash-safe report delivery."""

import asyncio
import uuid
from functools import partial
from unittest.mock import AsyncMock, Mock, patch

from fastapi import HTTPException
from mainloop.config import settings
from mainloop.db import db, environments
from mainloop.db import tasks as store
from mainloop.mcp_app import invoke
from mainloop.providers import (
    TASK_CODE_CAPABILITIES,
    TASK_REQUIRED_CAPABILITIES,
    registry,
)
from mainloop.runtime import native_sessions as ns
from mainloop.runtime.agent_identity import token_for
from mainloop.runtime.agent_tools import AgentService
from mainloop.runtime.delegation import PgStore, auto_report, ensure_main_session
from mainloop.tasks import lifecycle, provisioning, reports, service
from mainloop.tasks.principal import TaskPrincipal
from mainloop.tasks.service import ports, select_profile
from tests.runtime import test_postgres_task_provisioning as setup
from tests.runtime.test_postgres_ledger import KagentFakeCase
from tests.test_workspace_environments import validated_version

from models.environment import DevEnvironment, SelectEnvironment
from models.provider import CapabilityResult
from models.task import TaskReport


class TaskReportsPostgresTests(KagentFakeCase):
    # Reuse qualification setup, not its inherited test cases.
    async def asyncSetUp(self):
        await KagentFakeCase.asyncSetUp(self)
        await self.pool.execute(
            "TRUNCATE tasks,task_attempts,workspace_writer_claims,task_operations CASCADE"
        )
        await self.pool.execute("TRUNCATE native_deliveries,messages CASCADE")
        self.fake.next_session_ids = [str(uuid.uuid4()) for _ in range(12)]
        self.project = "p-" + self.user
        await self.pool.execute(
            """INSERT INTO projects(id,user_id,owner,name,full_name,html_url,default_branch)
               VALUES($1,$2,'example','app','example/app','https://github.com/example/app','main')""",
            self.project,
            self.user,
        )
        self.env_id, self.version_id = "env-" + self.user, "v-" + self.user
        version = validated_version().model_copy(
            update={"id": self.version_id, "environment_id": self.env_id}
        )
        async with self.pool.acquire() as conn, conn.transaction():
            await environments.register(
                conn,
                DevEnvironment(
                    id=self.env_id,
                    owner_id=self.user,
                    name="fixture",
                    source_kind="prebuilt_image",
                ),
                version,
            )
            await environments.set_default(
                conn, self.env_id, self.user, self.version_id
            )
            await environments.select(
                conn,
                self.project,
                self.user,
                SelectEnvironment(
                    environment_id=self.env_id, follow_default=True, expected_version=0
                ),
            )
        profiles = [
            p.model_copy(
                update={
                    "capabilities": tuple(
                        CapabilityResult(
                            capability=name,
                            state="proved",
                            scope="fixture",
                            evidence_ref="fixture:s1",
                        )
                        for name in sorted(
                            TASK_REQUIRED_CAPABILITIES | TASK_CODE_CAPABILITIES
                        )
                    )
                }
            )
            for p in registry().profiles
        ]
        # Validate dictionaries after copying: fixture evidence is explicit, never production proof.
        profiles = [type(p).model_validate(p.model_dump()) for p in profiles]
        self.worker = provisioning.Provisioning()
        self.saved_port = ports.provisioning
        ports.provisioning = self.worker
        self.spawn = Mock()
        for patcher in (
            patch.object(settings, "provider_profiles", profiles),
            patch.object(settings, "api_hosts", "localhost"),
            patch.dict("os.environ", {"WORKSPACE_DEVELOPMENT_PLATFORM": "linux/arm64"}),
            patch.object(
                provisioning,
                "select_profile",
                partial(select_profile, allow_fixture=True),
            ),
            patch.object(ns, "_spawn_deliver", self.spawn),
        ):
            patcher.start()
            self.addCleanup(patcher.stop)
        self.owner = TaskPrincipal(self.user)

    async def asyncTearDown(self):
        ports.provisioning = self.saved_port
        await KagentFakeCase.asyncTearDown(self)

    request = setup.TaskProvisioningPostgresTests.request
    create_task = setup.TaskProvisioningPostgresTests.create_task

    async def tree(self, *, mode="coordination"):
        main = await ensure_main_session(self.user)
        agent = AgentService(PgStore())
        main_ctx = await agent.authenticate(token_for(main["session_id"]))
        result = await invoke(
            agent, main_ctx, "delegate", self.request(mode=mode).model_dump(mode="json")
        )
        self.assertFalse(result.isError, result.content)
        operation_id = result.structuredContent["id"]
        async with self.pool.acquire() as conn:
            operation = await store.operation(conn, operation_id, self.owner)
        await self.worker.reconcile(db, operation)
        async with self.pool.acquire() as conn:
            parent = await lifecycle.load_attempt(conn, operation.attempt_id)
        parent_ctx = await agent.authenticate(token_for(parent.binding_id))
        result = await invoke(
            agent,
            parent_ctx,
            "delegate",
            self.request(mode=mode).model_dump(mode="json"),
        )
        self.assertFalse(result.isError, result.content)
        async with self.pool.acquire() as conn:
            operation = await store.operation(
                conn, result.structuredContent["id"], self.owner
            )
        await self.worker.reconcile(db, operation)
        async with self.pool.acquire() as conn:
            child = await lifecycle.load_attempt(conn, operation.attempt_id)
        child_ctx = await agent.authenticate(token_for(child.binding_id))
        return agent, main_ctx, parent_ctx, child_ctx, parent, child

    def report(self, child, *, outcome="progress", key="report-1"):
        return TaskReport(
            task_id=child.task_id,
            attempt_id=child.id,
            request_id=key,
            outcome=outcome,
            summary="Fixture checks recorded",
            evidence_refs=("fixture:checks",),
        )

    async def record(self, agent, ctx, request):
        result = await invoke(agent, ctx, "report", request.model_dump(mode="json"))
        self.assertFalse(result.isError, result.content)
        return result.structuredContent

    async def test_main_supervisor_child_progress_read_and_report_routing(self):
        agent, main, parent_ctx, child_ctx, parent, child = await self.tree()
        identity = await agent.whoami(child_ctx)
        self.assertEqual(
            (
                identity["task_id"],
                identity["attempt_id"],
                identity["parent_task_id"],
                identity["root_task_id"],
                identity["depth"],
            ),
            (child.task_id, child.id, parent.task_id, parent.task_id, 2),
        )
        result = await self.record(agent, child_ctx, self.report(child))
        before = len(self.fake.requests)
        for ctx in (main, parent_ctx, child_ctx):
            read = await invoke(agent, ctx, "task_get", {"task_id": child.task_id})
            self.assertFalse(read.isError, read.content)
            self.assertEqual(
                read.structuredContent["reports"][0]["outcome"], "progress"
            )
        self.assertEqual(len(self.fake.requests), before)
        # Reconstruct the reconciler after report commit, without another MCP request.
        await service.reconcile_once(db)
        rows = await self.pool.fetch(
            "SELECT d.*,n.session_id,n.state AS native_state FROM task_event_deliveries d JOIN native_deliveries n ON n.message_id=d.delivery_id WHERE event_id=$1",
            result["event_id"],
        )
        self.assertEqual(
            {r["session_id"] for r in rows},
            {main.binding["session_id"], parent.binding_id},
        )
        self.assertTrue(all(r["native_state"] == "queued" for r in rows))
        self.assertEqual(len(self.fake.requests), before)
        await asyncio.gather(service.reconcile_once(db), reports.dispatch_pending(db))
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM task_event_deliveries WHERE event_id=$1",
                result["event_id"],
            ),
            2,
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_deliveries WHERE source='report'"
            ),
            2,
        )

    async def test_atomic_report_and_intents_rollback_and_same_request_digest(self):
        agent, _, _, ctx, _, child = await self.tree()
        request = self.report(child)
        principal = await PgStore().task_principal(ctx.binding)
        with self.assertRaisesRegex(RuntimeError, "crash"):
            async with self.pool.acquire() as conn, conn.transaction():
                await reports.record(conn, principal, request)
                raise RuntimeError("crash before commit")
        self.assertEqual(
            await self.pool.fetchval("SELECT count(*) FROM task_reports"), 0
        )
        self.assertEqual(
            await self.pool.fetchval("SELECT count(*) FROM task_event_deliveries"), 0
        )
        first, second = await asyncio.gather(
            self.record(agent, ctx, request), self.record(agent, ctx, request)
        )
        self.assertEqual(first["report_id"], second["report_id"])
        result = await invoke(
            agent,
            ctx,
            "report",
            request.model_copy(update={"summary": "changed"}).model_dump(mode="json"),
        )
        self.assertTrue(result.isError)
        self.assertIn("report_request_conflict", result.content[0].text)
        self.assertEqual(
            await self.pool.fetchval("SELECT count(*) FROM task_reports"), 1
        )

    async def test_crash_before_and_after_enqueue_never_duplicates(self):
        agent, _, _, ctx, _, child = await self.tree()
        result = await self.record(agent, ctx, self.report(child))
        original = reports._enqueue

        async def crash(conn, event_id, key):
            await original(conn, event_id, key)
            raise RuntimeError("crash after insert before commit")

        with patch.object(reports, "_enqueue", crash):
            with self.assertRaises(RuntimeError):
                await reports.dispatch_pending(db)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_deliveries WHERE source='report'"
            ),
            0,
        )
        await service.reconcile_once(db)
        ids = await self.pool.fetch(
            "SELECT delivery_id FROM task_event_deliveries WHERE event_id=$1",
            result["event_id"],
        )
        await service.reconcile_once(db)
        self.assertEqual(
            ids,
            await self.pool.fetch(
                "SELECT delivery_id FROM task_event_deliveries WHERE event_id=$1",
                result["event_id"],
            ),
        )
        await self.pool.execute(
            "UPDATE native_deliveries SET state='uncertain' WHERE source='report'"
        )
        await reports.dispatch_pending(db)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_deliveries WHERE source='report'"
            ),
            2,
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM task_event_deliveries WHERE state='uncertain'"
            ),
            2,
        )

    async def test_report_mismatch_deeper_delegation_and_subtree_escape(self):
        agent, main, parent_ctx, child_ctx, parent, child = await self.tree()
        for name, args in (
            ("delegate", self.request().model_dump(mode="json")),
            (
                "task_cancel",
                {
                    "task_id": parent.task_id,
                    "request_id": "cancel",
                    "expected_version": 1,
                    "expected_attempt_id": parent.id,
                },
            ),
        ):
            result = await invoke(agent, child_ctx, name, args)
            self.assertTrue(result.isError)
        result = await invoke(
            agent, child_ctx, "report", self.report(parent).model_dump(mode="json")
        )
        self.assertTrue(result.isError)
        _, other, _ = await self.create_task(self.request(mode="coordination"))
        for ctx in (parent_ctx, child_ctx):
            result = await invoke(agent, ctx, "task_get", {"task_id": other.id})
            self.assertTrue(result.isError)
        # Persisted corrupt ancestry and a revoked bearer both deny authentication.
        # Inject corrupt stored ancestry only in the isolated scratch database, as S1 does.
        async with self.pool.acquire() as conn, conn.transaction():
            await conn.execute("SET LOCAL session_replication_role=replica")
            await conn.execute(
                "UPDATE tasks SET root_task_id=$2 WHERE id=$1", child.task_id, other.id
            )
        with self.assertRaises(HTTPException):
            await agent.authenticate(token_for(child.binding_id))
        await self.pool.execute(
            "UPDATE native_bindings SET token_hash=NULL WHERE session_id=$1",
            parent.binding_id,
        )
        with self.assertRaises(HTTPException):
            await agent.authenticate(token_for(parent.binding_id))
        result = await invoke(agent, parent_ctx, "task_list", {})
        self.assertTrue(result.isError)

    async def test_revoked_parent_cancels_unsent_report_and_holds_intent(self):
        agent, _, _, ctx, parent, child = await self.tree()
        await self.record(agent, ctx, self.report(child))
        await reports.dispatch_pending(db)
        await self.pool.execute(
            "UPDATE native_bindings SET token_hash=NULL WHERE session_id=$1",
            parent.binding_id,
        )
        await reports.dispatch_pending(db)
        row = await self.pool.fetchrow(
            "SELECT * FROM task_event_deliveries WHERE recipient_key=$1",
            f"parent-task:{parent.task_id}",
        )
        self.assertEqual((row["state"], row["delivery_id"]), ("pending", None))
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT state FROM native_deliveries WHERE session_id=$1 AND source='report'",
                parent.binding_id,
            ),
            "cancelled",
        )

    async def test_code_result_and_native_reply_never_complete_task(self):
        agent, _, _, ctx, _, child = await self.tree(mode="code")
        await auto_report(child.binding_id, "all done")
        self.assertEqual(
            await self.pool.fetchval("SELECT count(*) FROM task_reports"), 0
        )
        await self.record(agent, ctx, self.report(child, outcome="completed"))
        await service.reconcile_once(db)
        async with self.pool.acquire() as conn:
            task = await lifecycle.load_task(conn, child.task_id)
            attempt = await lifecycle.load_attempt(conn, child.id)
        self.assertEqual(
            (task.status, task.reason, attempt.state),
            ("waiting", "publication", "active"),
        )
        self.assertTrue(
            await self.pool.fetchval(
                "SELECT held FROM workspace_writer_claims WHERE attempt_id=$1", child.id
            )
        )

    async def test_coordination_completion_requires_explicit_result_and_no_live_children(
        self,
    ):
        agent, _, parent_ctx, child_ctx, parent, child = await self.tree()
        result = await invoke(
            agent,
            parent_ctx,
            "report",
            self.report(parent, outcome="completed").model_dump(mode="json"),
        )
        self.assertTrue(result.isError)
        self.assertIn("live_children", result.content[0].text)
        await self.record(agent, child_ctx, self.report(child, outcome="completed"))
        # Fake proof: finish already-recorded briefs, then use real lifecycle settlement SQL.
        await self.pool.execute(
            "UPDATE native_deliveries SET state='completed' WHERE source='brief'"
        )
        await reports.dispatch_pending(db)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT status FROM tasks WHERE id=$1", child.task_id
            ),
            "completed",
        )
        self.assertFalse(
            await self.pool.fetchval(
                "SELECT capacity_held FROM task_attempts WHERE id=$1", child.id
            )
        )
        self.assertIsNone(
            await self.pool.fetchval(
                "SELECT token_hash FROM native_bindings WHERE session_id=$1",
                child.binding_id,
            )
        )
        with self.assertRaises(HTTPException):
            await agent.authenticate(token_for(child.binding_id))
        await self.pool.execute(
            "UPDATE native_deliveries SET state='completed' WHERE session_id=$1",
            parent.binding_id,
        )
        await self.record(agent, parent_ctx, self.report(parent, outcome="completed"))
        await reports.dispatch_pending(db)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT status FROM tasks WHERE id=$1", parent.task_id
            ),
            "completed",
        )

    async def test_shared_create_rolls_back_enrollment_failure_and_pins_profile(self):
        main = await ensure_main_session(self.user)
        agent = AgentService(PgStore())
        ctx = await agent.authenticate(token_for(main["session_id"]))

        async def counts():
            return tuple(
                await self.pool.fetchrow(
                    """SELECT (SELECT count(*) FROM tasks),
                          (SELECT count(*) FROM task_attempts),
                          (SELECT count(*) FROM task_operations),
                          (SELECT count(*) FROM sessions),
                          (SELECT count(*) FROM native_bindings),
                          (SELECT count(*) FROM workspace_writer_claims),
                          (SELECT count(*) FROM conversations)"""
                )
            )

        before = await counts()
        with patch.object(settings, "agent_token_key", ""), patch.object(
            settings, "db_password", ""
        ):
            with self.assertRaises(RuntimeError):
                await agent.delegate(
                    ctx, **self.request(provider="codex").model_dump(mode="json")
                )
        self.assertEqual(before, await counts())
        request = self.request(mode="coordination", provider="codex")
        result = await agent.delegate(ctx, **request.model_dump(mode="json"))
        self.assertEqual(
            await agent.delegate(ctx, **request.model_dump(mode="json")), result
        )
        async with self.pool.acquire() as conn:
            attempt = await lifecycle.load_attempt(conn, result["attempt_id"])
        self.assertEqual(
            (attempt.profile_id, attempt.native_provider, attempt.role),
            ("codex", "codex", "supervisor"),
        )
        pinned = attempt.agent_ref
        changed = [
            profile.model_copy(update={"configuration_revision": "reloaded"})
            for profile in settings.provider_profiles
        ]
        with patch.object(settings, "provider_profiles", changed):
            async with self.pool.acquire() as conn:
                observed = await lifecycle.load_attempt(conn, attempt.id)
            self.assertEqual(
                (observed.agent_ref, observed.configuration_revision),
                (pinned, attempt.configuration_revision),
            )

    async def test_pending_and_queued_reports_follow_a_new_current_parent_attempt(self):
        from mainloop.runtime import workspaces

        agent, _, _, ctx, parent, child = await self.tree()
        first = await self.record(agent, ctx, self.report(child))
        await reports.dispatch_pending(db)
        old_id = await self.pool.fetchval(
            "SELECT delivery_id FROM task_event_deliveries WHERE event_id=$1 AND recipient_key=$2",
            first["event_id"],
            f"parent-task:{parent.task_id}",
        )
        second = await self.record(agent, ctx, self.report(child, key="report-2"))
        # S3 is not installed. This is a fixture of its persisted, confirmed-fence seam,
        # not a live handoff proof or permission to transfer an active child.
        await self.pool.execute(
            "UPDATE native_deliveries SET state='completed' WHERE source='brief'"
        )
        async with self.pool.acquire() as conn, conn.transaction():
            for attempt in (child, parent):
                await lifecycle.transition(
                    conn, attempt.id, "draining", from_states=("active",)
                )
                await lifecycle.settle(
                    conn,
                    attempt.id,
                    "superseded",
                    evidence="fixture:confirmed-runtime-absence",
                )
                await conn.execute(
                    "UPDATE native_bindings SET token_hash=NULL WHERE session_id=$1",
                    attempt.binding_id,
                )
            task = await lifecycle.load_task(conn, parent.task_id)
            task, successor = await store.admit_attempt(
                conn,
                task,
                registry().resolve("codex", "supervisor"),
                role="supervisor",
                depth=1,
                predecessor_id=parent.id,
            )
            enrolled = await workspaces.enroll_session(
                conn,
                user_id=self.user,
                kind="codex",
                role="supervisor",
                mcp_grant_kind="coordination",
                manifest=None,
                project_id=self.project,
                session_id=successor.id,
                parent_session_id=None,
                topic_id=None,
                title="Successor fixture",
                description="Fixture handoff seam",
                prompt="Continue",
                environment=None,
                claim_branch=False,
            )
            await lifecycle.save_attempt(
                conn,
                successor.model_copy(
                    update={
                        "binding_id": enrolled.workspace_id,
                        "session_id": enrolled.workspace_id,
                        "workspace_id": enrolled.workspace_id,
                        "state": "active",
                    }
                ),
            )
        await service.reconcile_once(db)
        for event_id in (first["event_id"], second["event_id"]):
            row = await self.pool.fetchrow(
                "SELECT d.target_attempt_id,n.session_id FROM task_event_deliveries d JOIN native_deliveries n ON n.message_id=d.delivery_id WHERE event_id=$1 AND recipient_key=$2",
                event_id,
                f"parent-task:{parent.task_id}",
            )
            self.assertEqual(tuple(row), (successor.id, successor.id))
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT state FROM native_deliveries WHERE message_id=$1", old_id
            ),
            "cancelled",
        )
        with self.assertRaises(HTTPException):
            await agent.authenticate(token_for(parent.binding_id))
        await reports.dispatch_pending(db)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_deliveries WHERE session_id=$1 AND source='report'",
                successor.id,
            ),
            2,
        )

    async def test_uncertain_old_parent_report_is_never_replayed_after_revocation(self):
        agent, _, _, ctx, parent, child = await self.tree()
        result = await self.record(agent, ctx, self.report(child))
        await reports.dispatch_pending(db)
        await self.pool.execute(
            "UPDATE native_deliveries SET state='uncertain' WHERE session_id=$1 AND source='report'",
            parent.binding_id,
        )
        await self.pool.execute(
            "UPDATE native_bindings SET token_hash=NULL WHERE session_id=$1",
            parent.binding_id,
        )
        await service.reconcile_once(db)
        row = await self.pool.fetchrow(
            "SELECT state,target_attempt_id FROM task_event_deliveries WHERE event_id=$1 AND recipient_key=$2",
            result["event_id"],
            f"parent-task:{parent.task_id}",
        )
        self.assertEqual(tuple(row), ("uncertain", parent.id))
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_deliveries WHERE session_id=$1 AND source='report'",
                parent.binding_id,
            ),
            1,
        )

    async def test_busy_parent_uses_held_queue_and_read_context_is_effect_free(self):
        from mainloop.runtime.delegation import render_for_binding

        agent, main, parent_ctx, child_ctx, parent, child = await self.tree()
        await self.pool.execute(
            "UPDATE native_bindings SET queue_held=TRUE WHERE session_id=$1",
            parent.binding_id,
        )
        await self.record(agent, child_ctx, self.report(child))
        before = len(self.fake.requests)
        parent_text = await render_for_binding(parent_ctx.binding)
        main_text = await render_for_binding(main.binding)
        self.assertIn("Fixture checks recorded", parent_text)
        self.assertIn("unverified", main_text)
        await service.reconcile_once(db)
        self.assertIsNone(await ns.ledger.promote_queued(parent.binding_id))
        self.assertEqual(len(self.fake.requests), before)
        # Its existing brief still blocks promotion independently of the queue hold.
        await self.pool.execute(
            "UPDATE native_bindings SET queue_held=FALSE WHERE session_id=$1",
            parent.binding_id,
        )
        self.assertIsNone(await ns.ledger.promote_queued(parent.binding_id))
        await self.pool.execute(
            "UPDATE native_deliveries SET state='completed' WHERE session_id=$1 AND source='brief'",
            parent.binding_id,
        )
        self.assertIsNotNone(await ns.ledger.promote_queued(parent.binding_id))

    async def test_coordination_unknown_delete_preserves_capacity_and_cancel_wins(self):
        from models.task import TaskAction

        agent, main, _, ctx, _, child = await self.tree()
        await self.record(agent, ctx, self.report(child, outcome="completed"))
        await self.pool.execute(
            "UPDATE native_deliveries SET state='completed' WHERE session_id=$1",
            child.binding_id,
        )
        with patch.object(ns, "delete_kagent_session", AsyncMock(return_value=False)):
            await reports.dispatch_pending(db)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT state FROM task_attempts WHERE id=$1", child.id
            ),
            "draining",
        )
        self.assertTrue(
            await self.pool.fetchval(
                "SELECT capacity_held FROM task_attempts WHERE id=$1", child.id
            )
        )
        async with self.pool.acquire() as conn, conn.transaction():
            task = await store.get_task(conn, child.task_id, self.owner)
            await service.mutate(
                conn,
                self.owner,
                "cancel",
                TaskAction(
                    request_id="cancel",
                    expected_version=task.version,
                    expected_attempt_id=child.id,
                ),
                task_id=task.id,
            )
        await service.reconcile_once(db)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT status FROM tasks WHERE id=$1", child.task_id
            ),
            "cancelled",
        )

    async def test_archived_main_is_not_reused_by_fresh_bootstrap(self):
        main = await ensure_main_session(self.user)
        await self.pool.execute(
            "UPDATE sessions SET archived_at=NOW() WHERE id=$1", main["session_id"]
        )
        replacements = await asyncio.gather(
            *(ensure_main_session(self.user) for _ in range(4))
        )
        self.assertEqual(len({item["session_id"] for item in replacements}), 1)
        self.assertNotEqual(replacements[0]["session_id"], main["session_id"])
        agent = AgentService(PgStore())
        with self.assertRaises(HTTPException):
            await agent.authenticate(token_for(main["session_id"]))
        self.assertEqual(
            (
                await agent.authenticate(token_for(replacements[0]["session_id"]))
            ).actor.role,
            "main",
        )

    async def test_root_report_only_queues_main_and_records_owner_event(self):
        agent, main, parent_ctx, _, parent, _ = await self.tree()
        result = await self.record(agent, parent_ctx, self.report(parent))
        await service.reconcile_once(db)
        rows = await self.pool.fetch(
            "SELECT d.recipient_key,n.session_id FROM task_event_deliveries d JOIN native_deliveries n ON n.message_id=d.delivery_id WHERE d.event_id=$1",
            result["event_id"],
        )
        self.assertEqual(
            [tuple(row) for row in rows], [("main", main.binding["session_id"])]
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT owner_id FROM task_events WHERE id=$1", result["event_id"]
            ),
            self.user,
        )

    async def test_recipient_revocation_orders_against_transactional_enqueue(self):
        agent, _, _, ctx, parent, child = await self.tree()
        await self.record(agent, ctx, self.report(child))
        ready, release, revoking = asyncio.Event(), asyncio.Event(), asyncio.Event()
        original = reports._lock_recipient

        async def pause(conn, sid):
            value = await original(conn, sid)
            if sid == parent.binding_id:
                ready.set()
                await release.wait()
            return value

        async def revoke_parent():
            async with self.pool.acquire() as conn:
                revoking.set()
                await conn.execute(
                    "UPDATE native_bindings SET token_hash=NULL WHERE session_id=$1",
                    parent.binding_id,
                )

        with patch.object(reports, "_lock_recipient", pause):
            dispatch = asyncio.create_task(reports.dispatch_pending(db))
            await ready.wait()
            revoke = asyncio.create_task(revoke_parent())
            await revoking.wait()
            self.assertFalse(revoke.done())
            release.set()
            await dispatch
            await revoke
        await service.reconcile_once(db)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT state FROM native_deliveries WHERE session_id=$1 AND source='report'",
                parent.binding_id,
            ),
            "cancelled",
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT state FROM task_event_deliveries WHERE recipient_key=$1",
                f"parent-task:{parent.task_id}",
            ),
            "pending",
        )

    async def test_cancel_between_coordination_drain_and_settlement_takes_precedence(
        self,
    ):
        from models.task import TaskAction

        agent, _, _, ctx, _, child = await self.tree()
        await self.record(agent, ctx, self.report(child, outcome="completed"))
        await self.pool.execute(
            "UPDATE native_deliveries SET state='completed' WHERE session_id=$1",
            child.binding_id,
        )
        original = ns.delete_kagent_session

        async def delete_and_cancel(sid):
            deleted = await original(sid)
            async with self.pool.acquire() as conn, conn.transaction():
                task = await store.get_task(conn, child.task_id, self.owner)
                await service.mutate(
                    conn,
                    self.owner,
                    "cancel",
                    TaskAction(
                        request_id="cancel-at-settle",
                        expected_version=task.version,
                        expected_attempt_id=child.id,
                    ),
                    task_id=task.id,
                )
            return deleted

        with patch.object(ns, "delete_kagent_session", delete_and_cancel):
            await reports.dispatch_pending(db)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT state FROM task_attempts WHERE id=$1", child.id
            ),
            "draining",
        )
        await service.reconcile_once(db)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT status FROM tasks WHERE id=$1", child.task_id
            ),
            "cancelled",
        )
        self.assertEqual(
            (await db.get_session(child.binding_id)).status.value, "cancelled"
        )
        self.assertIsNone(
            await self.pool.fetchval(
                "SELECT token_hash FROM native_bindings WHERE session_id=$1",
                child.binding_id,
            )
        )
        with self.assertRaises(HTTPException):
            await agent.authenticate(token_for(child.binding_id))

    async def test_bounded_dispatch_advances_past_busy_queued_reports(self):
        agent, _, parent_ctx, _, parent, _ = await self.tree()
        first = await self.record(agent, parent_ctx, self.report(parent, key="first"))
        await reports.dispatch_pending(db, limit=1)
        second = await self.record(agent, parent_ctx, self.report(parent, key="second"))
        await reports.dispatch_pending(db, limit=1)
        rows = await self.pool.fetch(
            "SELECT state FROM task_event_deliveries WHERE event_id=ANY($1)",
            [first["event_id"], second["event_id"]],
        )
        self.assertEqual([row["state"] for row in rows], ["queued", "queued"])

    async def _interleave_main_ledger(self, *, sending):
        agent, main, _, ctx, _, child = await self.tree()
        event = await self.record(agent, ctx, self.report(child))
        sid = main.binding["session_id"]
        conversation = await self.pool.fetchval(
            "SELECT conversation_id FROM sessions WHERE id=$1", sid
        )
        message = None
        if sending:
            message, _ = await ns.ledger.record_submission(
                session_id=sid,
                conversation_id=conversation,
                text="Owner turn",
                source="user",
            )
        else:
            await self.pool.execute(
                "UPDATE native_bindings SET queue_held=TRUE WHERE session_id=$1", sid
            )
        resolved, owner_locked, enqueue_locking = (asyncio.Event() for _ in range(3))
        original_recipient = reports._recipient
        original_lock = ns.ledger._lock_deliveries
        paused = False

        async def recipient(conn, task, key):
            nonlocal paused
            value = await original_recipient(conn, task, key)
            if not paused and asyncio.current_task().get_name() == "report-enqueue":
                paused = True
                resolved.set()
                await owner_locked.wait()
            return value

        async def lock(conn, target):
            if target == sid and asyncio.current_task().get_name() == "report-enqueue":
                enqueue_locking.set()
            await original_lock(conn, target)
            if target == sid and asyncio.current_task().get_name() == "owner-ledger":
                owner_locked.set()
                await enqueue_locking.wait()

        async def enqueue():
            async with self.pool.acquire() as conn, conn.transaction():
                await reports._enqueue(conn, event["event_id"], "main")

        async def owner():
            await resolved.wait()
            if sending:
                return await ns.ledger.transition(
                    message, "sending", from_states=("recorded",)
                )
            return await ns.ledger.record_submission(
                session_id=sid,
                conversation_id=conversation,
                text="Owner releases hold",
                source="user",
            )

        # Real separate connections, forced opposite-order callers; no sleeps or mocked SQL.
        with patch.object(reports, "_recipient", recipient), patch.object(
            ns.ledger, "_lock_deliveries", lock
        ):
            work = asyncio.create_task(enqueue(), name="report-enqueue")
            submit = asyncio.create_task(owner(), name="owner-ledger")
            try:
                outcomes = await asyncio.wait_for(asyncio.gather(work, submit), 10)
            finally:
                # A failing-before deadlock must not leave its other transaction running.
                for pending in (work, submit):
                    if not pending.done():
                        pending.cancel()
                await asyncio.gather(work, submit, return_exceptions=True)
        if sending:
            self.assertTrue(outcomes[1])
            self.assertEqual(await ns.ledger.delivery_state(message), "sending")
        else:
            self.assertEqual(outcomes[1][1], "recorded")
            self.assertFalse(
                await self.pool.fetchval(
                    "SELECT queue_held FROM native_bindings WHERE session_id=$1", sid
                )
            )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT state FROM native_deliveries WHERE session_id=$1 AND source='report'",
                sid,
            ),
            "queued",
        )

    async def test_held_main_submission_and_report_enqueue_lock_order(self):
        await self._interleave_main_ledger(sending=False)

    async def test_sending_claim_and_report_enqueue_lock_order(self):
        await self._interleave_main_ledger(sending=True)

    async def _revoked_main_report(self, *, path):
        from mainloop.runtime import agent_credentials

        agent, main, _, ctx, _, child = await self.tree()
        sid = main.binding["session_id"]
        await ns._ensure_kagent_session(await ns.get_binding(sid))
        # Resident main has already rendered standing in a prior turn; this reproduces queue
        # dispatch, rather than failing a first-turn principal read before native claiming.
        await self.pool.execute(
            "UPDATE native_bindings SET standing_hash='fixture:prior-turn' WHERE session_id=$1",
            sid,
        )
        event = await self.record(agent, ctx, self.report(child))
        await reports.dispatch_pending(db)
        mid = await self.pool.fetchval(
            "SELECT delivery_id FROM task_event_deliveries WHERE event_id=$1 AND recipient_key='main'",
            event["event_id"],
        )
        if path in ("claim", "deliver"):
            self.assertIsNotNone(await ns.ledger.promote_queued(sid))
        # Real revocation transaction/row locks, sanitized cleanup only.
        with patch.object(agent_credentials, "_cleanup", new=AsyncMock()):
            await agent_credentials.revoke(sid)
        sends_before = len(self.fake.rpc_calls("SendStreamingMessage"))
        if path == "promote":
            self.assertIsNone(await ns.ledger.promote_queued(sid))
        elif path == "claim":
            self.assertFalse(
                await ns.ledger.transition(mid, "sending", from_states=("recorded",))
            )
        else:
            await ns._deliver(sid, mid, "Fixture report")
        self.assertEqual(await ns.ledger.delivery_state(mid), "cancelled")
        self.assertEqual(len(self.fake.rpc_calls("SendStreamingMessage")), sends_before)
        # Native rejection must leave the authoritative intent available for replacement routing.
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT state FROM task_event_deliveries WHERE event_id=$1 AND recipient_key='main'",
                event["event_id"],
            ),
            "queued",
        )
        await self.pool.execute(
            "UPDATE sessions SET archived_at=NOW() WHERE id=$1", sid
        )
        replacement = await ensure_main_session(self.user)
        replacement_sid = replacement["session_id"]
        self.assertNotEqual(replacement_sid, sid)
        await reports.dispatch_pending(db)
        await reports.dispatch_pending(db)
        current = await self.pool.fetchrow(
            "SELECT n.message_id,n.session_id,n.state FROM task_event_deliveries d JOIN native_deliveries n ON n.message_id=d.delivery_id WHERE event_id=$1 AND recipient_key='main'",
            event["event_id"],
        )
        self.assertEqual(
            (current["session_id"], current["state"]), (replacement_sid, "queued")
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_deliveries WHERE session_id=$1 AND source='report'",
                replacement_sid,
            ),
            1,
        )
        self.assertIsNotNone(await ns.ledger.promote_queued(replacement_sid))
        await ns._deliver(replacement_sid, current["message_id"], "Fixture report")
        self.assertEqual(
            len(self.fake.rpc_calls("SendStreamingMessage")), sends_before + 1
        )
        await ns._deliver(replacement_sid, current["message_id"], "Fixture report")
        await reports.dispatch_pending(db)
        self.assertEqual(
            len(self.fake.rpc_calls("SendStreamingMessage")), sends_before + 1
        )

    async def test_revoked_live_main_report_is_cancelled_before_promotion(self):
        await self._revoked_main_report(path="promote")

    async def test_revoked_live_main_report_is_cancelled_at_send_claim(self):
        await self._revoked_main_report(path="claim")

    async def test_revoked_live_main_report_is_cancelled_before_native_delivery(self):
        await self._revoked_main_report(path="deliver")

    async def test_main_report_claimed_before_revocation_is_never_replayed(self):
        agent, main, _, ctx, _, child = await self.tree()
        event = await self.record(agent, ctx, self.report(child))
        await reports.dispatch_pending(db)
        sid = main.binding["session_id"]
        mid = (await ns.ledger.promote_queued(sid))[0]
        self.assertTrue(
            await ns.ledger.transition(mid, "sending", from_states=("recorded",))
        )
        await self.pool.execute(
            "UPDATE native_bindings SET token_hash=NULL WHERE session_id=$1", sid
        )
        await self.pool.execute(
            "UPDATE sessions SET archived_at=NOW() WHERE id=$1", sid
        )
        replacement = await ensure_main_session(self.user)
        await reports.dispatch_pending(db)
        row = await self.pool.fetchrow(
            "SELECT delivery_id,state FROM task_event_deliveries WHERE event_id=$1 AND recipient_key='main'",
            event["event_id"],
        )
        self.assertEqual((row["delivery_id"], row["state"]), (mid, "delivered"))
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_deliveries WHERE session_id=$1 AND source='report'",
                replacement["session_id"],
            ),
            0,
        )
        await ns.ledger.transition(mid, "uncertain", from_states=("sending",))
        await reports.dispatch_pending(db)
        self.assertEqual(await ns.ledger.delivery_state(mid), "uncertain")
        self.assertIsNone(await ns.ledger.promote_queued(sid))

    async def _revoked_during_preparation(self, *, delegated):
        from mainloop.runtime import agent_credentials, workspaces
        from mainloop.tasks.reports import STALE_RECIPIENT

        agent, main, _, ctx, parent, child = await self.tree()
        sid = parent.binding_id if delegated else main.binding["session_id"]
        key = f"parent-task:{parent.task_id}" if delegated else "main"
        await ns._ensure_kagent_session(await ns.get_binding(sid))
        self.assertIsNone((await ns.get_binding(sid))["standing_hash"])
        event = await self.record(agent, ctx, self.report(child))
        await reports.dispatch_pending(db)
        # Finish the delegated fixture's initial brief so its report can be promoted.
        await self.pool.execute(
            "UPDATE native_deliveries SET state='completed' WHERE session_id=$1 AND source='brief'",
            sid,
        )
        mid = (await ns.ledger.promote_queued(sid))[0]
        ready, resume = asyncio.Event(), asyncio.Event()
        original = ns.ledger.validate_report
        transition = ns.ledger.transition
        claims = []

        async def pause(message_id):
            valid = await original(message_id)
            if message_id == mid:
                self.assertTrue(valid)
                ready.set()
                await resume.wait()
            return valid

        async def track(message_id, state, **kwargs):
            if message_id == mid and state == "sending":
                claims.append(message_id)
            return await transition(message_id, state, **kwargs)

        before = len(self.fake.rpc_calls("SendStreamingMessage"))
        with patch.object(ns.ledger, "validate_report", pause), patch.object(
            ns.ledger, "transition", track
        ):
            delivery = asyncio.create_task(
                ns._deliver(sid, mid, "Preparation race fixture")
            )
            try:
                await asyncio.wait_for(ready.wait(), 10)
                with patch.object(agent_credentials, "_cleanup", new=AsyncMock()):
                    await agent_credentials.revoke(sid)
                resume.set()
                await asyncio.wait_for(delivery, 10)
            finally:
                resume.set()
                if not delivery.done():
                    delivery.cancel()
                await asyncio.gather(delivery, return_exceptions=True)
        self.assertEqual(claims, [])
        self.assertEqual(len(self.fake.rpc_calls("SendStreamingMessage")), before)
        old = await self.pool.fetchrow(
            "SELECT state,detail FROM native_deliveries WHERE message_id=$1", mid
        )
        self.assertEqual((old["state"], old["detail"]), ("cancelled", STALE_RECIPIENT))
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT state FROM task_event_deliveries WHERE event_id=$1 AND recipient_key=$2",
                event["event_id"],
                key,
            ),
            "queued",
        )
        if delegated:
            # Labelled offline successor enrollment, not an installed S3 handoff.
            async with self.pool.acquire() as conn, conn.transaction():
                for attempt in (child, parent):
                    await lifecycle.transition(
                        conn, attempt.id, "draining", from_states=("active",)
                    )
                    await lifecycle.settle(
                        conn,
                        attempt.id,
                        "superseded",
                        evidence="fixture:confirmed-runtime-absence",
                    )
                task = await lifecycle.load_task(conn, parent.task_id)
                task, successor = await store.admit_attempt(
                    conn,
                    task,
                    registry().resolve("codex", "supervisor"),
                    role="supervisor",
                    depth=1,
                    predecessor_id=parent.id,
                )
                enrolled = await workspaces.enroll_session(
                    conn,
                    user_id=self.user,
                    kind="codex",
                    role="supervisor",
                    mcp_grant_kind="coordination",
                    manifest=None,
                    project_id=self.project,
                    session_id=successor.id,
                    parent_session_id=None,
                    title="Offline preparation-race successor",
                    description="Offline fixture, not S3 proof",
                    prompt="Continue fixture",
                    environment=None,
                    claim_branch=False,
                )
                await lifecycle.save_attempt(
                    conn,
                    successor.model_copy(
                        update={
                            "binding_id": enrolled.workspace_id,
                            "session_id": enrolled.workspace_id,
                            "workspace_id": enrolled.workspace_id,
                            "state": "active",
                        }
                    ),
                )
                replacement_sid = enrolled.workspace_id
        else:
            await self.pool.execute(
                "UPDATE sessions SET archived_at=NOW() WHERE id=$1", sid
            )
            replacement_sid = (await ensure_main_session(self.user))["session_id"]
        await reports.dispatch_pending(db)
        await reports.dispatch_pending(db)
        current = await self.pool.fetchrow(
            "SELECT n.message_id,n.session_id,n.state FROM task_event_deliveries d JOIN native_deliveries n ON n.message_id=d.delivery_id WHERE event_id=$1 AND recipient_key=$2",
            event["event_id"],
            key,
        )
        self.assertEqual(
            (current["session_id"], current["state"]), (replacement_sid, "queued")
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_deliveries WHERE session_id=$1 AND source='report'",
                replacement_sid,
            ),
            1,
        )
        self.assertIsNotNone(await ns.ledger.promote_queued(replacement_sid))
        await ns._deliver(
            replacement_sid, current["message_id"], "Successor preparation fixture"
        )
        await ns._deliver(
            replacement_sid, current["message_id"], "Successor preparation fixture"
        )
        await reports.dispatch_pending(db)
        self.assertEqual(len(self.fake.rpc_calls("SendStreamingMessage")), before + 1)

    async def test_delegated_revocation_after_validation_recovers_preparation_failure(
        self,
    ):
        await self._revoked_during_preparation(delegated=True)

    async def test_main_revocation_after_validation_recovers_standing_failure(self):
        await self._revoked_during_preparation(delegated=False)
