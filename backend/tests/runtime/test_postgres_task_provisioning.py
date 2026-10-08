"""S1 admission, runtime races and authority on PostgreSQL 16 with fake kagent."""

import asyncio
import uuid
from contextlib import asynccontextmanager
from dataclasses import replace
from functools import partial
from unittest.mock import AsyncMock, Mock, patch

import httpx
from fastapi import HTTPException
from mainloop import api
from mainloop.config import settings
from mainloop.db import db, environments
from mainloop.db import tasks as store
from mainloop.identity import current_user
from mainloop.providers import (
    TASK_CODE_CAPABILITIES,
    TASK_REQUIRED_CAPABILITIES,
    registry,
)
from mainloop.push_gate import store as push_store
from mainloop.runtime import native_sessions as ns
from mainloop.runtime import preview_proxy, workspaces
from mainloop.runtime.agent_credentials import revoke
from mainloop.runtime.agent_identity import token_for
from mainloop.runtime.agent_tools import AgentService
from mainloop.runtime.delegation import PgStore
from mainloop.runtime.kagent_client import (
    OutcomeUnknown,
    RuntimeOperation,
    RuntimeState,
    ServiceConfigurationError,
    SessionError,
    decode_fields,
)
from mainloop.runtime.policy import PolicyError
from mainloop.services.workspace_authority import (
    ScopeUnavailable,
    resolve_project_authority,
)
from mainloop.tasks import lifecycle, provisioning
from mainloop.tasks.principal import TaskPrincipal
from mainloop.tasks.service import mutate, ports, select_profile
from tests.runtime.test_postgres_ledger import KagentFakeCase
from tests.test_workspace_environments import validated_version

from models import WorkspaceManifest
from models.environment import DevEnvironment, SelectEnvironment
from models.provider import CapabilityResult
from models.push_gate import ProtectedBranchPolicy, PushGrant, RefUpdate
from models.task import TaskAction, TaskCheckout, TaskCreate


class RejectedCreateCrash(BaseException):
    """Process-loss injection: bypass provisioning's ordinary error recovery."""


