"""Persisted task-result cleanup on scratch PostgreSQL; GitHub/kagent are fakes."""

import asyncio
import json
from unittest.mock import AsyncMock, patch

import httpx
from fastapi import HTTPException
from mainloop.db import db
from mainloop.db import tasks as store
from mainloop.mcp_app import create_app, invoke
from mainloop.runtime import native_sessions as ns
from mainloop.runtime.agent_identity import token_for
from mainloop.runtime.agent_tools import AgentService
from mainloop.runtime.delegation import PgStore, ensure_main_session
from mainloop.runtime.kagent_client import (
    KagentSession,
    OutcomeUnknown,
    RuntimeOperation,
    RuntimeState,
    SessionError,
)
from mainloop.runtime.policy import PolicyError, surface_tools
from mainloop.services import merge
from mainloop.tasks import lifecycle, provisioning, publication, service
from mainloop.tasks.principal import TaskPrincipal
from tests.runtime import test_merge as merge_fixtures
from tests.runtime import test_postgres_ledger as ledger_fixtures
from tests.runtime import test_postgres_task_attention as attention_fixtures
from tests.runtime import test_postgres_task_reports as report_fixtures

from models.task import TaskAction


class ProcessLost(BaseException):
    """A crash bypasses the reconciler's ordinary retry logging."""


