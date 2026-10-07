"""Real PostgreSQL authority, ledger and cross-connection locking; fake ancestry."""

import asyncio

from mainloop.db.push_gate_schema import PUSH_GATE_MIGRATION_SQL
from mainloop.push_gate import store
from tests.runtime.test_postgres_ledger import PostgresTestCase
from tests.runtime.test_push_gate import GRANT, POLICY, UPDATE

from models.push_gate import PublicationAttempt, PublicationState


class PushStoreTests(PostgresTestCase):
    async def asyncSetUp(self):
        await super().asyncSetUp()
        self.sid, _ = await self.session()
        self.pid = "p-" + self.sid
        await self.pool.execute(
            """INSERT INTO projects(id,user_id,owner,name,full_name,html_url)
            VALUES($1,$2,'Owner','Repo','Owner/Repo','https://github.com/Owner/Repo')""",
            self.pid,
            self.user,
        )
        await self.pool.execute(
            "UPDATE sessions SET project_id=$2,repo_url='https://github.com/Owner/Repo',branch_name='Feature' WHERE id=$1",
            self.sid,
            self.pid,
        )
        await self.pool.execute(
            """INSERT INTO native_bindings(session_id,kind,role,mcp_grant_kind,token_hash,kagent_session_id)
            VALUES($1,'claude','agent','workspace',$2,'runtime')""",
            self.sid,
            "mcp-fixture-" + self.sid,
        )
        await self.pool.execute(
            "INSERT INTO workspaces(session_id,repo,branch) VALUES($1,'https://github.com/Owner/Repo','Feature')",
            self.sid,
        )
        self.grant = GRANT.model_copy(
            update={
                "id": self.sid,
                "owner_id": self.user,
                "project_id": self.pid,
                "session_id": self.sid,
                "workspace_id": self.sid,
            }
        )
        self.policy = POLICY.model_copy(update={"project_id": self.pid})
        async with self.pool.acquire() as conn:
            await store.set_policy(conn, self.policy)
            self.token = await store.issue(conn, self.grant)

    async def test_authentication_and_live_lifecycle(self):
        async with self.pool.acquire() as conn:
            async with store.authorized(
                conn, self.token, "owner/repo", [UPDATE], lambda *_: True
            ):
                pass
            for sql, reason in [
                (
                    "UPDATE native_bindings SET kagent_session_id='replaced' WHERE session_id=$1",
                    "runtime_mismatch",
                ),
                (
                    "UPDATE native_bindings SET kagent_session_id='runtime' WHERE session_id=$1",
                    None,
                ),
                (
                    "UPDATE sessions SET archived_at=now() WHERE id=$1",
                    "session_archived",
                ),
                (
                    "UPDATE sessions SET archived_at=NULL,status='completed' WHERE id=$1",
                    "session_terminal",
                ),
            ]:
                await conn.execute(sql, self.sid)
                if reason:
                    with self.assertRaisesRegex(ValueError, reason):
                        async with store.authorized(
                            conn, self.token, "owner/repo", [UPDATE], lambda *_: True
                        ):
                            self.fail("authorized invalid lifecycle")
            with self.assertRaisesRegex(ValueError, "grant_unavailable"):
                async with store.authorized(
                    conn,
                    "mcp-fixture-" + self.sid,
                    "owner/repo",
                    [UPDATE],
                    lambda *_: True,
                ):
                    self.fail("MCP bearer accepted")

    async def test_revocation_serializes_authorization(self):
        started = asyncio.Event()
        done = asyncio.Event()

        async def revoke():
            async with self.pool.acquire() as other:
                started.set()
                await store.revoke(other, self.grant.id)
                done.set()

        async with self.pool.acquire() as conn:
            async with store.authorized(
                conn, self.token, "owner/repo", [UPDATE], lambda *_: True
            ):
                task = asyncio.create_task(revoke())
                await started.wait()
                # Wait until PostgreSQL observes the blocked advisory lock, not a timing guess.
                for _ in range(100):
                    blocked = await conn.fetchval(
                        "SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted"
                    )
                    if blocked:
                        break
                    await asyncio.sleep(0.01)
                self.assertTrue(blocked)
                self.assertFalse(done.is_set())
            await asyncio.wait_for(task, 5)
            with self.assertRaisesRegex(ValueError, "grant_revoked"):
                async with store.authorized(
                    conn, self.token, "owner/repo", [UPDATE], lambda *_: True
                ):
                    self.fail("revoked token authorized")

    async def test_protected_issue_policy_history_and_migration(self):
        async with self.pool.acquire() as conn:
            await conn.execute(PUSH_GATE_MIGRATION_SQL)
            await conn.execute(
                "UPDATE projects SET default_branch='Feature' WHERE id=$1", self.pid
            )
            await store.set_policy(
                conn,
                self.policy.model_copy(
                    update={"version": 2, "default_branch": "Feature"}
                ),
            )
            with self.assertRaisesRegex(ValueError, "default_branch"):
                await store.issue(conn, self.grant)
            policy = await store.load_policy(conn, self.pid)
            self.assertIn("main", policy.previous_defaults)
            with self.assertRaisesRegex(ValueError, "policy_metadata_stale"):
                await store.set_policy(conn, self.policy)
            await store.revalidate_on_runtime_replacement(conn, self.grant.id)
            self.assertIsNotNone(
                await conn.fetchval(
                    "SELECT revoked_at FROM push_grants WHERE id=$1", self.grant.id
                )
            )

    async def test_ledger_identity_and_terminal_states(self):
        async with self.pool.acquire() as conn:
            for final in (
                PublicationState.UNKNOWN,
                PublicationState.CONFIRMED,
                PublicationState.REJECTED,
            ):
                attempt = PublicationAttempt(
                    request_id=final.value,
                    grant_id=self.grant.id,
                    repository="Owner/Repo",
                    update=UPDATE,
                    grant_version=1,
                    policy_version=1,
                )
                await store.record_attempt(conn, attempt)
                await store.record_attempt(conn, attempt)
                with self.assertRaisesRegex(ValueError, "request_identity_conflict"):
                    await store.record_attempt(
                        conn, attempt.model_copy(update={"repository": "other/repo"})
                    )
                await store.transition(
                    conn, self.grant.id, final.value, PublicationState.DISPATCHING
                )
                await store.transition(conn, self.grant.id, final.value, final)
                with self.assertRaisesRegex(ValueError, "invalid_transition"):
                    await store.transition(
                        conn, self.grant.id, final.value, PublicationState.PENDING
                    )

    async def test_issue_denies_wrong_identity_and_scope(self):
        async with self.pool.acquire() as conn:
            for changes in (
                {"owner_id": "other"},
                {"runtime_identity": "forged"},
                {"workspace_id": "other"},
                {"repository": "other/repo"},
                {"branch": "Other"},
            ):
                with self.subTest(changes=changes), self.assertRaises(ValueError):
                    await store.issue(conn, self.grant.model_copy(update=changes))
            await conn.execute(
                "UPDATE native_bindings SET role='main',mcp_grant_kind='coordination' WHERE session_id=$1",
                self.sid,
            )
            with self.assertRaisesRegex(ValueError, "grant_kind"):
                await store.issue(conn, self.grant)

    async def test_unknown_blocks_new_authorization(self):
        async with self.pool.acquire() as conn:
            attempt = PublicationAttempt(
                request_id="lost",
                grant_id=self.grant.id,
                repository="Owner/Repo",
                update=UPDATE,
                grant_version=1,
                policy_version=1,
            )
            await store.record_attempt(conn, attempt)
            await store.transition(
                conn, self.grant.id, "lost", PublicationState.DISPATCHING
            )
            await store.transition(
                conn, self.grant.id, "lost", PublicationState.UNKNOWN
            )
            with self.assertRaisesRegex(ValueError, "publication_unresolved"):
                async with store.authorized(
                    conn, self.token, "owner/repo", [UPDATE], lambda *_: True
                ):
                    self.fail("unknown was replayed")

    async def test_reissue_rotates_purpose_bearer_and_version(self):
        async with self.pool.acquire() as conn:
            await store.revalidate_on_runtime_replacement(conn, self.grant.id)
            await conn.execute(
                "UPDATE native_bindings SET kagent_session_id='replacement' WHERE session_id=$1",
                self.sid,
            )
            token = await store.issue(
                conn,
                self.grant.model_copy(
                    update={"version": 2, "runtime_identity": "replacement"}
                ),
            )
            self.assertNotEqual(self.token, token)
            with self.assertRaisesRegex(ValueError, "grant_unavailable"):
                async with store.authorized(
                    conn, self.token, "owner/repo", [UPDATE], lambda *_: True
                ):
                    self.fail("old bearer revived")
            async with store.authorized(
                conn, token, "owner/repo", [UPDATE], lambda *_: True
            ) as (grant, _):
                self.assertEqual(grant.version, 2)

    async def test_old_bearer_waiting_during_rotation_is_denied(self):
        async with self.pool.acquire() as holder, self.pool.acquire() as waiter:
            async with store.policy_lock(holder, self.pid):

                async def authorize_old():
                    async with store.authorized(
                        waiter, self.token, "owner/repo", [UPDATE], lambda *_: True
                    ) as (grant, _):
                        return grant.version

                task = asyncio.create_task(authorize_old())
                try:
                    for _ in range(200):
                        blocked = await holder.fetchval(
                            "SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND NOT granted AND pid=$1)",
                            waiter.get_server_pid(),
                        )
                        if blocked:
                            break
                        await asyncio.sleep(0.01)
                    self.assertTrue(
                        blocked, "old bearer must have resolved before rotation"
                    )
                    await store.revoke(holder, self.grant.id)
                    await holder.execute(
                        "UPDATE native_bindings SET kagent_session_id='replacement' WHERE session_id=$1",
                        self.sid,
                    )
                    replacement = await store.issue(
                        holder,
                        self.grant.model_copy(
                            update={"version": 2, "runtime_identity": "replacement"}
                        ),
                    )
                except BaseException:
                    task.cancel()
                    await asyncio.gather(task, return_exceptions=True)
                    raise
            with self.assertRaisesRegex(ValueError, "^grant_revoked$"):
                await asyncio.wait_for(task, 5)
            async with store.authorized(
                holder, replacement, "owner/repo", [UPDATE], lambda *_: True
            ) as (grant, _):
                self.assertEqual(grant.version, 2)
                self.assertEqual(grant.runtime_identity, "replacement")
