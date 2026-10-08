"""Actual enrollment/native callers and PostgreSQL, with owned fake kagent/Kubernetes."""

import asyncio
import base64
import copy
import uuid
from dataclasses import asdict, replace
from types import SimpleNamespace
from unittest.mock import AsyncMock, Mock, patch

import asyncpg
from kubernetes.client.exceptions import ApiException
from mainloop.config import settings
from mainloop.db import db
from mainloop.db.postgres import MIGRATION_SQL
from mainloop.push_gate import credentials, store
from mainloop.push_gate.protocol import TransportError
from mainloop.push_gate.transport_authority import PostgresTransportAuthority
from mainloop.runtime import agent_credentials
from mainloop.runtime import native_sessions as ns
from mainloop.runtime import workspaces
from mainloop.runtime.agent_identity import token_for
from mainloop.runtime.kagent_client import (
    CurrentRuntimeAssociation,
    KagentSession,
    OutcomeUnknown,
    RuntimeComposition,
    RuntimeOperation,
    RuntimeState,
)
from mainloop.services.github_pr import RepoMetadata
from mainloop.tasks.lifecycle import LifecycleDenied
from tests.runtime.test_postgres_ledger import PostgresTestCase

from models import WorkspaceManifest
from models.push_gate import GitCreatePlan, ProtectedBranchPolicy, PushGrant


class FakeGitSecrets:
    def __init__(self):
        self.objects = {}
        self.creates = []
        self.deletes = []
        self.lose = False
        self.outage = False

    def create_namespaced_secret(self, namespace, body, **kwargs):
        name = body["metadata"]["name"]
        if name in self.objects:
            raise ApiException(status=409)
        obj = SimpleNamespace(
            data=copy.deepcopy(body["data"]),
            type=body["type"],
            immutable=body["immutable"],
            metadata=SimpleNamespace(
                name=name,
                labels=copy.deepcopy(body["metadata"]["labels"]),
                uid=str(uuid.uuid4()),
            ),
        )
        self.objects[name] = obj
        self.creates.append(name)
        if self.lose:
            self.lose = False
            raise RuntimeError("fixture Secret reply loss")
        return copy.deepcopy(obj)

    def read_namespaced_secret(self, name, namespace, **kwargs):
        if self.outage:
            raise ApiException(status=503)
        if name not in self.objects:
            raise ApiException(status=404)
        return copy.deepcopy(self.objects[name])

    def delete_namespaced_secret(self, name, namespace, *, body, **kwargs):
        if self.outage:
            raise ApiException(status=503)
        if name not in self.objects:
            raise ApiException(status=404)
        if body["preconditions"]["uid"] != self.objects[name].metadata.uid:
            raise ApiException(status=409)
        self.deletes.append((name, body["preconditions"]["uid"]))
        del self.objects[name]