class CodingTaskReleaseTests(merge_fixtures.MergeFixture):
    enroll = attention_fixtures.TaskAttentionTests.enroll
    view = attention_fixtures.TaskAttentionTests.view

    async def asyncSetUp(self):
        await super().asyncSetUp()
        await self.clear_tables(
            "tasks",
            "task_attempts",
            "workspace_writer_claims",
            "task_events",
            "task_operations",
        )
        self.task, self.attempt = await self.enroll(self.sid, "feature")
        self.binding = await PgStore().get_binding(self.sid)
        self.runtime = self.binding["kagent_session_id"]
        self.client = AsyncMock()
        self.client.delete_session.side_effect = self.deleted
        patcher = patch.object(ns, "get_client", return_value=self.client)
        patcher.start()
        self.addCleanup(patcher.stop)

    async def deleted(self, runtime):
        # Assert the external effect happens strictly after committed drain/revocation.
        binding = await ns.get_binding(self.sid)
        self.assertIsNone(binding["token_hash"])
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT state FROM task_attempts WHERE id=$1", self.attempt.id
            ),
            "draining",
        )
        self.assertTrue(await self.held())
        return KagentSession(
            runtime, RuntimeState.DELETED, RuntimeOperation.NONE, runtime
        )

    async def held(self):
        return await self.pool.fetchval(
            "SELECT capacity_held FROM task_attempts WHERE id=$1", self.attempt.id
        )

    async def reconcile(self):
        await service.reconcile_once(db, installed_ports=service.TaskPorts())

    async def merged(self):
        result = await merge.auto_merge(self.binding, self.args)
        self.assertEqual(result["state"], "merged")
        self.assertTrue(await self.held())
        return result

    async def mcp(self, method, params=None, *, sid=None, path="/mcp"):
        app = create_app(AgentService(PgStore()))
        async with app.app.router.lifespan_context(app.app):
            async with httpx.AsyncClient(
                transport=httpx.ASGITransport(app=app), base_url="http://testserver"
            ) as client:
                return await client.post(
                    path,
                    headers={
                        "Authorization": f"Bearer {token_for(sid or self.sid)}",
                        "Accept": "application/json, text/event-stream",
                    },
                    json={
                        "jsonrpc": "2.0",
                        "id": 1,
                        "method": method,
                        "params": params or {},
                    },
                )

    async def mcp_call(self, name, arguments):
        response = await self.mcp("tools/call", {"name": name, "arguments": arguments})
        self.assertEqual(response.status_code, 200, response.text)
        return response.json()["result"]

    def final_report(self, **changes):
        return {
            "task_id": self.task.id,
            "attempt_id": self.attempt.id,
            "request_id": "final-report",
            "summary": "Fixture merge verified; final report",
            "outcome": "completed",
            "evidence_refs": ["fixture:merged"],
            **changes,
        }

    async def assert_completed_mcp(self, result, invocation):
        agent = AgentService(PgStore())
        ctx = await agent.authenticate(token_for(self.sid))
        self.assertTrue(ctx.actor.merge_status_only)
        allowed = {"whoami", "task_get", "report", "get_pull_request_merge_status"}
        self.assertEqual(surface_tools(ctx.actor), allowed)
        discovery = await self.mcp("tools/list")
        self.assertEqual(discovery.status_code, 200)
        self.assertEqual(
            {t["name"] for t in discovery.json()["result"]["tools"]}, allowed
        )
        protected = await self.mcp("tools/list", path="/mcp/merge-approval")
        self.assertEqual(protected.json()["result"]["tools"], [])
        before_task, before_projection = await self.view()
        before_result_ref = await self.pool.fetchval(
            "SELECT snapshot->>'result_ref' FROM task_attempts WHERE id=$1",
            self.attempt.id,
        )
        for tool, arguments in (
            ("whoami", {}),
            ("task_get", {"task_id": self.task.id}),
            (
                "get_pull_request_merge_status",
                {"proposal_id": result["proposal_id"], "request_id": invocation},
            ),
        ):
            called = await self.mcp_call(tool, arguments)
            self.assertFalse(called.get("isError"), called)
            content = called["structuredContent"]
            if tool == "whoami":
                self.assertEqual(content["task_id"], self.task.id)
                self.assertEqual(content["attempt_id"], self.attempt.id)
            elif tool == "task_get":
                self.assertEqual(content["task"]["status"], "completed")
            else:
                self.assertEqual(content["state"], "merged")
        recorded = await self.mcp_call("report", self.final_report())
        self.assertFalse(recorded.get("isError"), recorded)
        replay = await self.mcp_call("report", self.final_report())
        self.assertFalse(replay.get("isError"), replay)
        self.assertEqual(
            recorded["structuredContent"]["report_id"],
            replay["structuredContent"]["report_id"],
        )
        conflict = await self.mcp_call(
            "report", self.final_report(summary="Changed fixture")
        )
        self.assertTrue(conflict["isError"])
        self.assertIn("report_request_conflict", conflict["content"][0]["text"])
        task, projection = await self.view()
        self.assertEqual(
            (task.status, task.reason), (before_task.status, before_task.reason)
        )
        self.assertEqual(task.status, "completed")
        self.assertEqual(task.version, before_task.version + 1)
        self.assertEqual(projection, before_projection)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT snapshot->>'result_ref' FROM task_attempts WHERE id=$1",
                self.attempt.id,
            ),
            before_result_ref,
        )
        self.assertTrue(await self.held())
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM task_reports WHERE attempt_id=$1", self.attempt.id
            ),
            1,
        )
        event_id = recorded["structuredContent"]["event_id"]
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT event_key FROM task_events WHERE id=$1", event_id
            ),
            f"report:{recorded['structuredContent']['report_id']}",
        )
        recipients = (
            [(f"parent-task:{self.task.parent_task_id}", "pending")]
            if self.task.parent_task_id
            else []
        )
        self.assertEqual(
            [
                (row["recipient_key"], row["state"])
                for row in await self.pool.fetch(
                    "SELECT recipient_key,state FROM task_event_deliveries WHERE event_id=$1",
                    event_id,
                )
            ],
            recipients,
        )
        for tool in (
            "delegate",
            "task_list",
            "task_history",
            "open_pull_request",
            "merge_pull_request",
            "merge_pull_request_with_approval",
            "task_cancel",
            "note",
        ):
            denied = await invoke(agent, ctx, tool, {})
            self.assertTrue(denied.isError, tool)
        for outcome in ("progress", "failed", "blocked"):
            denied = await self.mcp_call(
                "report",
                self.final_report(request_id=f"invalid-{outcome}", outcome=outcome),
            )
            self.assertTrue(denied["isError"], outcome)
            self.assertIn("completed_report_outcome", denied["content"][0]["text"])
        for arguments in (
            {"task_id": "unrelated"},
            {"attempt_id": "unrelated"},
        ):
            denied = await self.mcp_call("report", self.final_report(**arguments))
            self.assertTrue(denied["isError"])
            self.assertIn("report_attempt_scope", denied["content"][0]["text"])
        denied = await self.mcp_call("task_get", {"task_id": "unrelated"})
        self.assertTrue(denied["isError"])
        self.assertIn("completed_task_scope", denied["content"][0]["text"])
        self.assertEqual(len(self.fake.puts), 1)

    async def test_auto_completed_mcp_final_report_then_turn_cleanup_releases_capacity(
        self,
    ):
        cid = (await db.get_session(self.sid)).conversation_id
        message = await self.delivery(self.sid, cid, "delivered", source="brief")
        result = await self.merged()
        await self.assert_completed_mcp(result, self.args["request_id"])
        await self.reconcile()
        self.assertTrue(await self.held())
        self.client.delete_session.assert_not_awaited()
        await self.pool.execute(
            "UPDATE native_deliveries SET state='completed' WHERE message_id=$1",
            message,
        )
        await self.reconcile()
        await self.assert_released()
        self.assertEqual((await self.mcp("tools/list")).status_code, 401)

    async def test_approval_completed_mcp_keeps_exact_receipt_required(self):
        await self.pool.execute(
            "UPDATE projects SET merge_policy='approval' WHERE id=$1", self.project.id
        )
        proposal = await self.prepare()
        observer, projection, _ = await self.pause(proposal)
        await self.decide(observer, projection)
        result = await self.execute(proposal, approved=True)
        self.assertEqual(result["state"], "merged")
        await self.assert_completed_mcp(result, "invoke-1")
        agent = AgentService(PgStore())
        ctx = await agent.authenticate(token_for(self.sid))
        # This fixture has no receipt for the candidate's replacement invocation.
        # The unrelated original owner decision remains immutable.
        await self.pool.execute(
            "UPDATE merge_requests SET intent_invocation_id='no-receipt' WHERE owner_id=$1",
            self.user,
        )
        for path in ("/mcp", "/mcp/merge-approval"):
            response = await self.mcp("tools/list", path=path)
            self.assertIn(response.status_code, (401, 403))
            self.assertEqual(response.content, b"")
        for tool, args in (
            ("whoami", {}),
            ("task_get", {"task_id": self.task.id}),
            ("report", self.final_report()),
        ):
            denied = await invoke(agent, ctx, tool, args)
            self.assertTrue(denied.isError, tool)
        with self.assertRaises(PolicyError):
            await merge.completed_replay(
                self.binding,
                {"proposal_id": result["proposal_id"], "request_id": "no-receipt"},
                approved=False,
            )
        self.assertTrue(await self.held())

    async def test_protected_auto_policy_completion_still_requires_receipt(self):
        self.fake.files[0]["filename"] = "k8s/deployment.yaml"
        proposal = await self.prepare()
        self.assertEqual(proposal["route"], "approval")
        observer, projection, _ = await self.pause(proposal)
        await self.decide(observer, projection)
        result = await self.execute(proposal, approved=True)
        self.assertEqual(result["state"], "merged")
        self.assertTrue(
            (
                await AgentService(PgStore()).authenticate(token_for(self.sid))
            ).actor.merge_status_only
        )
        await self.pool.execute(
            "UPDATE merge_requests SET intent_invocation_id='no-receipt' WHERE owner_id=$1",
            self.user,
        )
        self.assertIn((await self.mcp("tools/list")).status_code, (401, 403))

    async def test_completed_mcp_denies_unrelated_and_other_owner_bindings(self):
        await self.merged()
        unrelated, _ = await self.bound_session(
            role="supervisor", mcp_grant_kind="workspace"
        )
        self.assertIn(
            (await self.mcp("tools/list", sid=unrelated)).status_code, (401, 403)
        )
        self.assertIsNone(
            await merge.completed_status_binding(
                {**self.binding, "user_id": "other-owner"}
            )
        )
        await self.pool.execute(
            "UPDATE sessions SET user_id='other-owner' WHERE id=$1", self.sid
        )
        self.assertIn((await self.mcp("tools/list")).status_code, (401, 403))

    async def test_completed_mcp_revalidates_claim_after_authentication(
        self,
    ):
        await self.merged()
        agent = AgentService(PgStore())
        ctx = await agent.authenticate(token_for(self.sid))
        await self.pool.execute(
            "UPDATE workspace_writer_claims SET held=FALSE WHERE attempt_id=$1",
            self.attempt.id,
        )
        for tool, args in (
            ("whoami", {}),
            ("task_get", {"task_id": self.task.id}),
            ("report", self.final_report()),
        ):
            denied = await invoke(agent, ctx, tool, args)
            self.assertTrue(denied.isError, tool)
        self.assertIn((await self.mcp("tools/list")).status_code, (401, 403))

    async def test_completed_child_mcp_requires_live_parent_ancestry(self):
        parent, parent_sid = await attention_fixtures.TaskAttentionTests.child(self)
        result = await self.merged()
        await self.assert_completed_mcp(result, self.args["request_id"])
        agent = AgentService(PgStore())
        ctx = await agent.authenticate(token_for(self.sid))
        denied = await invoke(agent, ctx, "task_get", {"task_id": parent.id})
        self.assertTrue(denied.isError)
        await self.pool.execute(
            "UPDATE native_bindings SET token_hash=NULL WHERE session_id=$1", parent_sid
        )
        self.assertIn((await self.mcp("tools/list")).status_code, (401, 403))
        denied = await invoke(
            agent, ctx, "report", self.final_report(request_id="after-revocation")
        )
        self.assertTrue(denied.isError)

    async def assert_released(self, final="completed", product=None):
        self.assertFalse(await self.held())
        async with self.pool.acquire() as conn:
            attempt = await lifecycle.load_attempt(conn, self.attempt.id)
            task = await lifecycle.load_task(conn, self.task.id)
            self.assertEqual(attempt.state, final)
            self.assertEqual(task.status, product or final)
            self.assertIsNone(task.current_attempt_id)
        binding = await ns.get_binding(self.sid)
        self.assertIsNone(binding["token_hash"])
        self.assertIsNotNone(binding["kagent_deleted_at"])
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM workspace_writer_claims WHERE held AND branch='feature' AND owner_id=$1",
                self.user,
            ),
            0,
        )

    async def cancel(self):
        async with self.pool.acquire() as conn, conn.transaction():
            task = await lifecycle.load_task(conn, self.task.id)
            return await service.mutate(
                conn,
                TaskPrincipal(self.user),
                "cancel",
                TaskAction(
                    request_id="cancel-result",
                    expected_version=task.version,
                    expected_attempt_id=self.attempt.id,
                ),
                task_id=task.id,
                installed_ports=service.TaskPorts(
                    provisioning=provisioning.Provisioning()
                ),
            )

    async def test_parent_merge_projects_blocked_task_without_child_merge_proposal(
        self,
    ):
        from mainloop.services import github_creation, github_merge
        from mainloop.tasks.projection import Projection
        from tests.runtime.github_app_fake import app_transport
        from tests.runtime.test_open_pull_request import ARGS, FakeGitHub

        # This task created its PR before the publication gate refused its work.
        fake = FakeGitHub()
        fake.repo["default_branch"] = "main"
        fake.pr_changes = lambda pr: pr["head"].update(ref="feature")
        cls = github_creation.GitHubCreationClient
        with patch.object(
            github_creation,
            "GitHubCreationClient",
            lambda repository: cls(repository, transport=app_transport(fake.handle)),
        ):
            await github_creation.open_pull_request(
                self.binding,
                {**ARGS, "project_id": self.project.id, "branch": "feature"},
            )
        async with self.pool.acquire() as conn, conn.transaction():
            task = await lifecycle.load_task(conn, self.task.id)
            await store.save_task(
                conn,
                task.model_copy(
                    update={
                        "status": "blocked",
                        "reason": "reconciliation",
                        "version": task.version + 1,
                    }
                ),
                task.version,
                "fixture:publication-blocked",
            )
        parent, _ = await self.bound_session(role="main")
        await self.pool.execute(
            "UPDATE native_bindings SET kagent_session_id=$2 WHERE session_id=$1",
            parent,
            f"runtime-{parent}",
        )
        result = await merge.auto_merge(await PgStore().get_binding(parent), self.args)
        self.assertEqual(result["state"], "merged")
        self.assertEqual((await self.view())[0].status, "blocked")
        # The parent outcome has no child task association or transferable consent.
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM merge_proposals WHERE facts->>'task_id'=$1",
                self.task.id,
            ),
            0,
        )
        self.fake.errors[f"/commits/{merge_fixtures.SHA}/check-runs"] = 503
        cls = github_merge.GitHubMergeClient
        with patch.object(
            github_merge,
            "GitHubMergeClient",
            lambda repository: cls(
                repository, transport=app_transport(self.fake.handle)
            ),
        ):
            await Projection().refresh(db, self.task.id)
            async with self.pool.acquire() as conn:
                view = await service.read(conn, TaskPrincipal(self.user), self.task.id)
            self.assertEqual(
                (view.projection.pr_state, view.projection.merge_state),
                ("merged", "merged"),
            )
            self.assertEqual(view.projection.ci_state, "unknown")
            self.assertIsNone(view.projection.merge_proposal_id)
            self.assertEqual(
                (view.task.status, view.task.reason), ("blocked", "reconciliation")
            )
            self.assertTrue(await self.held())
            self.client.delete_session.assert_not_awaited()
            await Projection().refresh(db, self.task.id)
            self.assertEqual((await self.view())[0].version, view.task.version)
        self.assertEqual(len(self.fake.puts), 1)

    async def test_merged_observation_outranks_old_child_proposal_on_read(self):
        from mainloop.services import github_merge
        from mainloop.tasks.projection import Projection
        from tests.runtime.github_app_fake import app_transport

        await self.prepare()
        # A merge outside this proposal leaves the child's durable candidate prepared.
        self.fake.pr.update(state="closed", merged=True, merge_commit_sha="c" * 40)
        cls = github_merge.GitHubMergeClient
        with patch.object(
            github_merge,
            "GitHubMergeClient",
            lambda repository: cls(
                repository, transport=app_transport(self.fake.handle)
            ),
        ):
            await Projection().refresh(db, self.task.id)
        async with self.pool.acquire() as conn:
            view = await service.read(conn, TaskPrincipal(self.user), self.task.id)
        self.assertEqual(view.projection.merge_state, "merged")
        self.assertNotEqual(view.task.status, "completed")
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT state FROM merge_requests WHERE owner_id=$1", self.user
            ),
            "prepared",
        )
        self.assertTrue(await self.held())
        self.assertFalse(self.fake.puts)

    async def test_merged_released_once_and_owner_result_survives_agent_revocation(
        self,
    ):
        result = await self.merged()
        await asyncio.gather(self.reconcile(), self.reconcile())
        await self.assert_released()
        task, projection = await self.view()
        self.assertEqual(
            (projection.pr_state, projection.merge_state, projection.pr_url),
            ("merged", "merged", result["url"]),
        )
        version = task.version
        await asyncio.gather(self.reconcile(), self.reconcile())
        self.assertEqual((await self.view())[0].version, version)
        self.client.delete_session.assert_awaited_once_with(self.runtime)
        self.assertEqual(len(self.fake.puts), 1)
        # The immutable completed-result exception ends when the binding is revoked.
        with self.assertRaises(HTTPException):
            await AgentService(PgStore()).authenticate(token_for(self.sid))
        async with self.pool.acquire() as conn:
            with self.assertRaises(lifecycle.LifecycleDenied):
                await lifecycle.authenticate_binding(
                    conn, self.binding, allow_completed_read=True
                )
            view = await service.read(conn, TaskPrincipal(self.user), self.task.id)
        self.assertEqual(view.projection.merge_state, "merged")
        self.assertEqual(view.projection.pr_url, result["url"])
        self.assertEqual(view.attempts[0].state, "completed")

    async def test_unknown_delete_retains_capacity_and_retries_exact_identity(self):
        await self.merged()
        self.client.delete_session.side_effect = OutcomeUnknown(
            "fixture lost delete reply"
        )
        await self.reconcile()
        self.assertTrue(await self.held())
        self.assertIsNone((await ns.get_binding(self.sid))["kagent_deleted_at"])
        self.client.delete_session.side_effect = self.deleted
        await self.reconcile()
        await self.assert_released()
        self.assertEqual(
            [call.args for call in self.client.delete_session.await_args_list],
            [(self.runtime,), (self.runtime,)],
        )

    async def test_restart_after_drain_and_revocation_resumes(self):
        await self.merged()
        with patch.object(ns, "delete_kagent_session", side_effect=ProcessLost):
            with self.assertRaises(ProcessLost):
                await self.reconcile()
        self.assertTrue(await self.held())
        self.assertIsNone((await ns.get_binding(self.sid))["token_hash"])
        await self.reconcile()
        await self.assert_released()

    async def test_restart_after_confirmed_delete_reuses_evidence(self):
        await self.merged()
        with patch.object(lifecycle, "settle", side_effect=ProcessLost):
            with self.assertRaises(ProcessLost):
                await self.reconcile()
        self.assertTrue(await self.held())
        self.assertIsNotNone((await ns.get_binding(self.sid))["kagent_deleted_at"])
        await self.reconcile()
        await self.assert_released()
        self.client.delete_session.assert_awaited_once_with(self.runtime)

    async def test_all_outstanding_deliveries_block_drain_including_held_queue(self):
        await self.merged()
        cid = (await db.get_session(self.sid)).conversation_id
        for state in (*ns.OPEN_STATES, "queued", "uncertain"):
            with self.subTest(state=state):
                message = await self.delivery(self.sid, cid, state)
                await self.pool.execute(
                    "UPDATE native_bindings SET queue_held=TRUE WHERE session_id=$1",
                    self.sid,
                )
                await self.reconcile()
                self.assertTrue(await self.held())
                self.assertIsNotNone((await ns.get_binding(self.sid))["token_hash"])
                self.client.delete_session.assert_not_awaited()
                await self.pool.execute(
                    "UPDATE native_deliveries SET state='completed' WHERE message_id=$1",
                    message,
                )
        await self.reconcile()
        await self.assert_released()

    async def test_mismatched_or_unsettled_delete_never_releases(self):
        await self.merged()
        for reply in (
            KagentSession(
                "different", RuntimeState.DELETED, RuntimeOperation.NONE, "different"
            ),
            KagentSession(
                self.runtime,
                RuntimeState.DELETING,
                RuntimeOperation.DELETE,
                self.runtime,
            ),
            KagentSession(
                self.runtime,
                RuntimeState.DELETED,
                RuntimeOperation.DELETE,
                self.runtime,
            ),
        ):
            self.client.delete_session.side_effect = None
            self.client.delete_session.return_value = reply
            await self.reconcile()
            self.assertTrue(await self.held())
            self.assertIsNone((await ns.get_binding(self.sid))["kagent_deleted_at"])
        self.client.delete_session.side_effect = self.deleted
        await self.reconcile()
        await self.assert_released()

    async def test_known_identity_not_found_confirms_absence(self):
        await self.merged()
        self.client.delete_session.side_effect = SessionError("gone", grpc_status=5)
        await self.reconcile()
        await self.assert_released()

    async def test_missing_runtime_identity_does_not_prove_deletion(self):
        await self.merged()
        await self.pool.execute(
            "UPDATE native_bindings SET kagent_session_id=NULL WHERE session_id=$1",
            self.sid,
        )
        await self.reconcile()
        self.assertTrue(await self.held())
        self.client.delete_session.assert_not_awaited()
        await self.pool.execute(
            "UPDATE native_bindings SET kagent_session_id=$2 WHERE session_id=$1",
            self.sid,
            self.runtime,
        )
        await self.reconcile()
        await self.assert_released()

    async def test_pending_cancel_wins_before_result_drain(self):
        await self.merged()
        operation = await self.cancel()
        await self.reconcile()
        self.assertTrue(await self.held())
        self.client.delete_session.assert_not_awaited()
        await provisioning.Provisioning().reconcile(db, operation)
        await self.assert_released("cancelled")
        self.assertEqual((await self.view())[1].merge_state, "merged")

    async def test_cancel_between_deletion_and_settlement_wins(self):
        await self.merged()
        operations = []

        async def delete_and_cancel(runtime):
            result = await self.deleted(runtime)
            operations.append(await self.cancel())
            return result

        self.client.delete_session.side_effect = delete_and_cancel
        await self.reconcile()
        self.assertTrue(await self.held())
        self.client.delete_session.side_effect = self.deleted
        await provisioning.Provisioning().reconcile(db, operations[0])
        await self.assert_released("cancelled")

    async def test_unresolved_publication_holds_before_drain_and_after_delete(self):
        await self.merged()
        with patch.object(
            publication, "unresolved_intents", return_value=("fixture:uncertain-PR",)
        ):
            await self.reconcile()
        self.client.delete_session.assert_not_awaited()
        with patch.object(
            publication,
            "unresolved_intents",
            side_effect=[(), ("fixture:uncertain-merge",)],
        ):
            await self.reconcile()
        self.assertTrue(await self.held())
        await self.reconcile()
        await self.assert_released()
        self.client.delete_session.assert_awaited_once_with(self.runtime)

    async def test_old_completed_rows_recover_without_an_operation_or_new_merge(self):
        # Persisted old row fixture, matching settlement before cleanup was integrated.
        task = (await self.view())[0]
        async with self.pool.acquire() as conn, conn.transaction():
            await store.save_task(
                conn,
                task.model_copy(
                    update={"status": "completed", "version": task.version + 1}
                ),
                task.version,
                "fixture:old-completed",
            )
        self.assertEqual(
            await self.pool.fetchval("SELECT count(*) FROM task_operations"), 0
        )
        await self.reconcile()
        await self.assert_released()
        self.assertFalse(self.fake.puts)
        self.assertFalse(self.gateway.sent)

    async def test_failed_and_cancelled_held_results_recover(self):
        for final in ("failed", "cancelled"):
            with self.subTest(final=final):
                if final == "cancelled":
                    self.sid, _ = await self.bound_session(
                        role="supervisor", mcp_grant_kind="workspace"
                    )
                    self.task, self.attempt = await self.enroll(self.sid, "feature")
                    self.runtime = f"runtime-{self.sid}"
                task = (await self.view())[0]
                async with self.pool.acquire() as conn, conn.transaction():
                    await store.save_task(
                        conn,
                        task.model_copy(
                            update={"status": final, "version": task.version + 1}
                        ),
                        task.version,
                        f"fixture:{final}",
                    )
                await self.reconcile()
                await self.assert_released(final)

    async def test_blocked_active_work_is_not_a_terminal_result(self):
        task = (await self.view())[0]
        async with self.pool.acquire() as conn, conn.transaction():
            await store.save_task(
                conn,
                task.model_copy(
                    update={
                        "status": "blocked",
                        "reason": "reconciliation",
                        "version": task.version + 1,
                    }
                ),
                task.version,
                "fixture:blocked-report",
            )
        await self.reconcile()
        self.assertTrue(await self.held())
        self.client.delete_session.assert_not_awaited()
        self.assertIsNotNone((await ns.get_binding(self.sid))["token_hash"])

    async def test_live_children_hold_supervisor_runtime_and_capacity(self):
        sid, _ = await self.bound_session(role="child", mcp_grant_kind="workspace")
        await self.enroll(sid, "feature/child", self.task)
        await self.merged()
        await self.reconcile()
        self.assertTrue(await self.held())
        self.client.delete_session.assert_not_awaited()
        self.assertIsNotNone((await ns.get_binding(self.sid))["token_hash"])

    async def test_cleanup_preserves_a_transferred_writer_claim(self):
        await self.merged()
        successor, _ = await self.bound_session(
            role="agent", mcp_grant_kind="workspace"
        )
        # Persist a confirmed transfer seam, using the actual claim generation guard.
        # Runtime cleanup of the old attempt must not release a newer reservation.
        async with self.pool.acquire() as conn, conn.transaction():
            await conn.execute(
                "UPDATE workspace_writer_claims SET held=FALSE WHERE attempt_id=$1",
                self.attempt.id,
            )
            generation = await store.reserve_writer(
                conn,
                owner_id=self.user,
                repository="owner/repo",
                branch="feature",
                binding_id=successor,
            )
        await self.reconcile()
        self.assertFalse(await self.held())
        row = await self.pool.fetchrow(
            "SELECT held,generation,binding_id FROM workspace_writer_claims WHERE owner_id=$1 AND branch='feature'",
            self.user,
        )
        self.assertEqual(tuple(row), (True, generation, successor))
        self.assertGreater(generation, self.attempt.writer_generation)
        self.assertIsNotNone((await ns.get_binding(self.sid))["kagent_deleted_at"])


