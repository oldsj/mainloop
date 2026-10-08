"""Real PostgreSQL + existing merge/HITL services; fake HTTP and gateway only.

Prepared for an explicitly granted heavy lane. These are not live provider proofs.
"""

import asyncio
import uuid
from datetime import UTC, datetime
from unittest.mock import patch

from mainloop.db import db
from mainloop.db import hitl as hitl_store
from mainloop.db import tasks as store
from mainloop.providers import registry
from mainloop.runtime.agent_credentials import revoke
from mainloop.runtime.delegation import PgStore
from mainloop.runtime.kagent_client import TaskStatus
from mainloop.runtime.policy import PolicyError
from mainloop.services import merge
from mainloop.tasks import attention, lifecycle, publication
from mainloop.tasks.principal import TaskPrincipal
from mainloop.tasks.projection import Projection
from tests.runtime.test_merge import SHA, MergeFixture

from models.hitl import HITLProjection
from models.task import Task, TaskCheckout, TaskReport


class TaskAttentionTests(MergeFixture):
    async def asyncSetUp(self):
        await super().asyncSetUp()
        await self.pool.execute(
            "TRUNCATE tasks,task_attempts,workspace_writer_claims,task_events,task_operations CASCADE"
        )
        self.task, self.attempt = await self.enroll(self.sid, "feature")
        self.binding = await PgStore().get_binding(self.sid)

    async def enroll(self, sid, branch, parent=None):
        role = "child" if parent else "supervisor"
        now = datetime.now(UTC)
        tid = uuid.uuid4().hex
        task = Task(
            id=tid,
            owner_id=self.user,
            project_id=self.project.id,
            parent_task_id=parent.id if parent else None,
            root_task_id=parent.root_task_id if parent else tid,
            title="Task",
            brief="Task brief",
            mode="code",
            assigned_profile_id="claude",
            selection_source="explicit",
            status="running",
            checkout=TaskCheckout(branch=branch),
            created_at=now,
            updated_at=now,
        )
        async with self.pool.acquire() as conn, conn.transaction():
            await store.admission_lock(conn)
            await conn.execute(
                "UPDATE sessions SET project_id=$2,repo_url='https://github.com/owner/repo',branch_name=$3 WHERE id=$1",
                sid,
                self.project.id,
                branch,
            )
            await conn.execute(
                "UPDATE native_bindings SET role=$2,mcp_grant_kind='workspace',kagent_session_id=$3 WHERE session_id=$1",
                sid,
                role,
                f"runtime-{sid}",
            )
            await conn.execute(
                "INSERT INTO workspaces(session_id,repo,branch) VALUES($1,'https://github.com/owner/repo',$2)",
                sid,
                branch,
            )
            await store.insert_task(conn, task)
            task, attempt = await store.admit_attempt(
                conn,
                task,
                registry().resolve("claude", role),
                role=role,
                depth=2 if parent else 1,
                per_parent_cap=3,
                global_cap=6,
            )
            attempt = await lifecycle.save_attempt(
                conn,
                attempt.model_copy(
                    update={
                        "session_id": sid,
                        "binding_id": sid,
                        "workspace_id": sid,
                        "state": "active",
                    }
                ),
            )
        return task, attempt

    async def child(self):
        parent, parent_sid = self.task, self.sid
        # The supervising task is working, not asking for a second owner card.
        runtime_task = self.gateway.tasks[f"task-runtime-{parent_sid}"]
        self.gateway.tasks[runtime_task.id] = runtime_task.model_copy(
            update={"status": TaskStatus(state="working")}
        )
        sid, _ = await self.bound_session(
            role="child", mcp_grant_kind="workspace", parent_session_id=parent_sid
        )
        self.task, self.attempt = await self.enroll(sid, "feature/leaf", parent)
        self.sid, self.binding = sid, await PgStore().get_binding(sid)
        self.fake.pr["head"]["ref"] = "feature/leaf"
        self.gateway.add(f"runtime-{sid}")
        return parent, parent_sid

    async def view(self, task=None):
        task = task or self.task
        async with self.pool.acquire() as conn:
            current = await store.get_task(conn, task.id, TaskPrincipal(self.user))
            return current, await store.projection(conn, task.id)

    async def pause(self, proposal):
        # Observe this exact synthetic leaf. The inventory scheduler has a bounded
        # pass and need not discover every fixture session on its first pass.
        runtime_id = self.binding["kagent_session_id"]
        live_session, native_task = self.gateway.add(
            runtime_id,
            {
                "type": "tool_approval_request",
                "tools": [
                    {
                        "id": "call",
                        "call_id": "native",
                        "name": "mcp__mainloop-merge-approval__merge_pull_request_with_approval",
                        "args": {
                            "proposal_id": proposal["proposal_id"],
                            "request_id": "invoke-1",
                        },
                    }
                ],
            },
        )
        async with self.pool.acquire() as conn:
            observed_session = await self.hitl_service.owned(conn, live_session)
            async with conn.transaction():
                await hitl_store.observe_session(conn, observed_session)
            await self.hitl_service.refresh(conn, runtime_id, native_task.id)
            raw = await conn.fetchval(
                """SELECT snapshot FROM native_hitl_requests WHERE owner_id=$1
                   AND snapshot->'outer'->>'runtime_session_id'=$2
                   AND snapshot->'outer'->>'task_id'=$3""",
                self.user,
                runtime_id,
                native_task.id,
            )
        return (
            self.hitl_service,
            HITLProjection.model_validate(store.decode(raw)),
            self.gateway,
        )

    async def test_auto_settles_leaf_and_one_parent_event_without_completing_parent(
        self,
    ):
        parent, _ = await self.child()
        result = await merge.auto_merge(self.binding, self.args)
        leaf, facts = await self.view()
        self.assertEqual(result["state"], "merged")
        self.assertEqual(
            (leaf.status, facts.pr_state, facts.ci_state),
            ("completed", "merged", "success"),
        )
        self.assertEqual(facts.ci_head_sha, SHA)
        self.assertEqual(await merge.auto_merge(self.binding, self.args), result)
        self.assertEqual((await self.execute(result))["state"], "merged")
        self.assertEqual(len(self.fake.puts), 1)
        root, _ = await self.view(parent)
        self.assertEqual(root.status, "running")
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM task_events WHERE task_id=$1 AND event_key LIKE 'child:%:merge-settled:%'",
                parent.id,
            ),
            1,
        )
        self.assertTrue(
            await self.pool.fetchval(
                "SELECT capacity_held FROM task_attempts WHERE id=$1", self.attempt.id
            )
        )
        self.assertTrue(
            await self.pool.fetchval(
                "SELECT held FROM workspace_writer_claims WHERE attempt_id=$1",
                self.attempt.id,
            )
        )

    async def test_merge_projection_event_and_owner_card_rollback_together(self):
        p = await self.prepare()
        self.fake.lose = True
        self.assertEqual((await self.execute(p))["state"], "uncertain")
        before, projection = await self.view()
        async with self.pool.acquire() as conn:
            proposal = await merge.proposal(conn, self.user, p["proposal_id"])
            candidate = await conn.fetchrow(
                "SELECT * FROM merge_requests WHERE id=$1", proposal["candidate_id"]
            )
        async with self.pool.acquire() as conn, conn.transaction():
            self.assertEqual(
                await publication.unresolved_intents(conn, self.task, self.attempt),
                (f"merge-intent:{candidate['intent_id']}",),
            )
        result = merge.merged_result(proposal, "c" * 40, "fixture_verified_response")
        with self.assertRaisesRegex(RuntimeError, "rollback"):
            async with publication.guard(db, self.binding, self.project.id):
                async with self.pool.acquire() as conn, conn.transaction():
                    await store.admission_lock(conn)
                    await merge.lock_candidate(
                        conn, self.user, self.project.id, 123, 17
                    )
                    await merge.finish(conn, candidate, result)
                    raise RuntimeError("rollback")
        after, facts = await self.view()
        self.assertEqual(
            (after.version, after.status, facts),
            (before.version, before.status, projection),
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM task_events WHERE task_id=$1 AND event_key LIKE 'merge-settled:%'",
                self.task.id,
            ),
            0,
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM queue_items WHERE id=$1",
                f"merge-outcome:{candidate['intent_id']}",
            ),
            0,
        )
        self.assertEqual((await self.execute(p))["state"], "merged")
        self.assertEqual(len(self.fake.puts), 1)
        async with self.pool.acquire() as conn, conn.transaction():
            self.assertEqual(
                (await merge.finish(conn, candidate, result))["state"], "merged"
            )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM task_events WHERE task_id=$1 AND event_key LIKE 'merge-settled:%'",
                self.task.id,
            ),
            1,
        )
        async with self.pool.acquire() as conn, conn.transaction():
            self.assertEqual(
                await publication.unresolved_intents(conn, self.task, self.attempt), ()
            )

    async def test_duplicate_card_rollup_reuses_leaf_receipt_and_parent_cannot_execute(
        self,
    ):
        parent, parent_sid = await self.child()
        await self.pool.execute(
            "UPDATE projects SET merge_policy='approval' WHERE id=$1", self.project.id
        )
        p = await self.prepare()
        observer, card, _ = await self.pause(p)
        await attention.refresh(db, self.sid)
        first, value = await self.view()
        await attention.refresh(db, self.sid)
        second, duplicate = await self.view()
        self.assertEqual(first.version, second.version)
        self.assertEqual(value.pending_approval_ids, (card.id,))
        self.assertEqual((first.status, first.reason), ("waiting", "approval"))
        self.assertEqual(value, duplicate)
        root_task, root = await self.view(parent)
        self.assertEqual((root_task.status, root_task.reason), ("waiting", "approval"))
        self.assertEqual(root.pending_approval_ids, (card.id,))
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM queue_items WHERE hitl_request_id=$1", card.id
            ),
            1,
        )
        with self.assertRaises(PolicyError):
            await merge.execute(
                await PgStore().get_binding(parent_sid),
                {"proposal_id": p["proposal_id"], "request_id": "invoke-1"},
                approved=True,
            )
        await self.decide(observer, card)
        await attention.refresh(db, self.sid)
        _, value = await self.view()
        self.assertEqual(value.pending_approval_ids, ())
        self.assertEqual((await self.execute(p, approved=True))["state"], "merged")

    async def test_protected_auto_requires_existing_owner_approval(self):
        self.fake.files[0]["filename"] = "k8s/manifest.yaml"
        result = await merge.auto_merge(self.binding, self.args)
        self.assertEqual(result["state"], "approval_required")
        self.assertFalse(self.fake.puts)
        self.assertEqual((await self.view())[0].status, "running")
        observer, card, _ = await self.pause(result)
        await self.decide(observer, card)
        self.assertEqual((await self.execute(result, approved=True))["state"], "merged")

    async def test_completed_report_is_neither_verified_publication_nor_consent(self):
        from mainloop.tasks.reports import record

        request = TaskReport(
            task_id=self.task.id,
            attempt_id=self.attempt.id,
            request_id="publication-claim",
            summary="PR merged and CI passed",
            outcome="completed",
            evidence_refs=("https://github.com/owner/repo/pull/17",),
        )
        async with self.pool.acquire() as conn, conn.transaction():
            principal = await lifecycle.authenticate_binding(conn, self.binding)
            await record(conn, principal, request)
        task, projection = await self.view()
        self.assertEqual((task.status, task.reason), ("waiting", "publication"))
        self.assertEqual(projection.ci_state, "unknown")
        self.assertIsNone(projection.pr_number)
        self.assertIsNone(projection.merge_proposal_id)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM merge_proposals WHERE owner_id=$1", self.user
            ),
            0,
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_hitl_responses WHERE owner_id=$1",
                self.user,
            ),
            0,
        )
        self.assertFalse(self.fake.puts)

    async def test_revoke_between_prepare_and_claim_refuses_old_source(self):
        p = await self.prepare()
        await revoke(self.sid)
        with self.assertRaises(PolicyError):
            await self.execute(p)
        self.assertFalse(self.fake.puts)
        self.assertNotEqual((await self.view())[0].status, "completed")

    async def test_revocation_waits_for_already_admitted_external_dispatch(self):
        p = await self.prepare()
        entered, release, revoked = asyncio.Event(), asyncio.Event(), asyncio.Event()

        async def hook(request):
            if request.method == "PUT":
                entered.set()
                await release.wait()

        async def remove():
            await revoke(self.sid)
            revoked.set()

        self.fake.hook = hook
        dispatch = asyncio.create_task(self.execute(p))
        await entered.wait()
        removing = asyncio.create_task(remove())
        try:
            await self.pool.fetchval("SELECT 1")
            self.assertFalse(revoked.is_set())
        finally:
            release.set()
        self.assertEqual((await dispatch)["state"], "merged")
        await removing
        self.assertTrue(revoked.is_set())
        self.assertEqual(len(self.fake.puts), 1)

    async def test_cross_owner_read_and_sibling_pr_denied(self):
        async with self.pool.acquire() as conn:
            with self.assertRaises(store.TaskError):
                await store.get_task(conn, self.task.id, TaskPrincipal("other-owner"))
        self.fake.pr["head"]["ref"] = "sibling/branch"
        with self.assertRaises(PolicyError):
            await self.prepare()
        self.assertFalse(self.fake.puts)

    async def test_head_change_and_closed_unmerged_observation_never_complete(self):
        p = await self.prepare()
        self.fake.pr.update(state="closed", merged=False)
        import httpx
        from mainloop.services import github_merge

        cls = github_merge.GitHubMergeClient
        with patch.object(
            github_merge,
            "GitHubMergeClient",
            lambda: cls(transport=httpx.MockTransport(self.fake.handle)),
        ):
            await Projection().refresh(db, self.task.id)
            first, value = await self.view()
            self.assertEqual(value.pr_state, "closed")
            self.assertNotEqual(first.status, "completed")
            await Projection().refresh(db, self.task.id)
            self.assertEqual((await self.view())[0].version, first.version)
        self.fake.pr.update(state="open")
        self.fake.pr["head"]["sha"] = "b" * 40
        with self.assertRaises(PolicyError):
            await self.execute(p, approved=True)
        self.assertFalse(self.fake.puts)

    async def assert_ci_cannot_complete(self, status, conclusion, expected):
        self.fake.runs = (
            [{**self.fake.runs[0], "status": status, "conclusion": conclusion}]
            if status
            else []
        )
        self.fake.suites = (
            [{**self.fake.suites[0], "status": status, "conclusion": conclusion}]
            if status
            else []
        )
        p = await self.prepare()
        result = await self.execute(p)
        self.assertEqual(result["state"], expected)
        self.assertFalse(self.fake.puts)
        self.assertNotEqual((await self.view())[0].status, "completed")

    async def test_ci_pending_cannot_complete(self):
        await self.assert_ci_cannot_complete("in_progress", None, "evaluating")

    async def test_ci_failure_cannot_complete(self):
        await self.assert_ci_cannot_complete("completed", "failure", "blocked")

    async def test_ci_missing_cannot_complete(self):
        await self.assert_ci_cannot_complete(None, None, "evaluating")

    async def test_read_masks_stale_ci_without_dispatch(self):
        from datetime import timedelta

        from mainloop.tasks.projection import read

        await self.prepare()
        task, value = await self.view()
        stale = value.model_copy(
            update={"observed_at": datetime.now(UTC) - timedelta(minutes=6)}
        )
        await self.pool.execute(
            "UPDATE tasks SET projection=$2::jsonb WHERE id=$1",
            task.id,
            stale.model_dump_json(),
        )
        async with self.pool.acquire() as conn:
            result = await read(conn, task)
        self.assertEqual(result.ci_state, "unknown")
        self.assertEqual(result.publication_state, "read_only")
        self.assertFalse(self.fake.puts)

    async def test_pr_creation_result_attaches_once_to_exact_attempt(self):
        import httpx
        from mainloop.services import github_creation
        from tests.runtime.test_open_pull_request import ARGS, FakeGitHub

        fake = FakeGitHub()
        fake.repo["default_branch"] = "main"
        original_pr = fake.pr

        def pr():
            result = original_pr()
            result["head"]["ref"] = "feature"
            return result

        fake.pr = pr
        cls = github_creation.GitHubCreationClient
        arguments = {**ARGS, "project_id": self.project.id, "branch": "feature"}
        with patch.object(
            github_creation,
            "GitHubCreationClient",
            lambda: cls(transport=httpx.MockTransport(fake.handle)),
        ):
            result = await github_creation.open_pull_request(self.binding, arguments)
            first, projection = await self.view()
            replay = await github_creation.open_pull_request(self.binding, arguments)
            second, duplicate = await self.view()
        self.assertEqual(result, replay)
        self.assertEqual(first.version, second.version)
        self.assertEqual(projection, duplicate)
        self.assertEqual(
            (projection.repository, projection.pr_number, projection.pr_head_sha),
            ("owner/repo", 17, SHA),
        )
        self.assertEqual(projection.ci_state, "unknown")
        async with self.pool.acquire() as conn:
            attempt = await lifecycle.load_attempt(conn, self.attempt.id)
        self.assertEqual(
            sum(ref.startswith("pr-creation:") for ref in attempt.evidence_refs), 1
        )
        self.assertEqual(sum(request.method == "POST" for request in fake.requests), 1)

    async def test_unassociated_uncertain_pr_intent_holds_handoff(self):
        intent, _ = await db.claim_pr_creation(
            user_id=self.user,
            project_id=self.project.id,
            request_id="unassociated-source",
            payload_hash="fixture",
            repo_id=123,
            head="feature",
            base="main",
            expected_sha=SHA,
        )
        async with self.pool.acquire() as conn, conn.transaction():
            attempt = await lifecycle.load_attempt(conn, self.attempt.id)
            self.assertEqual(attempt.evidence_refs, ())
            self.assertEqual(
                await publication.unresolved_intents(conn, self.task, attempt),
                (f"pr-creation:{intent['id']}",),
            )
        self.assertFalse(self.fake.puts)