class NativeGitClient:
    """Remote fixture state survives a new authority/pool; never a native qualification."""

    def __init__(self, pool, kube):
        self.pool, self.kube = pool, kube
        self.sessions, self.requests = {}, {}
        self.creates, self.gets, self.suspends, self.resumes = [], [], [], []
        self.lose_create = False
        self.lose_suspend = False
        self.lose_resume = False

    async def create_session(
        self,
        agent,
        *,
        request_id,
        credentials=(),
        workspace=None,
        development_environment=None
    ):
        row = await self.pool.fetchrow(
            "SELECT * FROM git_enrollments WHERE create_request_id=$1", request_id
        )
        if row:
            if not row["create_dispatched"]:
                raise AssertionError("Create bytes before committed marker")
            plan = store._decode(row["plan"], GitCreatePlan)
            if tuple(r.model_dump() for r in plan.references) != tuple(
                asdict(r) for r in credentials
            ):
                raise AssertionError("changed create references")
            if request_id not in self.requests and any(
                r.secret_name in self.kube.objects for r in credentials[1:]
            ):
                raise AssertionError("usable Git Secret published before readiness")
        frozen = (agent, credentials, workspace, development_environment)
        if request_id in self.requests and self.requests[request_id][0] != frozen:
            raise AssertionError("changed remote create tuple")
        if request_id not in self.requests:
            sid = str(uuid.uuid4())
            session = KagentSession(
                id=sid,
                context_id=sid,
                agent=agent,
                workspace=workspace,
                development_environment=development_environment,
                creator="mainloop",
                state=RuntimeState.READY,
                operation=RuntimeOperation.NONE,
                prepared_revision="prepared-fixture",
                runtime_association=CurrentRuntimeAssociation(
                    "gen-" + sid,
                    "fixture-space",
                    "session-" + sid,
                    "uid-" + sid,
                    "active",
                    True,
                ),
            )
            if development_environment:
                session = replace(
                    session,
                    runtime_composition=RuntimeComposition(
                        development_environment.image, "fixture", 1, "fixture-cli"
                    ),
                )
            self.requests[request_id] = frozen, sid
            self.sessions[sid] = session
        self.creates.append((request_id, frozen))
        if self.lose_create:
            self.lose_create = False
            raise OutcomeUnknown("fixture Create reply loss")
        return self.sessions[self.requests[request_id][1]]

    async def get_session(self, sid):
        self.gets.append(sid)
        return self.sessions[sid]

    async def ensure_ready(self, session, **kwargs):
        current = self.sessions[session.id]
        if current.state == RuntimeState.SUSPENDED:
            current = await self.resume_session(session.id)
        return current

    async def suspend_session(self, sid):
        self.suspends.append(sid)
        self.sessions[sid] = replace(self.sessions[sid], state=RuntimeState.SUSPENDED)
        if self.lose_suspend:
            self.lose_suspend = False
            raise OutcomeUnknown("fixture Suspend reply loss")
        return self.sessions[sid]

    async def resume_session(self, sid):
        self.resumes.append(sid)
        self.sessions[sid] = replace(self.sessions[sid], state=RuntimeState.READY)
        if self.lose_resume:
            self.lose_resume = False
            raise OutcomeUnknown("fixture Resume reply loss")
        return self.sessions[sid]

    async def delete_session(self, sid):
        self.sessions[sid] = replace(self.sessions[sid], state=RuntimeState.DELETED)
        return self.sessions[sid]


class GitCredentialsCase(PostgresTestCase):
    async def asyncSetUp(self):
        await super().asyncSetUp()
        self.kube = FakeGitSecrets()
        self.native = NativeGitClient(self.pool, self.kube)
        for patcher in (
            patch.object(settings, "git_transport_enabled", True),
            patch.object(settings, "push_gate_enabled", True),
            patch.object(ns, "get_client", return_value=self.native),
            patch.object(credentials, "secrets", credentials.GitSecretStore(self.kube)),
            patch.object(
                agent_credentials.credentials,
                "publish",
                AsyncMock(side_effect=lambda binding, ref: ref),
            ),
        ):
            patcher.start()
            self.addCleanup(patcher.stop)
        self.pid = "git-" + self.user
        await self.pool.execute(
            """INSERT INTO projects(id,user_id,owner,name,full_name,html_url,default_branch)
            VALUES($1,$2,'Owner','Repo','Owner/Repo','https://github.com/Owner/Repo','main')""",
            self.pid,
            self.user,
        )
        self.metadata = AsyncMock(
            return_value=RepoMetadata(
                owner="Owner",
                name="Repo",
                full_name="Owner/Repo",
                description=None,
                default_branch="main",
                avatar_url="",
                html_url="https://github.com/Owner/Repo",
                open_issues_count=0,
            )
        )
        self.authority = PostgresTransportAuthority(db, self.native, self.metadata)

    async def enroll(self, branch="feature"):
        async with self.pool.acquire() as conn, conn.transaction():
            enrolled = await workspaces.enroll_session(
                conn,
                user_id=self.user,
                kind="claude",
                role="agent",
                mcp_grant_kind="workspace",
                manifest=WorkspaceManifest(
                    repo_url="https://github.com/Owner/Repo", branch=branch
                ),
                project_id=self.pid,
                claim_branch=True,
                environment=None,
            )
        return enrolled.workspace_id

    async def create(self, branch="feature"):
        sid = await self.enroll(branch)
        await workspaces._create_session(sid, self.user, reject_removes_rows=False)
        return sid

    async def enrolled(self, sid):
        async with self.pool.acquire() as conn:
            issuance = await conn.fetchval(
                "SELECT issuance_id FROM git_enrollments WHERE binding_id=$1 AND revoked_at IS NULL",
                sid,
            )
            return (await credentials.enrollment_row(conn, issuance))[1]

    async def tokens(self, sid):
        plan = (await self.enrolled(sid)).plan
        return credentials.capability_for(plan, "git-read"), credentials.capability_for(
            plan, "git-push"
        )


