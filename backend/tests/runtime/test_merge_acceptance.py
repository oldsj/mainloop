"""Integrated b+c acceptance: real observer/API/PG, synthetic A2A and fake GitHub."""

import asyncio
import json
import os
from datetime import datetime, timedelta, timezone
from pathlib import Path
from unittest.mock import AsyncMock, patch

import asyncpg
import httpx
from fastapi import BackgroundTasks
from mainloop import api
from mainloop.config import settings
from mainloop.db import db
from mainloop.db import hitl as store
from mainloop.runtime import hitl_continuation, native_sessions
from mainloop.runtime.hitl_correlation import task_identity
from mainloop.runtime.hitl_observer import HITLObserver
from mainloop.runtime.kagent_client import RuntimeState, Task
from mainloop.services import merge
from mainloop.services.github_repo import parse_github_repo
from tests.runtime import test_merge as support
from tests.runtime.test_hitl_observer import Gateway

from models.hitl import (
    HITL_EXTENSION,
    MERGE_OPERATION,
    HITLProjection,
    VerifiedAssociation,
)

FIXTURES = Path(__file__).parent / "fixtures" / "merge_policy"


class MergeAcceptanceTests(support.MergeFixture):
    async def projections(self):
        return [
            HITLProjection.model_validate(merge.decode(row["snapshot"]))
            for row in await self.pool.fetch(
                "SELECT snapshot FROM native_hitl_requests WHERE owner_id=$1", self.user
            )
        ]

    async def observe(self, observer):
        with (
            patch("mainloop.runtime.hitl_observer.observer", return_value=observer),
            patch("mainloop.runtime.hitl_continuation.observer", return_value=observer),
        ):
            await native_sessions.reconcile_once(sweep=False)

    def observer(self, gateway):
        return HITLObserver(
            gateway,
            gateway=f"https://gateway/{self.user}",
            owner=self.user,
            creator="gateway-owner",
        )

    async def inventory(self, proposal, provider="claude", *, duplicate_proposal=False):
        await self.pool.execute(
            "UPDATE native_bindings SET kind=$2 WHERE session_id=$1", self.sid, provider
        )
        gateway = Gateway()
        session, _ = gateway.add(self.binding["kagent_session_id"])
        wire = json.loads((FIXTURES / f"{provider}.json").read_text())
        wire["id"] = f"task-{session.id}"
        wire["contextId"] = session.context_id
        message = wire["status"]["message"]
        message.update(taskId=wire["id"], contextId=session.context_id)
        tools = message["metadata"][HITL_EXTENSION]["tools"]
        tools[0]["args"]["proposal_id"] = proposal["proposal_id"]
        if duplicate_proposal:
            duplicate = json.loads(json.dumps(tools[0]))
            duplicate["id"] = "call-2"
            duplicate["call_id"] = "native-call-2"
            duplicate["args"]["request_id"] = "invoke-2"
            tools.append(duplicate)
        task = Task.model_validate(wire)
        gateway.tasks[task.id] = task
        self.assertEqual(session.state, RuntimeState.SUSPENDED)
        self.assertEqual(task.status.state, "input-required")
        config = {
            "template_name": "merge-template",
            "provider": provider,
            "compiled_alias": "mainloop-merge-approval",
            "endpoint": "http://mainloop-mcp.mainloop.svc.cluster.local/mcp/merge-approval",
            "tool": "merge_pull_request_with_approval",
            "require_approval": True,
            "operation": "mainloop.merge_pull_request_with_approval.v1",
        }
        p = patch.dict(os.environ, MAINLOOP_MERGE_CONFIGURATIONS=json.dumps([config]))
        p.start()
        self.addCleanup(p.stop)
        self.assertEqual(
            await self.pool.fetchval("SELECT count(*) FROM native_deliveries"), 0
        )
        self.assertEqual(await self.projections(), [])
        observer = self.observer(gateway)
        await self.observe(observer)
        return gateway, (await self.projections())[0]

    async def http(self, gateway, method, path, body=None):
        with (
            patch.object(settings, "api_hosts", "localhost"),
            patch.object(settings, "owner_id", self.user),
            patch.dict(os.environ, MAINLOOP_OWNER_HITL_WRITES_ENABLED="true"),
            patch(
                "mainloop.runtime.hitl_continuation.observer",
                return_value=self.observer(gateway),
            ),
        ):
            async with httpx.AsyncClient(
                transport=httpx.ASGITransport(app=api.app), base_url="http://localhost"
            ) as client:
                return await client.request(method, path, json=body)

    async def answer(self, gateway, projection, *, approved=True, action="desktop"):
        reviewed_context = {}
        if approved:
            tools = list(projection.payload.tools)
            if projection.payload.nested:
                tools.extend(projection.payload.nested.tools)
            tool = next(
                (
                    value
                    for value in tools
                    if value.name.endswith("merge_pull_request_with_approval")
                ),
                None,
            )
            if tool is not None:
                digest = await self.pool.fetchval(
                    "SELECT summary_digest FROM merge_proposals WHERE id=$1",
                    tool.args["proposal_id"],
                )
                reviewed_context = {tool.id: digest}
        return await self.http(
            gateway,
            "POST",
            f"/hitl/{projection.id}/respond",
            {
                "action_id": action,
                "response": {
                    "type": "tool_approval_response",
                    "approvals": [
                        {
                            "id": "call",
                            "approved": approved,
                            **(
                                {}
                                if approved
                                else {"rejection_reason": "Keep the protected tests."}
                            ),
                        }
                    ],
                    "reviewed_context": reviewed_context,
                },
            },
        )

    async def assert_receipt(
        self, response, projection, proposal, invocation="invoke-1"
    ):
        self.assertEqual(response.status_code, 200, response.text)
        receipt = response.json()["response"]
        self.assertEqual(receipt["outer"], projection.outer.model_dump(mode="json"))
        call = receipt["calls"][0]
        self.assertEqual(call["leaf"], projection.leaves[0].model_dump(mode="json"))
        self.assertEqual(call["mapping"]["operation"], MERGE_OPERATION)
        self.assertEqual(
            call["merge_key"],
            {
                "owner_id": self.user,
                "leaf_binding_id": self.sid,
                "leaf_runtime_session_id": self.binding["kagent_session_id"],
                "operation": MERGE_OPERATION,
                "proposal_id": proposal["proposal_id"],
                "invocation_request_id": invocation,
            },
        )
        facts = merge.decode(
            await self.pool.fetchval(
                "SELECT facts FROM merge_proposals WHERE id=$1", proposal["proposal_id"]
            )
        )
        self.assertEqual(
            (facts["head_sha"], facts["base_sha"], facts["pr_number"]),
            (support.SHA, support.BASE, 17),
        )
        return receipt

    async def approved_flow(self, provider, *, nested=False):
        # This class owns an isolated scratch database. Backdate INSERT defaults
        # rather than bypassing the immutable-proposal trigger or mocking an
        # unused application clock. Preparation still uses the real service.
        await self.pool.execute(
            "ALTER TABLE merge_proposals ALTER COLUMN created_at SET DEFAULT (now() - interval '16 hours')"
        )
        await self.pool.execute(
            "UPDATE native_bindings SET kind=$2 WHERE session_id=$1",
            self.sid,
            provider,
        )
        try:
            p = await self.prepare()
        finally:
            await self.pool.execute(
                "ALTER TABLE merge_proposals ALTER COLUMN created_at SET DEFAULT now()"
            )
        gateway, direct = await self.inventory(p, provider)
        projection = direct
        if nested:
            leaf_task = gateway.tasks[direct.outer.task_id]
            _, outer_task = gateway.add(
                "outer-parent",
                {
                    "type": "tool_approval_request",
                    "tools": [
                        {
                            "id": "parent",
                            "call_id": "parent-native",
                            "name": "delegate",
                            "args": {},
                        }
                    ],
                    "nested": {
                        "subagent_name": "untrusted-label",
                        "task_id": leaf_task.id,
                        "context_id": leaf_task.context_id,
                        "tools": leaf_task.status.message.metadata[HITL_EXTENSION][
                            "tools"
                        ],
                    },
                },
            )
            await self.pool.execute(
                "UPDATE native_hitl_inventory_state SET next_sweep=now() WHERE owner_id=$1",
                self.user,
            )
            await self.observe(self.observer(gateway))
            projection = next(
                p for p in await self.projections() if p.outer.task_id == outer_task.id
            )
            unavailable = (
                await self.http(gateway, "GET", f"/hitl/{projection.id}")
            ).json()
            self.assertFalse(unavailable["answerable"])
            self.assertTrue(unavailable["unavailable_reason"])
            self.assertEqual((await self.answer(gateway, projection)).status_code, 409)
            self.assertEqual(gateway.sent, [])
            async with self.pool.acquire() as conn:
                await store.save_association(
                    conn,
                    VerifiedAssociation(
                        owner_id=self.user,
                        outer=task_identity(projection.outer),
                        leaf=task_identity(direct.outer),
                        evidence_source="gateway_continuation",
                        evidence_reference="fixture://verified-outer-leaf",
                    ),
                )
        # Represent the persisted unanswered observation at the start of the
        # overnight pause, then let real recovery refresh it at database time.
        await self.pool.execute(
            "UPDATE native_hitl_requests SET observed_at=now()-interval '16 hours' WHERE owner_id=$1",
            self.user,
        )
        await self.pool.execute(
            "UPDATE native_hitl_inventory_state SET next_sweep=now()-interval '16 hours' WHERE owner_id=$1",
            self.user,
        )
        ages = await self.pool.fetchrow(
            "SELECT now()-p.created_at AS proposal_age, now()-h.observed_at AS request_age FROM merge_proposals p CROSS JOIN native_hitl_requests h WHERE p.id=$1 AND h.id=$2",
            p["proposal_id"],
            projection.id,
        )
        self.assertGreaterEqual(ages["proposal_age"], timedelta(hours=16))
        self.assertGreaterEqual(ages["request_age"], timedelta(hours=16))
        before = await self.pool.fetchrow(
            "SELECT state,deadline,intent_id,receipt_action_id FROM merge_requests WHERE owner_id=$1",
            self.user,
        )
        self.assertEqual(
            dict(before),
            {
                "state": "prepared",
                "deadline": None,
                "intent_id": None,
                "receipt_action_id": None,
            },
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_hitl_responses WHERE owner_id=$1",
                self.user,
            ),
            0,
        )
        self.assertEqual(gateway.sent, [])
        self.assertEqual(self.fake.puts, [])
        original_outer = projection.outer
        original_leaf = direct.leaves[0]
        # Recreate the observer against suspended gateway snapshots and persisted state.
        await self.observe(self.observer(gateway))
        await self.observe(self.observer(gateway))
        projection = next(p for p in await self.projections() if p.id == projection.id)
        self.assertEqual(projection.outer, original_outer)
        self.assertEqual(projection.leaves[0], original_leaf)
        self.assertGreaterEqual(
            await self.pool.fetchval(
                "SELECT now()-created_at FROM merge_proposals WHERE id=$1",
                p["proposal_id"],
            ),
            timedelta(hours=16),
        )
        inbox = await self.http(gateway, "GET", "/queue")
        self.assertEqual(inbox.status_code, 200)
        self.assertEqual(
            sum(
                x["item_type"] == "hitl_request" and x["status"] == "pending"
                for x in inbox.json()
            ),
            1,
        )
        view = (await self.http(gateway, "GET", f"/hitl/{projection.id}")).json()
        self.assertEqual(
            view["merge"][0]["tool_id"], projection.leaves[0].pending_request_id
        )
        self.assertEqual(view["merge"][0]["proposal_id"], p["proposal_id"])
        self.assertEqual(view["merge"][0]["head_sha"], support.SHA)
        self.assertEqual(
            view["merge"][0]["summary"]["title"], support.GitHub().pr["title"]
        )
        self.assertEqual(
            view["merge"][0]["summary_digest"],
            await self.pool.fetchval(
                "SELECT summary_digest FROM merge_proposals WHERE id=$1",
                p["proposal_id"],
            ),
        )
        with patch.dict(os.environ, MAINLOOP_MERGE_CONFIGURATIONS="[]"):
            unavailable = (
                await self.http(gateway, "GET", f"/hitl/{projection.id}")
            ).json()
        self.assertEqual(unavailable["merge"][0]["availability"], "stale")
        self.assertIn("template mapping", unavailable["merge"][0]["freshness_reason"])
        self.assertNotIn("merge_enrichment", unavailable)
        if provider == "codex" and not nested:
            if target := os.environ.get("MERGE_UI_FIXTURE_PATH"):
                Path(target).write_text(json.dumps(view))
        result = await self.answer(gateway, projection)
        await self.assert_receipt(result, projection, p)
        self.assertIsNone(
            await self.pool.fetchval(
                "SELECT deadline FROM merge_requests WHERE owner_id=$1", self.user
            )
        )
        self.assertEqual(
            (gateway.sent[0][1]["task_id"], gateway.sent[0][1]["context_id"]),
            (projection.outer.task_id, projection.outer.context_id),
        )

        if nested:
            self.assertNotEqual(
                projection.outer.runtime_session_id,
                projection.leaves[0].runtime_session_id,
            )
            self.assertEqual(
                (await self.answer(gateway, direct, action="mobile")).status_code, 409
            )
        # Keep the first real evaluation pending so its fresh PostgreSQL window
        # can be measured after the aged request is answered, before any PUT.
        self.fake.runs[0].update(status="queued", conclusion=None)
        evaluation_started = await self.pool.fetchval("SELECT now()")
        async with asyncio.timeout(2):
            pending = await self.execute(p, approved=True)
        self.assertEqual(pending["state"], "evaluating")
        self.assertIn("Do not call", pending["text"])
        deadline = datetime.fromisoformat(pending["deadline"])
        evaluation_observed = await self.pool.fetchval("SELECT now()")
        self.assertGreaterEqual(deadline, evaluation_started + timedelta(minutes=30))
        self.assertLessEqual(deadline, evaluation_observed + timedelta(minutes=30))
        self.assertEqual(self.fake.puts, [])
        self.fake.runs[0].update(status="completed", conclusion="success")
        await self.observe(self.observer(gateway))
        self.assertEqual((await self.execute(p, approved=True))["state"], "merged")
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT deadline FROM merge_requests WHERE owner_id=$1",
                self.user,
            ),
            deadline,
        )
        self.assertEqual(len(gateway.sent), 1)
        self.assertEqual(len(self.fake.puts), 1)
        candidate = await self.pool.fetchrow(
            "SELECT r.* FROM merge_requests r JOIN merge_proposals p ON p.candidate_id=r.id WHERE p.id=$1",
            p["proposal_id"],
        )
        self.assertEqual(candidate["active_proposal_id"], p["proposal_id"])
        self.assertEqual(candidate["head_sha"], support.SHA)
        self.assertEqual(candidate["repository_id"], self.fake.repo["id"])
        self.assertEqual(candidate["pr_number"], 17)
        self.assertEqual(candidate["receipt_action_id"], "desktop")
        self.assertEqual(candidate["intent_invocation_id"], "invoke-1")
        self.assertIsNotNone(candidate["intent_id"])
        self.assertEqual(candidate["state"], "merged")
        self.assertEqual(
            await self.pool.fetchval("SELECT count(*) FROM native_deliveries"), 0
        )

    async def test_approved_runless_pending_suite_settles_without_another_approval(
        self,
    ):
        self.fake.files[0]["filename"] = "k8s/app.yaml"
        self.fake.suites.append(
            support.abandoned_suite(created_at=datetime.now(timezone.utc).isoformat())
        )
        p = await self.prepare()
        gateway, projection = await self.inventory(p)
        await self.assert_receipt(await self.answer(gateway, projection), projection, p)

        async with asyncio.timeout(2):
            pending = await self.execute(p, approved=True)
        self.assertEqual(pending["state"], "evaluating")
        deadline = datetime.fromisoformat(pending["deadline"])
        async with self.pool.acquire() as conn, conn.transaction():
            self.assertTrue(
                await conn.fetchval(
                    "SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))",
                    f"merge:{self.user}:123:17",
                )
            )
        with self.assertRaisesRegex(support.PolicyError, "exact owner decision"):
            await self.execute(p, request="new-invocation", approved=True)
        calls = len(self.fake.calls)
        self.assertEqual((await self.execute(p, approved=True))["state"], "evaluating")
        self.assertEqual(len(self.fake.calls), calls)
        view = (await self.http(gateway, "GET", f"/hitl/{projection.id}")).json()
        self.assertEqual(view["merge"][0]["availability"], "stale")
        self.assertIn("evaluating CI", view["merge"][0]["freshness_reason"])
        self.assertEqual(view["merge"][0]["deadline"], deadline.isoformat())
        self.fake.suites[-1].update(status="completed", conclusion="success")
        await merge.reconcile_approved_merges()
        self.assertEqual((await self.execute(p, approved=True))["state"], "merged")
        self.assertEqual(len(self.fake.puts), 1)
        self.assertEqual(len(gateway.sent), 1)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_hitl_responses WHERE owner_id=$1",
                self.user,
            ),
            1,
        )

    async def test_approved_pending_ci_expires_without_a_merge(self):
        self.fake.files[0]["filename"] = "k8s/app.yaml"
        p = await self.prepare()
        gateway, projection = await self.inventory(p)
        await self.assert_receipt(await self.answer(gateway, projection), projection, p)
        self.fake.runs[0].update(status="queued", conclusion=None)

        self.assertEqual((await self.execute(p, approved=True))["state"], "evaluating")
        await self.pool.execute(
            "UPDATE merge_requests SET deadline=now()-interval '1 second' WHERE owner_id=$1",
            self.user,
        )
        self.fake.runs[0].update(status="completed", conclusion="success")
        await merge.reconcile_approved_merges()
        self.assertEqual(self.fake.puts, [])
        self.assertEqual((await self.execute(p, approved=True))["state"], "expired")

    async def test_approved_pending_ci_failure_blocks_even_with_unknown_mergeability(
        self,
    ):
        self.fake.files[0]["filename"] = "k8s/app.yaml"
        p = await self.prepare()
        gateway, projection = await self.inventory(p)
        await self.assert_receipt(await self.answer(gateway, projection), projection, p)
        self.fake.runs[0].update(status="queued", conclusion=None)

        self.assertEqual((await self.execute(p, approved=True))["state"], "evaluating")
        self.fake.runs[0].update(status="completed", conclusion="failure")
        self.fake.pr["mergeable"] = None
        result = await merge.reconcile_approved_merges()
        self.assertEqual(result["state"], "blocked")
        self.assertEqual(self.fake.puts, [])

    async def test_restart_mid_evaluation_resumes_only_with_exact_consent(self):
        self.fake.files[0]["filename"] = "k8s/app.yaml"
        p = await self.prepare()
        gateway, projection = await self.inventory(p)
        await self.assert_receipt(await self.answer(gateway, projection), projection, p)
        self.fake.runs[0].update(status="queued", conclusion=None)
        self.assertEqual((await self.execute(p, approved=True))["state"], "evaluating")
        # Restart the actual database facade: there is no in-memory merge worker.
        await self.pool.close()
        self.pool = await asyncpg.create_pool(self.url, min_size=1, max_size=6)
        db._pool = self.pool
        candidate = await self.pool.fetchrow(
            "SELECT state,deadline,intent_id FROM merge_requests WHERE owner_id=$1",
            self.user,
        )
        self.assertEqual(candidate["state"], "evaluating")
        self.assertIsNone(candidate["intent_id"])
        with self.assertRaisesRegex(support.PolicyError, "exact owner decision"):
            await self.execute(p, request="new-invocation", approved=True)
        self.fake.runs[0].update(status="completed", conclusion="success")
        await self.observe(self.observer(gateway))
        self.assertEqual((await self.execute(p, approved=True))["state"], "merged")
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT deadline FROM merge_requests WHERE owner_id=$1", self.user
            ),
            candidate["deadline"],
        )
        self.assertEqual(len(self.fake.puts), 1)

    async def test_reconciler_recovers_pre_repair_evaluation_from_exact_receipt(self):
        self.fake.files[0]["filename"] = "k8s/app.yaml"
        p = await self.prepare()
        gateway, projection = await self.inventory(p)
        await self.assert_receipt(await self.answer(gateway, projection), projection, p)
        self.fake.runs[0].update(status="queued", conclusion=None)
        pending = await self.execute(p, approved=True)
        # The former service only populated these references at intent claim.
        await self.pool.execute(
            "UPDATE merge_requests SET receipt_action_id=NULL,intent_invocation_id=NULL WHERE owner_id=$1",
            self.user,
        )
        self.fake.runs[0].update(status="completed", conclusion="success")
        result = await merge.reconcile_approved_merges()
        self.assertEqual(result["state"], "merged")
        self.assertEqual((await self.execute(p, approved=True))["state"], "merged")
        self.assertEqual(
            (
                await self.pool.fetchval(
                    "SELECT deadline FROM merge_requests WHERE owner_id=$1", self.user
                )
            ).isoformat(),
            pending["deadline"],
        )
        self.assertEqual(len(self.fake.puts), 1)

    async def test_auto_evaluation_without_receipt_is_not_continued(self):
        p = await self.prepare()
        self.fake.runs[0].update(status="queued", conclusion=None)
        self.assertEqual((await self.execute(p))["state"], "evaluating")
        self.fake.runs[0].update(status="completed", conclusion="success")
        self.assertIsNone(await merge.reconcile_approved_merges())
        self.assertEqual(self.fake.puts, [])
        self.assertEqual((await self.execute(p))["state"], "merged")

    async def test_reconciler_rechecks_policy_before_merging(self):
        self.fake.files[0]["filename"] = "k8s/app.yaml"
        p = await self.prepare()
        gateway, projection = await self.inventory(p)
        await self.assert_receipt(await self.answer(gateway, projection), projection, p)
        self.fake.runs[0].update(status="queued", conclusion=None)
        await self.execute(p, approved=True)
        await self.pool.execute(
            "UPDATE projects SET merge_policy_version=merge_policy_version+1 WHERE id=$1",
            self.project.id,
        )
        self.fake.runs[0].update(status="completed", conclusion="success")
        self.assertEqual((await merge.reconcile_approved_merges())["state"], "blocked")
        self.assertEqual(self.fake.puts, [])

    async def test_reconciler_refuses_replacement_runtime(self):
        self.fake.files[0]["filename"] = "k8s/app.yaml"
        p = await self.prepare()
        gateway, projection = await self.inventory(p)
        await self.assert_receipt(await self.answer(gateway, projection), projection, p)
        self.fake.runs[0].update(status="queued", conclusion=None)
        await self.execute(p, approved=True)
        await self.pool.execute(
            "UPDATE native_bindings SET kagent_session_id='replacement' WHERE session_id=$1",
            self.sid,
        )
        self.fake.runs[0].update(status="completed", conclusion="success")
        self.assertEqual((await merge.reconcile_approved_merges())["state"], "blocked")
        self.assertEqual(self.fake.puts, [])

    async def test_bounded_evidence_timeout_retains_durable_consent(self):
        self.fake.files[0]["filename"] = "k8s/app.yaml"
        p = await self.prepare()
        gateway, projection = await self.inventory(p)
        await self.assert_receipt(await self.answer(gateway, projection), projection, p)
        entered = asyncio.Event()

        async def stalled_read(req):
            if req.url.path.endswith("/check-runs"):
                entered.set()
                await asyncio.Event().wait()

        self.fake.hook = stalled_read
        with patch.object(merge, "EVALUATION_BUDGET_SECONDS", 0.1):
            async with asyncio.timeout(2):
                result = await self.execute(p, approved=True)
        self.assertTrue(entered.is_set())
        self.assertEqual(result["state"], "evaluating")
        self.assertEqual(self.fake.puts, [])
        self.fake.hook = None
        self.assertEqual((await merge.reconcile_approved_merges())["state"], "merged")
        self.assertEqual(len(self.fake.puts), 1)

    async def test_retry_racing_continuation_never_repeats_put(self):
        self.fake.files[0]["filename"] = "k8s/app.yaml"
        p = await self.prepare()
        gateway, projection = await self.inventory(p)
        await self.assert_receipt(await self.answer(gateway, projection), projection, p)
        self.fake.runs[0].update(status="queued", conclusion=None)
        self.assertEqual((await self.execute(p, approved=True))["state"], "evaluating")
        self.fake.runs[0].update(status="completed", conclusion="success")
        claimed, release = asyncio.Event(), asyncio.Event()

        async def hold_put(req):
            if req.method == "PUT":
                claimed.set()
                await release.wait()

        self.fake.hook = hold_put
        continuation = asyncio.create_task(merge.reconcile_approved_merges())
        try:
            async with asyncio.timeout(2):
                await claimed.wait()
                retry = await self.execute(p, approved=True)
            self.assertEqual(retry["state"], "uncertain")
            self.assertEqual(len(self.fake.puts), 1)
        finally:
            release.set()
            result = await continuation
        self.assertEqual(result["state"], "merged")
        await merge.reconcile_approved_merges()
        self.assertEqual((await self.execute(p, approved=True))["state"], "merged")
        self.assertEqual(len(self.fake.puts), 1)

    async def test_cancelled_put_recovers_read_only_after_restart(self):
        self.fake.files[0]["filename"] = "k8s/app.yaml"
        p = await self.prepare()
        gateway, projection = await self.inventory(p)
        await self.assert_receipt(await self.answer(gateway, projection), projection, p)
        self.fake.runs[0].update(status="queued", conclusion=None)
        await self.execute(p, approved=True)
        self.fake.runs[0].update(status="completed", conclusion="success")

        async def lose_after_dispatch(req):
            if req.method == "PUT":
                self.fake.pr.update(
                    merged=True, state="closed", merge_commit_sha=support.MERGED
                )
                raise asyncio.CancelledError

        self.fake.hook = lose_after_dispatch
        with self.assertRaises(asyncio.CancelledError):
            await merge.reconcile_approved_merges()
        self.assertEqual(len(self.fake.puts), 1)
        self.fake.hook = None
        await self.pool.close()
        self.pool = await asyncpg.create_pool(self.url, min_size=1, max_size=6)
        db._pool = self.pool
        self.assertEqual((await merge.reconcile_approved_merges())["state"], "merged")
        self.assertEqual(len(self.fake.puts), 1)

    async def test_card_and_respond_agree_on_candidate_state_and_deadline(self):
        self.fake.files[0]["filename"] = "k8s/app.yaml"
        p = await self.prepare()
        gateway, projection = await self.inventory(p)
        for state, expired in (
            ("evaluating", False),
            ("evaluating", True),
            ("prepared", True),
            ("blocked", False),
            ("expired", False),
            ("superseded", False),
            ("merging", False),
            ("uncertain", False),
            ("merged", False),
        ):
            with self.subTest(state=state, expired=expired):
                await self.pool.execute(
                    "UPDATE merge_requests SET state=$2,deadline=$3 WHERE owner_id=$1",
                    self.user,
                    state,
                    datetime.now(timezone.utc)
                    + timedelta(seconds=-1 if expired else 60),
                )
                view = (
                    await self.http(gateway, "GET", f"/hitl/{projection.id}")
                ).json()
                self.assertEqual(view["merge"][0]["availability"], "stale")
                self.assertTrue(view["merge"][0]["stale"])
                self.assertIn("fresh operation", view["merge"][0]["freshness_reason"])
                response = await self.answer(gateway, projection)
                self.assertEqual(response.status_code, 409, response.text)
                self.assertIn(
                    (
                        "expired"
                        if expired
                        else (
                            "intent"
                            if state in ("merging", "uncertain", "merged")
                            else state
                        )
                    ),
                    response.json()["detail"],
                )
        self.assertEqual(gateway.sent, [])
        self.assertEqual(self.fake.puts, [])
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_hitl_responses WHERE owner_id=$1",
                self.user,
            ),
            0,
        )

    async def test_respond_returns_recorded_receipt_before_dispatch_or_remote_enrichment(
        self,
    ):
        self.fake.files[0]["filename"] = "k8s/app.yaml"
        p = await self.prepare()
        gateway, projection = await self.inventory(p)
        tasks = BackgroundTasks()
        decision = api.HITLDecisionInput.model_validate(
            {
                "action_id": "desktop",
                "response": {
                    "type": "tool_approval_response",
                    "approvals": [{"id": "call", "approved": True}],
                    "reviewed_context": {"call": p["summary_digest"]},
                },
            }
        )
        with (
            patch.dict(os.environ, MAINLOOP_OWNER_HITL_WRITES_ENABLED="true"),
            patch.object(
                hitl_continuation, "observer", return_value=self.observer(gateway)
            ),
            patch(
                "mainloop.services.merge_authorization.enrichment",
                new=AsyncMock(
                    side_effect=AssertionError(
                        "remote enrichment must not delay receipt"
                    )
                ),
            ),
        ):
            result = await api.respond_to_hitl(
                projection.id, decision, tasks, self.user
            )
        self.assertEqual(result["transport_state"], "recorded")
        self.assertEqual(result["response"]["action_id"], "desktop")
        self.assertEqual(gateway.sent, [])
        self.assertEqual(len(tasks.tasks), 1)
        # Dropping the process-local background callback does not lose delivery.
        with patch.object(
            hitl_continuation, "observer", return_value=self.observer(gateway)
        ):
            await hitl_continuation.reconcile_hitl_responses()
        self.assertEqual(len(gateway.sent), 1)
        await tasks()
        self.assertEqual(len(gateway.sent), 1)

    async def test_summary_details_are_owner_and_proposal_scoped_and_bounded(self):
        proposal = await self.prepare()
        gateway, projection = await self.inventory(proposal)
        summary = (await self.http(gateway, "GET", f"/hitl/{projection.id}")).json()[
            "merge"
        ][0]
        self.assertEqual(summary["availability"], "ready")
        self.assertTrue(
            summary["pr_url"].startswith("https://github.com/owner/repo/pull/17")
        )
        self.assertTrue(
            summary["compare_url"].startswith("https://github.com/owner/repo/compare/")
        )

        for section in ("description", "files", "checks"):
            response = await self.http(
                gateway,
                "GET",
                f"/hitl/{projection.id}/merge/{proposal['proposal_id']}/details?section={section}&limit=1",
            )
            self.assertEqual(response.status_code, 200, response.text)
            self.assertLessEqual(len(response.content), 64 * 1024)
            self.assertEqual(
                response.json()["summary_digest"], proposal["summary_digest"]
            )
        description = (
            await self.http(
                gateway,
                "GET",
                f"/hitl/{projection.id}/merge/{proposal['proposal_id']}/details?section=description",
            )
        ).json()
        self.assertIn("workspace deployments", description["description"])
        files = (
            await self.http(
                gateway,
                "GET",
                f"/hitl/{projection.id}/merge/{proposal['proposal_id']}/details?section=files",
            )
        ).json()
        self.assertEqual(files["items"][0]["filename"], "src/app.py")
        denied = await self.http(
            gateway,
            "GET",
            f"/hitl/{projection.id}/merge/another-proposal/details?section=files",
        )
        self.assertEqual(denied.status_code, 404)

    async def test_summary_details_accept_two_calls_for_one_proposal(self):
        proposal = await self.prepare()
        gateway, projection = await self.inventory(proposal, duplicate_proposal=True)
        merge_cards = (
            await self.http(gateway, "GET", f"/hitl/{projection.id}")
        ).json()["merge"]
        self.assertEqual(len(merge_cards), 2)
        self.assertEqual({item["tool_id"] for item in merge_cards}, {"call", "call-2"})
        self.assertEqual(
            {item["proposal_id"] for item in merge_cards}, {proposal["proposal_id"]}
        )

        for card in merge_cards:
            for section in ("description", "files", "checks"):
                response = await self.http(
                    gateway,
                    "GET",
                    f"/hitl/{projection.id}/merge/{card['proposal_id']}/details?section={section}",
                )
                self.assertEqual(response.status_code, 200, response.text)
                details = response.json()
                self.assertEqual(details["section"], section)
                self.assertEqual(details["proposal_id"], proposal["proposal_id"])
                self.assertEqual(details["summary_digest"], proposal["summary_digest"])
                if section == "description":
                    self.assertIn("workspace deployments", details["description"])
                else:
                    self.assertTrue(details["items"])

    async def test_claude_explicit_approval_inventory_restart_overnight(self):
        await self.pool.execute(
            "UPDATE projects SET merge_policy='approval',merge_policy_version=2 WHERE id=$1",
            self.project.id,
        )
        await self.approved_flow("claude")

    async def test_codex_protected_rename_inventory_restart(self):
        self.fake.files = [
            {
                "filename": "src/moved.py",
                "previous_filename": "k8s/protected.yaml",
                "status": "renamed",
                "additions": 3,
                "deletions": 1,
            }
        ]
        await self.approved_flow("codex")

    async def test_claude_protected_delete_nested_duplicate_observation(self):
        self.fake.files = [
            {
                "filename": "migrations/001.sql",
                "status": "removed",
                "additions": 3,
                "deletions": 1,
            }
        ]
        await self.approved_flow("claude", nested=True)

    async def test_codex_nested_duplicate_observation(self):
        self.fake.files = [
            {
                "filename": "k8s/protected.yaml",
                "status": "removed",
                "additions": 3,
                "deletions": 1,
            }
        ]
        await self.approved_flow("codex", nested=True)

    async def test_denial_reason_and_simultaneous_decisions(self):
        self.fake.files[0]["filename"] = "k8s/app.yaml"
        p = await self.prepare()
        gateway, projection = await self.inventory(p, "codex")
        results = await asyncio.gather(
            *(
                self.answer(gateway, projection, approved=False, action=a)
                for a in ("desktop", "mobile")
            )
        )
        self.assertEqual(sorted(r.status_code for r in results), [200, 409])
        result = next(r for r in results if r.status_code == 200)
        await self.assert_receipt(result, projection, p)
        self.assertEqual(
            result.json()["response"]["response"]["approvals"][0]["rejection_reason"],
            "Keep the protected tests.",
        )
        self.assertEqual(len(gateway.sent), 1)
        with self.assertRaises(support.PolicyError):
            await self.execute(p, approved=True)
        self.assertEqual(self.fake.puts, [])

    async def default_auto(self, name):
        self.fake.repo["full_name"] = f"owner/{name}"
        self.project = await db.get_or_create_project(
            self.user, parse_github_repo(f"owner/{name}")
        )
        self.args["project_id"] = self.project.id
        self.assertEqual(self.project.merge_policy, "auto")
        result = await merge.auto_merge(self.binding, self.args)
        self.assertEqual(result["state"], "merged")
        self.assertEqual(await self.projections(), [])
        self.assertEqual(len(self.fake.puts), 1)

    async def test_default_testrepo_auto(self):
        await self.default_auto("testrepo")

    async def test_default_mainloop_auto(self):
        await self.default_auto("mainloop")

    async def test_stale_proposal_cannot_answer_or_merge(self):
        self.fake.files[0]["filename"] = "k8s/app.yaml"
        p = await self.prepare()
        gateway, projection = await self.inventory(p)
        newer = await self.prepare(request_id="replacement")
        self.assertNotEqual(p["proposal_id"], newer["proposal_id"])
        view = (await self.http(gateway, "GET", f"/hitl/{projection.id}")).json()
        self.assertEqual(view["merge"][0]["availability"], "stale")
        self.assertTrue(view["merge"][0]["stale"])
        self.assertEqual((await self.answer(gateway, projection)).status_code, 409)
        self.assertEqual(gateway.sent, [])
        self.assertEqual(self.fake.puts, [])

    async def test_approved_same_head_retry_needs_fresh_native_decision(self):
        self.fake.files[0]["filename"] = "k8s/app.yaml"
        p = await self.prepare()
        gateway, projection = await self.inventory(p)
        await self.assert_receipt(await self.answer(gateway, projection), projection, p)
        self.fake.runs[0]["conclusion"] = "failure"
        self.assertEqual((await self.execute(p, approved=True))["state"], "blocked")
        self.fake.runs[0]["conclusion"] = "success"
        newer = await self.prepare(request_id="retry")
        with self.assertRaises(support.PolicyError):
            await self.execute(newer, request="retry-invoke", approved=True)
        task = gateway.tasks[projection.outer.task_id]
        wire = json.loads((FIXTURES / "claude.json").read_text())
        wire.update(id=task.id, contextId=task.context_id)
        message = wire["status"]["message"]
        message.update(
            taskId=task.id, contextId=task.context_id, messageId="retry-status"
        )
        message["metadata"][HITL_EXTENSION]["tools"][0]["args"].update(
            proposal_id=newer["proposal_id"], request_id="retry-invoke"
        )
        gateway.tasks[task.id] = Task.model_validate(wire)
        await self.observe(self.observer(gateway))
        fresh = next(p for p in await self.projections() if p.availability == "pending")
        self.assertNotEqual(fresh.id, projection.id)
        await self.assert_receipt(
            await self.answer(gateway, fresh, action="retry"),
            fresh,
            newer,
            "retry-invoke",
        )
        self.assertEqual(
            (await self.execute(newer, request="retry-invoke", approved=True))["state"],
            "merged",
        )
        self.assertEqual((await self.execute(p, approved=True))["state"], "blocked")
        self.assertEqual(len(self.fake.puts), 1)

    async def test_uncertain_merge_after_owner_approval_never_replays_put(self):
        self.fake.files[0]["filename"] = "k8s/app.yaml"
        await self.pool.execute(
            "UPDATE native_bindings SET kind='codex' WHERE session_id=$1", self.sid
        )
        p = await self.prepare()
        gateway, projection = await self.inventory(p, "codex")
        await self.assert_receipt(await self.answer(gateway, projection), projection, p)
        self.fake.lose = True
        self.assertEqual((await self.execute(p, approved=True))["state"], "uncertain")
        await self.observe(self.observer(gateway))
        self.assertEqual((await self.execute(p, approved=True))["state"], "merged")
        self.assertEqual(len(self.fake.puts), 1)
        self.assertEqual(len(gateway.sent), 1)

    async def test_multiple_inventory_question_sessions_keep_original_destinations(
        self,
    ):
        gateway = Gateway()
        for name in ("main-question", "standalone-question"):
            gateway.add(
                name,
                {
                    "type": "ask_user_request",
                    "id": "same-question-id",
                    "questions": [
                        {
                            "question": "Which platform?",
                            "choices": ["Linux", "macOS"],
                            "multiple": False,
                        }
                    ],
                },
            )
        await self.observe(self.observer(gateway))
        await self.observe(self.observer(gateway))
        projections = await self.projections()
        self.assertEqual(len(projections), 2)
        for projection in projections:
            response = await self.http(
                gateway,
                "POST",
                f"/hitl/{projection.id}/respond",
                {
                    "action_id": projection.id,
                    "response": {
                        "type": "ask_user_response",
                        "id": "same-question-id",
                        "answers": [{"answer": ["Linux"]}],
                    },
                },
            )
            self.assertEqual(response.status_code, 200, response.text)
            receipt = response.json()["response"]
            self.assertEqual(receipt["outer"], projection.outer.model_dump(mode="json"))
            self.assertEqual(
                receipt["calls"][0]["leaf"],
                projection.leaves[0].model_dump(mode="json"),
            )
            self.assertIsNone(receipt["calls"][0]["merge_key"])
        self.assertEqual(
            {s[1]["task_id"] for s in gateway.sent},
            {p.outer.task_id for p in projections},
        )
        self.assertEqual(
            {s[1]["context_id"] for s in gateway.sent},
            {p.outer.context_id for p in projections},
        )
        self.assertEqual(
            await self.pool.fetchval("SELECT count(*) FROM native_deliveries"), 0
        )
