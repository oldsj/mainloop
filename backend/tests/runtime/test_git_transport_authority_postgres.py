"""Real PG/P1 ASGI, real local Git validation, fixed ASGI upstream and two connections."""

import asyncio
import hashlib
import json
from dataclasses import replace
from types import SimpleNamespace
from unittest.mock import patch

import asyncpg
import httpx
from mainloop.db import db
from mainloop.db import tasks as task_store
from mainloop.push_gate import store
from mainloop.push_gate.authorization import ZERO_OID
from mainloop.push_gate.protocol import TransportError, pkt, receipt
from mainloop.push_gate.transport import create_git_applications
from mainloop.push_gate.transport_authority import PostgresTransportAuthority, _context
from mainloop.push_gate.upstream import FixedGitUpstream, LoopbackFixture
from mainloop.runtime import agent_credentials
from mainloop.runtime import native_sessions as ns
from mainloop.runtime import workspaces
from tests.runtime.test_git_credentials_postgres import GitCredentialsCase
from tests.runtime.test_push_transport_git import PAT, GitBackedUpstream, GitFixtureCase
from tests.runtime.test_push_transport_http import asgi

from models.push_gate import PublicationState


class CapturingUpstream(GitBackedUpstream):
    def __init__(self, repo, case):
        # The inherited fixture verifies its own local gate. The authoritative
        # assertion below independently reads committed PG before any receive body.
        super().__init__(
            repo,
            SimpleNamespace(
                lock=SimpleNamespace(locked=lambda: _context.get() is not None),
                history=[PublicationState.DISPATCHING],
            ),
        )
        self.case = case
        self.markers = []
        self.pause_seed = None

    async def __call__(self, scope, receive, send):
        if scope["path"].endswith("git-receive-pack"):
            context = _context.get()
            self.case.assertTrue(context.active)
            self.case.assertFalse(context.conn.is_in_transaction())
            row = await self.case.pool.fetchrow(
                "SELECT state,transport_evidence FROM push_publications WHERE grant_id=$1 ORDER BY created_at DESC LIMIT 1",
                self.case.sid,
            )
            self.case.assertEqual(row["state"], "dispatching")
            payload = json.loads(row["transport_evidence"])
            self.case.assertEqual(
                payload["stamp"]["issuance_id"],
                str(self.case.enrollment.plan.issuance_id),
            )
            self.markers.append((row["state"], len(self.case.native.gets)))
        if self.pause_seed and b"git-upload-pack" in scope.get("query_string", b""):
            await self.pause_seed()
        await super().__call__(scope, receive, send)