class GitCredentialTests(GitCredentialsCase):
    async def test_complete_native_enrollment_and_purpose_separation(self):
        sid = await self.create()
        enrollment = await self.enrolled(sid)
        read, push = await self.tokens(sid)
        self.assertEqual(
            (enrollment.read_state, enrollment.push_state), ("published", "published")
        )
        self.assertEqual(len(enrollment.plan.references), 3)
        self.assertEqual(self.native.suspends, self.native.resumes)
        self.assertEqual(len(self.native.suspends), 1)
        for purpose, token in (("git-read", read), ("git-push", push)):
            obj = self.kube.objects[
                credentials.reference(enrollment.plan, purpose).secret_name
            ]
            self.assertEqual(
                base64.b64decode(obj.data["authorization"]),
                ("Bearer " + token).encode(),
            )
            self.assertTrue(obj.immutable)
            binding = await self.authority.authenticate(token, purpose)
            self.assertEqual(binding.stamp.issuance_id, enrollment.plan.issuance_id)
        for token, purpose in (
            (read, "git-push"),
            (push, "git-read"),
            (token_for(sid), "git-read"),
            (token_for(sid), "git-push"),
        ):
            with self.assertRaises(TransportError):
                await self.authority.authenticate(token, purpose)
        stored = await self.pool.fetchval(
            "SELECT plan::text FROM git_enrollments WHERE binding_id=$1", sid
        )
        self.assertNotIn(read, stored)
        self.assertNotIn(push, stored)

    async def test_default_and_protected_read_without_push(self):
        async with self.pool.acquire() as conn:
            await store.set_policy(
                conn,
                ProtectedBranchPolicy(
                    project_id=self.pid,
                    version=1,
                    default_branch="main",
                    patterns=("release/*",),
                ),
            )
        for branch in ("main", "release/stable"):
            sid = await self.create(branch)
            enrollment = await self.enrolled(sid)
            self.assertEqual(len(enrollment.plan.references), 2)
            self.assertIsNone(
                await self.pool.fetchrow("SELECT * FROM push_grants WHERE id=$1", sid)
            )
            read, _ = await self.tokens(sid)
            binding = await self.authority.authenticate(read, "git-read")
            async with self.authority.authorize_dispatch(binding, None):
                pass

    async def test_plan_and_unknown_create_recover_identical_tuple_after_new_pool(self):
        sid = await self.enroll()
        async with self.pool.acquire() as conn:
            original = await credentials.plan_for_create(conn, sid)
        self.assertFalse(self.kube.objects)
        self.native.lose_create = True
        binding = await ns.get_binding(sid)
        with self.assertRaises(OutcomeUnknown):
            await ns._create_bound_session(binding)
        new = await asyncpg.create_pool(self.url, min_size=1, max_size=2)
        old = db._pool
        db._pool = new
        try:
            await workspaces._create_session(sid, self.user, reject_removes_rows=False)
            recovered = await self.enrolled(sid)
            self.assertEqual(recovered.plan, original)
            self.assertEqual(self.native.creates[0], self.native.creates[1])
            self.assertEqual(len(self.native.requests), 1)
        finally:
            db._pool = old
            await new.close()

    async def test_secret_loss_association_and_grant_hash_restart(self):
        sid = await self.enroll()
        self.kube.lose = True
        with self.assertRaises(RuntimeError):
            await workspaces._create_session(sid, self.user, reject_removes_rows=False)
        before = await self.enrolled(sid)
        read, push = await self.tokens(sid)
        await workspaces._create_session(sid, self.user, reject_removes_rows=False)
        self.assertEqual((await self.enrolled(sid)).plan, before.plan)
        self.assertEqual(len(self.kube.creates), 2)
        self.assertEqual(await self.tokens(sid), (read, push))
        async with self.pool.acquire() as conn:
            _, enrollment = await credentials.enrollment_row(
                conn, before.plan.issuance_id
            )
            current = store._decode(
                await conn.fetchval(
                    "SELECT grant_data FROM push_grants WHERE id=$1", sid
                ),
                PushGrant,
            )
            self.assertEqual(
                await store.issue_derived_locked(conn, current, enrollment), current
            )
        self.assertEqual(current.version, 1)

    async def test_warmup_lost_suspend_and_resume_do_not_repeat_confirmed_operation(
        self,
    ):
        for phase in ("suspend", "resume"):
            sid = await self.enroll(phase)
            setattr(self.native, "lose_" + phase, True)
            with self.assertRaises(OutcomeUnknown):
                await workspaces._create_session(
                    sid, self.user, reject_removes_rows=False
                )
            await workspaces._create_session(sid, self.user, reject_removes_rows=False)
            runtime = (await ns.get_binding(sid))["kagent_session_id"]
            self.assertEqual(self.native.suspends.count(runtime), 1)
            self.assertEqual(self.native.resumes.count(runtime), 1)

    async def test_key_and_secret_conflicts_hold(self):
        sid = await self.create()
        enrollment = await self.enrolled(sid)
        async with self.pool.acquire() as conn:
            with patch.object(settings, "agent_token_key", "changed-fixture-key"):
                with self.assertRaisesRegex(ValueError, "git_key_mismatch"):
                    await credentials.publish_read(conn, enrollment.plan.issuance_id)
            obj = self.kube.objects[
                credentials.reference(enrollment.plan, "git-read").secret_name
            ]
            obj.immutable = False
            with self.assertRaisesRegex(ValueError, "git_secret_conflict"):
                await credentials.publish_read(conn, enrollment.plan.issuance_id)
        self.assertEqual(len(self.kube.creates), 2)

    async def test_revoke_flags_off_cleanup_outage_uid_replacement_and_tombstone(self):
        sid = await self.create()
        enrollment = await self.enrolled(sid)
        read, push = await self.tokens(sid)
        self.kube.outage = True
        with patch.object(settings, "git_transport_enabled", False), patch.object(
            settings, "push_gate_enabled", False
        ):
            await agent_credentials.revoke(sid)
        for token, purpose in ((read, "git-read"), (push, "git-push")):
            with self.assertRaises(TransportError):
                await self.authority.authenticate(token, purpose)
        self.kube.outage = False
        ref = credentials.reference(enrollment.plan, "git-read")
        original_uid = self.kube.objects[ref.secret_name].metadata.uid
        self.kube.objects[ref.secret_name].metadata.uid = "same-name-replacement"
        async with self.pool.acquire() as conn:
            with self.assertRaisesRegex(ValueError, "uid_conflict"):
                await credentials.reconcile_cleanup(conn, enrollment.plan.issuance_id)
            self.kube.objects[ref.secret_name].metadata.uid = original_uid
            await credentials.reconcile_cleanup(conn, enrollment.plan.issuance_id)
        self.assertEqual(len(self.kube.deletes), 2)
        await workspaces._delete_rows(sid, evidence="fixture-deleted")
        self.assertTrue(
            await self.pool.fetchval(
                "SELECT read_state='revoked' FROM git_enrollments WHERE binding_id=$1",
                sid,
            )
        )

    async def test_lost_secret_uid_reply_can_be_cleaned_without_create(self):
        sid = await self.enroll()
        self.kube.lose = True
        with self.assertRaises(RuntimeError):
            await workspaces._create_session(sid, self.user, reject_removes_rows=False)
        count = len(self.kube.creates)
        await agent_credentials.revoke(sid)
        self.assertEqual(len(self.kube.creates), count)
        self.assertFalse(self.kube.objects)
        self.assertEqual(len(self.kube.deletes), 1)

    async def test_revoked_unknown_create_uses_original_tuple_never_republishes(self):
        sid = await self.enroll()
        self.native.lose_create = True
        with self.assertRaises(OutcomeUnknown):
            await ns._create_bound_session(await ns.get_binding(sid))
        await agent_credentials.revoke(sid)
        await self.pool.execute(
            "UPDATE sessions SET status='cancelled' WHERE id=$1", sid
        )
        self.assertFalse(self.kube.objects)
        found = await ns.reconcile_revoked_workspace_creation(await ns.get_binding(sid))
        self.assertEqual(self.native.creates[0], self.native.creates[1])
        self.assertEqual(found.id, next(iter(self.native.sessions)))
        self.assertFalse(self.kube.objects)

    async def test_current_runtime_and_scope_negatives(self):
        sid = await self.create()
        read, push = await self.tokens(sid)
        runtime = (await ns.get_binding(sid))["kagent_session_id"]
        original = self.native.sessions[runtime]
        association = original.runtime_association
        for changed in (
            replace(original, runtime_association=None),
            replace(original, id="other-session"),
            replace(original, creator="other"),
            replace(original, context_id="other"),
            replace(original, context_confirmed=False),
            replace(original, agent=replace(original.agent, name="other-agent")),
            replace(
                original, workspace=replace(original.workspace, branch="other-branch")
            ),
            replace(original, prepared_revision="changed"),
            replace(original, state=RuntimeState.SUSPENDED),
            replace(original, operation=RuntimeOperation.RESUME),
            replace(
                original, runtime_association=replace(association, actor_uid="other")
            ),
            replace(
                original,
                runtime_association=replace(association, generation_id="other"),
            ),
            replace(
                original, runtime_association=replace(association, current_active=False)
            ),
            replace(
                original, runtime_association=replace(association, phase="inactive")
            ),
        ):
            self.native.sessions[runtime] = changed
            for token, purpose in ((read, "git-read"), (push, "git-push")):
                with self.assertRaises(TransportError):
                    await self.authority.authenticate(token, purpose)
        self.native.sessions[runtime] = original
        for sql in (
            "UPDATE sessions SET archived_at=now() WHERE id=$1",
            "UPDATE sessions SET status='completed' WHERE id=$1",
            "UPDATE workspaces SET branch='other' WHERE session_id=$1",
        ):
            async with self.pool.acquire() as conn:
                tx = conn.transaction()
                await tx.start()
                await conn.execute(sql, sid)
                # Authoritative resolver uses this connection to see the mutated facts.
                with self.assertRaises((ValueError, LifecycleDenied)):
                    await credentials.validate_scope(conn, await self.enrolled(sid))
                await tx.rollback()

    async def test_caller_owned_single_connection_no_nested_pool(self):
        sid = await self.enroll()
        tiny = await asyncpg.create_pool(self.url, min_size=1, max_size=1)
        old = db._pool
        db._pool = tiny
        try:
            async with tiny.acquire() as conn:
                binding = await ns.get_binding(sid, conn=conn)
                session = await asyncio.wait_for(
                    ns._create_bound_session(binding, conn=conn), 5
                )
                await ns.ledger.update_binding(
                    sid, kagent_session_id=session.id, conn=conn
                )
                await asyncio.wait_for(
                    credentials.ready_for_binding(conn, sid, session), 5
                )
                await ns.validate_bound_session(
                    await ns.get_binding(sid, conn=conn), session, conn=conn
                )
                await ns.ledger.record_composition(sid, session, conn=conn)
        finally:
            db._pool = old
            await tiny.close()

    async def test_migration_rerun_preserves_plan_and_immutability(self):
        sid = await self.create()
        before = await self.pool.fetchval(
            "SELECT plan::text FROM git_enrollments WHERE binding_id=$1", sid
        )
        await self.pool.execute(MIGRATION_SQL)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT plan::text FROM git_enrollments WHERE binding_id=$1", sid
            ),
            before,
        )
        with self.assertRaises(asyncpg.RaiseError):
            await self.pool.execute(
                'UPDATE git_enrollments SET plan=plan || \'{"branch":"changed"}\'::jsonb WHERE binding_id=$1',
                sid,
            )

    async def test_existing_dispatched_tuple_cannot_gain_git_refs(self):
        sid = await self.enroll()
        with patch.object(settings, "git_transport_enabled", False):
            await ns._create_bound_session(await ns.get_binding(sid))
        async with self.pool.acquire() as conn:
            with self.assertRaisesRegex(
                ValueError, "original_create_history_unavailable"
            ):
                await credentials.plan_for_create(conn, sid)
        self.assertFalse(self.kube.objects)

    async def test_replacement_cannot_reuse_original_create_identity(self):
        sid = await self.create()
        binding = await ns.get_binding(sid)
        original_plan = await self.enrolled(sid)
        with self.assertRaisesRegex(ValueError, "replacement_create_identity_required"):
            await ns.ledger.replace_kagent_session(
                sid, binding["kagent_session_id"], ns._request_id(binding)
            )
        self.assertEqual(await ns.get_binding(sid), binding)
        self.assertEqual(await self.enrolled(sid), original_plan)

    async def test_native_send_gate_refuses_revoked_or_incomplete_git_setup(self):
        sid = await self.create()
        binding = await ns.get_binding(sid)
        emitted = []

        async def send(*args, **kwargs):
            emitted.append("turn bytes")
            yield "event"

        # Keep the existing tuple but remove its runtime association observation.
        current = self.native.sessions[binding["kagent_session_id"]]
        self.native.sessions[current.id] = replace(current, runtime_association=None)
        with patch.object(self.native, "send_message", send, create=True):
            with self.assertRaises(ValueError):
                await anext(ns._guarded_send(binding, current.agent))
        self.assertFalse(emitted)

    async def test_warmup_cannot_change_prepared_revision(self):
        sid = await self.enroll()
        resume = self.native.resume_session

        async def changed(runtime):
            result = await resume(runtime)
            self.native.sessions[runtime] = replace(
                result, prepared_revision="changed-during-warmup"
            )
            return self.native.sessions[runtime]

        with patch.object(self.native, "resume_session", changed):
            with self.assertRaisesRegex(ValueError, "prepared_contract_changed"):
                await workspaces._create_session(
                    sid, self.user, reject_removes_rows=False
                )
        self.assertFalse(self.kube.objects)


