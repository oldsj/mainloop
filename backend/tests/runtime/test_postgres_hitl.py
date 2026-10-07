"""b1 contracts against isolated PostgreSQL; all external authority is fixture data."""

import asyncio
import os
import uuid
from unittest.mock import patch

import asyncpg
import httpx
from mainloop import api
from mainloop.config import settings
from mainloop.db import db
from mainloop.db.hitl import (
    HITLConflict,
    load_checkpoint,
    lookup_merge_receipt,
    observe_session,
    record_response,
    save_association,
    save_checkpoint,
    save_projection,
)
from mainloop.db.hitl_schema import HITL_MIGRATION_SQL
from mainloop.identity import current_user
from mainloop.services.github_repo import parse_github_repo
from tests.runtime.test_hitl_correlation import receipt, request, task
from tests.runtime.test_postgres_ledger import PostgresTestCase

from models.hitl import (
    HITLProjection,
    ObservedSession,
    ObserverCheckpoint,
    normalized_hash,
)
from models.merge_policy import MergePolicyUpdate


class PolicyPostgresTests(PostgresTestCase):
    async def test_migration_backfill_preserves_owner_edits_and_metadata(self):
        # A schema with no policy columns is the only supported migration input.
        schema = f"legacy_{uuid.uuid4().hex[:12]}"
        async with self.pool.acquire() as conn, conn.transaction():
            await conn.execute(f'CREATE SCHEMA "{schema}"')
            await conn.execute(f'SET LOCAL search_path TO "{schema}"')
            await conn.execute(
                "CREATE TABLE projects(id TEXT PRIMARY KEY,user_id TEXT NOT NULL)"
            )
            await conn.execute("CREATE TABLE queue_items(id TEXT PRIMARY KEY)")
            await conn.execute(
                "INSERT INTO projects VALUES('mainloop','owner'),('testrepo','owner')"
            )
            await conn.execute(HITL_MIGRATION_SQL)
            rows = await conn.fetch(
                "SELECT merge_policy,merge_policy_version FROM projects"
            )
            self.assertEqual([(r[0], r[1]) for r in rows], [("auto", 1), ("auto", 1)])
            await conn.execute(
                "UPDATE projects SET merge_policy='approval',merge_policy_version=2 WHERE id='mainloop'"
            )
            await conn.execute(HITL_MIGRATION_SQL)
            self.assertEqual(
                await conn.fetchval(
                    "SELECT merge_policy FROM projects WHERE id='mainloop'"
                ),
                "approval",
            )
        project = await db.get_or_create_project(
            self.user, parse_github_repo("https://github.com/oldsj/mainloop")
        )
        self.assertEqual(project.merge_policy, "auto")
        update = MergePolicyUpdate(merge_policy="approval", expected_version=1)
        edited = await db.update_merge_policy(project.id, self.user, update)
        self.assertEqual(
            (edited.merge_policy, edited.merge_policy_version), ("approval", 2)
        )
        await db.update_project_metadata(project.id, open_pr_count=3)
        await db.ensure_tables_exist()
        refreshed = await db.get_or_create_project(
            self.user, parse_github_repo("https://github.com/oldsj/Mainloop")
        )
        self.assertEqual(
            (refreshed.merge_policy, refreshed.merge_policy_version), ("approval", 2)
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM project_merge_policy_audit WHERE project_id=$1",
                project.id,
            ),
            1,
        )
        with self.assertRaises(ValueError):
            await db.update_merge_policy(project.id, self.user, update)
        self.assertIsNone(await db.update_merge_policy(project.id, "stranger", update))
        with self.assertRaises(asyncpg.CheckViolationError):
            await self.pool.execute(
                "UPDATE projects SET merge_policy='anything' WHERE id=$1", project.id
            )

    async def test_owner_policy_api_validation_and_concurrent_version(self):
        project = await db.get_or_create_project(
            self.user, parse_github_repo("https://github.com/oldsj/testrepo")
        )
        patcher = patch.object(settings, "api_hosts", "localhost")
        patcher.start()
        self.addCleanup(patcher.stop)
        enable = patch.dict(
            os.environ, {"MAINLOOP_OWNER_POLICY_WRITES_ENABLED": "true"}
        )
        enable.start()
        self.addCleanup(enable.stop)
        api.app.dependency_overrides[current_user] = lambda: self.user
        self.addCleanup(api.app.dependency_overrides.pop, current_user)
        async with httpx.AsyncClient(
            transport=httpx.ASGITransport(app=api.app), base_url="http://localhost"
        ) as client:
            read = await client.get(f"/projects/{project.id}/merge-policy")
            self.assertEqual(read.status_code, 200)
            self.assertEqual(
                read.json()["protected_globs"],
                ["k8s/**", ".github/**", "**/migrations/**"],
            )
            path = f"/projects/{project.id}/merge-policy"
            bad = await client.put(
                path,
                json={
                    "merge_policy": "auto",
                    "expected_version": 1,
                    "protected_globs": [],
                },
            )
            self.assertEqual(bad.status_code, 422)
            replies = await asyncio.gather(
                *[
                    client.put(
                        path, json={"merge_policy": "approval", "expected_version": 1}
                    )
                    for _ in range(2)
                ]
            )
            self.assertEqual(sorted(r.status_code for r in replies), [200, 409])
            api.app.dependency_overrides[current_user] = lambda: "stranger"
            self.assertEqual((await client.get(path)).status_code, 404)
            self.assertEqual(
                (
                    await client.put(
                        path, json={"merge_policy": "auto", "expected_version": 2}
                    )
                ).status_code,
                404,
            )

    async def test_policy_write_gate_with_real_configured_identity(self):
        project = await db.get_or_create_project(
            self.user, parse_github_repo("https://github.com/oldsj/testrepo")
        )
        project = await db.update_merge_policy(
            project.id,
            self.user,
            MergePolicyUpdate(merge_policy="approval", expected_version=1),
        )
        path = f"/projects/{project.id}/merge-policy"
        # No dependency override: exercise the real current_user boundary, allowed
        # Host and absent Origin, both with and without an actor-like bearer.
        with patch.object(settings, "owner_id", self.user), patch.object(
            settings, "api_hosts", "localhost"
        ):
            async with httpx.AsyncClient(
                transport=httpx.ASGITransport(app=api.app), base_url="http://localhost"
            ) as client:
                for flag in (None, "false", "1", "TRUE", "true"):
                    with patch.dict(os.environ):
                        os.environ.pop("MAINLOOP_OWNER_POLICY_WRITES_ENABLED", None)
                        if flag is not None:
                            os.environ["MAINLOOP_OWNER_POLICY_WRITES_ENABLED"] = flag
                        for headers in (
                            {},
                            {
                                "Authorization": "Bearer actor-token-not-an-owner-credential"
                            },
                        ):
                            visible = await client.get(path)
                            self.assertEqual(visible.status_code, 200)
                            self.assertEqual(
                                visible.json()["writes_enabled"], flag == "true"
                            )
                            self.assertEqual(visible.json()["merge_policy"], "approval")
                            before = await db.get_project(project.id)
                            audit_before = await self.pool.fetchval(
                                "SELECT count(*) FROM project_merge_policy_audit WHERE project_id=$1",
                                project.id,
                            )
                            reply = await client.put(
                                path,
                                headers=headers,
                                json={
                                    "merge_policy": "auto",
                                    "expected_version": before.merge_policy_version,
                                },
                            )
                            after = await db.get_project(project.id)
                            if flag != "true":
                                self.assertEqual(reply.status_code, 503)
                                self.assertEqual(after.merge_policy, "approval")
                                self.assertEqual(
                                    after.merge_policy_version,
                                    before.merge_policy_version,
                                )
                                self.assertEqual(
                                    await self.pool.fetchval(
                                        "SELECT count(*) FROM project_merge_policy_audit WHERE project_id=$1",
                                        project.id,
                                    ),
                                    audit_before,
                                )
                            else:
                                # Opt-in asserts the external isolation prerequisite;
                                # it deliberately does not turn headers into identities.
                                self.assertEqual(reply.status_code, 200)
                                self.assertTrue(reply.json()["writes_enabled"])
                                self.assertEqual(after.merge_policy, "auto")
                                self.assertEqual(
                                    after.merge_policy_version,
                                    before.merge_policy_version + 1,
                                )
                                await db.update_merge_policy(
                                    project.id,
                                    self.user,
                                    MergePolicyUpdate(
                                        merge_policy="approval",
                                        expected_version=after.merge_policy_version,
                                    ),
                                )