class GitTransportPostgresTests(GitCredentialsCase):
    async def asyncSetUp(self):
        await super().asyncSetUp()
        authority = self.authority
        await GitFixtureCase.asyncSetUp(self)
        self.authority = authority
        self.fake = CapturingUpstream(self.repo, self)
        self.sid = await self.create()
        self.enrollment = await self.enrolled(self.sid)
        self.read_token, self.push_token = await self.tokens(self.sid)
        self.upstream = FixedGitUpstream(
            "Owner/Repo",
            PAT,
            self.limits,
            fixture=LoopbackFixture(
                "http://127.0.0.1:19876",
                transport_factory=lambda: httpx.ASGITransport(app=self.fake),
            ),
        )
        self.read_app, self.push_app = create_git_applications(
            authority=self.authority,
            upstream=self.upstream,
            spool_root=self.spool,
            limits=self.limits,
            read_hosts=("testserver",),
            push_hosts=("testserver",),
            seed_upstream_factory=self.authority.seed_upstream,
        )

    commit = GitFixtureCase.commit
    raw_body = GitFixtureCase.raw_body

    async def request(self, body, *, token=None):
        return await asgi(
            self.push_app,
            body=body,
            headers=[
                (b"host", b"testserver"),
                (b"authorization", ("Bearer " + (token or self.push_token)).encode()),
                (b"content-type", b"application/x-git-receive-pack-request"),
                (b"content-length", str(len(body)).encode()),
            ],
        )

    async def row(self):
        return await self.pool.fetchrow(
            "SELECT * FROM push_publications WHERE grant_id=$1 ORDER BY created_at DESC LIMIT 1",
            self.sid,
        )

    async def test_real_pg_actual_p1_read_discovery_and_one_receive(self):
        status, data, _ = await asgi(
            self.read_app,
            path=b"/Owner/Repo.git/info/refs",
            query=b"service=git-upload-pack",
            method="GET",
            headers=[
                (b"host", b"testserver"),
                (b"authorization", ("Bearer " + self.read_token).encode()),
            ],
        )
        self.assertEqual(status, 200, data)
        initial = len(self.native.gets)
        new = await self.commit("first")
        body = await self.raw_body(ZERO_OID, new)
        status, data, _ = await self.request(body)
        self.assertEqual(status, 200, data)
        self.assertTrue(receipt(data, "refs/heads/feature", sideband=True))
        row = await self.row()
        self.assertEqual(row["state"], "confirmed")
        self.assertTrue(json.loads(row["transport_receipt"])["validated"])
        self.assertEqual(self.fake.receives, [body])
        self.assertGreater(self.fake.markers[0][1], initial)
        self.assertEqual(list(self.spool.iterdir()), [])
        for secret in (self.read_token, self.push_token, PAT):
            self.assertNotIn(secret, json.dumps(dict(row), default=str))
            self.assertNotIn(secret.encode(), data)

    async def test_existing_seed_commit_with_zero_incoming_objects(self):
        pack = b"PACK" + (2).to_bytes(4, "big") + (0).to_bytes(4, "big")
        body = (
            pkt(
                f"{ZERO_OID} {self.base} refs/heads/feature".encode()
                + b"\0report-status side-band-64k ofs-delta\n"
            )
            + b"0000"
            + pack
            + hashlib.sha1(pack, usedforsecurity=False).digest()
        )
        status, data, _ = await self.request(body)
        self.assertEqual(status, 200, data)
        self.assertEqual((await self.row())["state"], "confirmed")
        self.assertEqual(
            json.loads((await self.row())["transport_evidence"])["incoming_objects"], 0
        )
        self.assertEqual(self.fake.receives, [body])

    async def test_wrong_purpose_and_repository_deny_without_upstream(self):
        new = await self.commit("first")
        body = await self.raw_body(ZERO_OID, new)
        for token in (self.read_token, "mcp_fixture_wrong_purpose"):
            self.assertNotEqual((await self.request(body, token=token))[0], 200)
        status, _, _ = await asgi(
            self.push_app,
            path=b"/Other/Repo.git/info/refs",
            query=b"service=git-receive-pack",
            method="GET",
            headers=[
                (b"host", b"testserver"),
                (b"authorization", ("Bearer " + self.push_token).encode()),
            ],
        )
        self.assertNotEqual(status, 200)
        self.assertFalse(self.fake.requests)

    async def test_seed_stamp_revalidation_between_reads_and_closed_context(self):
        binding = await self.authority.authenticate(self.push_token, "git-push")
        facade = self.authority.seed_upstream(binding, self.upstream)
        await facade.discovery("git-upload-pack")
        async with self.pool.acquire() as conn:
            await agent_credentials.revoke(self.sid, conn=conn)
        with self.assertRaises(TransportError):
            await facade.upload_pack(b"0000")
        self.assertEqual(len(self.fake.requests), 1)
        with self.assertRaisesRegex(TransportError, "dispatch_context_required"):
            await self.authority.record(None)
        self.assertFalse(hasattr(facade, "receive_pack"))

    async def test_revoke_during_spool_prevents_any_receive(self):
        new = await self.commit("first")
        body = await self.raw_body(ZERO_OID, new)
        from mainloop.push_gate import transport

        original = transport.prepare_receive

        async def revoke_after_validation(*args, **kwargs):
            prepared = await original(*args, **kwargs)
            await agent_credentials.revoke(self.sid)
            return prepared

        with patch.object(transport, "prepare_receive", revoke_after_validation):
            status, _, _ = await self.request(body)
        self.assertNotEqual(status, 200)
        self.assertFalse(self.fake.receives)

    async def test_final_get_after_ref_discovery_denies_uid_change(self):
        new = await self.commit("first")
        body = await self.raw_body(ZERO_OID, new)
        original = self.upstream.discovery

        async def change(service, **kwargs):
            data = await original(service, **kwargs)
            if service == "git-receive-pack":
                runtime = self.enrollment.association.runtime.session_id
                session = self.native.sessions[runtime]
                self.native.sessions[runtime] = replace(
                    session,
                    runtime_association=replace(
                        session.runtime_association, actor_uid="changed"
                    ),
                )
            return data

        with patch.object(self.upstream, "discovery", change):
            status, _, _ = await self.request(body)
        self.assertNotEqual(status, 200)
        self.assertFalse(self.fake.receives)
        self.assertEqual((await self.row())["state"], "pending")

    async def test_peer_normal_fence_waits_in_pg_locks_until_outcome(self):
        new = await self.commit("first")
        body = await self.raw_body(ZERO_OID, new)
        self.fake.mode = "wait"
        request = asyncio.create_task(self.request(body))
        await asyncio.wait_for(self.fake.dispatched.wait(), 10)
        async with self.pool.acquire() as peer:
            pid = peer.get_server_pid()
            fence = asyncio.create_task(agent_credentials.revoke(self.sid, conn=peer))
            try:
                blocked = False
                for _ in range(200):
                    blocked = await self.pool.fetchval(
                        "SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND NOT granted)",
                        pid,
                    )
                    if blocked:
                        break
                    await asyncio.sleep(0.01)
                self.assertTrue(blocked)
                self.assertFalse(fence.done())
                self.assertEqual((await self.row())["state"], "dispatching")
            finally:
                self.fake.release.set()
                await request
                await fence
        self.assertEqual((await self.row())["state"], "confirmed")
        self.assertEqual(len(self.fake.receives), 1)

    async def test_lost_reply_restart_revoke_delete_and_successor_branch_fence(self):
        new = await self.commit("first")
        body = await self.raw_body(ZERO_OID, new)
        self.fake.mode = "loss"
        self.assertNotEqual((await self.request(body))[0], 200)
        self.assertEqual((await self.row())["state"], "unknown")
        await agent_credentials.revoke(self.sid)
        await ns.ledger.mark_kagent_deleted(self.sid)
        self.assertTrue(
            await self.pool.fetchval(
                "SELECT held FROM workspace_writer_claims WHERE binding_id=$1", self.sid
            )
        )
        new_pool = await asyncpg.create_pool(self.url, min_size=1, max_size=2)
        try:
            async with new_pool.acquire() as conn, conn.transaction():
                with self.assertRaisesRegex(
                    task_store.TaskError, "publication_unresolved"
                ):
                    await task_store.reserve_writer(
                        conn,
                        owner_id=self.user,
                        repository="owner/repo",
                        branch="feature",
                        binding_id="prospective-different-grant",
                    )
            with self.assertRaisesRegex(ValueError, "publication_unresolved"):
                await workspaces._delete_rows(
                    self.sid, evidence="fixture-known-deleted"
                )
            self.assertIsNotNone(await ns.get_binding(self.sid))
        finally:
            await new_pool.close()
        self.assertEqual(len(self.fake.receives), 1)

    async def test_receipt_failures_rejected_unknown_and_no_resend(self):
        self.fake.mode = "ng"
        new = await self.commit("first")
        body = await self.raw_body(ZERO_OID, new)
        status, data, _ = await self.request(body)
        self.assertEqual(status, 200)
        self.assertFalse(receipt(data, "refs/heads/feature", sideband=True))
        self.assertEqual((await self.row())["state"], "rejected")

    async def bad_receipt(self, mode):
        self.fake.mode = mode
        new = await self.commit("first")
        body = await self.raw_body(ZERO_OID, new)
        self.assertNotEqual((await self.request(body))[0], 200)
        self.assertEqual((await self.row())["state"], "unknown")
        before = len(self.fake.receives)
        self.assertNotEqual((await self.request(body))[0], 200)
        self.assertEqual(len(self.fake.receives), before)
        self.assertEqual(before, 1)

    async def test_truncated_receipt_unknown(self):
        await self.bad_receipt("before")

    async def test_extra_ref_receipt_unknown(self):
        await self.bad_receipt("extra")

    async def test_partial_upload_unknown(self):
        await self.bad_receipt("partial")

    async def test_receipt_db_failure_retains_committed_dispatching_across_restart(
        self,
    ):
        new = await self.commit("first")
        body = await self.raw_body(ZERO_OID, new)
        original = store.transition_transport_attempt

        async def fail(conn, evidence, state, receipt):
            if state != PublicationState.DISPATCHING:
                raise RuntimeError("fixture outcome persistence loss")
            return await original(conn, evidence, state, receipt)

        with patch.object(store, "transition_transport_attempt", fail):
            self.assertNotEqual((await self.request(body))[0], 200)
        self.assertEqual((await self.row())["state"], "dispatching")
        restarted = PostgresTransportAuthority(db, self.native, self.metadata)
        with self.assertRaisesRegex(ValueError, "publication_unresolved"):
            await restarted.authenticate(self.push_token, "git-push")
        self.assertEqual(len(self.fake.receives), 1)

    async def test_cancellation_shield_records_unknown_without_resend(self):
        new = await self.commit("first")
        body = await self.raw_body(ZERO_OID, new)
        self.fake.mode = "wait"
        request = asyncio.create_task(self.request(body))
        await asyncio.wait_for(self.fake.dispatched.wait(), 10)
        request.cancel()
        await asyncio.sleep(0)
        self.fake.release.set()
        with self.assertRaises(asyncio.CancelledError):
            await request
        self.assertEqual((await self.row())["state"], "unknown")
        self.assertEqual(len(self.fake.receives), 1)

    async def test_evidence_forgery_duplicate_dispatch_and_closed_context_deny(self):
        new = await self.commit("first")
        body = await self.raw_body(ZERO_OID, new)
        original_record, original_transition = (
            self.authority.record,
            self.authority.transition,
        )
        captured = {}

        async def checked_record(evidence):
            await original_record(evidence)
            await original_record(evidence)
            captured["evidence"], captured["context"] = evidence, _context.get()
            with self.assertRaisesRegex(
                TransportError, "publication_evidence_conflict"
            ):
                await original_record(replace(evidence, body_sha256="f" * 64))
            context = _context.get()
            payload = self.authority._payload(context, evidence)
            with self.assertRaises(ValueError):
                await store.record_transport_attempt(
                    context.conn,
                    payload,
                    context.proof.binding.stamp.model_copy(
                        update={"capability_hash": "f" * 64}
                    ),
                )

        async def checked_transition(evidence, state):
            await original_transition(evidence, state)
            if state == PublicationState.DISPATCHING:
                with self.assertRaisesRegex(ValueError, "publication_unresolved"):
                    await original_transition(evidence, state)
                context = _context.get()
                with self.assertRaisesRegex(ValueError, "invalid_transition"):
                    await store.transition_transport_attempt(
                        context.conn,
                        self.authority._payload(context, evidence),
                        state,
                        None,
                    )

        with patch.object(self.authority, "record", checked_record), patch.object(
            self.authority, "transition", checked_transition
        ):
            self.assertEqual((await self.request(body))[0], 200)
        token = _context.set(captured["context"])
        try:
            with self.assertRaisesRegex(TransportError, "dispatch_context_required"):
                await original_record(captured["evidence"])
        finally:
            _context.reset(token)
        self.assertEqual(len(self.fake.receives), 1)
        self.assertEqual((await self.row())["state"], "confirmed")

    async def test_peer_policy_change_after_validation_denies_receive(self):
        from mainloop.push_gate import transport

        from models.push_gate import ProtectedBranchPolicy

        new = await self.commit("first")
        body = await self.raw_body(ZERO_OID, new)
        original = transport.prepare_receive

        async def protect(*args, **kwargs):
            prepared = await original(*args, **kwargs)
            async with self.pool.acquire() as conn:
                await store.set_policy(
                    conn,
                    ProtectedBranchPolicy(
                        project_id=self.pid,
                        version=2,
                        default_branch="main",
                        patterns=("feature",),
                    ),
                )
            return prepared

        with patch.object(transport, "prepare_receive", protect):
            self.assertNotEqual((await self.request(body))[0], 200)
        self.assertFalse(self.fake.receives)

    async def test_invalid_legacy_scope_and_migration_never_erase_unknown(self):
        new = await self.commit("first")
        body = await self.raw_body(ZERO_OID, new)
        self.fake.mode = "loss"
        await self.request(body)
        await self.pool.execute(
            "UPDATE push_publications SET repository=NULL,branch=NULL WHERE grant_id=$1",
            self.sid,
        )
        from mainloop.db.git_transport_schema import GIT_TRANSPORT_MIGRATION_SQL

        await self.pool.execute(GIT_TRANSPORT_MIGRATION_SQL)
        async with self.pool.acquire() as conn:
            refs = await store.unresolved_for_branch(
                conn, self.user, "Other/Repository", "another-branch"
            )
        self.assertTrue(refs)
        self.assertEqual((await self.row())["state"], "unknown")