class GitTaskCredentialTests(GitCredentialsCase):
    async def asyncSetUp(self):
        await super().asyncSetUp()
        from functools import partial

        from mainloop.db import environments
        from mainloop.providers import (
            TASK_CODE_CAPABILITIES,
            TASK_REQUIRED_CAPABILITIES,
            registry,
        )
        from mainloop.tasks import provisioning
        from mainloop.tasks.service import ports, select_profile
        from tests.test_workspace_environments import validated_version

        from models.environment import DevEnvironment, SelectEnvironment
        from models.provider import CapabilityResult

        env_id, version_id = "env-" + self.user, "version-" + self.user
        version = validated_version().model_copy(
            update={"id": version_id, "environment_id": env_id}
        )
        async with self.pool.acquire() as conn, conn.transaction():
            await environments.register(
                conn,
                DevEnvironment(
                    id=env_id,
                    owner_id=self.user,
                    name="fixture",
                    source_kind="prebuilt_image",
                ),
                version,
            )
            await environments.set_default(conn, env_id, self.user, version_id)
            await environments.select(
                conn,
                self.pid,
                self.user,
                SelectEnvironment(
                    environment_id=env_id, follow_default=True, expected_version=0
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
                            evidence_ref="fixture:p2",
                        )
                        for name in sorted(
                            TASK_REQUIRED_CAPABILITIES | TASK_CODE_CAPABILITIES
                        )
                    )
                }
            )
            for p in registry().profiles
        ]
        self.worker = provisioning.Provisioning()
        saved = ports.provisioning
        ports.provisioning = self.worker
        self.addCleanup(setattr, ports, "provisioning", saved)
        self.spawn = Mock()
        for patcher in (
            patch.object(settings, "provider_profiles", profiles),
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

    async def task(self, *, parent=None, ready=True, branch=None):
        from mainloop.tasks import lifecycle
        from mainloop.tasks.principal import TaskPrincipal
        from mainloop.tasks.service import mutate

        from models.task import TaskCheckout, TaskCreate

        principal = TaskPrincipal(self.user)
        if parent:
            async with self.pool.acquire() as conn:
                principal = await lifecycle.authenticate_binding(
                    conn, await ns.get_binding(parent.session_id, conn=conn)
                )
        request = TaskCreate(
            request_id=uuid.uuid4().hex,
            title="fixture",
            brief="fixture brief",
            mode="code",
            project_id=self.pid,
            checkout=TaskCheckout(
                branch=branch or "feature/" + uuid.uuid4().hex, ref="a" * 40
            ),
        )
        async with self.pool.acquire() as conn, conn.transaction():
            operation = await mutate(conn, principal, "create", request)
        if ready:
            await self.worker.reconcile(db, operation)
        async with self.pool.acquire() as conn:
            return (
                operation,
                await lifecycle.load_task(conn, operation.task_id),
                await lifecycle.load_attempt(conn, operation.attempt_id),
            )

    async def test_actual_provisioning_active_parent_child_and_creating_read_only(self):
        from mainloop.tasks import lifecycle

        _, _, parent = await self.task()
        self.assertEqual(parent.state, "active")
        operation, _, child = await self.task(parent=parent, ready=False)
        binding = await ns.get_binding(child.session_id)
        session = await ns._create_bound_session(binding)
        await ns.ledger.update_binding(child.session_id, kagent_session_id=session.id)
        async with self.pool.acquire() as conn:
            await credentials.ready_for_binding(
                conn, child.session_id, session, push=False
            )
        read, push = await self.tokens(child.session_id)
        await self.authority.authenticate(read, "git-read")
        with self.assertRaises(TransportError):
            await self.authority.authenticate(push, "git-push")
        self.assertIsNone(
            await self.pool.fetchrow(
                "SELECT id FROM push_grants WHERE id=$1", child.session_id
            )
        )
        await self.worker.reconcile(db, operation)
        binding = await self.authority.authenticate(push, "git-push")
        self.assertEqual(binding.grant.attempt_id, child.id)
        self.assertEqual(binding.grant.branch_claim_generation, child.writer_generation)
        self.assertEqual(self.spawn.call_count, 2)
        async with self.pool.acquire() as conn:
            await lifecycle.check(conn, child.session_id, "submit")

    async def test_parent_claim_scope_and_terminal_negatives(self):
        _, _, parent = await self.task()
        _, _, child = await self.task(parent=parent)
        read, push = await self.tokens(child.session_id)
        stamp = (await self.authority.authenticate(read, "git-read")).stamp
        negatives = (
            (
                "UPDATE native_bindings SET token_hash=NULL WHERE session_id=$1",
                parent.session_id,
            ),
            ("UPDATE task_attempts SET state='draining' WHERE id=$1", parent.id),
            (
                "UPDATE workspace_writer_claims SET held=FALSE WHERE attempt_id=$1",
                parent.id,
            ),
            (
                "UPDATE workspace_writer_claims SET held=FALSE WHERE attempt_id=$1",
                child.id,
            ),
            ("UPDATE task_attempts SET state='superseded' WHERE id=$1", child.id),
            ("UPDATE tasks SET current_attempt_id=NULL WHERE id=$1", child.task_id),
            ("UPDATE sessions SET project_id=NULL WHERE id=$1", child.session_id),
        )
        for sql, key in negatives:
            async with self.pool.acquire() as conn:
                tx = conn.transaction()
                await tx.start()
                await conn.execute(sql, key)
                with self.assertRaises((ValueError, LifecycleDenied)):
                    await credentials.validate_scope(
                        conn, await self.enrolled(child.session_id), creating=True
                    )
                await tx.rollback()
        # Actual peer revocation, not a copied binding, invalidates both purposes.
        await agent_credentials.revoke(parent.session_id)
        for token, purpose in ((read, "git-read"), (push, "git-push")):
            with self.assertRaises(TransportError):
                await self.authority.authenticate(token, purpose)
        self.assertEqual(stamp.binding_id, child.session_id)

    async def test_unknown_git_cancellation_keeps_operation_claim_and_source(self):
        from mainloop.tasks import lifecycle, publication
        from mainloop.tasks.principal import TaskPrincipal
        from mainloop.tasks.service import mutate

        from models.push_gate import PublicationAttempt, PublicationState, RefUpdate
        from models.task import TaskAction

        _, task, attempt = await self.task()
        sid = attempt.session_id
        read, push = await self.tokens(sid)
        grant = (await self.authority.authenticate(push, "git-push")).grant
        async with self.pool.acquire() as conn:
            publication_attempt = PublicationAttempt(
                request_id="lost-push",
                grant_id=grant.id,
                repository=grant.repository,
                update=RefUpdate(
                    ref="refs/heads/" + grant.branch, old_oid="0" * 40, new_oid="b" * 40
                ),
                grant_version=grant.version,
                policy_version=1,
            )
            await store.record_attempt(conn, publication_attempt)
            await store.transition(
                conn, grant.id, "lost-push", PublicationState.DISPATCHING
            )
            async with conn.transaction():
                refs = await publication.unresolved_intents(conn, task, attempt)
                self.assertIn("git-push:" + sid + ":lost-push", refs)
                operation = await mutate(
                    conn,
                    TaskPrincipal(self.user),
                    "cancel",
                    TaskAction(
                        request_id="cancel",
                        expected_version=task.version,
                        expected_attempt_id=attempt.id,
                    ),
                    task_id=task.id,
                )
        await self.worker.reconcile(db, operation)
        async with self.pool.acquire() as conn:
            current = await lifecycle.load_attempt(conn, attempt.id)
            self.assertEqual(current.state, "draining")
            self.assertTrue(
                await conn.fetchval(
                    "SELECT held FROM workspace_writer_claims WHERE attempt_id=$1",
                    attempt.id,
                )
            )
            self.assertNotEqual(
                await conn.fetchval(
                    "SELECT state FROM task_operations WHERE id=$1", operation.id
                ),
                "completed",
            )
            self.assertEqual(
                await conn.fetchval(
                    "SELECT current_attempt_id FROM tasks WHERE id=$1", task.id
                ),
                attempt.id,
            )
        for token, purpose in ((read, "git-read"), (push, "git-push")):
            with self.assertRaises(TransportError):
                await self.authority.authenticate(token, purpose)
        self.assertIsNotNone(await ns.get_binding(sid))
