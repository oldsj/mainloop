"""Lifecycle integration against PostgreSQL and a sanitized fake kagent gateway."""

import asyncio
import uuid
from contextlib import asynccontextmanager
from dataclasses import replace
from unittest.mock import patch

import httpx
from mainloop import api
from mainloop.config import settings
from mainloop.db import db
from mainloop.push_gate import credentials as git_credentials
from mainloop.push_gate import lifecycle, store
from mainloop.runtime import agent_credentials
from mainloop.runtime import native_sessions as ns
from mainloop.runtime import workspaces
from mainloop.runtime.kagent_client import (
    CurrentRuntimeAssociation,
    OutcomeUnknown,
    RuntimeOperation,
    RuntimeState,
    SessionError,
)
from tests.runtime.test_git_credentials_postgres import FakeGitSecrets
from tests.runtime.test_postgres_ledger import KagentFakeCase
from tests.runtime.test_push_gate import UPDATE

from models import SessionStatus, WorkspaceManifest
from models.push_gate import ProtectedBranchPolicy, PushGrant


class PushLifecycleTests(KagentFakeCase):
    async def asyncSetUp(self):
        await super().asyncSetUp()
        enabled = patch.object(settings, "push_gate_enabled", True)
        enabled.start()
        self.addCleanup(enabled.stop)
        enabled_git = patch.object(settings, "git_transport_enabled", True)
        enabled_git.start()
        self.addCleanup(enabled_git.stop)
        git_store = patch.object(
            git_credentials, "secrets", git_credentials.GitSecretStore(FakeGitSecrets())
        )
        git_store.start()
        self.addCleanup(git_store.stop)
        get = ns.get_client().get_session

        async def observed(sid):
            session = await get(sid)
            return replace(
                session,
                creator="mainloop",
                prepared_revision="fixture-revision",
                runtime_association=CurrentRuntimeAssociation(
                    "generation-" + sid,
                    "fixture-space",
                    "actor-" + sid,
                    "uid-" + sid,
                    "active",
                    True,
                ),
            )

        association = patch.object(ns.get_client(), "get_session", observed)
        association.start()
        self.addCleanup(association.stop)
        self.pid = "push-" + uuid.uuid4().hex
        self.repo = "https://github.com/example/" + self.pid
        await self.pool.execute(
            """INSERT INTO projects(id,user_id,owner,name,full_name,html_url)
               VALUES($1,$2,'example',$1,$3,$4)""",
            self.pid,
            self.user,
            "example/" + self.pid,
            self.repo,
        )

    async def create(self, branch="feature"):
        self.fake.next_session_ids = [str(uuid.uuid4())]
        return await workspaces.create(
            self.user, self.pid, WorkspaceManifest(repo_url=self.repo, branch=branch)
        )

    async def grant(self, sid):
        row = await self.pool.fetchrow(
            "SELECT grant_data,revoked_at,token_hash FROM push_grants WHERE id=$1", sid
        )
        return row

    async def test_creation_projection_and_disabled_lifecycle(self):
        ws = await self.create()
        self.assertEqual((ws.publication_mode, ws.publication_reason), ("branch", None))
        self.assertEqual(
            store._decode(
                (await self.grant(ws.session_id))["grant_data"], PushGrant
            ).version,
            1,
        )
        default = await self.create("main")
        self.assertIsNone(await self.grant(default.session_id))
        self.assertEqual(default.publication_reason, "default_branch")
        async with self.pool.acquire() as conn:
            await store.set_policy(
                conn,
                ProtectedBranchPolicy(
                    project_id=self.pid,
                    version=2,
                    default_branch="main",
                    patterns=("release/*",),
                ),
            )
        protected = await self.create("release/stable")
        self.assertEqual(protected.publication_reason, "protected_branch")
        self.assertIsNone(await self.grant(protected.session_id))
        with patch.object(settings, "push_gate_enabled", False):
            disabled = await self.create("disabled")
            self.assertEqual(disabled.publication_reason, "disabled")
            self.assertIsNone(await self.grant(disabled.session_id))
            await workspaces.delete(disabled.session_id, self.user)
            self.assertIsNone(await db.get_session(disabled.session_id))
        await self.pool.execute(
            "UPDATE projects SET default_branch='' WHERE id=$1", self.pid
        )
        missing = await self.create("missing")
        self.assertEqual(missing.publication_reason, "missing_metadata")
        self.assertIsNone(await self.grant(missing.session_id))
        values = {w.session_id: w for w in await workspaces.list_for(self.user)}
        self.assertEqual(
            values[missing.session_id].model_dump(mode="json")["publication_mode"],
            "read_only",
        )

    async def test_runtime_replacement_and_stale_retry(self):
        ws = await self.create()
        sid = ws.session_id
        # Native values are immutable for this Create identity; replacement alone rotates.
        async with self.pool.acquire() as conn:
            issuance = await conn.fetchval(
                "SELECT issuance_id FROM git_enrollments WHERE binding_id=$1 AND revoked_at IS NULL",
                sid,
            )
            _, enrollment = await git_credentials.enrollment_row(conn, issuance)
            old = git_credentials.capability_for(enrollment.plan, "git-push")
            await lifecycle.enroll(conn, sid)
        old_id = (await ns.get_binding(sid))["kagent_session_id"]
        before = await self.grant(sid)
        await ns.ledger.update_binding(sid, kagent_session_id=old_id)
        self.assertEqual(
            (await self.grant(sid))["token_hash"],
            before["token_hash"],
            "same runtime confirmation does not rotate",
        )
        self.assertTrue(
            await ns.ledger.replace_kagent_session(sid, old_id, str(uuid.uuid4()))
        )
        self.assertIsNotNone((await self.grant(sid))["revoked_at"])
        # Unknown creation is not a grant; only refresh confirmation enrolls the replacement.
        self.assertEqual(
            (await workspaces.get(sid, self.user)).publication_reason, "no_grant"
        )
        await workspaces.refresh(sid, self.user)
        row = await self.grant(sid)
        self.assertEqual(store._decode(row["grant_data"], PushGrant).version, 2)
        self.assertNotEqual(row["token_hash"], store.token_hash(old))
        self.assertFalse(
            await ns.ledger.replace_kagent_session(sid, old_id, str(uuid.uuid4()))
        )
        self.assertIsNone((await self.grant(sid))["revoked_at"])
        async with self.pool.acquire() as conn:
            with self.assertRaisesRegex(ValueError, "grant_unavailable"):
                async with store.authorized(
                    conn, old, self.repo, [UPDATE], lambda *_: True
                ):
                    self.fail("old bearer revived")

    async def test_policy_default_update_protects_existing_grants(self):
        ws = await self.create()
        async with self.pool.acquire() as conn:
            issuance = await conn.fetchval(
                "SELECT issuance_id FROM git_enrollments WHERE binding_id=$1 AND revoked_at IS NULL",
                ws.session_id,
            )
            _, enrollment = await git_credentials.enrollment_row(conn, issuance)
            bearer = git_credentials.capability_for(enrollment.plan, "git-push")
            await store.set_policy(
                conn,
                ProtectedBranchPolicy(
                    project_id=self.pid,
                    version=2,
                    default_branch="main",
                    patterns=("feature",),
                ),
            )
            with self.assertRaisesRegex(ValueError, "protected_branch"):
                async with store.authorized(
                    conn,
                    bearer,
                    self.repo,
                    [UPDATE.model_copy(update={"ref": "refs/heads/feature"})],
                    lambda *_: True,
                ):
                    self.fail("policy override did not protect existing grant")
        self.assertEqual(
            (await workspaces.get(ws.session_id, self.user)).publication_reason,
            "protected_branch",
        )
        await db.update_project_metadata(self.pid, default_branch="feature")
        self.assertEqual(
            (await workspaces.get(ws.session_id, self.user)).publication_reason,
            "default_branch",
        )
        async with self.pool.acquire() as conn:
            policy = await store.load_policy(conn, self.pid)
        self.assertEqual(policy.version, 3)
        self.assertIn("main", policy.previous_defaults)
        self.assertEqual(
            (await self.create("main")).publication_reason, "default_branch"
        )

    async def test_revoke_lock_spans_each_durable_mutation(self):
        # Observe the ordered locks from another connection after revoke and again after
        # the caller's durable mutation, before its outer lock releases.
        original = lifecycle.locked
        for operation in (
            "credential",
            "terminal",
            "archive",
            "delete",
            "failed_create",
        ):
            with self.subTest(operation=operation):
                ws = await self.create(operation)
                sid = ws.session_id
                if operation == "archive":
                    await self.pool.execute(
                        "UPDATE sessions SET status='completed' WHERE id=$1", sid
                    )
                observed = []

                @asynccontextmanager
                async def inspect(
                    conn, session_id, *, revoke=False, sid=sid, observed=observed
                ):
                    async with original(conn, session_id, revoke=revoke):
                        yield
                        if session_id == sid and revoke:
                            async with self.pool.acquire() as other:
                                count = await other.fetchval(
                                    "SELECT count(*) FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND granted",
                                    conn.get_server_pid(),
                                )
                            self.assertGreaterEqual(count, 2)
                            self.assertIsNotNone(
                                await conn.fetchval(
                                    "SELECT revoked_at FROM push_grants WHERE id=$1",
                                    sid,
                                )
                            )
                            state = await conn.fetchrow(
                                "SELECT s.status,s.archived_at,n.token_hash FROM sessions s LEFT JOIN native_bindings n ON n.session_id=s.id WHERE s.id=$1",
                                sid,
                            )
                            observed.append(state)

                with patch.object(lifecycle, "locked", inspect):
                    if operation == "credential":
                        await agent_credentials.revoke(sid)
                    elif operation == "terminal":
                        await db.update_session(sid, status=SessionStatus.CANCELLED)
                    elif operation == "archive":
                        await db.archive_sessions(self.user, [sid])
                    elif operation == "delete":
                        await workspaces.delete(sid, self.user)
                    else:
                        await workspaces._delete_rows(sid)
                self.assertTrue(observed)
                if operation in ("delete", "failed_create"):
                    self.assertIsNone(observed[-1])
                elif operation == "archive":
                    self.assertIsNotNone(observed[0]["archived_at"])
                elif operation == "terminal":
                    self.assertEqual(observed[0]["status"], "cancelled")
                else:
                    self.assertIsNone(observed[0]["token_hash"])
                self.assertIsNotNone(await self.grant(sid), "audit survives deletion")

    async def test_authorizer_waits_for_terminal_durable_change(self):
        ws = await self.create()
        sid = ws.session_id
        reached, release = asyncio.Event(), asyncio.Event()
        original = lifecycle.revoke_locked

        async def pause(conn, grant_id):
            await original(conn, grant_id)
            reached.set()
            await release.wait()

        async with self.pool.acquire() as waiter:
            with patch.object(lifecycle, "revoke_locked", pause):
                mutation = asyncio.create_task(
                    db.update_session(sid, status=SessionStatus.COMPLETED)
                )
                await asyncio.wait_for(reached.wait(), 5)

                async def read_after_lock():
                    async with store.policy_lock(
                        waiter, self.pid
                    ), store.publication_lock(waiter, sid):
                        return await waiter.fetchval(
                            "SELECT status FROM sessions WHERE id=$1", sid
                        )

                reader = asyncio.create_task(read_after_lock())
                try:
                    for _ in range(200):
                        blocked = await self.pool.fetchval(
                            "SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND NOT granted)",
                            waiter.get_server_pid(),
                        )
                        if blocked:
                            break
                        await asyncio.sleep(0.01)
                    self.assertTrue(blocked)
                    self.assertFalse(reader.done())
                finally:
                    release.set()
                    await asyncio.wait_for(mutation, 5)
                self.assertEqual(await asyncio.wait_for(reader, 5), "completed")

    async def test_unknown_create_and_rejected_cleanup_never_issue(self):
        create = ns.get_client().create_session

        async def lose_reply(*args, **kwargs):
            await create(*args, **kwargs)
            raise OutcomeUnknown("sanitized unknown create")

        with patch.object(ns.get_client(), "create_session", lose_reply):
            ws = await self.create("unknown")
        self.assertIsNone(await self.grant(ws.session_id))
        self.assertEqual(ws.publication_reason, "no_grant")
        # A confirmed retry uses the frozen request identity before enrolling.
        await workspaces.refresh(ws.session_id, self.user)
        self.assertIsNotNone(await self.grant(ws.session_id))
        with patch.object(
            ns.get_client(),
            "create_session",
            side_effect=SessionError("rejected", grpc_status=3),
        ):
            with self.assertRaises(workspaces.WorkspaceRejected):
                await self.create("rejected")
        self.assertFalse(
            await self.pool.fetchval(
                "SELECT EXISTS(SELECT 1 FROM sessions WHERE project_id=$1 AND branch_name='rejected')",
                self.pid,
            )
        )

    async def test_refresh_revokes_missing_or_failed_runtime(self):
        for state in (RuntimeState.FAILED, RuntimeState.DELETED):
            ws = await self.create(state.name.lower())
            binding = await ns.get_binding(ws.session_id)
            self.fake.sessions[binding["kagent_session_id"]] = (
                state,
                RuntimeOperation.NONE,
            )
            result = await workspaces.refresh(ws.session_id, self.user)
            self.assertEqual(result.publication_reason, "no_grant")
            self.assertIsNotNone((await self.grant(ws.session_id))["revoked_at"])
        ws = await self.create("gone")
        binding = await ns.get_binding(ws.session_id)
        del self.fake.sessions[binding["kagent_session_id"]]
        self.assertEqual(
            (await workspaces.refresh(ws.session_id, self.user)).publication_reason,
            "no_grant",
        )
        self.assertIsNotNone((await self.grant(ws.session_id))["revoked_at"])

    async def test_api_projects_publication_independently_of_runtime_state(self):
        ws = await self.create()
        async with httpx.AsyncClient(
            transport=httpx.ASGITransport(app=api.app), base_url="http://test"
        ) as client:
            with patch.object(settings, "owner_id", self.user), patch.object(
                settings, "api_hosts", "test"
            ):
                for endpoint in (f"/workspaces/{ws.session_id}", "/workspaces"):
                    response = await client.get(endpoint)
                    self.assertEqual(response.status_code, 200, response.text)
                    body = response.json()
                    record = body[0] if isinstance(body, list) else body
                    self.assertEqual(record["publication_mode"], "branch")
                    self.assertIsNone(record["publication_reason"])
                await agent_credentials.revoke(ws.session_id)
                response = await client.post(f"/workspaces/{ws.session_id}/refresh")
                self.assertEqual(response.status_code, 200, response.text)
                self.assertEqual(response.json()["publication_reason"], "no_grant")
                self.assertEqual(response.json()["observed_state"], "running")

    async def test_delayed_old_runtime_observation_preserves_replacement_grant(self):
        for outcome in (
            None,
            RuntimeState.FAILED,
            RuntimeState.DELETING,
            RuntimeState.DELETED,
        ):
            with self.subTest(outcome=outcome):
                ws = await self.create(
                    "delayed-"
                    + (outcome.name.lower() if outcome is not None else "missing")
                )
                sid = ws.session_id
                old_id = (await ns.get_binding(sid))["kagent_session_id"]
                started, release = asyncio.Event(), asyncio.Event()
                get_session = ns.get_client().get_session

                async def delayed(
                    runtime_id,
                    old_id=old_id,
                    outcome=outcome,
                    started=started,
                    release=release,
                    get_session=get_session,
                ):
                    if runtime_id != old_id:
                        return await get_session(runtime_id)
                    old_session = await get_session(runtime_id)
                    started.set()
                    await release.wait()
                    if outcome is None:
                        raise SessionError("old runtime missing", grpc_status=5)
                    from dataclasses import replace

                    return replace(old_session, state=outcome)

                with patch.object(ns.get_client(), "get_session", delayed):
                    observation = asyncio.create_task(
                        workspaces._observe(old_id, workspace_id=sid)
                    )
                    try:
                        await asyncio.wait_for(started.wait(), 5)
                        self.assertTrue(
                            await ns.ledger.replace_kagent_session(
                                sid, old_id, str(uuid.uuid4())
                            )
                        )
                        self.fake.next_session_ids = [str(uuid.uuid4())]
                        issued = []
                        issue = store.issue_derived_locked

                        async def capture(
                            conn, grant, enrollment, issue=issue, issued=issued
                        ):
                            result = await issue(conn, grant, enrollment)
                            issued.append(
                                git_credentials.capability_for(
                                    enrollment.plan, "git-push"
                                )
                            )
                            return result

                        with patch.object(store, "issue_derived_locked", capture):
                            await workspaces.refresh(sid, self.user)
                        self.assertEqual(len(issued), 1)
                        bearer = issued[0]
                        before = await self.grant(sid)
                        replacement = store._decode(before["grant_data"], PushGrant)
                        self.assertEqual(replacement.version, 2)
                        self.assertNotEqual(replacement.runtime_identity, old_id)
                        release.set()
                        await asyncio.wait_for(observation, 5)
                        after = await self.grant(sid)
                        self.assertIsNone(after["revoked_at"])
                        self.assertEqual(after["token_hash"], before["token_hash"])
                        async with self.pool.acquire() as conn:
                            async with store.authorized(
                                conn,
                                bearer,
                                self.repo,
                                [
                                    UPDATE.model_copy(
                                        update={
                                            "ref": "refs/heads/" + ws.manifest.branch
                                        }
                                    )
                                ],
                                lambda *_: True,
                            ) as (grant, _):
                                self.assertEqual(grant.version, replacement.version)
                    finally:
                        release.set()
                        if not observation.done():
                            observation.cancel()
                        await asyncio.gather(observation, return_exceptions=True)
