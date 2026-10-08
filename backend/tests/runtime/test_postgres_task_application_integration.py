"""Shared application seams on isolated PostgreSQL with fake GitHub and kagent."""

import asyncio
import json
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
from tests.runtime.test_merge import MergeFixture

from models.hitl import (
    HITL_EXTENSION,
    DecisionReceipt,
    ToolApproval,
    ToolApprovalResponse,
)


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
