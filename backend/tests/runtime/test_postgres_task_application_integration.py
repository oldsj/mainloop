"""Shared application seams on isolated PostgreSQL with fake GitHub and kagent."""

import asyncio
import json
import uuid
from datetime import UTC, datetime, timedelta
from unittest.mock import AsyncMock, patch

import httpx
from mainloop import api
from mainloop.config import settings
from mainloop.db import db
from mainloop.db import tasks as store
from mainloop.identity import current_user
from mainloop.runtime import hitl_continuation as continuation
from mainloop.runtime import native_sessions
from mainloop.runtime.agent_credentials import revoke
from mainloop.runtime.delegation import PgStore
from mainloop.runtime.kagent_client import OutcomeUnknown
from mainloop.runtime.policy import PolicyError
from mainloop.services import github_merge, merge
from mainloop.tasks import attention, lifecycle, projection, provisioning, service
from tests.runtime import test_postgres_task_attention as fixtures
from tests.runtime import test_postgres_task_handoff as s3
from tests.runtime import test_postgres_task_provisioning as s1
from tests.runtime.test_merge import MergeFixture

from models.hitl import (
    HITL_EXTENSION,
    DecisionReceipt,
    ToolApproval,
    ToolApprovalResponse,
)
from models.task import TaskAction, TaskReassign, TaskReport