class HITLPostgresTests(PostgresTestCase):
    def fresh_receipt(self, **kwargs):
        # Per-test fixtures occupy separate tasks; no deletion of immutable receipts.
        kwargs.setdefault("leaf_task", task(task_id=self.user))
        kwargs.setdefault("action", self.user)
        kwargs.setdefault("req", request(request_id=self.user))
        return receipt(**kwargs)

    async def record(self, value):
        async with self.pool.acquire() as conn, conn.transaction():
            return await record_response(conn, value)

    async def test_record_reload_lookup_and_projection_independence(self):
        value = self.fresh_receipt(nested=True)
        req = value.request
        projection = HITLProjection(
            id=self.user,
            owner_id="owner",
            outer=value.outer,
            payload=req,
            leaves=tuple(c.leaf for c in value.calls),
            associations=value.associations,
            availability="pending",
        )
        async with self.pool.acquire() as conn, conn.transaction():
            for association in value.associations:
                await save_association(conn, association)
            await save_projection(conn, projection)
            await save_projection(conn, projection)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM queue_items WHERE hitl_request_id=$1", self.user
            ),
            1,
        )
        self.assertEqual(await self.record(value), value)
        self.assertEqual(await self.record(value), value)
        key, digest = value.calls[0].merge_key, value.calls[0].arguments_hash
        # Restart: use a new physical connection, retaining no projection cache.
        conn = await asyncpg.connect(self.url)
        try:
            await conn.execute(
                "DELETE FROM native_hitl_requests WHERE id=$1", self.user
            )
            self.assertEqual(await lookup_merge_receipt(conn, key, digest), value)
            async with conn.transaction():
                await save_projection(conn, projection)
            self.assertEqual(
                await conn.fetchval(
                    "SELECT count(*) FROM queue_items WHERE hitl_request_id=$1",
                    self.user,
                ),
                1,
            )
            self.assertEqual(await lookup_merge_receipt(conn, key, digest), value)
            self.assertEqual(
                await conn.fetchval(
                    "SELECT state FROM native_hitl_response_transport WHERE action_id=$1",
                    value.action_id,
                ),
                "recorded",
            )
            for changed in (
                {"leaf_binding_id": "sibling"},
                {"leaf_binding_id": "parent"},
                {"leaf_runtime_session_id": "replacement"},
                {"owner_id": "stranger"},
                {"invocation_request_id": "different"},
                {"proposal_id": "different"},
            ):
                self.assertIsNone(
                    await lookup_merge_receipt(
                        conn, key.model_copy(update=changed), digest
                    )
                )
            self.assertIsNone(
                await lookup_merge_receipt(
                    conn, key, normalized_hash({"changed": True})
                )
            )
            with self.assertRaises(asyncpg.RaiseError):
                await conn.execute(
                    "UPDATE native_hitl_responses SET snapshot='{}' WHERE action_id=$1",
                    value.action_id,
                )
            with self.assertRaises(asyncpg.RaiseError):
                await conn.execute(
                    "DELETE FROM native_hitl_response_members WHERE action_id=$1",
                    value.action_id,
                )
        finally:
            await conn.close()

    async def test_same_action_body_conflict_and_simultaneous_alias_decisions(self):
        first = self.fresh_receipt()
        nested = self.fresh_receipt(nested=True, action=f"nested-{self.user}")
        results = await asyncio.gather(
            self.record(first), self.record(nested), return_exceptions=True
        )
        self.assertEqual(sum(isinstance(r, HITLConflict) for r in results), 1)
        winner = next(r for r in results if not isinstance(r, Exception))
        with self.assertRaises(HITLConflict):
            await self.record(
                winner.model_copy(update={"outbound_message_id": "changed"})
            )
        self.assertEqual(await self.record(winner), winner)

    async def test_rejection_unrelated_tool_and_ambiguous_consent_cannot_authorize(
        self,
    ):
        denied = self.fresh_receipt(approved=False)
        await self.record(denied)
        async with self.pool.acquire() as conn:
            self.assertIsNone(
                await lookup_merge_receipt(
                    conn, denied.calls[0].merge_key, denied.calls[0].arguments_hash
                )
            )
        generic = self.fresh_receipt(
            req=request("other.tool"),
            action=f"generic-{self.user}",
            leaf_task=task(task_id=f"generic-{self.user}"),
        )
        await self.record(generic)
        self.assertIsNone(generic.calls[0].merge_key)
        # Two different pending requests for an identical invocation are ambiguous.
        first = self.fresh_receipt(
            action=f"one-{self.user}",
            leaf_task=task(task_id=f"one-{self.user}"),
            req=request(request_id=self.user),
        )
        second = self.fresh_receipt(
            action=f"two-{self.user}",
            leaf_task=task(task_id=f"two-{self.user}"),
            req=request(request_id=self.user),
        )
        await self.record(first)
        await self.record(second)
        async with self.pool.acquire() as conn:
            self.assertIsNone(
                await lookup_merge_receipt(
                    conn, first.calls[0].merge_key, first.calls[0].arguments_hash
                )
            )

    async def test_invalid_decision_reserves_no_key_or_merge_authority(self):
        value = self.fresh_receipt()
        invalid_approval = value.response.approvals[0].model_copy(
            update={"rejection_reason": "Do not merge"}
        )
        invalid = value.model_copy(
            update={
                "response": value.response.model_copy(
                    update={"approvals": (invalid_approval,)}
                )
            }
        )
        with self.assertRaises(ValueError):
            await self.record(invalid)
        async with self.pool.acquire() as conn:
            self.assertEqual(
                await conn.fetchval(
                    "SELECT count(*) FROM native_hitl_response_members WHERE leaf_key=$1",
                    value.calls[0].leaf.key(),
                ),
                0,
            )
            self.assertEqual(
                await conn.fetchval(
                    "SELECT count(*) FROM native_hitl_responses WHERE action_id=$1",
                    value.action_id,
                ),
                0,
            )
            self.assertIsNone(
                await lookup_merge_receipt(
                    conn, value.calls[0].merge_key, value.calls[0].arguments_hash
                )
            )
        # A real denial with a reason can still claim that same leaf/action ID.
        denied = self.fresh_receipt(approved=False)
        reason = denied.response.approvals[0].model_copy(
            update={"rejection_reason": "Do not merge"}
        )
        denied = denied.model_copy(
            update={
                "response": denied.response.model_copy(update={"approvals": (reason,)})
            }
        )
        self.assertEqual(await self.record(denied), denied)
        async with self.pool.acquire() as conn:
            self.assertIsNone(
                await lookup_merge_receipt(
                    conn, denied.calls[0].merge_key, denied.calls[0].arguments_hash
                )
            )

    async def test_batch_conflict_rolls_back_every_member(self):
        first = self.fresh_receipt()
        await self.record(first)
        # A changed request hash identifies a successive pause, even with reused call IDs.
        from mainloop.runtime.hitl_correlation import build_decision_receipt

        from models.hitl import ContinuationIdentity, ToolApprovalResponse

        req = request(request_id=self.user)
        other = req.tools[0].model_copy(
            update={"id": "new-pending", "call_id": "new-call"}
        )
        req.tools.append(other)
        outer = ContinuationIdentity(
            **task(task_id=self.user).model_dump(),
            status_message_id="new-status",
            request_hash=normalized_hash(req.model_dump(mode="json")),
        )
        reply = ToolApprovalResponse.model_validate_json(
            '{"type":"tool_approval_response","approvals":['
            '{"id":"pending-1","approved":true},{"id":"new-pending","approved":false}]}'
        )
        batch = build_decision_receipt(
            action_id=f"batch-{self.user}",
            owner_id="owner",
            outbound_message_id=f"batch-message-{self.user}",
            outer=outer,
            request=req,
            leaf_task=task(task_id=self.user),
            leaf_request=req,
            leaf_binding_id="binding",
            response=reply,
        )
        # Distinct request hashes mean this really is a new pause. Record it, then
        # contend on one of its members in another batch/destination.
        await self.record(batch)
        contender = batch.model_copy(
            update={
                "action_id": f"contender-{self.user}",
                "outbound_message_id": f"contender-message-{self.user}",
            }
        )
        with self.assertRaises(HITLConflict):
            await self.record(contender)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_hitl_response_members WHERE action_id=$1",
                batch.action_id,
            ),
            2,
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_hitl_responses WHERE action_id=$1",
                contender.action_id,
            ),
            0,
        )

    async def test_observation_checkpoint_atomicity_and_alias_isolation(self):
        value = self.fresh_receipt()
        observed = ObservedSession(
            gateway="gateway",
            runtime_session_id=self.user,
            owner_id=self.user,
            verified_creator="configured-kagent-user",
            agent_id="agents/claude",
            endpoint="http://fixture/claude",
            context_id=self.user,
            prepared_revision="rev",
            lifecycle="ready",
        )
        checkpoint = ObserverCheckpoint(
            inventory_cursor="page-two",
            task_cursors={self.user: "task-page"},
            next_session_id=self.user,
        )
        async with self.pool.acquire() as conn:
            with self.assertRaisesRegex(RuntimeError, "crash"):
                async with conn.transaction():
                    await observe_session(conn, observed)
                    await save_checkpoint(conn, "gateway", self.user, checkpoint)
                    raise RuntimeError("crash")
            self.assertEqual(
                await load_checkpoint(conn, "gateway", self.user), ObserverCheckpoint()
            )
            async with conn.transaction():
                await observe_session(conn, observed)
                await save_checkpoint(conn, "gateway", self.user, checkpoint)
            self.assertEqual(
                await load_checkpoint(conn, "gateway", self.user), checkpoint
            )
            with self.assertRaises(HITLConflict):
                await observe_session(
                    conn, observed.model_copy(update={"owner_id": "stranger"})
                )
            direct = HITLProjection(
                id=self.user,
                owner_id="owner",
                outer=value.outer,
                payload=value.request,
                leaves=(value.calls[0].leaf,),
                availability="pending",
            )
            nested = self.fresh_receipt(nested=True)
            forged = HITLProjection(
                id=f"forged-{self.user}",
                owner_id="owner",
                outer=nested.outer,
                payload=nested.request,
                availability="unavailable",
                unavailable_reason="nested continuation mapping unavailable",
            )
            async with conn.transaction():
                await save_projection(conn, direct)
                await save_projection(conn, forged)
            self.assertEqual(
                await conn.fetchval(
                    "SELECT count(*) FROM native_hitl_aliases WHERE request_id=$1",
                    forged.id,
                ),
                0,
            )
            with self.assertRaises(ValueError):
                async with conn.transaction():
                    await save_projection(
                        conn, forged.model_copy(update={"leaves": direct.leaves})
                    )
        await self.record(value)
        # An unavailable hint never reserves the direct request's response key.
        self.assertEqual(await self.record(value), value)