class CodingCapacityReleaseTests(ledger_fixtures.KagentFakeCase):
    asyncSetUp = report_fixtures.TaskReportsPostgresTests.asyncSetUp
    asyncTearDown = report_fixtures.TaskReportsPostgresTests.asyncTearDown
    request = report_fixtures.TaskReportsPostgresTests.request
    create_task = report_fixtures.TaskReportsPostgresTests.create_task

    async def test_parent_can_identify_and_cancel_abandoned_blocked_reservation(self):
        main = await ensure_main_session(self.user)
        agent = AgentService(PgStore())
        ctx = await agent.authenticate(token_for(main["session_id"]))
        held = {}
        for status in ("blocked", "waiting", "completed"):
            result = await invoke(
                agent, ctx, "delegate", self.request().model_dump(mode="json")
            )
            self.assertFalse(result.isError, result.content)
            async with self.pool.acquire() as conn:
                operation = await store.operation(
                    conn, result.structuredContent["id"], self.owner
                )
            await self.worker.reconcile(db, operation)
            if status in ("blocked", "waiting"):
                async with self.pool.acquire() as conn:
                    attempt = await lifecycle.load_attempt(conn, operation.attempt_id)
                writer = await agent.authenticate(token_for(attempt.binding_id))
                report = await invoke(
                    agent,
                    writer,
                    "report",
                    {
                        "task_id": operation.task_id,
                        "attempt_id": attempt.id,
                        "request_id": "result",
                        "summary": "Fixture result",
                        "outcome": "blocked" if status == "blocked" else "completed",
                    },
                )
                self.assertFalse(report.isError, report.content)
            async with self.pool.acquire() as conn, conn.transaction():
                task = await lifecycle.load_task(conn, operation.task_id)
                if status == "completed":
                    await store.save_task(
                        conn,
                        task.model_copy(
                            update={"status": status, "version": task.version + 1}
                        ),
                        task.version,
                        "fixture:old-completed",
                    )
                else:
                    self.assertEqual(task.status, status)
                held[status] = task.id
        refusal = await invoke(
            agent, ctx, "delegate", self.request().model_dump(mode="json")
        )
        self.assertTrue(refusal.isError)
        detail = json.loads(refusal.content[0].text.split(" ", 2)[2])
        self.assertEqual(detail["limit"], 3)
        self.assertEqual(
            {row["task_id"] for row in detail["held_tasks"]}, set(held.values())
        )
        self.assertIn("task_cancel", detail["recovery"])
        blocked = next(
            row for row in detail["held_tasks"] if row["status"] == "blocked"
        )
        self.assertEqual(blocked["task_id"], held["blocked"])
        self.assertEqual(blocked["attempt_state"], "active")
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM task_attempts WHERE capacity_held"
            ),
            3,
        )
        cancellation = await invoke(
            agent,
            ctx,
            "task_cancel",
            {
                "task_id": blocked["task_id"],
                "request_id": "cancel-abandoned",
                "expected_version": blocked["version"],
                "expected_attempt_id": blocked["current_attempt_id"],
            },
        )
        self.assertFalse(cancellation.isError, cancellation.content)
        async with self.pool.acquire() as conn:
            operation = await store.operation(
                conn, cancellation.structuredContent["id"], self.owner
            )
        with patch.object(
            ns.get_client(),
            "delete_session",
            AsyncMock(side_effect=OutcomeUnknown("delete reply lost")),
        ):
            await self.worker.reconcile(db, operation)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM task_attempts WHERE capacity_held"
            ),
            3,
        )
        still_full = await invoke(
            agent, ctx, "delegate", self.request().model_dump(mode="json")
        )
        self.assertTrue(still_full.isError)
        self.assertIn(blocked["task_id"], still_full.content[0].text)
        await self.worker.reconcile(db, operation)
        async with self.pool.acquire() as conn:
            task = await lifecycle.load_task(conn, blocked["task_id"])
            attempt = await lifecycle.load_attempt(conn, blocked["held_attempt_id"])
        self.assertEqual((task.status, attempt.state), ("cancelled", "cancelled"))
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM task_attempts WHERE capacity_held"
            ),
            2,
        )
        admitted = await invoke(
            agent, ctx, "delegate", self.request().model_dump(mode="json")
        )
        self.assertFalse(admitted.isError, admitted.content)

    async def test_new_main_delegate_succeeds_after_full_parent_bucket_released(self):
        main = await ensure_main_session(self.user)
        agent = AgentService(PgStore())
        ctx = await agent.authenticate(token_for(main["session_id"]))
        attempts = []
        for _ in range(3):
            result = await invoke(
                agent, ctx, "delegate", self.request().model_dump(mode="json")
            )
            self.assertFalse(result.isError, result.content)
            async with self.pool.acquire() as conn:
                operation = await store.operation(
                    conn, result.structuredContent["id"], self.owner
                )
            await self.worker.reconcile(db, operation)
            async with self.pool.acquire() as conn, conn.transaction():
                attempt = await lifecycle.load_attempt(conn, operation.attempt_id)
                task = await lifecycle.load_task(conn, attempt.task_id)
                # Old completed-row fixture; the merge service itself is covered above.
                await store.save_task(
                    conn,
                    task.model_copy(
                        update={"status": "completed", "version": task.version + 1}
                    ),
                    task.version,
                    "fixture:old-completed",
                )
                await conn.execute(
                    "UPDATE native_deliveries SET state='completed' WHERE session_id=$1",
                    attempt.binding_id,
                )
                attempts.append(attempt)
        blocked = await invoke(
            agent, ctx, "delegate", self.request().model_dump(mode="json")
        )
        self.assertTrue(blocked.isError)
        self.assertIn("parent_capacity", blocked.content[0].text)
        await service.reconcile_once(
            db, installed_ports=service.TaskPorts(provisioning=self.worker)
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM task_attempts WHERE capacity_held"
            ),
            0,
        )
        admitted = await invoke(
            agent, ctx, "delegate", self.request().model_dump(mode="json")
        )
        self.assertFalse(admitted.isError, admitted.content)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM task_attempts WHERE capacity_held"
            ),
            1,
        )
        self.assertTrue(
            all(
                [
                    (await ns.get_binding(a.binding_id))["kagent_deleted_at"]
                    for a in attempts
                ]
            )
        )

    async def test_blocked_failed_first_brief_releases_after_confirmed_deletion(self):
        _, task, attempt = await self.create_task()
        await self.pool.execute(
            "UPDATE native_deliveries SET state='failed',task_id='fixture-failed',evidence_ref='a2a:task/fixture-failed#failed',detail='bootstrap failed' WHERE message_id=$1",
            attempt.brief_delivery_id,
        )
        await service.reconcile_once(
            db, installed_ports=service.TaskPorts(provisioning=self.worker)
        )
        async with self.pool.acquire() as conn:
            task = await lifecycle.load_task(conn, task.id)
            attempt = await lifecycle.load_attempt(conn, attempt.id)
        self.assertEqual(
            (task.status, task.reason, task.current_attempt_id, attempt.state),
            ("blocked", "reconciliation", None, "failed"),
        )
        self.assertFalse(
            await self.pool.fetchval(
                "SELECT capacity_held FROM task_attempts WHERE id=$1", attempt.id
            )
        )
        self.assertIsNotNone(
            (await ns.get_binding(attempt.binding_id))["kagent_deleted_at"]
        )