class TaskApplicationIntegrationTests(MergeFixture):
    # Reuse the accepted fixture helpers without inheriting its test methods.
    enroll = fixtures.TaskAttentionTests.enroll
    child = fixtures.TaskAttentionTests.child
    view = fixtures.TaskAttentionTests.view
    pause = fixtures.TaskAttentionTests.pause

    async def asyncSetUp(self):
        await super().asyncSetUp()
        await self.pool.execute(
            "TRUNCATE tasks,task_attempts,workspace_writer_claims,task_events,task_operations CASCADE"
        )
        self.task, self.attempt = await self.enroll(self.sid, "feature")
        self.binding = await PgStore().get_binding(self.sid)

    async def get(self, owner):
        api.app.dependency_overrides[current_user] = lambda: owner
        try:
            with patch.object(settings, "api_hosts", "fixture"):
                return await self.request_get()
        finally:
            api.app.dependency_overrides.pop(current_user, None)

    async def request_get(self):
        async with httpx.AsyncClient(
            transport=httpx.ASGITransport(app=api.app), base_url="http://fixture"
        ) as client:
            return await client.get(f"/tasks/{self.task.id}")

    async def test_task_get_authorizes_before_projection_and_stays_db_only(self):
        await self.prepare()
        calls = len(self.fake.calls)
        reads = len(self.gateway.reads)
        original = projection.read
        with patch.object(projection, "read", wraps=original) as read:
            denied = await self.get("other-owner")
            self.assertEqual(denied.status_code, 404)
            read.assert_not_called()
            current = await self.get(self.user)
            self.assertEqual(current.status_code, 200)
            self.assertEqual(current.json()["projection"]["ci_state"], "success")
            self.assertEqual(
                current.json()["projection"]["publication_state"], "read_only"
            )
            read.assert_awaited_once()
        task, value = await self.view()
        for updates in (
            {"observed_at": datetime.now(UTC) - timedelta(minutes=6)},
            {"ci_head_sha": "d" * 40},
            {"observed_at": datetime.now(UTC) + timedelta(minutes=1)},
        ):
            await self.pool.execute(
                "UPDATE tasks SET projection=$2::jsonb WHERE id=$1",
                task.id,
                value.model_copy(update=updates).model_dump_json(),
            )
            self.assertEqual(
                (await self.get(self.user)).json()["projection"]["ci_state"],
                "unknown",
            )
        self.assertEqual(len(self.fake.calls), calls)
        self.assertEqual(len(self.gateway.reads), reads)
        self.assertFalse(self.gateway.sent)
        self.assertFalse(self.fake.puts)

    async def test_native_reads_exclude_unlinked_artifacts_and_private_evidence(self):
        from mainloop.mcp_app import invoke
        from mainloop.runtime.agent_identity import token_for
        from mainloop.runtime.agent_tools import AgentService
        from mainloop.runtime.delegation import render_for_binding
        from mainloop.tasks.principal import TaskPrincipal

        async with self.pool.acquire() as conn, conn.transaction():
            operation, _ = await store.begin_operation(
                conn,
                TaskPrincipal(self.user),
                "private-reader-fixture",
                "retry",
                {"_s3": {"adapter_snapshot": "private-reader-marker"}},
                task_id=self.task.id,
            )
            await store.add_artifact(
                conn,
                operation.id,
                "retention_receipt",
                {"private": "private-reader-marker"},
            )
            await lifecycle.save_attempt(
                conn,
                self.attempt.model_copy(
                    update={
                        "evidence_refs": ("private-reader-marker",),
                        "retention_hold": "private-reader-marker",
                        "result_ref": "private-reader-marker",
                    }
                ),
            )
        agent = AgentService(PgStore(), [])
        ctx = await agent.authenticate(token_for(self.sid))
        calls, reads = len(self.fake.calls), len(self.gateway.reads)
        for action in ("task_get", "task_history", "task_list"):
            result = await invoke(
                agent,
                ctx,
                action,
                {} if action == "task_list" else {"task_id": self.task.id},
            )
            self.assertFalse(result.isError, result.content)
            value = result.structuredContent
            view = value["tasks"][0] if action == "task_list" else value
            self.assertEqual(view["artifacts"], [])
            self.assertNotIn("private-reader-marker", json.dumps(value))
        text = await render_for_binding(ctx.binding)
        self.assertNotIn("private-reader-marker", text)
        self.assertEqual(
            (len(self.fake.calls), len(self.gateway.reads)), (calls, reads)
        )
        self.assertFalse(self.gateway.sent)
        self.assertFalse(self.fake.puts)
        owner = await self.get(self.user)
        self.assertEqual(owner.status_code, 200)
        self.assertIn("private-reader-marker", json.dumps(owner.json()))

    async def test_startup_installs_one_projection_at_existing_seam(self):
        cls = projection.Projection
        old = service.ports.provisioning, service.ports.projection
        self.addCleanup(setattr, service.ports, "provisioning", old[0])
        self.addCleanup(setattr, service.ports, "projection", old[1])
        service.ports.projection = None
        with (
            patch.object(db, "connect", new=AsyncMock()),
            patch.object(db, "ensure_tables_exist", new=AsyncMock()),
            patch.object(api.DBOS, "launch"),
            patch.object(api, "_apply_mock_github"),
            patch.object(native_sessions, "reconcile_loop", new=AsyncMock()),
            patch.object(service, "reconciliation_dispatcher", new=AsyncMock()),
            patch.object(
                provisioning, "install", wraps=provisioning.install
            ) as install,
            patch.object(projection, "Projection", wraps=projection.Projection) as port,
        ):
            try:
                await api.startup_event()
                install.assert_called_once_with()
                port.assert_called_once_with()
                self.assertIsInstance(service.ports.projection, cls)
            finally:
                tasks = [api.app.state.native_reconcile, api.app.state.task_reconcile]
                for task in tasks:
                    task.cancel()
                await asyncio.gather(*tasks, return_exceptions=True)

    async def test_existing_reconciler_consumes_real_projection_without_merge(self):
        await self.prepare()
        self.fake.pr["state"] = "closed"
        cls = github_merge.GitHubMergeClient
        with patch.object(
            github_merge,
            "GitHubMergeClient",
            lambda: cls(transport=httpx.MockTransport(self.fake.handle)),
        ):
            await service.reconcile_once(
                db,
                installed_ports=service.TaskPorts(projection=projection.Projection()),
            )
        task, value = await self.view()
        self.assertEqual(value.pr_state, "closed")
        self.assertNotEqual(task.status, "completed")
        self.assertFalse(self.fake.puts)
        self.assertFalse(self.gateway.sent)

    async def test_projection_batches_rotate_past_revocation_and_failed_items(self):
        identities = {self.task.id: self.sid}
        for i in range(2):
            sid, _ = await self.bound_session(
                role="supervisor", mcp_grant_kind="workspace"
            )
            task, _ = await self.enroll(sid, f"rotation/{i}")
            identities[task.id] = sid
        ordered = sorted(identities)
        seen = []

        async def refresh(database, tid):
            seen.append(tid)
            if tid == ordered[0]:
                await revoke(identities[tid])
                await projection.Projection().refresh(database, tid)
            if tid == ordered[1]:
                raise RuntimeError("fixture projection unavailable")

        ports = service.TaskPorts(projection=AsyncMock(refresh=refresh))
        with patch.object(service, "PROJECTION_RECONCILE_LIMIT", 2, create=True):
            await service.reconcile_once(db, installed_ports=ports)
            self.assertGreater(len(seen), 1)
            self.assertLessEqual(len(seen), 2)
            await service.reconcile_once(db, installed_ports=ports)
        self.assertTrue(set(ordered).issubset(seen))
        self.assertFalse(self.gateway.sent)

    async def approval(self):
        parent, parent_sid = await self.child()
        await self.pool.execute(
            "UPDATE projects SET merge_policy='approval' WHERE id=$1", self.project.id
        )
        proposal = await self.prepare()
        observer, card, _ = await self.pause(proposal)
        self.assertEqual((await self.view(parent))[1].pending_approval_ids, (card.id,))
        return parent, parent_sid, proposal, observer, card

    async def counts(self, card):
        return tuple(
            [
                await self.pool.fetchval(
                    "SELECT count(*) FROM native_hitl_responses WHERE owner_id=$1",
                    self.user,
                ),
                await self.pool.fetchval(
                    "SELECT count(*) FROM native_hitl_response_members WHERE owner_id=$1",
                    self.user,
                ),
                await self.pool.fetchval(
                    "SELECT count(*) FROM queue_items WHERE hitl_request_id=$1", card.id
                ),
                len(self.gateway.sent),
            ]
        )

    async def test_receipt_refresh_commits_first_and_releases_decision_locks(self):
        parent, _, _, observer, card = await self.approval()
        original = attention.refresh

        async def refresh(database, binding):
            async with database.connection() as conn, conn.transaction():
                self.assertEqual(
                    await conn.fetchval(
                        "SELECT count(*) FROM native_hitl_responses WHERE owner_id=$1",
                        self.user,
                    ),
                    1,
                )
                for leaf in card.leaves:
                    self.assertTrue(
                        await conn.fetchval(
                            "SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))",
                            leaf.key(),
                        )
                    )
                self.assertTrue(
                    await conn.fetchval(
                        "SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))",
                        f"merge:{self.user}:123:17",
                    )
                )
                await conn.fetchrow(
                    "SELECT id FROM projects WHERE id=$1 FOR UPDATE NOWAIT",
                    self.project.id,
                )
            await original(database, binding)

        with patch.object(attention, "refresh", side_effect=refresh) as called:
            result = await self.decide(observer, card)
            called.assert_awaited_once_with(db, self.sid)
        self.assertEqual(result["transport_state"], "accepted")
        self.assertEqual((await self.view())[1].pending_approval_ids, ())
        root, value = await self.view(parent)
        self.assertEqual(
            (root.status, root.reason, value.pending_approval_ids),
            ("running", None, ()),
        )
        self.assertEqual(await self.counts(card), (1, 1, 1, 1))

    async def test_receipt_batch_refreshes_unique_binding_once(self):
        _, _, _, observer, _ = await self.approval()
        runtime = self.binding["kagent_session_id"]
        native_task = self.gateway.tasks[f"task-{runtime}"]
        native_task.status.message.message_id += "-batch"
        native_task.status.message.metadata[HITL_EXTENSION]["tools"].append(
            {
                "id": "read",
                "call_id": "native-read",
                "name": "ordinary.read",
                "args": {},
            }
        )
        async with self.pool.acquire() as conn:
            await observer.refresh(conn, runtime, native_task.id)
            raw = await conn.fetchval(
                "SELECT snapshot FROM native_hitl_requests WHERE owner_id=$1 AND NOT superseded",
                self.user,
            )
        card = fixtures.HITLProjection.model_validate(store.decode(raw))
        tool = card.payload.tools[0]
        digest = await self.pool.fetchval(
            "SELECT summary_digest FROM merge_proposals WHERE id=$1",
            tool.args["proposal_id"],
        )
        with patch.object(attention, "refresh", wraps=attention.refresh) as refreshed:
            result = await continuation.submit(
                self.user,
                card.id,
                "batch",
                ToolApprovalResponse(
                    type="tool_approval_response",
                    approvals=(
                        ToolApproval(id="call", approved=True),
                        ToolApproval(id="read", approved=True),
                    ),
                    reviewed_context={"call": digest},
                ),
                service=observer,
            )
            refreshed.assert_awaited_once_with(db, self.sid)
        receipt = DecisionReceipt.model_validate_json(json.dumps(result["response"]))
        self.assertEqual(len(receipt.calls), 2)
        self.assertEqual({c.leaf.binding_id for c in receipt.calls}, {self.sid})
        self.assertEqual((await self.view())[1].pending_approval_ids, ())
        self.assertEqual(await self.counts(card), (1, 2, 1, 1))

    async def test_attention_timeout_cannot_suppress_recorded_transport_recovery(self):
        parent, _, _, observer, card = await self.approval()
        with patch.object(continuation, "dispatch", new=AsyncMock()):
            result = await self.decide(observer, card)
        self.assertEqual(result["transport_state"], "recorded")

        async def hung_attention(*args):
            await asyncio.Future()

        with (
            patch.object(continuation, "observer", return_value=observer),
            patch.object(
                continuation, "ATTENTION_RECOVERY_BUDGET_SECONDS", 0.03, create=True
            ),
            patch.object(attention, "refresh", side_effect=hung_attention),
        ):
            await continuation.reconcile_hitl_responses()
        self.assertEqual(await self.counts(card), (1, 1, 1, 1))
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT state FROM native_hitl_response_transport WHERE owner_id=$1",
                self.user,
            ),
            "accepted",
        )
        with patch.object(continuation, "observer", return_value=observer):
            await continuation.reconcile_hitl_responses()
        self.assertEqual((await self.view(parent))[1].pending_approval_ids, ())

    async def test_projection_timeout_rotates_before_next_pass(self):
        sid, _ = await self.bound_session(role="supervisor", mcp_grant_kind="workspace")
        task, _ = await self.enroll(sid, "rotation/later")
        ordered = sorted((self.task.id, task.id))
        seen = []

        async def refresh(database, tid):
            seen.append(tid)
            if tid == ordered[0]:
                await asyncio.Future()

        ports = service.TaskPorts(projection=AsyncMock(refresh=refresh))
        with patch.object(
            service, "PROJECTION_RECONCILE_BUDGET_SECONDS", 0.05, create=True
        ):
            await service.reconcile_once(db, installed_ports=ports)
            self.assertEqual(seen, ordered[:1])
            await service.reconcile_once(db, installed_ports=ports)
        self.assertEqual(seen[1], ordered[1])

    async def test_committed_receipt_repairs_attention_after_request_projection_removal(
        self,
    ):
        parent, _, _, observer, card = await self.approval()
        with patch.object(
            attention, "refresh", side_effect=RuntimeError("fixture attention outage")
        ):
            await self.decide(observer, card)
        await self.pool.execute(
            "DELETE FROM native_hitl_requests WHERE owner_id=$1 AND id=$2",
            self.user,
            card.id,
        )
        reads = len(self.gateway.reads)
        with patch.object(continuation, "observer", return_value=observer):
            await continuation.reconcile_hitl_responses()
        self.assertEqual((await self.view(parent))[1].pending_approval_ids, ())
        # The fixture removed the old card with its projection. Recovery does
        # not rebuild it, reread the native task or send another decision.
        self.assertEqual(await self.counts(card), (1, 1, 0, 1))
        self.assertEqual(len(self.gateway.reads), reads)

    async def test_recorded_replay_recovers_attention_without_second_send(self):
        parent, _, _, observer, card = await self.approval()
        with patch.object(
            attention, "refresh", side_effect=RuntimeError("fixture attention outage")
        ):
            result = await self.decide(observer, card)
        self.assertEqual(result["transport_state"], "accepted")
        self.assertEqual((await self.view(parent))[0].reason, "approval")
        original = result["response"]
        replay = await self.decide(observer, card)
        self.assertEqual(replay["response"], original)
        self.assertEqual((await self.view(parent))[1].pending_approval_ids, ())
        versions = (await self.view(parent))[0].version, (await self.view())[0].version
        await self.decide(observer, card)
        self.assertEqual(
            versions,
            ((await self.view(parent))[0].version, (await self.view())[0].version),
        )
        self.assertEqual(await self.counts(card), (1, 1, 1, 1))

    async def test_uncertain_transport_recovers_attention_without_replaying_native_action(
        self,
    ):
        parent, _, _, observer, card = await self.approval()
        self.gateway.send_failure = OutcomeUnknown("fixture lost response")
        with patch.object(
            attention, "refresh", side_effect=RuntimeError("fixture attention outage")
        ):
            result = await self.decide(observer, card)
        self.assertEqual(result["transport_state"], "uncertain")
        self.gateway.send_failure = None
        with patch.object(continuation, "observer", return_value=observer):
            await continuation.reconcile_hitl_responses()
            await continuation.reconcile_hitl_responses()
        self.assertEqual((await self.view(parent))[1].pending_approval_ids, ())
        self.assertEqual(await self.counts(card), (1, 1, 1, 1))
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT state FROM native_hitl_response_transport WHERE owner_id=$1",
                self.user,
            ),
            "uncertain",
        )

    async def test_completed_leaf_recovers_active_ancestor_without_submit_or_parent_consent(
        self,
    ):
        parent, parent_sid, p, observer, card = await self.approval()
        with patch.object(
            attention, "refresh", side_effect=RuntimeError("fixture attention outage")
        ):
            result = await self.decide(observer, card)
        self.assertEqual((await self.execute(p, approved=True))["state"], "merged")
        self.assertEqual((await self.view())[0].status, "completed")
        self.assertEqual((await self.view(parent))[0].reason, "approval")
        with self.assertRaises(lifecycle.LifecycleDenied):
            async with self.pool.acquire() as conn:
                await lifecycle.check(conn, self.sid, "submit")
        with patch.object(continuation, "observer", return_value=observer):
            await continuation.reconcile_hitl_responses()
            await continuation.reconcile_hitl_responses()
        root, value = await self.view(parent)
        self.assertEqual(
            (root.status, root.reason, value.pending_approval_ids),
            ("running", None, ()),
        )
        self.assertEqual((await self.view())[0].status, "completed")
        with self.assertRaises(PolicyError):
            await merge.execute(
                await PgStore().get_binding(parent_sid),
                {"proposal_id": p["proposal_id"], "request_id": "invoke-1"},
                approved=True,
            )
        replay = await self.decide(observer, card)
        self.assertEqual(replay["response"], result["response"])
        self.assertEqual(await self.counts(card), (1, 1, 1, 1))
        self.assertEqual(len(self.fake.puts), 1)