class TaskProvisioningPostgresTests(KagentFakeCase):
    async def asyncSetUp(self):
        await super().asyncSetUp()
        await self.pool.execute(
            "TRUNCATE tasks,task_attempts,workspace_writer_claims,task_operations CASCADE"
        )
        self.fake.next_session_ids = [str(uuid.uuid4()) for _ in range(12)]
        self.project = "p-" + self.user
        await self.pool.execute(
            """INSERT INTO projects(id,user_id,owner,name,full_name,html_url,default_branch)
               VALUES($1,$2,'example','app','example/app','https://github.com/example/app','main')""",
            self.project,
            self.user,
        )
        self.env_id, self.version_id = "env-" + self.user, "v-" + self.user
        version = validated_version().model_copy(
            update={"id": self.version_id, "environment_id": self.env_id}
        )
        async with self.pool.acquire() as conn, conn.transaction():
            await environments.register(
                conn,
                DevEnvironment(
                    id=self.env_id,
                    owner_id=self.user,
                    name="fixture",
                    source_kind="prebuilt_image",
                ),
                version,
            )
            await environments.set_default(
                conn, self.env_id, self.user, self.version_id
            )
            await environments.select(
                conn,
                self.project,
                self.user,
                SelectEnvironment(
                    environment_id=self.env_id, follow_default=True, expected_version=0
                ),
            )
        profiles = [
            p.model_copy(
                update={
                    "capabilities": tuple(
                        CapabilityResult(
                            capability=name,
                            state="proved",
                            scope="fixture",
                            evidence_ref="fixture:s1",
                        )
                        for name in sorted(
                            TASK_REQUIRED_CAPABILITIES | TASK_CODE_CAPABILITIES
                        )
                    )
                }
            )
            for p in registry().profiles
        ]
        # Validate dictionaries after copying: fixture evidence is explicit, never production proof.
        profiles = [type(p).model_validate(p.model_dump()) for p in profiles]
        self.worker = provisioning.Provisioning()
        self.saved_port = ports.provisioning
        ports.provisioning = self.worker
        self.spawn = Mock()
        for patcher in (
            patch.object(settings, "provider_profiles", profiles),
            patch.object(settings, "api_hosts", "localhost"),
            patch.dict("os.environ", {"WORKSPACE_DEVELOPMENT_PLATFORM": "linux/arm64"}),
            patch.object(
                provisioning,
                "select_profile",
                partial(select_profile, allow_fixture=True),
            ),
            patch.object(ns, "_spawn_deliver", self.spawn),
        ):
            patcher.start()
            self.addCleanup(patcher.stop)
        self.owner = TaskPrincipal(self.user)

    async def asyncTearDown(self):
        ports.provisioning = self.saved_port
        await super().asyncTearDown()

    def request(self, *, branch=None, mode="code", provider=None, key=None):
        return TaskCreate(
            request_id=key or uuid.uuid4().hex,
            title="Fixture task",
            brief="Work on this branch",
            mode=mode,
            project_id=self.project,
            provider_profile_id=provider,
            checkout=(
                TaskCheckout(
                    branch=branch or "feature/" + uuid.uuid4().hex, ref="a" * 40
                )
                if mode == "code"
                else None
            ),
        )

    async def create_task(self, request=None, principal=None, ready=True):
        async with self.pool.acquire() as conn, conn.transaction():
            operation = await mutate(
                conn, principal or self.owner, "create", request or self.request()
            )
        if ready:
            await self.worker.reconcile(db, operation)
        async with self.pool.acquire() as conn:
            return (
                operation,
                await lifecycle.load_task(conn, operation.task_id),
                await lifecycle.load_attempt(conn, operation.attempt_id),
            )

    async def principal(self, attempt):
        binding = await PgStore().binding_by_token_hash(
            (await ns.get_binding(attempt.session_id))["token_hash"]
        )
        async with self.pool.acquire() as conn:
            return await lifecycle.authenticate_binding(conn, binding)

    @asynccontextmanager
    async def observed_parent_revoke(self, sid):
        """Expose the revoker connection before it requests its authority locks."""
        original = provisioning.push_lifecycle.locked
        observed = {}

        @asynccontextmanager
        async def locked(conn, session_id, **kwargs):
            if session_id == sid:
                observed["pid"] = await conn.fetchval("SELECT pg_backend_pid()")
            async with original(conn, session_id, **kwargs):
                yield

        with patch.object(provisioning.push_lifecycle, "locked", locked):
            yield observed

    async def assert_revoke_blocked(self, observed, finished):
        async def observe():
            while not finished.is_set():
                pid = observed.get("pid")
                if pid and await self.pool.fetchval(
                    "SELECT cardinality(pg_blocking_pids($1)) > 0", pid
                ):
                    return
                await self.pool.fetchval("SELECT 1")
            self.fail("parent authority mutation committed inside child admission")

        await asyncio.wait_for(observe(), 10)

    async def test_both_providers_supervisor_child_independent_scopes(self):
        for provider in ("claude", "codex"):
            op, task, supervisor = await self.create_task(
                self.request(provider=provider)
            )
            child_op, child_task, child = await self.create_task(
                self.request(), await self.principal(supervisor)
            )
            self.assertEqual((supervisor.state, child.state), ("active", "active"))
            self.assertEqual((supervisor.depth, child.depth), (1, 2))
            self.assertEqual(child_task.parent_task_id, task.id)
            self.assertEqual(child.profile_id, provider)
            self.assertNotEqual(child.session_id, supervisor.session_id)
            self.assertEqual(child.workspace_id, child.binding_id)
            self.assertEqual(child.environment.version_id, self.version_id)
            self.assertEqual(supervisor.environment, child.environment)
            self.assertNotEqual(
                (await ns.get_binding(child.session_id))["credential_ref"],
                (await ns.get_binding(supervisor.session_id))["credential_ref"],
            )
            with self.assertRaises(store.TaskError):
                await self.create_task(self.request(), await self.principal(child))
            with self.assertRaises(HTTPException):
                await AgentService(PgStore()).authenticate("forged-token")
            ctx = await AgentService(PgStore()).authenticate(
                token_for(child.session_id)
            )
            self.assertEqual(ctx.actor.depth, 2)
            self.assertEqual(
                (await AgentService(PgStore()).whoami(ctx))["scope_status"], "available"
            )
            async with self.pool.acquire() as conn, conn.transaction():
                binding = await PgStore().binding_by_token_hash(
                    ctx.binding["token_hash"]
                )
                self.assertIsNotNone(
                    await resolve_project_authority(
                        conn, binding, self.project, branch=child_task.checkout.branch
                    )
                )
                with self.assertRaises(PolicyError):
                    await resolve_project_authority(
                        conn, binding, self.project, branch=task.checkout.branch
                    )
        self.assertEqual(self.spawn.call_count, 4)
        requests = self.fake.created_workspaces()
        self.assertEqual(len(requests), 4)
        self.assertTrue(
            all(
                w.ref == "a" * 40 and w.repo == "https://github.com/example/app"
                for w in requests
            )
        )
        self.assertEqual(len({w.branch for w in requests}), 4)
        for body in self.fake.session_calls("CreateSession"):
            fields = decode_fields(body)
            self.assertIn(7, fields)  # credential envelope supplied before create

    async def test_duplicate_concurrent_creation_and_one_brief(self):
        request = self.request()
        a, b = await asyncio.gather(
            self.create_task(request, ready=False),
            self.create_task(request, ready=False),
        )
        self.assertEqual(a[0].id, b[0].id)
        await asyncio.gather(
            self.worker.reconcile(db, a[0]), self.worker.reconcile(db, b[0])
        )
        await self.worker.reconcile(db, a[0])
        self.assertEqual(
            await self.pool.fetchval("SELECT count(*) FROM task_attempts"), 1
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_deliveries WHERE source='brief' AND session_id=$1",
                a[2].session_id,
            ),
            1,
        )
        self.assertEqual(self.spawn.call_count, 1)
        with self.assertRaises(store.TaskError):
            await self.create_task(request.model_copy(update={"brief": "changed"}))

    async def test_lost_create_response_preserves_identity_and_claim(self):
        create = ns.get_client().create_session
        calls = []

        async def lost(agent, **kwargs):
            calls.append((agent, kwargs))
            session = await create(agent, **kwargs)
            if len(calls) == 1:
                raise OutcomeUnknown("lost fixture response")
            return session

        with patch.object(ns.get_client(), "create_session", lost):
            op, task, attempt = await self.create_task()
            self.assertEqual(attempt.state, "creating")
            self.assertTrue(
                await self.pool.fetchval(
                    "SELECT held FROM workspace_writer_claims WHERE attempt_id=$1",
                    attempt.id,
                )
            )
            await self.worker.reconcile(db, op)
        self.assertEqual(calls[0], calls[1])
        self.assertEqual(self.spawn.call_count, 1)

    async def test_parent_revocation_before_child_brief_fences_and_retains_audit(self):
        _, _, parent = await self.create_task()
        op, _, child = await self.create_task(
            principal=await self.principal(parent), ready=False
        )
        await revoke(parent.session_id)
        await self.worker.reconcile(db, op)
        async with self.pool.acquire() as conn:
            child = await lifecycle.load_attempt(conn, child.id)
        self.assertEqual(child.state, "cancelled")
        self.assertIsNone(child.brief_delivery_id)
        self.assertFalse(
            await self.pool.fetchval(
                "SELECT EXISTS(SELECT 1 FROM workspace_writer_claims WHERE attempt_id=$1 AND held)",
                child.id,
            )
        )
        self.assertFalse(
            await self.pool.fetchval(
                "SELECT EXISTS(SELECT 1 FROM sessions WHERE id=$1)", child.session_id
            )
        )
        self.assertEqual(self.spawn.call_count, 1)

    async def test_ordinary_and_delegated_writers_share_repository_branch_claim(self):
        _, _, attempt = await self.create_task(
            self.request(branch="shared"), ready=False
        )
        with self.assertRaises(workspaces.WorkspaceConflict):
            await workspaces.create(
                self.user,
                self.project,
                WorkspaceManifest(
                    repo_url="https://github.com/example/app", branch="shared"
                ),
            )
        with self.assertRaises(workspaces.WorkspaceConflict):
            await workspaces.create(
                self.user,
                self.project,
                WorkspaceManifest(
                    repo_url="https://github.com/EXAMPLE/APP.git", branch="shared"
                ),
            )
        await workspaces.create(
            self.user,
            self.project,
            WorkspaceManifest(
                repo_url="https://github.com/example/app", branch="ordinary"
            ),
        )
        with self.assertRaises(store.TaskError):
            await self.create_task(self.request(branch="ordinary"), ready=False)
        self.assertEqual(await self.pool.fetchval("SELECT count(*) FROM tasks"), 1)

    async def test_coordination_has_no_checkout_claim_or_repository_authority(self):
        _, _, attempt = await self.create_task(
            self.request(mode="coordination", provider="codex")
        )
        self.assertIsNone(self.fake.created_workspaces()[-1])
        self.assertEqual(
            await self.pool.fetchval("SELECT count(*) FROM workspace_writer_claims"), 0
        )
        self.assertIsNone(attempt.writer_generation)
        ctx = await AgentService(PgStore()).authenticate(token_for(attempt.session_id))
        self.assertEqual((ctx.actor.role, ctx.actor.depth), ("supervisor", 1))
        async with self.pool.acquire() as conn, conn.transaction():
            with self.assertRaises(ScopeUnavailable):
                await resolve_project_authority(conn, ctx.binding, self.project)

    async def test_cancel_fences_then_releases_and_guards_all_runtime_paths(self):
        _, task, attempt = await self.create_task()
        self.assertEqual(await ns.cancel(attempt.session_id), "stopped")
        async with self.pool.acquire() as conn:
            attempt = await lifecycle.load_attempt(conn, attempt.id)
            self.assertEqual(attempt.state, "cancelled")
            for action in ("create", "submit", "resume", "preview"):
                with self.assertRaises(lifecycle.LifecycleDenied):
                    await lifecycle.check(conn, attempt.session_id, action)
        with self.assertRaises(ValueError):
            await ns.submit_message(attempt.session_id, "stale")
        self.assertIsNone(await workspaces.preview_row(attempt.session_id, self.user))
        with self.assertRaises(lifecycle.LifecycleDenied):
            await ns._create_bound_session(await ns.get_binding(attempt.session_id))
        with self.assertRaises(HTTPException):
            await AgentService(PgStore()).authenticate(token_for(attempt.session_id))
        self.assertFalse(
            await self.pool.fetchval(
                "SELECT EXISTS(SELECT 1 FROM workspace_writer_claims WHERE attempt_id=$1 AND held)",
                attempt.id,
            )
        )
        self.assertEqual(
            await db.archive_sessions(self.user, [attempt.session_id]),
            [attempt.session_id],
        )

    async def test_archive_projection_cannot_revoke_unsettled_attempt(self):
        _, _, attempt = await self.create_task()
        await self.pool.execute(
            "UPDATE sessions SET status='cancelled' WHERE id=$1", attempt.session_id
        )
        self.assertEqual(await db.archive_sessions(self.user, [attempt.session_id]), [])
        self.assertIsNotNone((await ns.get_binding(attempt.session_id))["token_hash"])
        self.assertTrue(
            await self.pool.fetchval(
                "SELECT held FROM workspace_writer_claims WHERE attempt_id=$1",
                attempt.id,
            )
        )

    async def test_stale_generation_settlement_rolls_back_fence_and_claim_release(self):
        _, _, attempt = await self.create_task(ready=False)
        # Corruption fixture only: the schema otherwise prevents this mismatch.
        async with self.pool.acquire() as conn, conn.transaction():
            await conn.execute("SET LOCAL session_replication_role=replica")
            await conn.execute(
                "UPDATE workspace_writer_claims SET generation=generation+1 WHERE attempt_id=$1",
                attempt.id,
            )
        async with self.pool.acquire() as conn, conn.transaction():
            await lifecycle.transition(
                conn, attempt.id, "draining", from_states=("creating",)
            )
        with self.assertRaises(store.TaskError):
            async with self.pool.acquire() as conn, conn.transaction():
                await lifecycle.settle(
                    conn, attempt.id, "cancelled", evidence="fixture:deleted"
                )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT state FROM task_attempts WHERE id=$1", attempt.id
            ),
            "draining",
        )
        self.assertTrue(
            await self.pool.fetchval(
                "SELECT held FROM workspace_writer_claims WHERE attempt_id=$1",
                attempt.id,
            )
        )

    async def test_runtime_guard_orders_cross_connection_drain(self):
        _, _, attempt = await self.create_task()
        entered, finished = asyncio.Event(), asyncio.Event()

        async def drain():
            entered.set()
            async with self.pool.acquire() as conn:
                async with (
                    lifecycle.locked(conn, attempt.session_id),
                    conn.transaction(),
                ):
                    await store.admission_lock(conn)
                    await lifecycle.transition(
                        conn, attempt.id, "draining", from_states=("active",)
                    )
            finished.set()

        async with lifecycle.guard(attempt.session_id, "resume"):
            waiter = asyncio.create_task(drain())
            await entered.wait()
            # Give the other connection a query round-trip, without wall-clock polling.
            await self.pool.fetchval("SELECT 1")
            self.assertFalse(finished.is_set())
        await waiter
        with self.assertRaises(lifecycle.LifecycleDenied):
            async with lifecycle.guard(attempt.session_id, "resume"):
                self.fail("draining actor admitted")

    async def test_cancel_intent_before_readiness_prevents_first_brief(self):
        op, task, attempt = await self.create_task(ready=False)
        async with self.pool.acquire() as conn, conn.transaction():
            await mutate(
                conn,
                self.owner,
                "cancel",
                TaskAction(
                    request_id="cancel-before-ready",
                    expected_version=task.version,
                    expected_attempt_id=attempt.id,
                ),
                task_id=task.id,
            )
        await self.worker.reconcile(db, op)
        self.spawn.assert_not_called()
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT state FROM task_attempts WHERE id=$1", attempt.id
            ),
            "cancelled",
        )

    async def test_missing_attempt_and_corrupt_ancestry_and_generation_deny_auth(self):
        _, _, parent = await self.create_task()
        _, child_task, child = await self.create_task(
            principal=await self.principal(parent)
        )
        service = AgentService(PgStore())
        for statement, restore, args in (
            (
                "UPDATE tasks SET root_task_id=$2 WHERE id=$1",
                "UPDATE tasks SET root_task_id=$2 WHERE id=$1",
                (child_task.id, "missing-root"),
            ),
            (
                "UPDATE task_attempts SET depth=1 WHERE id=$1",
                "UPDATE task_attempts SET depth=2 WHERE id=$1",
                (child.id,),
            ),
            (
                "UPDATE workspace_writer_claims SET generation=generation+1 WHERE attempt_id=$1",
                "UPDATE workspace_writer_claims SET generation=generation-1 WHERE attempt_id=$1",
                (child.id,),
            ),
        ):
            async with self.pool.acquire() as conn, conn.transaction():
                await conn.execute("SET LOCAL session_replication_role=replica")
                if len(args) == 1 and "depth" in statement:
                    # Role/depth CHECK is not bypassed by replication mode: corrupt role too.
                    await conn.execute(
                        "UPDATE task_attempts SET role='supervisor',depth=1 WHERE id=$1",
                        child.id,
                    )
                else:
                    await conn.execute(statement, *args)
            with self.assertRaises(HTTPException):
                await service.authenticate(token_for(child.session_id))
            async with self.pool.acquire() as conn, conn.transaction():
                await conn.execute("SET LOCAL session_replication_role=replica")
                if "root_task" in restore:
                    await conn.execute(restore, child_task.id, child_task.root_task_id)
                elif "depth" in restore:
                    await conn.execute(
                        "UPDATE task_attempts SET role='child',depth=2 WHERE id=$1",
                        child.id,
                    )
                else:
                    await conn.execute(restore, *args)
            self.assertEqual(
                (await service.authenticate(token_for(child.session_id))).actor.depth, 2
            )
        sid, _ = await self.bound_session(role="supervisor", mcp_grant_kind="workspace")
        with self.assertRaises(HTTPException):
            await service.authenticate(token_for(sid))

    async def test_activation_reenrolls_only_after_attempt_is_active(self):
        seen = []

        async def enroll(conn, sid):
            seen.append(
                await conn.fetchval(
                    "SELECT state FROM task_attempts WHERE binding_id=$1", sid
                )
            )

        with patch.object(provisioning.push_lifecycle, "enroll", enroll):
            await self.create_task()
        self.assertEqual(seen, ["creating", "active"])

    async def test_settlement_holds_publication_locks_on_same_connection(self):
        held = []
        settle = lifecycle.settle

        @asynccontextmanager
        async def locked(conn, sid, *, revoke=False):
            held.append((conn, sid, revoke))
            try:
                yield
            finally:
                held.pop()

        async def checked(conn, attempt_id, final, *, evidence):
            self.assertTrue(held)
            self.assertIs(held[-1][0], conn)
            self.assertTrue(held[-1][2])
            return await settle(conn, attempt_id, final, evidence=evidence)

        _, _, attempt = await self.create_task()
        with (
            patch.object(provisioning.push_lifecycle, "locked", locked),
            patch.object(lifecycle, "settle", checked),
        ):
            self.assertEqual(await ns.cancel(attempt.session_id), "stopped")

    async def test_unknown_delete_retains_capacity_and_writer_until_reconciled(self):
        _, task, attempt = await self.create_task()
        async with self.pool.acquire() as conn, conn.transaction():
            op = await mutate(
                conn,
                self.owner,
                "cancel",
                TaskAction(
                    request_id="unknown-delete",
                    expected_version=task.version,
                    expected_attempt_id=attempt.id,
                ),
                task_id=task.id,
            )
        with patch.object(
            ns.get_client(),
            "delete_session",
            AsyncMock(side_effect=OutcomeUnknown("delete reply lost")),
        ):
            await self.worker.reconcile(db, op)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT state FROM task_attempts WHERE id=$1", attempt.id
            ),
            "draining",
        )
        self.assertTrue(
            await self.pool.fetchval(
                "SELECT capacity_held FROM task_attempts WHERE id=$1", attempt.id
            )
        )
        self.assertTrue(
            await self.pool.fetchval(
                "SELECT held FROM workspace_writer_claims WHERE attempt_id=$1",
                attempt.id,
            )
        )
        self.assertIsNone((await ns.get_binding(attempt.session_id))["token_hash"])
        await self.worker.reconcile(db, op)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT state FROM task_attempts WHERE id=$1", attempt.id
            ),
            "cancelled",
        )

    async def test_publish_reply_loss_reuses_secret_and_does_not_create_early(self):
        published = []

        async def publish(sid, ref):
            published.append((sid, ref))
            if len(published) == 1:
                raise RuntimeError("fixture lost Secret publish response")
            return ref

        with patch("mainloop.runtime.agent_credentials.credentials.publish", publish):
            op, _, attempt = await self.create_task()
            self.assertEqual(attempt.state, "creating")
            self.assertFalse(self.fake.session_calls("CreateSession"))
            await self.worker.reconcile(db, op)
        self.assertEqual(published[0], published[1])
        self.assertEqual(self.spawn.call_count, 1)

    async def test_parent_loss_blocks_active_child_submission_and_preview(self):
        _, _, parent = await self.create_task()
        _, _, child = await self.create_task(principal=await self.principal(parent))
        await revoke(parent.session_id)
        with self.assertRaises(ValueError):
            await ns.submit_message(child.session_id, "not authorized")
        self.assertIsNone(await workspaces.preview_row(child.session_id, self.user))

    async def test_first_turn_finishes_without_completing_task_or_releasing_writer(
        self,
    ):
        _, task, attempt = await self.create_task()
        await ns._deliver(attempt.session_id, attempt.brief_delivery_id, task.brief)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT state FROM native_deliveries WHERE message_id=$1",
                attempt.brief_delivery_id,
            ),
            "completed",
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT state FROM task_attempts WHERE id=$1", attempt.id
            ),
            "active",
        )
        self.assertTrue(
            await self.pool.fetchval(
                "SELECT held FROM workspace_writer_claims WHERE attempt_id=$1",
                attempt.id,
            )
        )
        self.assertEqual(
            await self.pool.fetchval("SELECT status FROM tasks WHERE id=$1", task.id),
            "running",
        )

    async def test_rest_and_binding_principal_compete_for_one_branch(self):
        _, _, parent = await self.create_task()
        principal = await self.principal(parent)
        branch = "feature/mixed"
        previous = dict(api.app.dependency_overrides)
        api.app.dependency_overrides[current_user] = lambda: self.user
        try:
            async with httpx.AsyncClient(
                transport=httpx.ASGITransport(app=api.app), base_url="http://localhost"
            ) as client:
                rest, scoped = await asyncio.gather(
                    client.post(
                        "/tasks",
                        json=self.request(branch=branch).model_dump(mode="json"),
                    ),
                    self.create_task(
                        self.request(branch=branch), principal, ready=False
                    ),
                    return_exceptions=True,
                )
            successes = int(
                isinstance(rest, httpx.Response) and rest.status_code == 202
            ) + int(isinstance(scoped, tuple))
            self.assertEqual(successes, 1)
            if isinstance(rest, httpx.Response) and rest.status_code != 202:
                self.assertEqual(rest.status_code, 409)
            if isinstance(scoped, Exception):
                self.assertIsInstance(scoped, store.TaskError)
            self.assertEqual(
                await self.pool.fetchval(
                    "SELECT count(*) FROM workspace_writer_claims WHERE branch=$1 AND held",
                    branch,
                ),
                1,
            )
        finally:
            api.app.dependency_overrides.clear()
            api.app.dependency_overrides.update(previous)

    async def test_cancel_racing_secret_publication_never_delivers_brief(self):
        op, task, attempt = await self.create_task(ready=False)
        entered, release = asyncio.Event(), asyncio.Event()

        async def publish(_sid, ref):
            entered.set()
            await release.wait()
            return ref

        with patch("mainloop.runtime.agent_credentials.credentials.publish", publish):
            creating = asyncio.create_task(self.worker.reconcile(db, op))
            await entered.wait()
            async with self.pool.acquire() as conn, conn.transaction():
                cancel = await mutate(
                    conn,
                    self.owner,
                    "cancel",
                    TaskAction(
                        request_id="race-cancel",
                        expected_version=task.version,
                        expected_attempt_id=attempt.id,
                    ),
                    task_id=task.id,
                )
            cancelling = asyncio.create_task(self.worker.reconcile(db, cancel))
            release.set()
            await asyncio.gather(creating, cancelling)
        self.spawn.assert_not_called()
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT state FROM task_attempts WHERE id=$1", attempt.id
            ),
            "cancelled",
        )

    async def test_ordinary_archive_releases_claim_only_after_settled_delete(self):
        ws = await workspaces.create(
            self.user,
            self.project,
            WorkspaceManifest(
                repo_url="https://github.com/example/app", branch="archive-owner"
            ),
        )
        await self.pool.execute(
            "UPDATE sessions SET status='completed' WHERE id=$1", ws.session_id
        )
        client = ns.get_client()
        ready = await client.get_session(
            (await ns.get_binding(ws.session_id))["kagent_session_id"]
        )
        with patch.object(
            client,
            "delete_session",
            AsyncMock(
                return_value=replace(
                    ready,
                    state=RuntimeState.DELETING,
                    operation=RuntimeOperation.DELETE,
                )
            ),
        ):
            await db.archive_sessions(self.user, [ws.session_id])
        self.assertTrue(
            await self.pool.fetchval(
                "SELECT held FROM workspace_writer_claims WHERE binding_id=$1",
                ws.session_id,
            )
        )
        await ns.reconcile_archived_deletes()
        self.assertFalse(
            await self.pool.fetchval(
                "SELECT held FROM workspace_writer_claims WHERE owner_id=$1 AND branch='archive-owner'",
                self.user,
            )
        )

    async def test_create_retry_refusal_after_lost_reply_cannot_release_writer(self):
        create = ns.get_client().create_session

        async def lost(agent, **kwargs):
            await create(agent, **kwargs)
            raise OutcomeUnknown("fixture lost accepted create reply")

        with patch.object(ns.get_client(), "create_session", lost):
            op, _, attempt = await self.create_task()
        self.assertIsNone(
            (await ns.get_binding(attempt.session_id))["kagent_session_id"]
        )
        for code in (3, 5, 7, 16):
            with patch.object(
                ns.get_client(),
                "create_session",
                AsyncMock(
                    side_effect=SessionError(
                        "fixture control/template refusal", grpc_status=code
                    )
                ),
            ):
                self.assertEqual(await ns.cancel(attempt.session_id), "unknown")
            self.assertTrue(
                await self.pool.fetchval(
                    "SELECT held FROM workspace_writer_claims WHERE attempt_id=$1",
                    attempt.id,
                )
            )
            self.assertTrue(
                await self.pool.fetchval(
                    "SELECT capacity_held FROM task_attempts WHERE id=$1", attempt.id
                )
            )
            self.assertEqual(
                await self.pool.fetchval(
                    "SELECT state FROM task_attempts WHERE id=$1", attempt.id
                ),
                "draining",
            )
        self.assertEqual(await ns.cancel(attempt.session_id), "stopped")
        self.assertFalse(
            await self.pool.fetchval(
                "SELECT EXISTS(SELECT 1 FROM workspace_writer_claims WHERE attempt_id=$1 AND held)",
                attempt.id,
            )
        )
        self.spawn.assert_not_called()

    async def test_create_recovery_respects_committed_cancel_intent(self):
        op, task, attempt = await self.create_task(ready=False)
        async with self.pool.acquire() as conn, conn.transaction():
            await mutate(
                conn,
                self.owner,
                "cancel",
                TaskAction(
                    request_id="cancel-recovery",
                    expected_version=task.version,
                    expected_attempt_id=attempt.id,
                ),
                task_id=task.id,
            )
            await lifecycle.transition(
                conn, attempt.id, "draining", from_states=("creating",)
            )
        # The dispatcher sees the earlier create operation before its cancel operation.
        await self.worker.reconcile(db, op)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT state FROM task_attempts WHERE id=$1", attempt.id
            ),
            "cancelled",
        )
        self.assertEqual(
            await self.pool.fetchval("SELECT status FROM tasks WHERE id=$1", task.id),
            "cancelled",
        )
        self.spawn.assert_not_called()

    async def test_native_main_to_supervisor_to_child_for_both_profiles(self):
        for provider in ("claude", "codex"):
            sid, _ = await self.bound_session(kind=provider, role="main")
            binding = await ns.get_binding(sid)
            runtime = await ns._create_bound_session(binding)
            await ns.ledger.update_binding(sid, kagent_session_id=runtime.id)
            main = TaskPrincipal(self.user, binding_id=sid, role="main")
            _, _, parent = await self.create_task(self.request(provider=provider), main)
            _, _, child = await self.create_task(principal=await self.principal(parent))
            self.assertEqual(
                (await ns.get_binding(parent.session_id))["parent_session_id"], sid
            )
            self.assertEqual(
                (await ns.get_binding(child.session_id))["parent_session_id"],
                parent.session_id,
            )
            self.assertEqual(len({sid, parent.session_id, child.session_id}), 3)
            self.assertEqual(
                (parent.profile_id, child.profile_id), (provider, provider)
            )
            self.assertEqual((parent.depth, child.depth), (1, 2))
        workspaces_sent = self.fake.created_workspaces()
        self.assertEqual(len(workspaces_sent), 6)
        self.assertIsNone(workspaces_sent[0])
        self.assertIsNone(workspaces_sent[3])
        self.assertTrue(all(workspaces_sent[i] is not None for i in (1, 2, 4, 5)))

    async def test_rejected_create_crash_recovers_without_creating_and_allows_successor(
        self,
    ):
        request = self.request()
        op, _, attempt = await self.create_task(request, ready=False)
        original = lifecycle.create_rejected

        async def crash(sid, *, conn=None):
            await original(sid, conn=conn)
            raise RejectedCreateCrash()

        with (
            patch.object(lifecycle, "create_rejected", crash),
            patch.object(
                ns.get_client(),
                "create_session",
                AsyncMock(
                    side_effect=ServiceConfigurationError("control auth rejected")
                ),
            ) as create,
        ):
            with self.assertRaises(RejectedCreateCrash):
                await self.worker.reconcile(db, op)
        self.assertEqual(create.await_count, 1)
        async with self.pool.acquire() as conn:
            crashed = await lifecycle.load_attempt(conn, attempt.id)
        with self.subTest("rejection and closed admission commit atomically"):
            self.assertEqual(crashed.state, "draining")
            self.assertIn(lifecycle.CREATE_REJECTED, crashed.evidence_refs)
        self.assertTrue(
            await self.pool.fetchval(
                "SELECT capacity_held FROM task_attempts WHERE id=$1", attempt.id
            )
        )
        # Authorization is now restored. Restart with a fresh reconciler and the real fake client.
        await provisioning.Provisioning().reconcile(db, op)
        async with self.pool.acquire() as conn:
            recovered = await lifecycle.load_attempt(conn, attempt.id)
        # On the held source this activates a second create; cancellation then releases
        # its claim without deleting the live runtime. Assert that actual safety failure first.
        if recovered.state == "active":
            await ns.cancel(attempt.session_id)
        live = any(
            state != RuntimeState.DELETED for state, _ in self.fake.sessions.values()
        )
        capacity = await self.pool.fetchval(
            "SELECT capacity_held FROM task_attempts WHERE id=$1", attempt.id
        )
        claim = await self.pool.fetchval(
            "SELECT held FROM workspace_writer_claims WHERE attempt_id=$1", attempt.id
        )
        self.assertTrue(
            not live or (capacity and claim),
            f"runtime_live={live}, capacity_held={capacity}, claim_held={claim}",
        )
        self.assertFalse(
            any(
                state != RuntimeState.DELETED
                for state, _ in self.fake.sessions.values()
            )
        )
        self.assertEqual(len(self.fake.session_calls("CreateSession")), 0)
        self.assertEqual(recovered.state, "failed")
        self.assertIn(lifecycle.CREATE_REJECTED, recovered.evidence_refs)
        self.assertFalse(
            await self.pool.fetchval(
                "SELECT capacity_held FROM task_attempts WHERE id=$1", attempt.id
            )
        )
        self.assertFalse(
            await self.pool.fetchval(
                "SELECT EXISTS(SELECT 1 FROM workspace_writer_claims WHERE attempt_id=$1 AND held)",
                attempt.id,
            )
        )
        _, _, successor = await self.create_task(
            self.request(branch=request.checkout.branch)
        )
        self.assertNotEqual(successor.id, attempt.id)
        self.assertNotEqual(successor.session_id, attempt.session_id)
        self.assertEqual(successor.state, "active")

    async def test_refresh_queued_at_rejection_commit_cannot_create(self):
        op, _, attempt = await self.create_task(ready=False)
        committed, release, refresh_entered = (
            asyncio.Event(),
            asyncio.Event(),
            asyncio.Event(),
        )
        original_rejected = lifecycle.create_rejected
        original_create = workspaces._create_session
        original_dispatch = ns.get_client().create_session
        dispatches = 0

        async def restored_authorization(*args, **kwargs):
            nonlocal dispatches
            dispatches += 1
            if dispatches == 1:
                raise SessionError("permission denied", grpc_status=7)
            return await original_dispatch(*args, **kwargs)

        async def crash(sid, *, conn=None):
            await original_rejected(sid, conn=conn)
            committed.set()
            await release.wait()
            raise RejectedCreateCrash()

        async def observe_refresh(*args, **kwargs):
            refresh_entered.set()
            return await original_create(*args, **kwargs)

        with (
            patch.object(lifecycle, "create_rejected", crash),
            patch.object(workspaces, "_create_session", observe_refresh),
            patch.object(ns.get_client(), "create_session", restored_authorization),
        ):
            reconcile = asyncio.create_task(self.worker.reconcile(db, op))
            refresh = None
            try:
                await asyncio.wait_for(committed.wait(), 10)
                refresh = asyncio.create_task(
                    workspaces.refresh(attempt.session_id, self.user)
                )
                await asyncio.wait_for(refresh_entered.wait(), 10)
                self.assertFalse(refresh.done())
                release.set()
                with self.assertRaises(RejectedCreateCrash):
                    await asyncio.wait_for(reconcile, 10)
                with self.assertRaises(workspaces.WorkspaceConflict):
                    await asyncio.wait_for(refresh, 10)
            finally:
                release.set()
                await asyncio.gather(reconcile, return_exceptions=True)
                if refresh is not None and not refresh.done():
                    refresh.cancel()
                    await asyncio.gather(refresh, return_exceptions=True)
        self.assertEqual(dispatches, 1)
        self.assertEqual(len(self.fake.session_calls("CreateSession")), 0)
        self.assertFalse(self.fake.sessions)
        self.assertTrue(
            await self.pool.fetchval(
                "SELECT held FROM workspace_writer_claims WHERE attempt_id=$1",
                attempt.id,
            )
        )
        await provisioning.Provisioning().reconcile(db, op)
        self.assertFalse(
            await self.pool.fetchval(
                "SELECT capacity_held FROM task_attempts WHERE id=$1", attempt.id
            )
        )

    async def test_rejection_marker_never_skips_known_runtime_deletion(self):
        _, _, attempt = await self.create_task()
        sid = attempt.session_id
        runtime_id = (await ns.get_binding(sid))["kagent_session_id"]
        async with self.pool.acquire() as conn, conn.transaction():
            await lifecycle.save_attempt(
                conn,
                attempt.model_copy(
                    update={
                        "evidence_refs": (
                            *attempt.evidence_refs,
                            lifecycle.CREATE_REJECTED,
                        )
                    }
                ),
            )
        with patch.object(
            ns.get_client(),
            "delete_session",
            AsyncMock(side_effect=OutcomeUnknown("delete reply lost")),
        ) as delete:
            outcome = await ns.cancel(sid)
        self.assertEqual(outcome, "unknown")
        delete.assert_awaited_once_with(runtime_id)
        self.assertNotEqual(self.fake.sessions[runtime_id][0], RuntimeState.DELETED)
        self.assertTrue(
            await self.pool.fetchval(
                "SELECT capacity_held FROM task_attempts WHERE id=$1", attempt.id
            )
        )
        self.assertTrue(
            await self.pool.fetchval(
                "SELECT held FROM workspace_writer_claims WHERE attempt_id=$1",
                attempt.id,
            )
        )
        self.assertEqual(await ns.cancel(sid), "stopped")
        self.assertEqual(self.fake.sessions[runtime_id][0], RuntimeState.DELETED)
        async with self.pool.acquire() as conn:
            ended = await lifecycle.load_attempt(conn, attempt.id)
        self.assertIn(f"kagent-deleted:{runtime_id}", ended.evidence_refs)
        self.assertFalse(
            await self.pool.fetchval(
                "SELECT capacity_held FROM task_attempts WHERE id=$1", attempt.id
            )
        )

    async def test_legacy_creating_rejection_denies_refresh_and_dispatch_then_settles(
        self,
    ):
        op, _, attempt = await self.create_task(ready=False)
        async with self.pool.acquire() as conn, conn.transaction():
            await lifecycle.save_attempt(
                conn,
                attempt.model_copy(
                    update={
                        "evidence_refs": (
                            lifecycle.CREATE_DISPATCH,
                            lifecycle.CREATE_REJECTED,
                        )
                    }
                ),
            )
        with self.assertRaises(workspaces.WorkspaceConflict):
            await workspaces.refresh(attempt.session_id, self.user)
        with self.assertRaises(lifecycle.LifecycleDenied):
            await lifecycle.create_dispatch(attempt.session_id)
        await provisioning.Provisioning().reconcile(db, op)
        self.assertFalse(self.fake.sessions)
        self.assertEqual(len(self.fake.session_calls("CreateSession")), 0)
        async with self.pool.acquire() as conn:
            ended = await lifecycle.load_attempt(conn, attempt.id)
        self.assertEqual(ended.state, "failed")
        self.assertIn(lifecycle.CREATE_REJECTED, ended.evidence_refs)

    async def test_prior_uncertain_create_cannot_be_settled_by_later_auth_refusal(self):
        op, _, attempt = await self.create_task(ready=False)
        original = ns.get_client().create_session

        async def lost_reply(*args, **kwargs):
            await original(*args, **kwargs)
            raise OutcomeUnknown("create accepted, reply lost")

        with patch.object(ns.get_client(), "create_session", lost_reply):
            await self.worker.reconcile(db, op)
        self.assertTrue(self.fake.sessions)
        with patch.object(
            ns.get_client(),
            "create_session",
            AsyncMock(
                side_effect=ServiceConfigurationError("control authorization changed")
            ),
        ):
            await self.worker.reconcile(db, op)
            self.assertEqual(await ns.cancel(attempt.session_id), "unknown")
        async with self.pool.acquire() as conn:
            pending = await lifecycle.load_attempt(conn, attempt.id)
        self.assertNotIn(lifecycle.CREATE_REJECTED, pending.evidence_refs)
        self.assertIn(lifecycle.CREATE_DISPATCH, pending.evidence_refs)
        self.assertEqual(pending.state, "draining")
        self.assertTrue(
            await self.pool.fetchval(
                "SELECT capacity_held FROM task_attempts WHERE id=$1", attempt.id
            )
        )
        self.assertTrue(
            await self.pool.fetchval(
                "SELECT held FROM workspace_writer_claims WHERE attempt_id=$1",
                attempt.id,
            )
        )
        self.assertEqual(await ns.cancel(attempt.session_id), "stopped")
        self.assertTrue(
            all(
                state == RuntimeState.DELETED
                for state, _ in self.fake.sessions.values()
            )
        )

    async def test_first_invalid_argument_settles_without_retry_and_retains_audit(self):
        op, _, attempt = await self.create_task(ready=False)
        client = ns.get_client()
        with patch.object(
            client,
            "create_session",
            AsyncMock(
                side_effect=SessionError(
                    "workspace repository host is not one of the Agent's Git origins",
                    grpc_status=3,
                )
            ),
        ) as create:
            await self.worker.reconcile(db, op)
        self.assertEqual(create.await_count, 1)
        async with self.pool.acquire() as conn:
            ended = await lifecycle.load_attempt(conn, attempt.id)
        self.assertEqual(ended.state, "failed")
        self.assertIn(lifecycle.CREATE_REJECTED, ended.evidence_refs)
        self.assertFalse(
            await self.pool.fetchval(
                "SELECT capacity_held FROM task_attempts WHERE id=$1", attempt.id
            )
        )
        self.assertFalse(
            await self.pool.fetchval(
                "SELECT EXISTS(SELECT 1 FROM workspace_writer_claims WHERE attempt_id=$1 AND held)",
                attempt.id,
            )
        )
        self.assertIn(lifecycle.CREATE_DISPATCH, ended.evidence_refs)

    async def test_wrong_deleted_identity_never_releases_delegated_or_ordinary_writer(
        self,
    ):
        _, _, attempt = await self.create_task()
        client = ns.get_client()
        live = await client.get_session(
            (await ns.get_binding(attempt.session_id))["kagent_session_id"]
        )
        wrong = replace(
            live,
            id="wrong-runtime",
            context_id="wrong-runtime",
            state=RuntimeState.DELETED,
        )
        with patch.object(client, "delete_session", AsyncMock(return_value=wrong)):
            self.assertEqual(await ns.cancel(attempt.session_id), "unknown")
        self.assertIsNone(
            (await ns.get_binding(attempt.session_id))["kagent_deleted_at"]
        )
        self.assertTrue(
            await self.pool.fetchval(
                "SELECT capacity_held FROM task_attempts WHERE id=$1", attempt.id
            )
        )
        self.assertTrue(
            await self.pool.fetchval(
                "SELECT held FROM workspace_writer_claims WHERE attempt_id=$1",
                attempt.id,
            )
        )
        ws = await workspaces.create(
            self.user,
            self.project,
            WorkspaceManifest(
                repo_url="https://github.com/example/app",
                branch="feature/ordinary-wrong-delete",
            ),
        )
        with patch.object(client, "delete_session", AsyncMock(return_value=wrong)):
            with self.assertRaises(workspaces.WorkspaceUnconfirmed):
                await workspaces.delete(ws.session_id, self.user)
            self.assertFalse(await ns.delete_kagent_session(ws.session_id))
        self.assertIsNone((await ns.get_binding(ws.session_id))["kagent_deleted_at"])
        self.assertTrue(
            await self.pool.fetchval(
                "SELECT held FROM workspace_writer_claims WHERE binding_id=$1",
                ws.session_id,
            )
        )

    async def test_enrollment_failure_is_recovered_without_duplicate_brief(self):
        op, _, attempt = await self.create_task(ready=False)
        seen = []
        original_enroll = provisioning.push_lifecycle.enroll

        async def enroll(conn, sid):
            state = await conn.fetchval(
                "SELECT state FROM task_attempts WHERE binding_id=$1", sid
            )
            seen.append(state)
            if state == "active" and seen.count("active") == 1:
                raise RuntimeError("fixture failure after activation")
            return await original_enroll(conn, sid)

        with (
            patch.object(settings, "push_gate_enabled", True),
            patch.object(provisioning.push_lifecycle, "enroll", enroll),
        ):
            await self.worker.reconcile(db, op)
            current, active = await self.worker._current(op.id)
            self.assertEqual(active.state, "active")
            self.assertEqual(current.state, "uncertain")
            brief = active.brief_delivery_id
            await provisioning.Provisioning().reconcile(db, current)
            current, active = await self.worker._current(op.id)
            self.assertEqual(current.state, "completed")
            self.assertEqual(active.brief_delivery_id, brief)
            async with self.pool.acquire() as conn:
                row = await conn.fetchrow(
                    "SELECT grant_data,attempt_id,writer_generation FROM push_grants WHERE id=$1",
                    attempt.session_id,
                )
                grant = await push_store.live_grant(conn, push_store.stored_grant(row))
                self.assertEqual(
                    (grant.attempt_id, grant.writer_generation, grant.version),
                    (attempt.id, attempt.writer_generation, 1),
                )
            await provisioning.Provisioning().reconcile(db, current)
        self.assertEqual(seen, ["creating", "active", "active"])
        self.assertEqual(self.spawn.call_count, 1)

    async def test_cancel_before_enrollment_recovery_does_not_resurrect_authority(self):
        op, _, attempt = await self.create_task(ready=False)

        async def failed(conn, sid):
            if (
                await conn.fetchval(
                    "SELECT state FROM task_attempts WHERE binding_id=$1", sid
                )
                == "active"
            ):
                raise RuntimeError("fixture enrollment outage")

        with patch.object(provisioning.push_lifecycle, "enroll", failed):
            await self.worker.reconcile(db, op)
        await ns.cancel(attempt.session_id)
        with patch.object(provisioning.push_lifecycle, "enroll", AsyncMock()) as enroll:
            await provisioning.Provisioning().reconcile(db, op)
        enroll.assert_not_awaited()
        self.assertIsNone((await ns.get_binding(attempt.session_id))["token_hash"])
        self.assertEqual(self.spawn.call_count, 1)

    async def test_child_admission_serializes_parent_revoke_for_each_runtime_action(
        self,
    ):
        for action in ("submit", "resume", "preview"):
            with self.subTest(action=action):
                _, _, parent = await self.create_task()
                _, _, child = await self.create_task(
                    principal=await self.principal(parent)
                )
                entered, finished = asyncio.Event(), asyncio.Event()

                async def remove_parent(
                    parent=parent, entered=entered, finished=finished
                ):
                    entered.set()
                    await revoke(parent.session_id)
                    finished.set()

                async with (
                    self.observed_parent_revoke(parent.session_id) as observed,
                    lifecycle.guard(child.session_id, action),
                ):
                    waiter = asyncio.create_task(remove_parent())
                    await entered.wait()
                    await self.assert_revoke_blocked(observed, finished)
                await asyncio.wait_for(waiter, 10)
                with self.assertRaises(lifecycle.LifecycleDenied):
                    async with (
                        self.observed_parent_revoke(parent.session_id) as observed,
                        lifecycle.guard(child.session_id, action),
                    ):
                        self.fail("revoked ancestor admitted child work")
                await ns.cancel(child.session_id)
                await ns.cancel(parent.session_id)

    async def test_parent_authority_invalidates_old_and_new_child_push_grants(self):
        _, _, parent = await self.create_task()
        _, _, child = await self.create_task(principal=await self.principal(parent))
        binding = await ns.get_binding(child.session_id)
        branch = await self.pool.fetchval(
            "SELECT branch FROM workspaces WHERE session_id=$1", child.session_id
        )
        grant = PushGrant(
            id=child.session_id,
            owner_id=self.user,
            project_id=self.project,
            repository="example/app",
            branch=branch,
            workspace_id=child.session_id,
            session_id=child.session_id,
            runtime_identity=binding["kagent_session_id"],
            role="child",
            grant_kind="workspace",
        )
        async with self.pool.acquire() as conn:
            await push_store.set_policy(
                conn,
                ProtectedBranchPolicy(
                    project_id=self.project, version=1, default_branch="main"
                ),
            )
            token = await push_store.issue(conn, grant)
            stored = await conn.fetchrow(
                "SELECT grant_data,attempt_id,writer_generation FROM push_grants WHERE id=$1",
                child.session_id,
            )
            pinned = push_store.stored_grant(stored)
        updates = (
            RefUpdate(ref=f"refs/heads/{branch}", old_oid="a" * 40, new_oid="b" * 40),
        )
        entered, finished = asyncio.Event(), asyncio.Event()

        async def remove_parent():
            entered.set()
            await revoke(parent.session_id)
            finished.set()

        async with self.pool.acquire() as conn:
            async with push_store.authorized(
                conn, token, "example/app", updates, lambda *_: True
            ):
                waiter = asyncio.create_task(remove_parent())
                await entered.wait()
                await self.pool.fetchval("SELECT 1")
                self.assertFalse(finished.is_set())
        await asyncio.wait_for(waiter, 10)
        async with self.pool.acquire() as conn:
            for bind in (False, True):
                with self.assertRaises(ValueError):
                    await push_store.live_grant(conn, pinned, bind=bind)
            with self.assertRaises(ValueError):
                await push_store.issue(conn, grant)
            with self.assertRaises(ValueError):
                async with push_store.authorized(
                    conn, token, "example/app", updates, lambda *_: True
                ):
                    self.fail("old child token survived ancestor revocation")

    async def test_http_body_delay_and_retry_cannot_admit_after_parent_revoke(self):
        from tests.runtime.test_preview_proxy import make_request

        _, _, parent = await self.create_task()
        _, _, child = await self.create_task(principal=await self.principal(parent))
        parsed = preview_proxy.PreviewHost(5173, child.session_id)
        target = preview_proxy.PreviewTarget(
            child.session_id, self.user, "kagent", "old", {5173: "fixture"}
        )
        body_started, release_body = asyncio.Event(), asyncio.Event()
        request = make_request("POST", {"host": "fixture"})

        async def body():
            body_started.set()
            await release_body.wait()
            yield b"body"

        with (
            patch.object(request, "stream", body),
            patch.object(preview_proxy, "current_user", lambda: self.user),
            patch.object(
                preview_proxy, "_resolve_target", AsyncMock(return_value=target)
            ),
            patch.object(
                preview_proxy, "_wake_workspace", AsyncMock(return_value=True)
            ),
            patch.object(preview_proxy, "_connect_router", Mock()) as connect,
        ):
            response = asyncio.create_task(preview_proxy._preview_http(request, parsed))
            await body_started.wait()
            await revoke(parent.session_id)
            release_body.set()
            self.assertEqual((await response).status_code, 404)
            connect.assert_not_called()
        await ns.cancel(child.session_id)
        await ns.cancel(parent.session_id)
        _, _, parent = await self.create_task()
        _, _, child = await self.create_task(principal=await self.principal(parent))
        parsed = preview_proxy.PreviewHost(5173, child.session_id)
        target = replace(target, workspace_id=child.session_id)
        original = preview_proxy._admit_http
        calls = []

        async def admit(*args, **kwargs):
            try:
                return await original(*args, **kwargs)
            except preview_proxy._PreviewPreForwardFailure:
                await revoke(parent.session_id)
                raise

        def connect(*args, **kwargs):
            calls.append(args)
            raise preview_proxy._PreviewPreForwardFailure(
                "fixture first CONNECT failed"
            )

        with (
            patch.object(preview_proxy, "current_user", lambda: self.user),
            patch.object(
                preview_proxy, "_resolve_target", AsyncMock(return_value=target)
            ),
            patch.object(
                preview_proxy, "_wake_workspace", AsyncMock(return_value=True)
            ),
            patch.object(preview_proxy, "_admit_http", admit),
            patch.object(preview_proxy, "_connect_router", connect),
        ):
            response = await preview_proxy._preview_http(
                make_request("GET", {"host": "fixture"}), parsed
            )
        self.assertEqual(response.status_code, 404)
        self.assertEqual(len(calls), 1)

    async def test_http_connect_holds_parent_fence_only_until_admission(self):
        import threading

        _, _, parent = await self.create_task()
        _, _, child = await self.create_task(principal=await self.principal(parent))
        parsed = preview_proxy.PreviewHost(5173, child.session_id)
        target = preview_proxy.PreviewTarget(
            child.session_id, self.user, "kagent", "runtime", {5173: "fixture"}
        )
        entered, release = threading.Event(), threading.Event()
        revoking, revoked = asyncio.Event(), asyncio.Event()
        result = Mock()

        def connect(*args, **kwargs):
            entered.set()
            if not release.wait(10):
                raise RuntimeError("fixture barrier not released")
            return result

        async def remove_parent():
            revoking.set()
            await revoke(parent.session_id)
            revoked.set()

        with (
            patch.object(preview_proxy, "current_user", lambda: self.user),
            patch.object(
                preview_proxy, "_resolve_target", AsyncMock(return_value=target)
            ),
            patch.object(preview_proxy, "_connect_router", connect),
        ):
            admission = asyncio.create_task(preview_proxy._admit_http(parsed, 1))
            await asyncio.to_thread(entered.wait, 10)
            waiter = asyncio.create_task(remove_parent())
            await revoking.wait()
            await self.pool.fetchval("SELECT 1")
            self.assertFalse(revoked.is_set())
            release.set()
            self.assertIs(await admission, result)
            await asyncio.wait_for(waiter, 10)
            # The response is still open; revocation did not wait for its body stream.
            result.close.assert_not_called()

    async def test_websocket_retry_revalidates_after_parent_revoke(self):
        from tests.runtime.test_preview_proxy import make_websocket

        _, _, parent = await self.create_task()
        _, _, child = await self.create_task(principal=await self.principal(parent))
        parsed = preview_proxy.PreviewHost(5173, child.session_id)
        target = preview_proxy.PreviewTarget(
            child.session_id, self.user, "kagent", "runtime", {5173: "fixture"}
        )
        original = preview_proxy._router_admission
        entered, release, revoking, revoked = (asyncio.Event() for _ in range(4))
        writer = Mock()
        writer.drain = AsyncMock()
        writer.wait_closed = AsyncMock()

        async def remove_parent():
            revoking.set()
            await revoke(parent.session_id)
            revoked.set()

        async def head(reader):
            entered.set()
            await release.wait()
            return 503, {}

        waiter = None

        @asynccontextmanager
        async def admission(request):
            nonlocal waiter
            async with original(request) as admitted:
                yield admitted
            # Ensure revocation commits between the first attempt and its retry.
            if waiter is not None:
                await waiter

        ws, sent = make_websocket({"host": "fixture", "sec-websocket-key": "a2V5"})
        with (
            patch.object(preview_proxy, "current_user", lambda: self.user),
            patch.object(
                preview_proxy, "_resolve_target", AsyncMock(return_value=target)
            ),
            patch.object(
                preview_proxy, "_wake_workspace", AsyncMock(return_value=True)
            ),
            patch.object(preview_proxy, "_router_admission", admission),
            patch.object(
                preview_proxy.asyncio,
                "open_connection",
                AsyncMock(return_value=(Mock(), writer)),
            ) as connect,
            patch.object(preview_proxy, "_read_async_head", head),
            patch.object(settings, "substrate_router_address", "http://fixture:80"),
        ):
            preview = asyncio.create_task(preview_proxy._preview_websocket(ws, parsed))
            await entered.wait()
            waiter = asyncio.create_task(remove_parent())
            await revoking.wait()
            await self.pool.fetchval("SELECT 1")
            self.assertFalse(revoked.is_set())
            release.set()
            await asyncio.wait_for(preview, 10)
            connect.assert_awaited_once()
        self.assertTrue(revoked.is_set())
        self.assertEqual(sent[-1]["type"], "websocket.close")

    async def test_real_child_dispatch_and_resume_hold_parent_authority(self):
        for action in ("dispatch", "resume"):
            _, _, parent = await self.create_task()
            _, _, child = await self.create_task(principal=await self.principal(parent))
            binding = await ns.get_binding(child.session_id)
            entered, release, revoking, revoked = (asyncio.Event() for _ in range(4))

            async def remove_parent(parent=parent, revoking=revoking, revoked=revoked):
                revoking.set()
                await revoke(parent.session_id)
                revoked.set()

            async def send(*args, entered=entered, release=release, **kwargs):
                entered.set()
                await release.wait()
                yield "first-receipt"

            client = ns.get_client()
            resume = client.resume_session

            async def paused_resume(
                sid, entered=entered, release=release, resume=resume
            ):
                entered.set()
                await release.wait()
                return await resume(sid)

            if action == "dispatch":
                patcher = patch.object(client, "send_message", send)

                async def invoke(binding=binding):
                    events = ns._guarded_send(
                        binding, await ns.binding_agent_ref(binding)
                    )
                    try:
                        self.assertEqual(await anext(events), "first-receipt")
                    finally:
                        await events.aclose()

            else:
                await client.suspend_session(binding["kagent_session_id"])
                patcher = patch.object(client, "resume_session", paused_resume)

                async def invoke(child=child):
                    await workspaces.resume(child.session_id, self.user)

            with patcher:
                admission = asyncio.create_task(invoke())
                await entered.wait()
                waiter = asyncio.create_task(remove_parent())
                await revoking.wait()
                await self.pool.fetchval("SELECT 1")
                self.assertFalse(revoked.is_set())
                release.set()
                await asyncio.wait_for(admission, 10)
                await asyncio.wait_for(waiter, 10)
            await ns.cancel(child.session_id)
            await ns.cancel(parent.session_id)

    async def test_initial_control_auth_refusal_has_no_started_runtime(self):
        op, _, attempt = await self.create_task(ready=False)
        with patch.object(
            ns.get_client(),
            "create_session",
            AsyncMock(
                side_effect=ServiceConfigurationError(
                    "kagent control service configuration failure (grpc 7)"
                )
            ),
        ) as create:
            await self.worker.reconcile(db, op)
        self.assertEqual(create.await_count, 1)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT state FROM task_attempts WHERE id=$1", attempt.id
            ),
            "failed",
        )
        self.assertFalse(
            await self.pool.fetchval(
                "SELECT capacity_held FROM task_attempts WHERE id=$1", attempt.id
            )
        )
        self.assertFalse(
            await self.pool.fetchval(
                "SELECT EXISTS(SELECT 1 FROM workspace_writer_claims WHERE attempt_id=$1 AND held)",
                attempt.id,
            )
        )

    async def test_router_admission_after_completed_parent_revoke(self):
        from tests.runtime.test_preview_proxy import make_request, make_websocket

        for protocol in ("http", "websocket"):
            with self.subTest(protocol=protocol):
                _, _, parent = await self.create_task()
                _, _, child = await self.create_task(
                    principal=await self.principal(parent)
                )
                parsed = preview_proxy.PreviewHost(5173, child.session_id)
                target = preview_proxy.PreviewTarget(
                    child.session_id, self.user, "kagent", "old", {5173: "fixture"}
                )

                async def wake(_target, parent=parent):
                    # A successful wake has finished; cancellation commits before CONNECT.
                    await revoke(parent.session_id)
                    return True

                with (
                    patch.object(preview_proxy, "current_user", lambda: self.user),
                    patch.object(
                        preview_proxy, "_resolve_target", AsyncMock(return_value=target)
                    ),
                    patch.object(preview_proxy, "_wake_workspace", wake),
                    patch.object(
                        preview_proxy,
                        "_connect_router",
                        Mock(
                            side_effect=preview_proxy._PreviewPreForwardFailure(
                                "fixture CONNECT"
                            )
                        ),
                    ) as connect,
                    patch.object(
                        preview_proxy.asyncio,
                        "open_connection",
                        AsyncMock(
                            side_effect=ConnectionRefusedError("fixture CONNECT")
                        ),
                    ) as open_connection,
                    patch.object(
                        settings, "substrate_router_address", "http://fixture:80"
                    ),
                ):
                    if protocol == "http":
                        response = await preview_proxy._preview_http(
                            make_request("GET", {"host": "fixture"}), parsed
                        )
                        self.assertEqual(
                            connect.call_count,
                            0,
                            "HTTP CONNECT was admitted after parent revocation",
                        )
                        self.assertEqual(response.status_code, 404)
                    else:
                        websocket, _ = make_websocket(
                            {"host": "fixture", "sec-websocket-key": "a2V5"}
                        )
                        await preview_proxy._preview_websocket(websocket, parsed)
                        self.assertEqual(
                            open_connection.await_count,
                            0,
                            "WS CONNECT was admitted after parent revocation",
                        )
                await ns.cancel(child.session_id)
                await ns.cancel(parent.session_id)

    async def test_separate_backend_process_parent_drain_waits_for_child_admission(
        self,
    ):
        _, _, parent = await self.create_task()
        _, _, child = await self.create_task(principal=await self.principal(parent))
        code = """
import asyncio, sys
import asyncpg
from mainloop.push_gate import lifecycle as push
from mainloop.tasks import lifecycle
from mainloop.db import tasks as store
async def main():
    conn = await asyncpg.connect(sys.argv[1])
    try:
        print(await conn.fetchval('SELECT pg_backend_pid()'), flush=True)
        async with push.locked(conn, sys.argv[2], revoke=True), lifecycle.locked(conn, sys.argv[2]), conn.transaction():
            await store.admission_lock(conn)
            await lifecycle.transition(conn, sys.argv[3], 'draining', from_states=('active',))
            await conn.execute('UPDATE native_bindings SET token_hash=NULL WHERE session_id=$1', sys.argv[2])
        print('committed', flush=True)
    finally:
        await conn.close()
asyncio.run(main())
"""
        process = None
        try:
            async with lifecycle.guard(child.session_id, "submit"):
                process = await asyncio.create_subprocess_exec(
                    "uv",
                    "run",
                    "python",
                    "-c",
                    code,
                    self.url,
                    parent.session_id,
                    parent.id,
                    stdout=asyncio.subprocess.PIPE,
                    stderr=asyncio.subprocess.PIPE,
                )
                pid = int(await asyncio.wait_for(process.stdout.readline(), 10))
                committed = asyncio.create_task(process.stdout.readline())

                async def observe():
                    while not committed.done():
                        if await self.pool.fetchval(
                            "SELECT cardinality(pg_blocking_pids($1)) > 0", pid
                        ):
                            return
                    self.fail(
                        "separate process committed parent drain during child admission"
                    )

                await asyncio.wait_for(observe(), 10)
                self.assertIsNotNone(
                    (await ns.get_binding(parent.session_id))["token_hash"]
                )
            self.assertEqual(await asyncio.wait_for(committed, 10), b"committed\n")
            self.assertEqual(await asyncio.wait_for(process.wait(), 10), 0)
            with self.assertRaises(lifecycle.LifecycleDenied):
                async with lifecycle.guard(child.session_id, "submit"):
                    self.fail("child admitted after separate-process parent drain")
        finally:
            if process is not None and process.returncode is None:
                process.kill()
                await process.wait()

    async def test_enrollment_reply_loss_recovers_existing_grant_once(self):
        op, _, attempt = await self.create_task(ready=False)
        original = provisioning.push_lifecycle.enroll
        lost = False

        async def enroll(conn, sid):
            nonlocal lost
            result = await original(conn, sid)
            state = await conn.fetchval(
                "SELECT state FROM task_attempts WHERE binding_id=$1", sid
            )
            if state == "active" and not lost:
                lost = True
                raise RuntimeError("fixture lost enrollment acknowledgement")
            return result

        with (
            patch.object(settings, "push_gate_enabled", True),
            patch.object(provisioning.push_lifecycle, "enroll", enroll),
        ):
            await self.worker.reconcile(db, op)
            current, active = await self.worker._current(op.id)
            self.assertEqual(current.state, "uncertain")
            brief = active.brief_delivery_id
            await provisioning.Provisioning().reconcile(db, current)
            current, active = await self.worker._current(op.id)
            self.assertEqual(
                (current.state, active.brief_delivery_id), ("completed", brief)
            )
            async with self.pool.acquire() as conn:
                row = await conn.fetchrow(
                    "SELECT grant_data,attempt_id,writer_generation FROM push_grants WHERE id=$1",
                    attempt.session_id,
                )
                grant = await push_store.live_grant(conn, push_store.stored_grant(row))
                self.assertEqual((grant.attempt_id, grant.version), (attempt.id, 1))
                self.assertEqual(
                    await conn.fetchval(
                        "SELECT count(*) FROM push_grants WHERE id=$1",
                        attempt.session_id,
                    ),
                    1,
                )
        self.assertEqual(self.spawn.call_count, 1)
