"""Real PostgreSQL authority, ledger and cross-connection locking; fake ancestry."""

import asyncio
import uuid
from unittest.mock import patch

from mainloop.config import settings
from mainloop.db import tasks as task_store
from mainloop.db.push_gate_schema import PUSH_GATE_MIGRATION_SQL
from mainloop.push_gate import lifecycle, store
from tests.runtime.test_postgres_ledger import PostgresTestCase
from tests.runtime.test_push_gate import GRANT, POLICY, UPDATE

from models.push_gate import PublicationAttempt, PublicationState, PushGrant, RefUpdate


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


class DelegatedWriterTests(PostgresTestCase):
    """supervisor/workspace and child/workspace writers need the same live attempt proof."""

    async def asyncSetUp(self):
        await super().asyncSetUp()
        self.pid = "p-" + uuid.uuid4().hex[:8]
        await self.pool.execute(
            """INSERT INTO projects(id,user_id,owner,name,full_name,html_url)
            VALUES($1,$2,'Owner','Repo','Owner/Repo','https://github.com/Owner/Repo')""",
            self.pid,
            self.user,
        )
        self.policy = POLICY.model_copy(update={"project_id": self.pid})
        async with self.pool.acquire() as conn:
            await store.set_policy(conn, self.policy)
        self.supervisor = await self.writer("supervisor", "feature/sup")
        self.child = await self.writer(
            "child", "feature/child-a", parent=self.supervisor
        )
        self.sibling = await self.writer(
            "child", "feature/child-b", parent=self.supervisor
        )

    async def writer(
        self, role, branch, *, parent=None, kind="workspace", claim=True, attempt=True
    ):
        """Seed a live delegated session, task, active attempt and held generation-1 claim."""
        sid, _ = await self.session()
        repo = "https://github.com/Owner/Repo"
        await self.pool.execute(
            "UPDATE sessions SET project_id=$2,repo_url=$3,branch_name=$4 WHERE id=$1",
            sid,
            self.pid,
            repo,
            branch,
        )
        await self.pool.execute(
            """INSERT INTO native_bindings(session_id,kind,role,mcp_grant_kind,token_hash,kagent_session_id)
            VALUES($1,'claude',$2,$3,$4,$5)""",
            sid,
            role,
            kind,
            "mcp-" + sid,
            "rt-" + sid,
        )
        await self.pool.execute(
            "INSERT INTO workspaces(session_id,repo,branch) VALUES($1,$2,$3)",
            sid,
            repo,
            branch,
        )
        task_id, attempt_id = uuid.uuid4().hex, uuid.uuid4().hex
        info = {
            "sid": sid,
            "task": task_id,
            "attempt": attempt_id,
            "branch": branch,
            "role": role,
            "kind": kind,
            "root": parent["root"] if parent else task_id,
            "parent": parent["task"] if parent else None,
        }
        async with self.pool.acquire() as conn, conn.transaction():
            await conn.execute(
                """INSERT INTO tasks(id,owner_id,project_id,parent_task_id,root_task_id,mode,status,
                    current_attempt_id,version,snapshot) VALUES($1,$2,$3,$4,$5,'code','running',$6,1,'{}')""",
                task_id,
                self.user,
                self.pid,
                info["parent"],
                info["root"],
                attempt_id if attempt else None,
            )
            if attempt:
                await conn.execute(
                    """INSERT INTO task_attempts(id,task_id,number,profile_id,native_provider,
                        configuration_revision,agent_ref,role,depth,state,session_id,binding_id,
                        workspace_id,writer_generation,snapshot)
                    VALUES($1,$2,1,'p','claude','r','{}',$3,$4,'active',$5,$5,$5,$6,'{}')""",
                    attempt_id,
                    task_id,
                    role,
                    1 if role == "supervisor" else 2,
                    sid,
                    1 if claim else None,
                )
            if claim and attempt:
                await conn.execute(
                    """INSERT INTO workspace_writer_claims(owner_id,repository,branch,generation,attempt_id)
                    VALUES($1,'owner/repo',$2,1,$3)""",
                    self.user,
                    branch,
                    attempt_id,
                )
        return info

    def grant(self, info, **changes):
        return PushGrant(
            id=info["sid"],
            owner_id=self.user,
            project_id=self.pid,
            repository="Owner/Repo",
            branch=info["branch"],
            workspace_id=info["sid"],
            session_id=info["sid"],
            runtime_identity="rt-" + info["sid"],
            role=info["role"],
            grant_kind=info["kind"],
            **changes,
        )

    async def authorize(self, conn, token, info):
        update = RefUpdate(
            ref="refs/heads/" + info["branch"], old_oid="a" * 40, new_oid="b" * 40
        )
        async with store.authorized(
            conn, token, "owner/repo", [update], lambda *_: True
        ) as (grant, _):
            return grant

    async def issue(self, conn, info):
        return await store.issue(conn, self.grant(info))

    async def assert_denied(self, conn, token, info, reason):
        with self.assertRaisesRegex(ValueError, f"^{reason}$"):
            await self.authorize(conn, token, info)

    async def test_supervisor_and_child_writers_are_authorized(self):
        async with self.pool.acquire() as conn:
            for info in (self.supervisor, self.child, self.sibling):
                token = await self.issue(conn, info)
                grant = await self.authorize(conn, token, info)
                self.assertEqual(
                    (grant.role, grant.grant_kind), (info["role"], "workspace")
                )

    async def test_enroll_issues_delegated_roles_and_never_coordination(self):
        coordination = await self.writer(
            "child", "feature/coord", parent=self.supervisor, kind="coordination"
        )
        main = await self.writer(
            "main", "feature/main", kind="coordination", attempt=False
        )
        with patch.object(settings, "push_gate_enabled", True):
            async with self.pool.acquire() as conn:
                for info in (self.supervisor, self.child):
                    token = await lifecycle.enroll(conn, info["sid"])
                    self.assertTrue(token)
                    grant = await self.authorize(conn, token, info)
                    self.assertEqual(grant.role, info["role"])
                for info in (coordination, main):
                    self.assertIsNone(await lifecycle.enroll(conn, info["sid"]))
                    self.assertIsNone(
                        await conn.fetchval(
                            "SELECT id FROM push_grants WHERE id=$1", info["sid"]
                        )
                    )
                    self.assertEqual(
                        await lifecycle.projection(conn, info["sid"]),
                        ("read_only", "no_grant"),
                    )
                self.assertEqual(
                    await lifecycle.projection(conn, self.child["sid"]),
                    ("branch", None),
                )

    async def test_coordination_grants_never_hold_push_authority(self):
        coordination = {
            "supervisor": await self.writer(
                "supervisor", "feature/sc", kind="coordination"
            ),
            "child": await self.writer(
                "child", "feature/cc", parent=self.supervisor, kind="coordination"
            ),
            "main": await self.writer(
                "main", "feature/mc", kind="coordination", attempt=False
            ),
        }
        async with self.pool.acquire() as conn:
            for role, info in coordination.items():
                with self.subTest(role=role):
                    # Neither the true pair nor a grant forged as a workspace writer qualifies.
                    with self.assertRaisesRegex(ValueError, "^grant_kind$"):
                        await self.issue(conn, info)
                    with self.assertRaisesRegex(ValueError, "^grant_kind$"):
                        await store.issue(
                            conn,
                            self.grant(info, version=1).model_copy(
                                update={"grant_kind": "workspace"}
                            ),
                        )
            # A workspace writer whose live binding is later downgraded loses authority.
            token = await self.issue(conn, self.child)
            await conn.execute(
                "UPDATE native_bindings SET mcp_grant_kind='coordination' WHERE session_id=$1",
                self.child["sid"],
            )
            await self.assert_denied(conn, token, self.child, "grant_kind")

    async def test_grant_role_must_match_binding(self):
        async with self.pool.acquire() as conn:
            with self.assertRaisesRegex(ValueError, "^grant_kind$"):
                await store.issue(
                    conn,
                    self.grant(self.child).model_copy(update={"role": "supervisor"}),
                )
            token = await self.issue(conn, self.child)
            await conn.execute(
                "UPDATE native_bindings SET role='supervisor' WHERE session_id=$1",
                self.child["sid"],
            )
            await self.assert_denied(conn, token, self.child, "grant_kind")
            await conn.execute(
                "UPDATE native_bindings SET role='agent' WHERE session_id=$1",
                self.child["sid"],
            )
            # An owner-workspace `agent` binding cannot borrow a delegated attempt.
            await self.assert_denied(conn, token, self.child, "grant_kind")

    async def test_missing_attempt_or_claim_confers_nothing(self):
        orphan = await self.writer(
            "child", "feature/orphan", parent=self.supervisor, attempt=False
        )
        unclaimed = await self.writer(
            "child", "feature/unclaimed", parent=self.supervisor, claim=False
        )
        async with self.pool.acquire() as conn:
            with self.assertRaisesRegex(ValueError, "^attempt_unavailable$"):
                await self.issue(conn, orphan)
            with self.assertRaisesRegex(ValueError, "^writer_claim_lost$"):
                await self.issue(conn, unclaimed)

    async def test_revoked_source_is_denied_after_cutover(self):
        async with self.pool.acquire() as conn:
            token = await self.issue(conn, self.child)
            await self.authorize(conn, token, self.child)
            # Reassignment: the source fences, releases its claim and a successor claims it.
            await conn.execute(
                "UPDATE task_attempts SET state='fenced' WHERE id=$1",
                self.child["attempt"],
            )
            async with conn.transaction():
                await task_store.release_writer(
                    conn,
                    owner_id=self.user,
                    repository="owner/repo",
                    branch=self.child["branch"],
                    generation=1,
                    attempt_id=self.child["attempt"],
                    fence_evidence_ref="evidence",
                )
            successor = uuid.uuid4().hex
            async with conn.transaction():
                await conn.execute(
                    """INSERT INTO task_attempts(id,task_id,number,profile_id,native_provider,
                        configuration_revision,agent_ref,role,depth,state,snapshot)
                    VALUES($1,$2,2,'p','claude','r','{}','child',2,'creating','{}')""",
                    successor,
                    self.child["task"],
                )
                await conn.execute(
                    "UPDATE tasks SET current_attempt_id=$2 WHERE id=$1",
                    self.child["task"],
                    successor,
                )
                self.assertEqual(
                    await task_store.reserve_writer(
                        conn,
                        owner_id=self.user,
                        repository="owner/repo",
                        branch=self.child["branch"],
                        attempt_id=successor,
                    ),
                    2,
                )
            await self.assert_denied(conn, token, self.child, "attempt_not_current")
            # Reissue cannot revive the superseded source either.
            await store.revoke(conn, self.child["sid"])
            with self.assertRaisesRegex(ValueError, "^attempt_not_current$"):
                await store.issue(conn, self.grant(self.child, version=2))

    async def test_lifecycle_revocation_and_terminal_task(self):
        async with self.pool.acquire() as conn:
            token = await self.issue(conn, self.supervisor)
            await conn.execute(
                "UPDATE tasks SET status='completed' WHERE id=$1",
                self.supervisor["task"],
            )
            await self.assert_denied(conn, token, self.supervisor, "task_terminal")
            await conn.execute(
                "UPDATE tasks SET status='running' WHERE id=$1", self.supervisor["task"]
            )
            await self.authorize(conn, token, self.supervisor)
            await conn.execute(
                "UPDATE task_attempts SET state='draining' WHERE id=$1",
                self.supervisor["attempt"],
            )
            await self.assert_denied(
                conn, token, self.supervisor, "attempt_not_current"
            )
            await conn.execute(
                "UPDATE task_attempts SET state='active' WHERE id=$1",
                self.supervisor["attempt"],
            )
            await conn.execute(
                "UPDATE native_bindings SET token_hash=NULL WHERE session_id=$1",
                self.supervisor["sid"],
            )
            await self.assert_denied(conn, token, self.supervisor, "grant_revoked")
            async with lifecycle.locked(conn, self.child["sid"], revoke=True):
                pass

    async def test_stale_generation_is_denied(self):
        async with self.pool.acquire() as conn:
            token = await self.issue(conn, self.child)
            claim = dict(
                owner_id=self.user,
                repository="owner/repo",
                branch=self.child["branch"],
            )
            # Claim lost: released with the attempt still marked active.
            await conn.execute(
                "UPDATE workspace_writer_claims SET held=FALSE,attempt_id=NULL WHERE branch=$1",
                self.child["branch"],
            )
            await self.assert_denied(conn, token, self.child, "writer_claim_lost")
            # Generation advanced under the same attempt: the attempt's recorded one is stale.
            async with conn.transaction():
                self.assertEqual(
                    await task_store.reserve_writer(
                        conn, attempt_id=self.child["attempt"], **claim
                    ),
                    2,
                )
            await self.assert_denied(conn, token, self.child, "stale_writer_generation")
            await conn.execute(
                "UPDATE task_attempts SET writer_generation=2 WHERE id=$1",
                self.child["attempt"],
            )
            # The older bearer stays stale even once the attempt records the new generation.
            await self.assert_denied(conn, token, self.child, "stale_writer_generation")
            await store.revoke(conn, self.child["sid"])
            token = await store.issue(conn, self.grant(self.child, version=2))
            await self.authorize(conn, token, self.child)
            await conn.execute(
                "UPDATE task_attempts SET writer_generation=NULL WHERE id=$1",
                self.child["attempt"],
            )
            await self.assert_denied(conn, token, self.child, "stale_writer_generation")

    async def test_sibling_and_parent_cannot_use_each_others_authority(self):
        async with self.pool.acquire() as conn:
            child_token = await self.issue(conn, self.child)
            sibling_token = await self.issue(conn, self.sibling)
            supervisor_token = await self.issue(conn, self.supervisor)
            # Each bearer pushes only its own exact branch.
            for token, own, other in (
                (child_token, self.child, self.sibling),
                (sibling_token, self.sibling, self.child),
                (supervisor_token, self.supervisor, self.child),
            ):
                await self.authorize(conn, token, own)
                await self.assert_denied(conn, token, other, "branch_mismatch")
            # A grant cannot be minted for another writer's branch or attempt.
            for forged in (
                self.grant(self.child).model_copy(
                    update={"branch": self.sibling["branch"]}
                ),
                self.grant(self.supervisor).model_copy(
                    update={"branch": self.child["branch"]}
                ),
            ):
                await store.revoke(conn, forged.id)
                with self.assertRaisesRegex(ValueError, "^binding_mismatch$"):
                    await store.issue(conn, forged.model_copy(update={"version": 2}))

    async def mutate(self, sql, *args):
        """Apply a change the immutability triggers forbid, to simulate corruption."""
        async with self.pool.acquire() as conn, conn.transaction():
            await conn.execute("SET LOCAL session_replication_role = replica")
            await conn.execute(sql, *args)

    async def test_attempt_scope_must_match_grant_and_binding(self):
        other_project = "p-" + uuid.uuid4().hex[:8]
        await self.pool.execute(
            """INSERT INTO projects(id,user_id,owner,name,full_name,html_url)
            VALUES($1,$2,'Owner','Other','Owner/Other','https://github.com/Owner/Other')""",
            other_project,
            self.user,
        )
        cases = (
            ("UPDATE tasks SET owner_id='someone-else' WHERE id=$1", "task", ()),
            ("UPDATE tasks SET project_id=$2 WHERE id=$1", "task", (other_project,)),
            ("UPDATE tasks SET mode='coordination' WHERE id=$1", "task", ()),
            (
                "UPDATE task_attempts SET role='child',depth=2 WHERE id=$1",
                "attempt",
                (),
            ),
        )
        for index, (sql, key, args) in enumerate(cases):
            with self.subTest(sql=sql):
                info = await self.writer("supervisor", f"feature/scope-{index}")
                async with self.pool.acquire() as conn:
                    token = await self.issue(conn, info)
                    await self.authorize(conn, token, info)
                    await self.mutate(sql, info[key], *args)
                    await self.assert_denied(conn, token, info, "attempt_scope")
                    with self.assertRaisesRegex(ValueError, "^attempt_scope$"):
                        await store.issue(conn, self.grant(info, version=2))

    async def test_superseded_by_other_current_attempt_while_active(self):
        async with self.pool.acquire() as conn:
            token = await self.issue(conn, self.child)
            await self.authorize(conn, token, self.child)
            other = uuid.uuid4().hex
            await conn.execute(
                """INSERT INTO task_attempts(id,task_id,number,profile_id,native_provider,
                    configuration_revision,agent_ref,role,depth,state,snapshot)
                VALUES($1,$2,2,'p','claude','r','{}','child',2,'superseded','{}')""",
                other,
                self.child["task"],
            )
            await conn.execute(
                "UPDATE tasks SET current_attempt_id=$2 WHERE id=$1",
                self.child["task"],
                other,
            )
            self.assertEqual(
                await conn.fetchval(
                    "SELECT state FROM task_attempts WHERE id=$1", self.child["attempt"]
                ),
                "active",
            )
            await self.assert_denied(conn, token, self.child, "attempt_not_current")

    async def test_issue_rejects_attempt_and_claim_generation_disagreement(self):
        async with self.pool.acquire() as conn:
            fence = self.child
            await conn.execute(
                "UPDATE task_attempts SET state='fenced' WHERE id=$1", fence["attempt"]
            )
            async with conn.transaction():
                await task_store.release_writer(
                    conn,
                    owner_id=self.user,
                    repository="owner/repo",
                    branch=fence["branch"],
                    generation=1,
                    attempt_id=fence["attempt"],
                    fence_evidence_ref="evidence",
                )
            await conn.execute(
                "UPDATE task_attempts SET state='active' WHERE id=$1", fence["attempt"]
            )
            async with conn.transaction():
                await task_store.reserve_writer(
                    conn,
                    owner_id=self.user,
                    repository="owner/repo",
                    branch=fence["branch"],
                    attempt_id=fence["attempt"],
                )
            # The same attempt holds generation 2 but still records generation 1.
            with self.assertRaisesRegex(ValueError, "^stale_writer_generation$"):
                await self.issue(conn, fence)
            self.assertIsNone(
                await conn.fetchval(
                    "SELECT id FROM push_grants WHERE id=$1", fence["sid"]
                )
            )

    async def test_claim_held_by_binding_only_is_not_the_attempts_claim(self):
        async with self.pool.acquire() as conn:
            info = self.child
            await conn.execute(
                "UPDATE task_attempts SET state='fenced' WHERE id=$1", info["attempt"]
            )
            async with conn.transaction():
                await task_store.release_writer(
                    conn,
                    owner_id=self.user,
                    repository="owner/repo",
                    branch=info["branch"],
                    generation=1,
                    attempt_id=info["attempt"],
                    fence_evidence_ref="evidence",
                )
            await conn.execute(
                "UPDATE task_attempts SET state='active' WHERE id=$1", info["attempt"]
            )
            async with conn.transaction():
                generation = await task_store.reserve_writer(
                    conn,
                    owner_id=self.user,
                    repository="owner/repo",
                    branch=info["branch"],
                    binding_id=info["sid"],
                )
            # The attempt's recorded generation matches the claim, but a binding holds it.
            await conn.execute(
                "UPDATE task_attempts SET writer_generation=$2 WHERE id=$1",
                info["attempt"],
                generation,
            )
            with self.assertRaisesRegex(ValueError, "^writer_claim_lost$"):
                await self.issue(conn, info)

    async def test_proof_columns_are_the_authority(self):
        tampering = (
            (
                "UPDATE push_grants SET attempt_id='other' WHERE id=$1",
                "attempt_not_current",
            ),
            (
                "UPDATE push_grants SET writer_generation=7 WHERE id=$1",
                "stale_writer_generation",
            ),
            (
                "UPDATE push_grants SET attempt_id=NULL,writer_generation=NULL WHERE id=$1",
                "attempt_not_current",
            ),
        )
        async with self.pool.acquire() as conn:
            token = await self.issue(conn, self.child)
            for sql, reason in tampering:
                original = await conn.fetchrow(
                    "SELECT attempt_id,writer_generation FROM push_grants WHERE id=$1",
                    self.child["sid"],
                )
                await conn.execute(sql, self.child["sid"])
                await self.assert_denied(conn, token, self.child, reason)
                with patch.object(settings, "push_gate_enabled", True):
                    self.assertEqual(
                        await lifecycle.projection(conn, self.child["sid"]),
                        ("read_only", "no_grant"),
                    )
                await conn.execute(
                    "UPDATE push_grants SET attempt_id=$2,writer_generation=$3 WHERE id=$1",
                    self.child["sid"],
                    original["attempt_id"],
                    original["writer_generation"],
                )
            await self.authorize(conn, token, self.child)
            with patch.object(settings, "push_gate_enabled", True):
                self.assertEqual(
                    await lifecycle.projection(conn, self.child["sid"]),
                    ("branch", None),
                )

    async def test_pair_check_is_installed_on_existing_tables(self):
        async with self.pool.acquire() as conn:
            await conn.execute(
                "ALTER TABLE push_grants DROP CONSTRAINT push_grants_writer_proof_pair"
            )
            await conn.execute(
                "ALTER TABLE push_grants DROP COLUMN attempt_id, DROP COLUMN writer_generation"
            )
            await conn.execute(PUSH_GATE_MIGRATION_SQL)
            await conn.execute(PUSH_GATE_MIGRATION_SQL)
            with self.assertRaises(Exception) as caught:
                await conn.execute(
                    """INSERT INTO push_grants(id,token_hash,owner_id,project_id,session_id,
                        grant_data,attempt_id) VALUES('x','x',$1,$2,'x','{}','a')""",
                    self.user,
                    self.pid,
                )
            self.assertIn("push_grants_writer_proof_pair", str(caught.exception))

    async def test_issue_records_live_attempt_and_generation(self):
        async with self.pool.acquire() as conn:
            token = await self.issue(conn, self.child)
            grant = await self.authorize(conn, token, self.child)
            self.assertEqual(
                (grant.attempt_id, grant.writer_generation),
                (self.child["attempt"], 1),
            )
            row = await conn.fetchrow(
                "SELECT attempt_id,writer_generation FROM push_grants WHERE id=$1",
                self.child["sid"],
            )
            self.assertEqual(tuple(row), (self.child["attempt"], 1))
            agent = await self.writer("agent", "feature/agent2", attempt=False)
            await self.issue(conn, agent)
            row = await conn.fetchrow(
                "SELECT attempt_id,writer_generation FROM push_grants WHERE id=$1",
                agent["sid"],
            )
            self.assertEqual(tuple(row), (None, None))

    async def test_caller_cannot_forge_attempt_or_generation(self):
        async with self.pool.acquire() as conn:
            for changes, reason in (
                (
                    {"attempt_id": self.sibling["attempt"], "writer_generation": 1},
                    "attempt_not_current",
                ),
                (
                    {"attempt_id": self.child["attempt"], "writer_generation": 2},
                    "stale_writer_generation",
                ),
            ):
                with self.subTest(changes=changes):
                    with self.assertRaisesRegex(ValueError, f"^{reason}$"):
                        await store.issue(conn, self.grant(self.child, **changes))
            agent = await self.writer("agent", "feature/agent3", attempt=False)
            forged = self.grant(agent).model_copy(
                update={"attempt_id": self.child["attempt"], "writer_generation": 1}
            )
            with self.assertRaisesRegex(ValueError, "^attempt_scope$"):
                await store.issue(conn, forged)

    async def test_stored_proof_is_required_and_compared(self):
        tampering = (
            (
                "UPDATE push_grants SET grant_data=jsonb_set(grant_data,'{attempt_id}','\"other\"') WHERE id=$1",
                "attempt_not_current",
            ),
            (
                "UPDATE push_grants SET grant_data=jsonb_set(grant_data,'{writer_generation}','7') WHERE id=$1",
                "stale_writer_generation",
            ),
            (
                "UPDATE push_grants SET grant_data=grant_data-'attempt_id'-'writer_generation' WHERE id=$1",
                "attempt_not_current",
            ),
        )
        async with self.pool.acquire() as conn:
            token = await self.issue(conn, self.child)
            for sql, reason in tampering:
                original = await conn.fetchval(
                    "SELECT grant_data::text FROM push_grants WHERE id=$1",
                    self.child["sid"],
                )
                await conn.execute(sql, self.child["sid"])
                await self.assert_denied(conn, token, self.child, reason)
                await conn.execute(
                    "UPDATE push_grants SET grant_data=$2::jsonb WHERE id=$1",
                    self.child["sid"],
                    original,
                )
            await self.authorize(conn, token, self.child)

    async def test_same_attempt_retaking_claim_denies_older_bearer(self):
        async with self.pool.acquire() as conn:
            old_token = await self.issue(conn, self.child)
            await self.authorize(conn, old_token, self.child)
            # The attempt fences, releases the claim and re-takes it at generation 2, and the
            # attempt's recorded generation is updated; the bearer is not revoked.
            await conn.execute(
                "UPDATE task_attempts SET state='fenced' WHERE id=$1",
                self.child["attempt"],
            )
            async with conn.transaction():
                await task_store.release_writer(
                    conn,
                    owner_id=self.user,
                    repository="owner/repo",
                    branch=self.child["branch"],
                    generation=1,
                    attempt_id=self.child["attempt"],
                    fence_evidence_ref="evidence",
                )
            await conn.execute(
                "UPDATE task_attempts SET state='active' WHERE id=$1",
                self.child["attempt"],
            )
            async with conn.transaction():
                self.assertEqual(
                    await task_store.reserve_writer(
                        conn,
                        owner_id=self.user,
                        repository="owner/repo",
                        branch=self.child["branch"],
                        attempt_id=self.child["attempt"],
                    ),
                    2,
                )
            await conn.execute(
                "UPDATE task_attempts SET writer_generation=2 WHERE id=$1",
                self.child["attempt"],
            )
            await self.assert_denied(
                conn, old_token, self.child, "stale_writer_generation"
            )
            # Only a reissue after revocation, bound to generation 2, restores authority.
            await store.revoke(conn, self.child["sid"])
            new_token = await store.issue(conn, self.grant(self.child, version=2))
            grant = await self.authorize(conn, new_token, self.child)
            self.assertEqual((grant.version, grant.writer_generation), (2, 2))
            await self.assert_denied(conn, old_token, self.child, "grant_unavailable")

    async def test_agent_workspace_behaviour_is_unchanged(self):
        agent = await self.writer("agent", "feature/agent", attempt=False)
        async with self.pool.acquire() as conn:
            token = await self.issue(conn, agent)
            grant = await self.authorize(conn, token, agent)
            self.assertEqual((grant.role, grant.grant_kind), ("agent", "workspace"))
            # Delegated attempts do not change owner workspaces, and agents never need them.
            await conn.execute(
                "UPDATE native_bindings SET role='child' WHERE session_id=$1",
                agent["sid"],
            )
            await self.assert_denied(conn, token, agent, "grant_kind")

    async def test_parent_revocation_or_drain_denies_old_and_new_child_grants(self):
        async with self.pool.acquire() as conn:
            token = await self.issue(conn, self.child)
            await conn.execute(
                "UPDATE native_bindings SET token_hash=NULL WHERE session_id=$1",
                self.supervisor["sid"],
            )
            with self.assertRaises(ValueError):
                await self.authorize(conn, token, self.child)
            with self.assertRaises(ValueError):
                await self.issue(conn, self.child)
            await conn.execute(
                "UPDATE native_bindings SET token_hash=$2 WHERE session_id=$1",
                self.supervisor["sid"],
                "mcp-" + self.supervisor["sid"],
            )
            await conn.execute(
                "UPDATE task_attempts SET state='draining' WHERE id=$1",
                self.supervisor["attempt"],
            )
            with self.assertRaises(ValueError):
                await self.authorize(conn, token, self.child)
            with self.assertRaises(ValueError):
                await self.issue(conn, self.child)

    async def test_forged_child_root_and_parent_claim_loss_deny_publication(self):
        unrelated = await self.writer("supervisor", "feature/unrelated")
        async with self.pool.acquire() as conn:
            token = await self.issue(conn, self.child)
            async with conn.transaction():
                await conn.execute("SET LOCAL session_replication_role = replica")
                await conn.execute(
                    "UPDATE tasks SET root_task_id=$2 WHERE id=$1",
                    self.child["task"],
                    unrelated["task"],
                )
            with self.assertRaisesRegex(ValueError, "attempt_ancestry"):
                await self.authorize(conn, token, self.child)
            with self.assertRaisesRegex(ValueError, "attempt_ancestry"):
                await self.issue(conn, self.child)
            async with conn.transaction():
                await conn.execute("SET LOCAL session_replication_role = replica")
                await conn.execute(
                    "UPDATE tasks SET root_task_id=$2 WHERE id=$1",
                    self.child["task"],
                    self.supervisor["task"],
                )
                await conn.execute(
                    "UPDATE workspace_writer_claims SET held=FALSE WHERE attempt_id=$1",
                    self.supervisor["attempt"],
                )
            with self.assertRaises(ValueError):
                await self.authorize(conn, token, self.child)
            with self.assertRaises(ValueError):
                await self.issue(conn, self.child)