class NativeContinuationApplicationTests(s1.TaskProvisioningPostgresTests):
    # Reuse the accepted real-Pg/fake-native enrollment and its retained checks.
    coordinator = s3.CoordinatorPostgresTests.coordinator

    async def context(
        self, source_provider="codex", target_provider="claude", *, parent=None
    ):
        from mainloop.tasks import reports

        _, task, source = await self.create_task(
            self.request(provider=source_provider), principal=parent
        )
        source_binding = await PgStore().get_binding(source.binding_id)
        selected = TaskReport(
            task_id=task.id,
            attempt_id=source.id,
            request_id=uuid.uuid4().hex,
            summary="Selected predecessor claim",
            outcome="progress",
            evidence_refs=("https://untrusted.invalid/do-not-fetch",),
        )
        async with self.pool.acquire() as conn, conn.transaction():
            await reports.record(conn, await self.principal(source), selected)
        async with self.pool.acquire() as conn:
            task = await lifecycle.load_task(conn, task.id)
        coordinator, external = self.coordinator()
        kind = "retry" if source_provider == target_provider else "reassign"
        payload = dict(
            request_id=uuid.uuid4().hex,
            expected_version=task.version,
            expected_attempt_id=source.id,
        )
        request = (
            TaskAction(**payload)
            if kind == "retry"
            else TaskReassign(**payload, target_profile_id=target_provider)
        )
        async with self.pool.acquire() as conn, conn.transaction():
            operation = await service.mutate(
                conn,
                self.owner,
                kind,
                request,
                task_id=task.id,
                installed_ports=service.TaskPorts(handoff=coordinator),
            )
        for _ in range(12):
            await coordinator.reconcile(db, operation)
            async with self.pool.acquire() as conn:
                operation = await store.handoff_operation(conn, operation.id)
            if operation.state == "target_ready":
                break
        self.assertEqual(operation.state, "target_ready", operation)
        async with self.pool.acquire() as conn:
            task = await lifecycle.load_task(conn, task.id)
            target = await lifecycle.load_attempt(conn, task.current_attempt_id)
        return task, source, target, operation, selected, source_binding, external

    async def activate(self, task, target):
        async with self.pool.acquire() as conn, conn.transaction():
            target = await provisioning.admit_successor(conn, task, target)
        return target

    async def client(self, session_id):
        from mainloop.runtime.agent_identity import token_for
        from mainloop.runtime.agent_tools import AgentService

        agent = AgentService(PgStore(), [])
        return agent, await agent.authenticate(token_for(session_id))

    async def native_views(self, agent, ctx, task, *, denied=False):
        from mainloop.mcp_app import invoke

        results = []
        for name in ("task_get", "task_history", "task_list"):
            result = await invoke(
                agent, ctx, name, {} if name == "task_list" else {"task_id": task.id}
            )
            self.assertEqual(bool(result.isError), denied, (name, result.content))
            if denied:
                self.assertIsNone(result.structuredContent)
            else:
                value = result.structuredContent
                results.append(
                    next(v for v in value["tasks"] if v["task"]["id"] == task.id)
                    if name == "task_list"
                    else value
                )
        return results

    async def replace_artifact(self, artifact_id, payload, *, checksum=None, raw=None):
        # Deliberate corruption of this class's scratch database only. The
        # production immutable-artifact trigger remains unchanged and tested.
        content = (
            raw
            if raw is not None
            else json.dumps(
                payload, sort_keys=True, separators=(",", ":"), ensure_ascii=False
            )
        )
        async with self.pool.acquire() as conn, conn.transaction():
            await conn.execute(
                "ALTER TABLE task_artifacts DISABLE TRIGGER immutable_task_artifact"
            )
            await conn.execute(
                "UPDATE task_artifacts SET content=$2,sha256=$3 WHERE id=$1",
                artifact_id,
                content,
                checksum if checksum is not None else store.digest(payload),
            )
            await conn.execute(
                "ALTER TABLE task_artifacts ENABLE TRIGGER immutable_task_artifact"
            )

    async def test_exact_links_both_provider_directions_and_retry_without_turns(self):
        from mainloop.mcp_app import invoke
        from mainloop.runtime.delegation import render_for_binding
        from mainloop.tasks.principal import TaskPrincipal

        for source_provider, target_provider in (
            ("codex", "claude"),
            ("claude", "codex"),
            ("codex", "codex"),
        ):
            with self.subTest(source=source_provider, target=target_provider):
                task, source, target, op, selected, old_binding, _ = await self.context(
                    source_provider, target_provider
                )
                target = await self.activate(task, target)
                agent, ctx = await self.client(target.session_id)
                async with self.pool.acquire() as conn, conn.transaction():
                    historical, _ = await store.begin_operation(
                        conn,
                        self.owner,
                        uuid.uuid4().hex,
                        "retry",
                        {"private": "unlinked-reader-marker"},
                        task_id=task.id,
                    )
                    await store.add_artifact(
                        conn,
                        historical.id,
                        "checkpoint",
                        {"private": "unlinked-reader-marker"},
                    )
                    await store.add_artifact(
                        conn,
                        op.id,
                        "retention_receipt",
                        {"private": "unlinked-reader-marker"},
                    )
                    await store.add_artifact(
                        conn,
                        op.id,
                        "unverified_provider_summary",
                        {"note": "unlinked-reader-marker"},
                    )
                    report = selected.model_copy(
                        update={
                            "request_id": uuid.uuid4().hex,
                            "summary": "unlinked-reader-marker",
                        }
                    )
                    await conn.execute(
                        "INSERT INTO task_reports(id,task_id,attempt_id,request_id,request_digest,snapshot) VALUES($1,$2,$3,$4,$5,$6::jsonb)",
                        uuid.uuid4().hex,
                        task.id,
                        source.id,
                        report.request_id,
                        store.digest(report.model_dump(mode="json")),
                        report.model_dump_json(),
                    )
                counts = len(self.fake.requests), await self.pool.fetchval(
                    "SELECT count(*) FROM native_deliveries"
                )
                identity = await invoke(agent, ctx, "whoami", {})
                self.assertFalse(identity.isError)
                self.assertEqual(identity.structuredContent["task_id"], task.id)
                self.assertEqual(
                    identity.structuredContent["writer_generation"],
                    target.writer_generation,
                )
                for view in await self.native_views(agent, ctx, task):
                    self.assertEqual(
                        {a["id"] for a in view["artifacts"]},
                        {op.manifest_ref, op.checkpoint_ref},
                    )
                    self.assertEqual(
                        view["reports"], [selected.model_dump(mode="json")]
                    )
                    self.assertNotIn("unlinked-reader-marker", json.dumps(view))
                    self.assertTrue(
                        all(not o["request_payload"] for o in view["operations"])
                    )
                    self.assertTrue(
                        all(not a["evidence_refs"] for a in view["attempts"])
                    )
                text = await render_for_binding(ctx.binding)
                self.assertIn(selected.summary, text)
                self.assertNotIn("unlinked-reader-marker", text)
                main_sid, _ = await self.bound_session(
                    role="main", mcp_grant_kind="coordination"
                )
                main_agent, main_ctx = await self.client(main_sid)
                await self.native_views(main_agent, main_ctx, task)
                self.assertNotIn(
                    "unlinked-reader-marker", await render_for_binding(main_ctx.binding)
                )
                with self.assertRaises(store.TaskError):
                    await PgStore().task_call(
                        old_binding, "task_get", {"task_id": task.id}
                    )
                self.assertEqual(
                    counts,
                    (
                        len(self.fake.requests),
                        await self.pool.fetchval(
                            "SELECT count(*) FROM native_deliveries"
                        ),
                    ),
                )
                async with self.pool.acquire() as conn:
                    owner = await service.read(conn, TaskPrincipal(self.user), task.id)
                self.assertIn("unlinked-reader-marker", owner.model_dump_json())
                # Release only this scenario through the retained S1 cancellation path.
                async with self.pool.acquire() as conn, conn.transaction():
                    current = await lifecycle.load_task(conn, task.id)
                    cancel = await service.mutate(
                        conn,
                        self.owner,
                        "cancel",
                        TaskAction(
                            request_id=uuid.uuid4().hex,
                            expected_version=current.version,
                            expected_attempt_id=target.id,
                        ),
                        task_id=task.id,
                    )
                await self.worker.reconcile(db, cancel)

    async def test_forged_manifest_and_checkpoint_fields_fail_on_all_alternate_reads(
        self,
    ):
        task, source, target, op, _, _, _ = await self.context()
        await self.activate(task, target)
        agent, ctx = await self.client(target.session_id)
        async with self.pool.acquire() as conn:
            manifest = (await store.get_artifact(conn, op.manifest_ref, self.owner))[
                "payload"
            ]
            checkpoint = (
                await store.get_artifact(conn, op.checkpoint_ref, self.owner)
            )["payload"]
        for key, value in (
            ("task_id", "forged"),
            ("operation_id", "forged"),
            ("predecessor_id", target.id),
            ("target_profile_id", "codex"),
            ("repository", "other/repo"),
            ("branch", "other"),
            ("checkpoint_sha", "b" * 40),
            ("caller_instructions", task.brief + "extra"),
            ("environment", {**manifest["environment"], "version_id": "forged"}),
            ("report_refs", ["https://untrusted.invalid/do-not-fetch"]),
            ("report_refs", ["task-report:missing"]),
            ("extra_private_field", "private-reader-marker"),
        ):
            with self.subTest(manifest=key, value=value):
                await self.replace_artifact(op.manifest_ref, {**manifest, key: value})
                await self.native_views(agent, ctx, task, denied=True)
        await self.replace_artifact(op.manifest_ref, manifest)
        for key, value in (
            ("attempt_id", target.id),
            ("binding_id", target.id),
            ("session_id", target.id),
            ("operation_id", "forged"),
            ("runtime_identity", "forged"),
            ("writer_generation", source.writer_generation + 1),
            ("repository", "other/repo"),
            ("branch", "other"),
            ("remote_sha", "b" * 40),
            ("git_dispatch", "unknown"),
            ("merge_dispatch", "unknown"),
            ("committed_checkpoint", False),
        ):
            with self.subTest(checkpoint=key):
                await self.replace_artifact(
                    op.checkpoint_ref, {**checkpoint, key: value}
                )
                await self.native_views(agent, ctx, task, denied=True)
        await self.replace_artifact(op.checkpoint_ref, checkpoint)
        await self.native_views(agent, ctx, task)

    async def test_corrupt_noncanonical_and_untyped_artifacts_fail_closed(self):
        import hashlib

        import asyncpg

        task, _, target, op, _, _, _ = await self.context()
        await self.activate(task, target)
        agent, ctx = await self.client(target.session_id)
        async with self.pool.acquire() as conn:
            payload = (await store.get_artifact(conn, op.manifest_ref, self.owner))[
                "payload"
            ]
        pretty = json.dumps(payload, indent=2)
        for raw, checksum in (
            (None, "0" * 64),
            (pretty, hashlib.sha256(pretty.encode()).hexdigest()),
            ("{", hashlib.sha256(b"{").hexdigest()),
            ("[]", store.digest([])),
        ):
            with self.subTest(raw=raw, checksum=checksum):
                await self.replace_artifact(
                    op.manifest_ref, payload, raw=raw, checksum=checksum
                )
                await self.native_views(agent, ctx, task, denied=True)
        await self.replace_artifact(op.manifest_ref, payload)
        await self.native_views(agent, ctx, task)
        with self.assertRaises(asyncpg.CheckViolationError):
            await self.replace_artifact(
                op.manifest_ref,
                {**payload, "report_refs": ["task-report:" + "x" * 1000] * 64},
            )
        await self.native_views(agent, ctx, task)

    async def test_real_mcp_http_discovery_auth_and_linked_reads_are_database_only(
        self,
    ):
        from mainloop.mcp_app import create_app
        from mainloop.runtime.agent_identity import token_for
        from mainloop.runtime.agent_tools import AgentService

        task, _, target, op, _, _, _ = await self.context()
        app = create_app(AgentService(PgStore(), []))
        headers = {
            "Authorization": f"Bearer {token_for(target.session_id)}",
            "Accept": "application/json, text/event-stream",
        }
        counts = len(self.fake.requests), await self.pool.fetchval(
            "SELECT count(*) FROM native_deliveries"
        )
        async with (
            app.app.router.lifespan_context(app.app),
            httpx.AsyncClient(
                transport=httpx.ASGITransport(app=app),
                base_url="http://testserver",
                headers=headers,
            ) as client,
        ):

            async def rpc(method, params=None):
                return await client.post(
                    "/mcp",
                    json={
                        "jsonrpc": "2.0",
                        "id": 1,
                        "method": method,
                        "params": params or {},
                    },
                )

            self.assertEqual((await rpc("tools/list")).status_code, 401)
            await self.activate(task, target)
            tools = await rpc("tools/list")
            self.assertEqual(tools.status_code, 200)
            schemas = {
                t["name"]: t["inputSchema"] for t in tools.json()["result"]["tools"]
            }
            self.assertTrue(
                {"whoami", "task_get", "task_history", "task_list"}.issubset(schemas)
            )
            self.assertEqual(set(schemas["task_get"]["properties"]), {"task_id"})
            identity = await rpc("tools/call", {"name": "whoami", "arguments": {}})
            self.assertEqual(
                identity.json()["result"]["structuredContent"]["task_id"], task.id
            )
            for name in ("task_get", "task_history", "task_list"):
                response = await rpc(
                    "tools/call",
                    {
                        "name": name,
                        "arguments": (
                            {} if name == "task_list" else {"task_id": task.id}
                        ),
                    },
                )
                result = response.json()["result"]
                self.assertFalse(result.get("isError", False), result)
                view = result["structuredContent"]
                view = view["tasks"][0] if name == "task_list" else view
                self.assertEqual(
                    {a["id"] for a in view["artifacts"]},
                    {op.manifest_ref, op.checkpoint_ref},
                )
            await revoke(target.session_id)
            self.assertEqual((await rpc("tools/list")).status_code, 401)
        self.assertEqual(
            counts,
            (
                len(self.fake.requests),
                await self.pool.fetchval("SELECT count(*) FROM native_deliveries"),
            ),
        )

    async def test_provider_note_comes_only_from_labelled_linked_manifest(self):
        task, _, target, op, _, _, _ = await self.context()
        await self.activate(task, target)
        agent, ctx = await self.client(target.session_id)
        async with self.pool.acquire() as conn:
            payload = (await store.get_artifact(conn, op.manifest_ref, self.owner))[
                "payload"
            ]
        await self.replace_artifact(
            op.manifest_ref, {**payload, "unverified_note": "Unverified provider claim"}
        )
        async with self.pool.acquire() as conn, conn.transaction():
            unused = await store.add_artifact(
                conn,
                op.id,
                "unverified_provider_summary",
                {"private": "private-reader-marker"},
            )
        await self.replace_artifact(unused, {}, checksum="0" * 64)
        for view in await self.native_views(agent, ctx, task):
            manifest = next(
                a["payload"]
                for a in view["artifacts"]
                if a["kind"] == "handoff_manifest"
            )
            self.assertEqual(manifest["unverified_note"], "Unverified provider claim")
            self.assertEqual(manifest["note_label"], "unverified provider note")
            self.assertNotIn("private-reader-marker", json.dumps(view))

    async def test_forged_operation_attempt_workspace_and_sql_current_links_fail_closed(
        self,
    ):
        task, source, target, op, _, _, _ = await self.context()
        target = await self.activate(task, target)
        agent, ctx = await self.client(target.session_id)
        for key, value in (
            ("target_attempt_id", source.id),
            ("source_attempt_id", target.id),
            ("manifest_ref", op.checkpoint_ref),
            ("checkpoint_ref", None),
            ("task_id", "forged"),
            ("owner_id", "forged"),
        ):
            with self.subTest(operation=key):
                async with self.pool.acquire() as conn, conn.transaction():
                    if key in ("task_id", "owner_id"):
                        await conn.execute(
                            "UPDATE task_operations SET snapshot=$2::jsonb WHERE id=$1",
                            op.id,
                            op.model_copy(update={key: value}).model_dump_json(),
                        )
                    else:
                        await store.save_operation(
                            conn, op.model_copy(update={key: value})
                        )
                await self.native_views(agent, ctx, task, denied=True)
                async with self.pool.acquire() as conn, conn.transaction():
                    await store.save_operation(conn, op)
        for updates in (
            {"manifest_ref": None},
            {"checkpoint_ref": None},
            {"predecessor_id": target.id},
            {"manifest_ref": op.checkpoint_ref, "checkpoint_ref": op.manifest_ref},
        ):
            with self.subTest(attempt=updates):
                async with self.pool.acquire() as conn, conn.transaction():
                    await lifecycle.save_attempt(
                        conn, target.model_copy(update=updates)
                    )
                await self.native_views(agent, ctx, task, denied=True)
        async with self.pool.acquire() as conn, conn.transaction():
            await lifecycle.save_attempt(conn, target)
        for field, select_query, update_query, value in (
            (
                "ref",
                "SELECT ref FROM workspaces WHERE session_id=$1",
                "UPDATE workspaces SET ref=$2 WHERE session_id=$1",
                "b" * 40,
            ),
            (
                "branch",
                "SELECT branch FROM workspaces WHERE session_id=$1",
                "UPDATE workspaces SET branch=$2 WHERE session_id=$1",
                "forged",
            ),
            (
                "repo",
                "SELECT repo FROM workspaces WHERE session_id=$1",
                "UPDATE workspaces SET repo=$2 WHERE session_id=$1",
                "https://github.com/other/repo",
            ),
            (
                "depth",
                "SELECT depth FROM workspaces WHERE session_id=$1",
                "UPDATE workspaces SET depth=$2 WHERE session_id=$1",
                7,
            ),
        ):
            with self.subTest(workspace=field):
                old = await self.pool.fetchval(select_query, target.id)
                await self.pool.execute(update_query, target.id, value)
                await self.native_views(agent, ctx, task, denied=True)
                await self.pool.execute(update_query, target.id, old)
        # A stale task snapshot cannot redirect an authoritative current-attempt read.
        await self.pool.execute(
            "UPDATE tasks SET snapshot=$2::jsonb WHERE id=$1",
            task.id,
            task.model_copy(update={"current_attempt_id": source.id}).model_dump_json(),
        )
        await self.native_views(agent, ctx, task, denied=True)
        await self.pool.execute(
            "UPDATE tasks SET snapshot=$2::jsonb WHERE id=$1",
            task.id,
            task.model_dump_json(),
        )
        await self.native_views(agent, ctx, task)

    async def test_selected_reports_require_digest_and_exact_predecessor_identity(self):
        task, source, target, op, selected, _, _ = await self.context()
        await self.activate(task, target)
        agent, ctx = await self.client(target.session_id)
        row = await self.pool.fetchrow(
            "SELECT * FROM task_reports WHERE task_id=$1 AND attempt_id=$2",
            task.id,
            source.id,
        )
        for updates, checksum in (
            ({"summary": "private-reader-marker"}, row["request_digest"]),
            ({"attempt_id": target.id}, None),
            ({"task_id": "forged"}, None),
            ({"request_id": "forged"}, None),
        ):
            with self.subTest(report=updates):
                value = selected.model_copy(update=updates)
                await self.pool.execute(
                    "UPDATE task_reports SET snapshot=$2::jsonb,request_digest=$3 WHERE id=$1",
                    row["id"],
                    value.model_dump_json(),
                    checksum or store.digest(value.model_dump(mode="json")),
                )
                await self.native_views(agent, ctx, task, denied=True)
        await self.pool.execute(
            "UPDATE task_reports SET snapshot=$2::jsonb,request_digest=$3 WHERE id=$1",
            row["id"],
            row["snapshot"],
            row["request_digest"],
        )
        for field, query, value in (
            (
                "attempt_id",
                "UPDATE task_reports SET attempt_id=$2 WHERE id=$1",
                target.id,
            ),
            (
                "request_id",
                "UPDATE task_reports SET request_id=$2 WHERE id=$1",
                "forged",
            ),
        ):
            await self.pool.execute(query, row["id"], value)
            await self.native_views(agent, ctx, task, denied=True)
            await self.pool.execute(query, row["id"], row[field])
        await self.native_views(agent, ctx, task)

    async def test_current_reports_remain_visible_and_require_current_identity_and_digest(
        self,
    ):
        from mainloop.tasks import reports

        task, _, target, _, predecessor_report, _, _ = await self.context()
        await self.activate(task, target)
        agent, ctx = await self.client(target.session_id)
        current_report = TaskReport(
            task_id=task.id,
            attempt_id=target.id,
            request_id=uuid.uuid4().hex,
            summary="Current attempt progress claim",
            outcome="progress",
        )
        async with self.pool.acquire() as conn, conn.transaction():
            result = await reports.record(
                conn, await self.principal(target), current_report
            )
        for view in await self.native_views(agent, ctx, task):
            self.assertEqual(
                view["reports"],
                [
                    predecessor_report.model_dump(mode="json"),
                    current_report.model_dump(mode="json"),
                ],
            )
        row = await self.pool.fetchrow(
            "SELECT * FROM task_reports WHERE id=$1", result["report_id"]
        )
        for updates, checksum in (
            ({"summary": "private-reader-marker"}, row["request_digest"]),
            ({"attempt_id": predecessor_report.attempt_id}, None),
            ({"task_id": "forged"}, None),
            ({"request_id": "forged"}, None),
        ):
            with self.subTest(current_report=updates):
                value = current_report.model_copy(update=updates)
                await self.pool.execute(
                    "UPDATE task_reports SET snapshot=$2::jsonb,request_digest=$3 WHERE id=$1",
                    row["id"],
                    value.model_dump_json(),
                    checksum or store.digest(value.model_dump(mode="json")),
                )
                await self.native_views(agent, ctx, task, denied=True)
        await self.pool.execute(
            "UPDATE task_reports SET snapshot=$2::jsonb,request_digest=$3 WHERE id=$1",
            row["id"],
            row["snapshot"],
            row["request_digest"],
        )
        await self.native_views(agent, ctx, task)

    async def test_supervisor_direct_child_scope_and_stale_parent_authority(self):
        from fastapi import HTTPException
        from mainloop.runtime.agent_identity import token_for

        _, parent, parent_attempt = await self.create_task(self.request())
        task, _, target, _, _, _, _ = await self.context(
            "claude", "claude", parent=await self.principal(parent_attempt)
        )
        await self.activate(task, target)
        parent_agent, parent_ctx = await self.client(parent_attempt.session_id)
        await self.native_views(parent_agent, parent_ctx, task)
        _, sibling, sibling_attempt = await self.create_task(
            self.request(), principal=await self.principal(parent_attempt)
        )
        child_agent, child_ctx = await self.client(target.session_id)
        from mainloop.mcp_app import invoke

        for forbidden in (parent.id, sibling.id):
            for name in ("task_get", "task_history"):
                self.assertTrue(
                    (
                        await invoke(
                            child_agent, child_ctx, name, {"task_id": forbidden}
                        )
                    ).isError
                )
        listing = await invoke(child_agent, child_ctx, "task_list", {})
        self.assertEqual(
            [v["task"]["id"] for v in listing.structuredContent["tasks"]], [task.id]
        )
        _, outside, _ = await self.create_task(self.request())
        for name in ("task_get", "task_history"):
            self.assertTrue(
                (
                    await invoke(
                        parent_agent, parent_ctx, name, {"task_id": outside.id}
                    )
                ).isError
            )
        async with self.pool.acquire() as conn, conn.transaction():
            foreign_id = uuid.uuid4().hex
            from models.task import Task

            foreign = Task.model_validate(
                {
                    **parent.model_dump(),
                    "id": foreign_id,
                    "root_task_id": foreign_id,
                    "owner_id": "other-owner",
                    "project_id": None,
                    "mode": "coordination",
                    "checkout": None,
                    "current_attempt_id": None,
                }
            )
            await store.insert_task(conn, foreign)
        for actor_agent, actor_ctx in (
            (parent_agent, parent_ctx),
            (child_agent, child_ctx),
        ):
            for name in ("task_get", "task_history"):
                self.assertTrue(
                    (
                        await invoke(
                            actor_agent, actor_ctx, name, {"task_id": foreign_id}
                        )
                    ).isError
                )
        async with self.pool.acquire() as conn, conn.transaction():
            await lifecycle.save_attempt(
                conn,
                parent_attempt.model_copy(
                    update={"writer_generation": parent_attempt.writer_generation + 1}
                ),
            )
        await self.native_views(child_agent, child_ctx, task, denied=True)
        async with self.pool.acquire() as conn, conn.transaction():
            await lifecycle.save_attempt(conn, parent_attempt)
        old = await PgStore().get_binding(parent_attempt.session_id)
        await self.pool.execute(
            "UPDATE native_bindings SET token_hash=NULL WHERE session_id=$1",
            parent_attempt.session_id,
        )
        await self.native_views(child_agent, child_ctx, task, denied=True)
        with self.assertRaises(HTTPException):
            await child_agent.authenticate(token_for(target.session_id))
        await self.pool.execute(
            "UPDATE native_bindings SET token_hash=$2 WHERE session_id=$1",
            parent_attempt.session_id,
            old["token_hash"],
        )
        await self.native_views(child_agent, child_ctx, task)

    async def test_stale_generation_revocation_and_cancellation_after_mcp_authentication(
        self,
    ):
        task, _, target, _, _, _, _ = await self.context()
        await self.activate(task, target)
        agent, ctx = await self.client(target.session_id)
        async with self.pool.acquire() as conn, conn.transaction():
            await lifecycle.save_attempt(
                conn,
                target.model_copy(
                    update={
                        "state": "active",
                        "writer_generation": target.writer_generation + 1,
                    }
                ),
            )
        await self.native_views(agent, ctx, task, denied=True)
        async with self.pool.acquire() as conn, conn.transaction():
            await lifecycle.save_attempt(
                conn, target.model_copy(update={"state": "active"})
            )
        await self.native_views(agent, ctx, task)
        await revoke(target.session_id)
        await self.native_views(agent, ctx, task, denied=True)
        # Cached context also loses access if the binding is terminal or the
        # attempt/current routing changes after authentication.
        await self.pool.execute(
            "UPDATE native_bindings SET token_hash=$2 WHERE session_id=$1",
            target.id,
            ctx.binding["token_hash"],
        )
        for state in ("creating", "draining", "superseded"):
            async with self.pool.acquire() as conn, conn.transaction():
                await lifecycle.save_attempt(
                    conn, target.model_copy(update={"state": state})
                )
            await self.native_views(agent, ctx, task, denied=True)
        async with self.pool.acquire() as conn, conn.transaction():
            await lifecycle.save_attempt(
                conn, target.model_copy(update={"state": "active"})
            )
        async with self.pool.acquire() as conn, conn.transaction():
            cancel = await service.mutate(
                conn,
                self.owner,
                "cancel",
                TaskAction(
                    request_id=uuid.uuid4().hex,
                    expected_version=task.version,
                    expected_attempt_id=target.id,
                ),
                task_id=task.id,
            )
        await self.worker.reconcile(db, cancel)
        await self.native_views(agent, ctx, task, denied=True)
        main_sid, _ = await self.bound_session(
            role="main", mcp_grant_kind="coordination"
        )
        main_agent, main_ctx = await self.client(main_sid)
        for view in await self.native_views(main_agent, main_ctx, task):
            self.assertIsNone(view["task"]["current_attempt_id"])
            self.assertEqual(view["artifacts"], [])
            self.assertEqual(view["reports"], [])

    async def test_inflight_read_holds_binding_and_current_task_until_transaction_finishes(
        self,
    ):
        import asyncpg

        task, _, target, _, _, _, _ = await self.context()
        await self.activate(task, target)
        _, ctx = await self.client(target.session_id)
        entered, release = asyncio.Event(), asyncio.Event()
        original = projection.read

        async def paused(conn, value):
            entered.set()
            await release.wait()
            return await original(conn, value)

        with patch.object(projection, "read", side_effect=paused):
            pending = asyncio.create_task(
                PgStore().task_call(ctx.binding, "task_get", {"task_id": task.id})
            )
            try:
                await asyncio.wait_for(entered.wait(), 5)
                for query in (
                    "UPDATE native_bindings SET token_hash=NULL WHERE session_id=$1",
                    "UPDATE tasks SET current_attempt_id=NULL WHERE id=$1",
                ):
                    async with self.pool.acquire() as conn:
                        with self.assertRaises(asyncpg.LockNotAvailableError):
                            async with conn.transaction():
                                await conn.execute("SET LOCAL lock_timeout='100ms'")
                                await conn.execute(
                                    query,
                                    (
                                        target.id
                                        if "native_bindings" in query
                                        else task.id
                                    ),
                                )
                release.set()
                value = await asyncio.wait_for(pending, 5)
                self.assertEqual(value["task"]["current_attempt_id"], target.id)
            finally:
                release.set()
                await asyncio.gather(pending, return_exceptions=True)
        await revoke(target.session_id)
        with self.assertRaises(store.TaskError):
            await PgStore().task_call(ctx.binding, "task_get", {"task_id": task.id})

    async def test_historical_link_pair_cannot_replace_current_continuation(self):
        from mainloop.tasks.handoff import Handoff

        task, _, first_target, old_op, _, _, external = await self.context()
        await Handoff(external.runtime, external.reader, live=False).reconcile(
            db, old_op
        )
        async with self.pool.acquire() as conn:
            task = await lifecycle.load_task(conn, task.id)
        coordinator, external = self.coordinator()
        async with self.pool.acquire() as conn, conn.transaction():
            operation = await service.mutate(
                conn,
                self.owner,
                "reassign",
                TaskReassign(
                    request_id=uuid.uuid4().hex,
                    expected_version=task.version,
                    expected_attempt_id=first_target.id,
                    target_profile_id="codex",
                ),
                task_id=task.id,
                installed_ports=service.TaskPorts(handoff=coordinator),
            )
        for _ in range(12):
            await coordinator.reconcile(db, operation)
            async with self.pool.acquire() as conn:
                operation = await store.handoff_operation(conn, operation.id)
            if operation.state == "target_ready":
                break
        self.assertEqual(operation.state, "target_ready")
        async with self.pool.acquire() as conn:
            task = await lifecycle.load_task(conn, task.id)
            target = await lifecycle.load_attempt(conn, task.current_attempt_id)
        target = await self.activate(task, target)
        agent, ctx = await self.client(target.session_id)
        for view in await self.native_views(agent, ctx, task):
            self.assertEqual(
                {a["id"] for a in view["artifacts"]},
                {operation.manifest_ref, operation.checkpoint_ref},
            )
            self.assertEqual(view["reports"], [])
        async with self.pool.acquire() as conn, conn.transaction():
            await lifecycle.save_attempt(
                conn,
                target.model_copy(
                    update={
                        "manifest_ref": old_op.manifest_ref,
                        "checkpoint_ref": old_op.checkpoint_ref,
                    }
                ),
            )
        await self.native_views(agent, ctx, task, denied=True)
