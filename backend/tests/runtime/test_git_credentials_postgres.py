"""Actual enrollment/native callers and PostgreSQL, with owned fake kagent/Kubernetes."""

import asyncio
import base64
import copy
import json
import uuid
from dataclasses import asdict, replace
from functools import partial
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, Mock, patch

import asyncpg
import httpx
from kubernetes.client.exceptions import ApiException
from mainloop import api
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
    AGENT_SETUP_DIGEST,
    CurrentRuntimeAssociation,
    KagentSession,
    OutcomeUnknown,
    PreparationReceipt,
    PreparationRequest,
    RuntimeComposition,
    RuntimeOperation,
    RuntimeState,
    SessionError,
)
from mainloop.services import github_checkout
from mainloop.services.github_creation import GitHubCreationClient
from mainloop.services.github_pr import RepoMetadata
from mainloop.tasks.lifecycle import LifecycleDenied
from tests.runtime.github_app_fake import app_settings
from tests.runtime.test_github_checkout import CheckoutServer
from tests.runtime.test_postgres_ledger import PostgresTestCase

from models import WorkspaceManifest
from models.push_gate import GitCreatePlan, ProtectedBranchPolicy, PushGrant
from models.workspace import WorkspaceEnvironment


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
        self.prepares = []
        self.prepare_classification = "confirmed"
        self.lose_prepare = False
        self.prepare_not_received = False
        self.prepare_error = None

    async def create_session(
        self,
        agent,
        *,
        request_id,
        credentials=(),
        workspace=None,
        development_environment=None,
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
        session = self.sessions[sid]
        receipt = session.workspace_preparation
        if receipt is None:
            return session
        # kagent d6de0e4 native_handoff.go projects history on GetSession;
        # a temporary lifecycle state does not mutate the stored receipt.
        original, association = receipt.original, session.runtime_association
        historical = (
            receipt.historical
            or session.state != RuntimeState.READY
            or session.operation != RuntimeOperation.NONE
            or session.id != original.session_id
            or session.prepared_revision != original.prepared_revision
            or session.workspace != original.workspace
            or session.development_environment != original.development_environment
            or session.runtime_composition != original.runtime_composition
            or association is None
            or association.phase != "active"
            or not association.current_active
            or association.generation_id != original.generation_id
            or association.actor_uid != original.actor_uid
        )
        return replace(
            session, workspace_preparation=replace(receipt, historical=historical)
        )

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

    async def prepare_session_workspace(self, session_id, **kwargs):
        request = PreparationRequest(session_id=session_id, **kwargs)
        row = await self.pool.fetchrow(
            "SELECT * FROM git_enrollments WHERE create_request_id=$1",
            request.create_request_id,
        )
        if (
            row["prepare_state"] != "requested"
            or row["prepare_action_id"] != request.action_id
            or json.loads(row["prepare_receipt"])["original"] != asdict(request)
            or row["read_state"] != "published"
        ):
            raise AssertionError(
                "Prepare bytes before committed request/read publication"
            )
        self.prepares.append(request.encode())
        if self.prepare_error:
            raise self.prepare_error
        if self.prepare_not_received:
            self.prepare_not_received = False
            raise OutcomeUnknown("fixture Prepare lost before admission")
        current = self.sessions[session_id]
        receipt = PreparationReceipt(
            original=request,
            context_id=session_id,
            atespace=current.runtime_association.atespace,
            actor_name=current.runtime_association.actor_name,
            classification=self.prepare_classification,
        )
        self.sessions[session_id] = replace(current, workspace_preparation=receipt)
        if self.lose_prepare:
            self.lose_prepare = False
            raise OutcomeUnknown("fixture Prepare reply loss")
        return receipt


class GitCredentialsCase(PostgresTestCase):
    async def asyncSetUp(self):
        await super().asyncSetUp()
        self.kube = FakeGitSecrets()
        self.native = NativeGitClient(self.pool, self.kube)
        self.real_ref_resolver = github_checkout.resolve_checkout_ref
        self.ref_resolver = AsyncMock(return_value="a" * 40)
        for patcher in (
            patch.object(settings, "git_transport_enabled", True),
            patch.object(settings, "push_gate_enabled", True),
            patch.object(github_checkout, "resolve_checkout_ref", self.ref_resolver),
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

    async def enroll(self, branch="feature", ref=""):
        async with self.pool.acquire() as conn, conn.transaction():
            enrolled = await workspaces.enroll_session(
                conn,
                user_id=self.user,
                kind="claude",
                role="agent",
                mcp_grant_kind="workspace",
                manifest=WorkspaceManifest(
                    repo_url="https://github.com/Owner/Repo", branch=branch, ref=ref
                ),
                project_id=self.pid,
                claim_branch=True,
                environment=WorkspaceEnvironment(
                    environment_id="fixture-env",
                    version_id="fixture-version",
                    image="ghcr.io/example/dev@sha256:" + "a" * 64,
                    platform="linux/arm64",
                    policy_identity="fixture-version:oci-static-v2",
                ),
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

    async def creation_counts(self):
        return dict(
            await self.pool.fetchrow(
                """SELECT
            (SELECT count(*) FROM main_threads) AS main_threads,
            (SELECT count(*) FROM conversations) AS conversations,
            (SELECT count(*) FROM sessions) AS sessions,
            (SELECT count(*) FROM workspaces) AS workspaces,
            (SELECT count(*) FROM native_bindings) AS native_bindings,
            (SELECT count(*) FROM tasks) AS tasks,
            (SELECT count(*) FROM task_attempts) AS task_attempts,
            (SELECT count(*) FROM task_operations) AS task_operations,
            (SELECT count(*) FROM workspace_writer_claims) AS workspace_writer_claims,
            (SELECT count(*) FROM git_enrollments) AS git_enrollments"""
            )
        )


class OwnerWorkspaceApiCheckoutTests(GitCredentialsCase):
    """Public owner routes with real admission/DB and offline GitHub/native effects."""

    async def asyncSetUp(self):
        await super().asyncSetUp()
        self.server = CheckoutServer()
        self.server.sha = "b" * 40
        self.resolve = AsyncMock(wraps=self.real_ref_resolver)
        for patcher in (
            patch.object(settings, "owner_id", self.user),
            patch.object(settings, "api_hosts", "test"),
            app_settings(),
            patch.object(github_checkout, "resolve_checkout_ref", self.resolve),
            patch.object(
                github_checkout,
                "GitHubCreationClient",
                partial(
                    GitHubCreationClient,
                    transport=httpx.MockTransport(self.server.handle),
                ),
            ),
            patch(
                "mainloop.environments.resolution.resolve",
                AsyncMock(
                    return_value=WorkspaceEnvironment(
                        environment_id="fixture-env",
                        version_id="fixture-version",
                        image="ghcr.io/example/dev@sha256:" + "a" * 64,
                        platform="linux/arm64",
                        policy_identity="fixture-version:oci-static-v2",
                    )
                ),
            ),
            patch.object(workspaces, "publish", AsyncMock()),
        ):
            patcher.start()
            self.addCleanup(patcher.stop)
        self.client = httpx.AsyncClient(
            transport=httpx.ASGITransport(app=api.app), base_url="http://test"
        )
        self.addAsyncCleanup(self.client.aclose)

    async def project_count(self):
        return await self.pool.fetchval("SELECT count(*) FROM projects")

    async def assert_frozen(self, response, branch):
        self.assertEqual(response.status_code, 201, response.text)
        sid = response.json()["workspace_id"]
        row = await self.pool.fetchrow(
            "SELECT ref,branch FROM workspaces WHERE session_id=$1", sid
        )
        self.assertEqual((row["ref"], row["branch"]), (self.server.sha, branch))
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT base_branch FROM sessions WHERE id=$1", sid
            ),
            self.server.sha,
        )
        self.assertEqual((await self.enrolled(sid)).plan.workspace.ref, self.server.sha)

    async def test_empty_owner_ref_uses_remote_default_even_with_a_stored_default(self):
        await self.pool.execute(
            "UPDATE projects SET default_branch='stale-main' WHERE id=$1", self.pid
        )
        for index, target in enumerate(
            ({"project_id": self.pid}, {"repo": "Owner/Repo"})
        ):
            with self.subTest(target=target):
                self.resolve.reset_mock()
                branch = f"remote-default-{index}"
                response = await self.client.post(
                    "/workspaces", json={**target, "branch": branch}
                )
                await self.assert_frozen(response, branch)
                self.resolve.assert_awaited_once_with("owner/repo", "")
        commits = [
            r.url.path for r in self.server.requests if "/commits/" in r.url.path
        ]
        self.assertEqual(commits, ["/repos/owner/repo/commits/trunk"] * 2)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT default_branch FROM projects WHERE id=$1", self.pid
            ),
            "stale-main",
        )

    async def test_unavailable_repo_ref_leaves_project_counts_unchanged(self):
        self.server.status = 404
        before_projects = await self.project_count()
        before_rows = await self.creation_counts()
        response = await self.client.post(
            "/workspaces",
            json={"repo": "owner/new-repo", "branch": "missing", "ref": "unavailable"},
        )
        self.assertEqual(response.status_code, 422, response.text)
        self.assertIn("could not be resolved", response.json()["detail"])
        self.resolve.assert_awaited_once_with("owner/new-repo", "unavailable")
        self.assertEqual(await self.project_count(), before_projects)
        self.assertEqual(await self.creation_counts(), before_rows)
        self.assertFalse(self.native.creates)
        self.assertFalse(self.kube.objects)

    async def test_unavailable_repo_ref_does_not_touch_an_existing_project(self):
        self.server.status = 404
        await self.pool.execute(
            "UPDATE projects SET last_used_at='2020-01-01T00:00:00Z' WHERE id=$1",
            self.pid,
        )
        before = await self.pool.fetchrow(
            "SELECT * FROM projects WHERE id=$1", self.pid
        )
        before_projects = await self.project_count()
        before_rows = await self.creation_counts()
        response = await self.client.post(
            "/workspaces",
            json={"repo": "owner/repo", "branch": "missing", "ref": "unavailable"},
        )
        self.assertEqual(response.status_code, 422, response.text)
        self.assertEqual(
            await self.pool.fetchrow("SELECT * FROM projects WHERE id=$1", self.pid),
            before,
        )
        self.assertEqual(await self.project_count(), before_projects)
        self.assertEqual(await self.creation_counts(), before_rows)
        self.assertFalse(self.native.creates)
        self.assertFalse(self.kube.objects)

    async def test_repo_ref_resolves_before_insert_and_is_reused_by_enrollment(self):
        before_projects = await self.project_count()

        async def resolve(repository, ref):
            self.assertEqual(await self.project_count(), before_projects)
            return await self.real_ref_resolver(repository, ref)

        self.resolve.side_effect = resolve
        response = await self.client.post(
            "/workspaces",
            json={
                "repo": "owner/new-repo",
                "branch": "new-workspace",
                "ref": "base/topic",
            },
        )
        await self.assert_frozen(response, "new-workspace")
        self.resolve.assert_awaited_once_with("owner/new-repo", "base/topic")
        self.assertEqual(await self.project_count(), before_projects + 1)
        self.assertEqual(len(self.native.creates), 1)
        # A freshly imported project caches '' until the writer plan learns GitHub's value.
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT default_branch FROM projects WHERE lower(full_name)='owner/new-repo' AND user_id=$1",
                self.user,
            ),
            "trunk",
        )
        plan = (await self.enrolled(response.json()["workspace_id"])).plan
        self.assertEqual(
            credentials.git_reference_counts(plan.references), {"read": 1, "push": 1}
        )


class GitCredentialTests(GitCredentialsCase):
    async def test_owner_freezes_default_branch_tag_and_sha_before_rows(self):
        from functools import partial

        import httpx
        from mainloop.services.github_creation import GitHubCreationClient
        from tests.runtime.github_app_fake import app_settings
        from tests.runtime.test_github_checkout import CheckoutServer

        server = CheckoutServer()
        with (
            app_settings(),
            patch.object(
                github_checkout, "resolve_checkout_ref", self.real_ref_resolver
            ),
            patch.object(
                github_checkout,
                "GitHubCreationClient",
                partial(
                    GitHubCreationClient, transport=httpx.MockTransport(server.handle)
                ),
            ),
        ):
            for index, ref in enumerate(("", "base/topic", "v1.2.3", "a" * 40)):
                sid = await self.enroll(branch=f"owner-ref-{index}", ref=ref)
                row = await self.pool.fetchrow(
                    "SELECT ref,branch FROM workspaces WHERE session_id=$1", sid
                )
                self.assertEqual(
                    (row["ref"], row["branch"]), ("a" * 40, f"owner-ref-{index}")
                )
                self.assertEqual(
                    await self.pool.fetchval(
                        "SELECT base_branch FROM sessions WHERE id=$1", sid
                    ),
                    "a" * 40,
                )
        self.assertFalse(self.native.creates)

    async def test_owner_ref_failure_precedes_any_enrollment_or_create(self):
        before = await self.creation_counts()
        self.ref_resolver.side_effect = github_checkout.CheckoutRefUnavailable(
            "Checkout ref could not be resolved to a GitHub commit."
        )
        with self.assertRaisesRegex(
            workspaces.WorkspaceRejected, "could not be resolved"
        ):
            await self.enroll(ref="missing")
        self.assertEqual(await self.creation_counts(), before)
        self.assertFalse(self.native.creates)
        self.assertFalse(self.kube.objects)

    async def test_owner_create_loss_retry_and_replacement_reuse_frozen_sha(self):
        sid = await self.enroll(ref="feature/base")
        self.ref_resolver.assert_awaited_once_with("owner/repo", "feature/base")
        self.native.lose_create = True
        await workspaces._create_session(sid, self.user, reject_removes_rows=False)
        first = (await self.enrolled(sid)).plan
        self.ref_resolver.return_value = "b" * 40
        await workspaces._create_session(sid, self.user, reject_removes_rows=False)
        self.assertEqual((await self.enrolled(sid)).plan, first)
        self.assertEqual(self.native.creates[0], self.native.creates[1])
        self.assertEqual(
            (first.workspace.ref, first.workspace.branch), ("a" * 40, "feature")
        )
        old = await ns.get_binding(sid)
        await self.native.delete_session(old["kagent_session_id"])
        await ns._replace_kagent_session(old)
        await workspaces._create_session(sid, self.user, reject_removes_rows=False)
        replacement = (await self.enrolled(sid)).plan
        self.assertNotEqual(replacement.create_request_id, first.create_request_id)
        self.assertEqual(replacement.workspace.ref, first.workspace.ref)
        self.assertEqual(self.ref_resolver.await_count, 1)

    async def test_owner_enrollment_leaves_refs_unchanged_when_either_flag_is_off(self):
        for git, push in ((False, False), (False, True), (True, False)):
            with patch.object(settings, "git_transport_enabled", git), patch.object(
                settings, "push_gate_enabled", push
            ):
                sid = await self.enroll(branch=f"off-{git}-{push}", ref="base-branch")
                self.assertEqual(
                    await self.pool.fetchval(
                        "SELECT ref FROM workspaces WHERE session_id=$1", sid
                    ),
                    "base-branch",
                )
        self.ref_resolver.assert_not_awaited()

    async def test_replacement_first_send_requires_confirmation(self):
        # Adapted from prep-slice2/review-probes.py: use the actual replacement,
        # readiness and send paths, with a binding-wide completed-turn count.
        with patch.object(settings, "git_transport_enabled", False), patch.object(
            settings, "push_gate_enabled", False
        ):
            sid = await self.create("owner-replacement")
            await ns.ledger.bump_turns(sid)
        old = await ns.get_binding(sid)
        await self.native.delete_session(old["kagent_session_id"])
        await ns._replace_kagent_session(old)
        self.native.prepare_not_received = True
        with self.assertRaisesRegex(ValueError, "git_prepare_pending"):
            await workspaces._create_session(sid, self.user, reject_removes_rows=False)
        binding = await ns.get_binding(sid)
        row = await self.pool.fetchrow(
            "SELECT * FROM git_enrollments WHERE binding_id=$1 AND revoked_at IS NULL",
            sid,
        )
        current = self.native.sessions[binding["kagent_session_id"]]
        self.assertNotEqual(current.id, old["kagent_session_id"])
        self.assertEqual(binding["turns"], 1)
        self.assertEqual(row["prepare_state"], "requested")
        self.assertEqual(len(self.native.prepares), 1)
        emitted = []

        async def send(*args, **kwargs):
            emitted.append("turn bytes")
            yield "first event"

        self.native.prepare_not_received = True
        error = None
        with patch.object(self.native, "send_message", send, create=True):
            events = ns._guarded_send(binding, current.agent)
            try:
                await anext(events)
            except ValueError as exc:
                error = str(exc)
            finally:
                await events.aclose()
        self.assertEqual(emitted, [], "Replacement's first turn must await preparation")
        self.assertEqual(error, "git_prepare_pending")

    async def test_existing_row_migration_preserves_authority(self):
        sid = await self.create("migration-probe")
        before = await self.pool.fetchrow(
            "SELECT * FROM git_enrollments WHERE binding_id=$1", sid
        )
        # The sanitized base migration keeps CI independent of local Git history.
        old_sql = (
            Path(__file__).parent / "fixtures" / "git-enrollment-before-preparation.sql"
        ).read_text()
        await self.pool.execute(old_sql)
        await self.pool.execute(
            "ALTER TABLE git_enrollments DROP COLUMN prepare_action_id, "
            "DROP COLUMN prepare_state, DROP COLUMN prepare_receipt"
        )
        await self.pool.execute(MIGRATION_SQL)
        await self.pool.execute(MIGRATION_SQL)
        after = await self.pool.fetchrow(
            "SELECT * FROM git_enrollments WHERE binding_id=$1", sid
        )
        self.assertEqual(after["prepare_state"], "absent")
        self.assertIsNone(after["prepare_action_id"])
        self.assertIsNone(after["prepare_receipt"])
        for field in ("plan", "plan_digest", "association", "read_state", "push_state"):
            self.assertEqual(after[field], before[field])

    async def test_owner_preparation_uses_agent_profile_and_allows_first_turn(self):
        sid = await self.create()
        row = await self.pool.fetchrow(
            "SELECT * FROM git_enrollments WHERE binding_id=$1", sid
        )
        self.assertEqual(row["prepare_state"], "confirmed")
        request = PreparationRequest.decode(self.native.prepares[0])
        self.assertEqual(
            (request.setup_profile, request.setup_digest), ("agent", AGENT_SETUP_DIGEST)
        )
        self.assertEqual(request.workspace.ref, "a" * 40)
        binding = await ns.get_binding(sid)
        current = self.native.sessions[binding["kagent_session_id"]]
        emitted = []

        async def send(*args, **kwargs):
            emitted.append("turn bytes")
            yield "event"

        with patch.object(self.native, "send_message", send, create=True):
            events = ns._guarded_send(binding, current.agent)
            try:
                self.assertEqual(await anext(events), "event")
            finally:
                await events.aclose()
        self.assertEqual(emitted, ["turn bytes"])

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
        # The scratch database is shared across this class; old held attempts
        # must not consume the next test's global admission capacity.
        await self.pool.execute(
            "TRUNCATE tasks,task_attempts,workspace_writer_claims,task_operations CASCADE"
        )
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

    async def task(
        self, *, parent=None, ready=True, branch=None, ref="a" * 40, request_id=None
    ):
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
            request_id=request_id or uuid.uuid4().hex,
            title="fixture",
            brief="fixture brief",
            mode="code",
            project_id=self.pid,
            checkout=TaskCheckout(
                branch=branch or "feature/" + uuid.uuid4().hex, ref=ref
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

    async def preparation_target(self):
        operation, _, attempt = await self.task(ready=False)
        binding = await ns.get_binding(attempt.session_id)
        session = await ns._create_bound_session(binding)
        await ns.ledger.update_binding(attempt.session_id, kagent_session_id=session.id)
        return operation, attempt.session_id, session.id

    async def test_task_freezes_default_ref_before_admission(self):
        await self.assert_task_ref_frozen("")

    async def test_task_freezes_branch_ref_before_admission(self):
        await self.assert_task_ref_frozen("base/topic")

    async def test_task_freezes_tag_ref_before_admission(self):
        await self.assert_task_ref_frozen("v1.2.3")

    async def test_task_verifies_full_sha_before_admission(self):
        await self.assert_task_ref_frozen("a" * 40)

    async def assert_task_ref_frozen(self, ref):
        from functools import partial

        import httpx
        from mainloop.services.github_creation import GitHubCreationClient
        from tests.runtime.github_app_fake import app_settings
        from tests.runtime.test_github_checkout import CheckoutServer

        server = CheckoutServer()
        with (
            app_settings(),
            patch.object(
                github_checkout, "resolve_checkout_ref", self.real_ref_resolver
            ),
            patch.object(
                github_checkout,
                "GitHubCreationClient",
                partial(
                    GitHubCreationClient, transport=httpx.MockTransport(server.handle)
                ),
            ),
        ):
            _, task, attempt = await self.task(ready=False, ref=ref)
            self.assertEqual(task.checkout.ref, "a" * 40)
            row = await self.pool.fetchrow(
                "SELECT ref,branch FROM workspaces WHERE session_id=$1",
                attempt.session_id,
            )
            self.assertEqual(
                (row["ref"], row["branch"]), ("a" * 40, task.checkout.branch)
            )
        self.assertFalse(self.native.creates)

    async def test_task_ref_failure_rolls_back_before_admission(self):
        from mainloop.db.tasks import TaskError

        before = await self.creation_counts()
        self.ref_resolver.side_effect = github_checkout.CheckoutRefUnavailable(
            "ref unavailable"
        )
        with self.assertRaises(TaskError) as raised:
            await self.task(ready=False, ref="missing")
        self.assertEqual(
            (raised.exception.status, raised.exception.code),
            (422, "checkout_ref_unavailable"),
        )
        self.assertEqual(await self.creation_counts(), before)
        self.assertFalse(self.native.creates)
        self.assertFalse(self.kube.objects)

    async def test_task_idempotent_request_and_runtime_retry_keep_initial_sha(self):
        args = dict(
            ready=False,
            branch="frozen-task",
            ref="base-branch",
            request_id="frozen-task-create",
        )
        operation, task, attempt = await self.task(**args)
        self.ref_resolver.assert_awaited_once_with("Owner/Repo", "base-branch")
        self.assertEqual(
            (task.checkout.ref, task.checkout.branch), ("a" * 40, "frozen-task")
        )
        self.native.lose_create = True
        await self.worker.reconcile(db, operation)
        plan = (await self.enrolled(attempt.session_id)).plan
        self.ref_resolver.return_value = "b" * 40
        replay, task, replay_attempt = await self.task(**args)
        self.assertEqual((replay.id, replay_attempt.id), (operation.id, attempt.id))
        self.assertEqual(task.checkout.ref, "a" * 40)
        await self.worker.reconcile(db, replay)
        self.assertEqual((await self.enrolled(attempt.session_id)).plan, plan)
        self.assertEqual(self.native.creates[0], self.native.creates[1])
        self.assertEqual(self.ref_resolver.await_count, 1)

    async def test_task_refs_unchanged_when_either_flag_is_off(self):
        for git, push in ((False, False), (False, True), (True, False)):
            with patch.object(settings, "git_transport_enabled", git), patch.object(
                settings, "push_gate_enabled", push
            ):
                _, task, attempt = await self.task(ready=False, ref="base-branch")
                self.assertEqual(task.checkout.ref, "base-branch")
                self.assertEqual(
                    await self.pool.fetchval(
                        "SELECT ref FROM workspaces WHERE session_id=$1",
                        attempt.session_id,
                    ),
                    "base-branch",
                )
        self.ref_resolver.assert_not_awaited()

    async def preparation_row(self, sid):
        return await self.pool.fetchrow(
            "SELECT * FROM git_enrollments WHERE binding_id=$1", sid
        )

    async def ready_preparation(self, sid, runtime):
        async with self.pool.acquire() as conn:
            return await credentials.ready_for_binding(
                conn, sid, self.native.sessions[runtime], push=False
            )

    async def test_crash_after_reservation_recovers_same_request(self):
        class CrashBeforeBytes(BaseException):
            pass

        _, sid, runtime = await self.preparation_target()
        reserved = []

        async def crash(session_id, **kwargs):
            reserved.append(PreparationRequest(session_id=session_id, **kwargs))
            raise CrashBeforeBytes()

        with patch.object(self.native, "prepare_session_workspace", crash):
            with self.assertRaises(CrashBeforeBytes):
                await self.ready_preparation(sid, runtime)
        row = await self.preparation_row(sid)
        self.assertEqual(row["prepare_state"], "requested")
        self.assertEqual(
            json.loads(row["prepare_receipt"])["original"], asdict(reserved[0])
        )
        self.assertFalse(self.native.prepares)
        self.assertIsNone(self.native.sessions[runtime].workspace_preparation)
        await self.ready_preparation(sid, runtime)
        self.assertEqual(self.native.prepares, [reserved[0].encode()])
        self.assertEqual(
            (await self.preparation_row(sid))["prepare_state"], "confirmed"
        )

    async def test_concurrent_readiness_serializes_prepare(self):
        _, sid, runtime = await self.preparation_target()
        self.native.prepare_classification = "pending"
        entered, release = asyncio.Event(), asyncio.Event()
        original = self.native.prepare_session_workspace

        async def paused(session_id, **kwargs):
            entered.set()
            await release.wait()
            return await original(session_id, **kwargs)

        jobs = []
        blocked = False
        with patch.object(self.native, "prepare_session_workspace", paused):
            try:
                jobs.append(asyncio.create_task(self.ready_preparation(sid, runtime)))
                await asyncio.wait_for(entered.wait(), 10)
                jobs.append(asyncio.create_task(self.ready_preparation(sid, runtime)))
                async with asyncio.timeout(10):
                    while not blocked:
                        blocked = await self.pool.fetchval(
                            "SELECT EXISTS(SELECT 1 FROM pg_stat_activity "
                            "WHERE datname=current_database() AND wait_event_type='Lock' "
                            "AND cardinality(pg_blocking_pids(pid))>0)"
                        )
            finally:
                release.set()
                results = await asyncio.gather(*jobs, return_exceptions=True)
        self.assertTrue(blocked)
        self.assertEqual(len(self.native.prepares), 1)
        self.assertEqual(
            [str(result) for result in results],
            ["git_prepare_pending", "git_prepare_pending"],
        )

    async def test_observation_ignores_stale_and_revoked_enrollment(self):
        from models import WorkspaceObservedState

        self.native.prepare_not_received = True
        _, sid, runtime = await self.preparation_target()
        with self.assertRaisesRegex(ValueError, "git_prepare_pending"):
            await self.ready_preparation(sid, runtime)
        self.assertEqual(
            await workspaces._observe(runtime, workspace_id=sid),
            (WorkspaceObservedState.RESUMING, "Preparing workspace"),
        )
        binding = await ns.get_binding(sid)
        await self.pool.execute(
            "UPDATE native_bindings SET kagent_request_id=$2 WHERE session_id=$1",
            sid,
            "00000000-0000-4000-8000-000000000099",
        )
        self.assertEqual(
            await workspaces._observe(runtime, workspace_id=sid),
            (WorkspaceObservedState.RUNNING, None),
        )
        await self.pool.execute(
            "UPDATE native_bindings SET kagent_request_id=$2,kagent_session_id=$3 WHERE session_id=$1",
            sid,
            binding["kagent_request_id"],
            "00000000-0000-4000-8000-000000000098",
        )
        self.assertEqual(
            await workspaces._observe(runtime, workspace_id=sid),
            (WorkspaceObservedState.RUNNING, None),
        )
        await self.pool.execute(
            "UPDATE native_bindings SET kagent_session_id=$2 WHERE session_id=$1",
            sid,
            runtime,
        )
        await self.pool.execute(
            "UPDATE git_enrollments SET revoked_at=now() WHERE binding_id=$1", sid
        )
        self.assertEqual(
            await workspaces._observe(runtime, workspace_id=sid),
            (WorkspaceObservedState.RUNNING, None),
        )

    async def test_normal_suspension_keeps_suspended_projection(self):
        from models import WorkspaceObservedState

        _, _, attempt = await self.task()
        sid = attempt.session_id
        runtime = (await ns.get_binding(sid))["kagent_session_id"]
        ready = self.native.sessions[runtime]
        await self.native.suspend_session(runtime)
        projected = await self.native.get_session(runtime)
        self.assertTrue(projected.workspace_preparation.historical)
        self.assertFalse(self.native.sessions[runtime].workspace_preparation.historical)
        suspended = await workspaces._observe(runtime, workspace_id=sid)
        await self.native.resume_session(runtime)
        self.assertFalse(
            (await self.native.get_session(runtime)).workspace_preparation.historical
        )
        self.assertEqual(
            self.native.sessions[runtime].runtime_association, ready.runtime_association
        )
        self.assertEqual(
            await workspaces._observe(runtime, workspace_id=sid),
            (WorkspaceObservedState.RUNNING, None),
        )
        self.assertEqual(
            (await self.preparation_row(sid))["prepare_state"], "confirmed"
        )
        self.assertEqual(suspended, (WorkspaceObservedState.SUSPENDED, None))

    async def test_transient_historical_read_does_not_permanently_fail(self):
        _, _, attempt = await self.task()
        sid = attempt.session_id
        runtime = (await ns.get_binding(sid))["kagent_session_id"]
        ready = self.native.sessions[runtime]
        await self.native.suspend_session(runtime)
        suspended = await self.native.get_session(runtime)
        await self.native.resume_session(runtime)
        # ensure_ready returns READY, then the final Get sees concurrent suspension.
        with patch.object(self.native, "get_session", return_value=suspended):
            try:
                await self.ready_preparation(sid, runtime)
            except ValueError as exc:
                self.assertEqual(str(exc), "git_prepare_pending")
            else:
                self.fail("A suspended observation must hold readiness")
        self.assertEqual(
            (await self.preparation_row(sid))["prepare_state"], "confirmed"
        )
        self.assertEqual(
            self.native.sessions[runtime].runtime_association, ready.runtime_association
        )
        await self.ready_preparation(sid, runtime)
        self.assertEqual(
            (await self.preparation_row(sid))["prepare_state"], "confirmed"
        )
        self.assertEqual(len(self.native.prepares), 1)

    async def test_lifecycle_projection_holds_every_preparation_read(self):
        from models import WorkspaceObservedState

        _, _, attempt = await self.task()
        sid = attempt.session_id
        runtime = (await ns.get_binding(sid))["kagent_session_id"]
        ready = self.native.sessions[runtime]
        row = await self.preparation_row(sid)
        cases = (
            (
                RuntimeState.SUSPENDED,
                RuntimeOperation.NONE,
                (WorkspaceObservedState.SUSPENDED, None),
            ),
            (
                RuntimeState.READY,
                RuntimeOperation.SUSPEND,
                (WorkspaceObservedState.SUSPENDING, "Suspending."),
            ),
            (
                RuntimeState.READY,
                RuntimeOperation.RESUME,
                (WorkspaceObservedState.RESUMING, "Starting."),
            ),
            (
                RuntimeState.SUSPENDED,
                RuntimeOperation.RESUME,
                (WorkspaceObservedState.RESUMING, "Starting."),
            ),
            (
                RuntimeState.CREATING,
                RuntimeOperation.CREATE,
                (WorkspaceObservedState.RESUMING, "Starting."),
            ),
            (
                RuntimeState.READY,
                RuntimeOperation.CREATE,
                (WorkspaceObservedState.RESUMING, "Starting."),
            ),
        )
        for state, operation, expected_view in cases:
            for boundary in ("readiness", "publication", "prepare"):
                with self.subTest(state=state, operation=operation, boundary=boundary):
                    self.native.sessions[runtime] = replace(
                        ready, state=state, operation=operation
                    )
                    projected = await self.native.get_session(runtime)
                    self.assertTrue(projected.workspace_preparation.historical)
                    self.assertEqual(
                        await workspaces._observe(runtime, workspace_id=sid),
                        expected_view,
                    )
                    # Readiness can settle before a later Get sees a new operation.
                    with patch.object(
                        self.native, "ensure_ready", AsyncMock(return_value=ready)
                    ):
                        async with self.pool.acquire() as conn:
                            with self.assertRaisesRegex(
                                ValueError, "git_prepare_pending"
                            ):
                                if boundary == "readiness":
                                    await credentials.ready_for_binding(
                                        conn, sid, ready, push=False
                                    )
                                elif boundary == "publication":
                                    await credentials.publish_read(
                                        conn, row["issuance_id"]
                                    )
                                else:
                                    async with credentials.locked(conn, sid):
                                        await credentials.prepare_for_binding(
                                            conn, row["issuance_id"], self.native
                                        )
                    held = await self.preparation_row(sid)
                    self.assertEqual(held["prepare_state"], "confirmed")
                    self.assertEqual(held["prepare_receipt"], row["prepare_receipt"])
                    self.native.sessions[runtime] = ready
                    await self.ready_preparation(sid, runtime)
        self.assertEqual(len(self.native.prepares), 1)

    async def test_changed_runtime_is_historical_only_after_settled_ready(self):
        from models import WorkspaceObservedState

        for field in ("generation_id", "actor_uid"):
            with self.subTest(field=field):
                _, _, attempt = await self.task()
                sid = attempt.session_id
                runtime = (await ns.get_binding(sid))["kagent_session_id"]
                ready = self.native.sessions[runtime]
                changed = replace(
                    ready,
                    state=RuntimeState.SUSPENDED,
                    runtime_association=replace(
                        ready.runtime_association, **{field: "new-" + field}
                    ),
                )
                self.native.sessions[runtime] = changed
                self.assertFalse(changed.workspace_preparation.historical)
                self.assertTrue(
                    (
                        await self.native.get_session(runtime)
                    ).workspace_preparation.historical
                )
                with patch.object(
                    self.native, "ensure_ready", AsyncMock(return_value=ready)
                ):
                    with self.assertRaisesRegex(ValueError, "git_prepare_pending"):
                        await self.ready_preparation(sid, runtime)
                self.assertEqual(
                    (await self.preparation_row(sid))["prepare_state"], "confirmed"
                )
                self.assertEqual(
                    await workspaces._observe(runtime, workspace_id=sid),
                    (WorkspaceObservedState.SUSPENDED, None),
                )
                await self.native.resume_session(runtime)
                self.assertTrue(
                    (
                        await self.native.get_session(runtime)
                    ).workspace_preparation.historical
                )
                count = len(self.native.prepares)
                with self.assertRaisesRegex(ValueError, "git_prepare_failed"):
                    await self.ready_preparation(sid, runtime)
                self.assertEqual(
                    (await self.preparation_row(sid))["prepare_state"], "failed"
                )
                self.assertEqual(
                    await workspaces._observe(runtime, workspace_id=sid),
                    (
                        WorkspaceObservedState.FAILED,
                        "Workspace preparation failed; replace the session",
                    ),
                )
                with self.assertRaisesRegex(ValueError, "git_prepare_failed"):
                    await self.ready_preparation(sid, runtime)
                self.assertEqual(len(self.native.prepares), count)

    async def test_terminal_prepare_reply_during_suspension_waits_for_ready(self):
        prepare = self.native.prepare_session_workspace
        for classification in ("confirmed", "definite-failure"):
            with self.subTest(classification=classification):
                _, sid, runtime = await self.preparation_target()
                self.native.prepare_classification = classification

                async def suspend_before_reply(
                    session_id, classification=classification, **kwargs
                ):
                    receipt = await prepare(session_id, **kwargs)
                    await self.native.suspend_session(session_id)
                    # A historical reply needs a fresh lifecycle observation; a
                    # definite failure needs the same check even without this flag.
                    return replace(receipt, historical=classification == "confirmed")

                with patch.object(
                    self.native, "prepare_session_workspace", suspend_before_reply
                ):
                    with self.assertRaisesRegex(ValueError, "git_prepare_pending"):
                        await self.ready_preparation(sid, runtime)
                self.assertEqual(
                    (await self.preparation_row(sid))["prepare_state"], "requested"
                )
                count = len(self.native.prepares)
                await self.native.resume_session(runtime)
                if classification == "confirmed":
                    await self.ready_preparation(sid, runtime)
                    self.assertEqual(
                        (await self.preparation_row(sid))["prepare_state"], "confirmed"
                    )
                else:
                    with self.assertRaisesRegex(ValueError, "git_prepare_failed"):
                        await self.ready_preparation(sid, runtime)
                    self.assertEqual(
                        (await self.preparation_row(sid))["prepare_state"], "failed"
                    )
                self.assertEqual(len(self.native.prepares), count)

    async def test_prepare_refusal_during_operation_retries_same_action_after_resume(
        self,
    ):
        _, sid, runtime = await self.preparation_target()
        prepare = self.native.prepare_session_workspace
        self.native.prepare_error = SessionError("lifecycle changed", grpc_status=9)

        async def reject_during_resume(session_id, **kwargs):
            try:
                return await prepare(session_id, **kwargs)
            finally:
                self.native.sessions[session_id] = replace(
                    self.native.sessions[session_id], operation=RuntimeOperation.RESUME
                )

        with patch.object(
            self.native, "prepare_session_workspace", reject_during_resume
        ):
            with self.assertRaisesRegex(ValueError, "git_prepare_pending"):
                await self.ready_preparation(sid, runtime)
        first = await self.preparation_row(sid)
        self.assertEqual(first["prepare_state"], "requested")
        self.native.prepare_error = None
        self.native.sessions[runtime] = replace(
            self.native.sessions[runtime], operation=RuntimeOperation.NONE
        )
        await self.ready_preparation(sid, runtime)
        self.assertEqual(self.native.prepares[0], self.native.prepares[1])
        row = await self.preparation_row(sid)
        self.assertEqual(row["prepare_action_id"], first["prepare_action_id"])
        self.assertEqual(row["prepare_state"], "confirmed")

    async def test_preparation_pending_uncertain_confirmed_and_no_challenge(self):
        from models import WorkspaceObservedState

        operation, sid, runtime = await self.preparation_target()
        self.native.prepare_classification = "pending"
        with self.assertRaisesRegex(ValueError, "git_prepare_pending"):
            await self.ready_preparation(sid, runtime)
        first = await self.preparation_row(sid)
        self.assertEqual(first["prepare_state"], "requested")
        self.assertEqual(
            await workspaces._observe(runtime, workspace_id=sid),
            (WorkspaceObservedState.RESUMING, "Preparing workspace"),
        )
        for classification in ("uncertain", "confirmed"):
            current = self.native.sessions[runtime]
            receipt = replace(
                current.workspace_preparation, classification=classification
            )
            self.native.sessions[runtime] = replace(
                current, workspace_preparation=receipt
            )
            if classification == "uncertain":
                with self.assertRaisesRegex(ValueError, "git_prepare_pending"):
                    await self.ready_preparation(sid, runtime)
                await self.worker.reconcile(db, operation)
                self.assertEqual(
                    await self.pool.fetchval(
                        "SELECT state FROM task_attempts WHERE session_id=$1", sid
                    ),
                    "creating",
                )
                self.assertEqual(self.spawn.call_count, 0)
            else:
                await self.ready_preparation(sid, runtime)
        await self.ready_preparation(sid, runtime)
        row = await self.preparation_row(sid)
        self.assertEqual(row["prepare_state"], "confirmed")
        self.assertEqual(row["prepare_action_id"], first["prepare_action_id"])
        self.assertEqual(len(self.native.prepares), 1)
        self.assertEqual(
            json.loads(row["prepare_receipt"])["classification"], "confirmed"
        )
        await self.worker.reconcile(db, operation)
        self.assertEqual(self.spawn.call_count, 1)

    async def test_lost_prepare_without_receipt_retries_identical_bytes_after_pool_restart(
        self,
    ):
        from models import WorkspaceObservedState

        _, sid, runtime = await self.preparation_target()
        self.native.prepare_not_received = True
        with self.assertRaisesRegex(ValueError, "git_prepare_pending"):
            await self.ready_preparation(sid, runtime)
        self.assertIsNone(self.native.sessions[runtime].workspace_preparation)
        self.assertEqual(
            await workspaces._observe(runtime, workspace_id=sid),
            (WorkspaceObservedState.RESUMING, "Preparing workspace"),
        )
        first = await self.preparation_row(sid)
        new = await asyncpg.create_pool(self.url, min_size=1, max_size=2)
        old = db._pool
        db._pool = new
        try:
            async with new.acquire() as conn:
                await credentials.ready_for_binding(
                    conn, sid, self.native.sessions[runtime], push=False
                )
        finally:
            db._pool = old
            await new.close()
        self.assertEqual(self.native.prepares[0], self.native.prepares[1])
        row = await self.preparation_row(sid)
        self.assertEqual(row["prepare_action_id"], first["prepare_action_id"])
        self.assertEqual(row["prepare_state"], "confirmed")
        self.assertEqual(len(self.native.suspends), 1)

    async def test_lost_prepare_reply_reconciles_receipt_without_resend(self):
        _, sid, runtime = await self.preparation_target()
        self.native.lose_prepare = True
        with self.assertRaisesRegex(ValueError, "git_prepare_pending"):
            await self.ready_preparation(sid, runtime)
        await self.ready_preparation(sid, runtime)
        self.assertEqual(len(self.native.prepares), 1)
        self.assertEqual(
            (await self.preparation_row(sid))["prepare_state"], "confirmed"
        )

    async def test_prepare_already_exists_and_definite_rpc_failure_hold_original_action(
        self,
    ):
        for status in (6, 3, 9):
            with self.subTest(status=status):
                _, sid, runtime = await self.preparation_target()
                self.native.prepare_error = SessionError(
                    "fixture rejection", grpc_status=status
                )
                code = "git_prepare_conflict" if status == 6 else "git_prepare_failed"
                with self.assertRaisesRegex(ValueError, code):
                    await self.ready_preparation(sid, runtime)
                row = await self.preparation_row(sid)
                self.assertEqual(row["prepare_state"], "failed")
                count = len(self.native.prepares)
                self.native.prepare_error = None
                with self.assertRaisesRegex(ValueError, "git_prepare_failed"):
                    await self.ready_preparation(sid, runtime)
                self.assertEqual(len(self.native.prepares), count)
                self.assertEqual(
                    (await self.preparation_row(sid))["prepare_action_id"],
                    row["prepare_action_id"],
                )

    async def test_definite_failure_and_historical_receipts_hold(self):
        from models import WorkspaceObservedState

        for classification, historical in (
            ("definite-failure", False),
            ("confirmed", True),
        ):
            _, sid, runtime = await self.preparation_target()
            self.native.prepare_classification = "pending"
            with self.assertRaisesRegex(ValueError, "git_prepare_pending"):
                await self.ready_preparation(sid, runtime)
            current = self.native.sessions[runtime]
            receipt = replace(
                current.workspace_preparation,
                classification=classification,
                historical=historical,
            )
            self.native.sessions[runtime] = replace(
                current, workspace_preparation=receipt
            )
            with self.assertRaisesRegex(ValueError, "git_prepare_failed"):
                await self.ready_preparation(sid, runtime)
            row = await self.preparation_row(sid)
            self.assertEqual(row["prepare_state"], "failed")
            self.assertEqual(
                json.loads(row["prepare_receipt"])["historical"], historical
            )
            self.assertEqual(
                await workspaces._observe(runtime, workspace_id=sid),
                (
                    WorkspaceObservedState.FAILED,
                    "Workspace preparation failed; replace the session",
                ),
            )
            with self.assertRaisesRegex(ValueError, "git_prepare_failed"):
                await self.ready_preparation(sid, runtime)

    async def test_profile_comes_from_binding_and_receipt_cannot_select_owner(self):
        _, _, parent = await self.task()
        _, _, child = await self.task(parent=parent)
        for attempt, expected in ((parent, "supervisor"), (child, "child")):
            runtime = (await ns.get_binding(attempt.session_id))["kagent_session_id"]
            current = self.native.sessions[runtime]
            self.assertEqual(
                current.workspace_preparation.original.setup_profile, expected
            )
            request = next(
                decoded
                for raw in self.native.prepares
                if (decoded := PreparationRequest.decode(raw)).session_id == runtime
            )
            self.assertEqual(request.setup_profile, expected)
            enrollment = await self.enrolled(attempt.session_id)
            self.assertEqual(
                asdict(request.workspace), enrollment.plan.workspace.model_dump()
            )
            self.assertEqual(
                asdict(request.development_environment),
                enrollment.plan.development_environment.model_dump(
                    include={"image", "platform", "policy_identity"}
                ),
            )
            self.assertEqual(
                asdict(request.runtime_composition),
                enrollment.association.runtime_composition.model_dump(),
            )
            self.assertEqual(
                (request.generation_id, request.actor_uid, request.prepared_revision),
                (
                    enrollment.association.runtime.generation_id,
                    enrollment.association.runtime.actor_uid,
                    enrollment.association.runtime.revision,
                ),
            )
            malicious = replace(
                current.workspace_preparation,
                original=replace(
                    current.workspace_preparation.original,
                    setup_profile="agent",
                    setup_digest="owner-claim",
                ),
            )
            self.native.sessions[runtime] = replace(
                current, workspace_preparation=malicious
            )
            count = len(self.native.prepares)
            with self.assertRaisesRegex(ValueError, "git_prepare_conflict"):
                await self.ready_preparation(attempt.session_id, runtime)
            self.assertEqual(len(self.native.prepares), count)

    async def test_every_send_requires_durable_confirmation_even_if_readiness_returns(
        self,
    ):
        _, _, attempt = await self.task()
        binding = await ns.get_binding(attempt.session_id)
        current = self.native.sessions[binding["kagent_session_id"]]
        emitted = []

        async def send(*args, **kwargs):
            emitted.append("turn bytes")
            yield "event"

        await self.pool.execute(
            "UPDATE git_enrollments SET prepare_state='failed' WHERE binding_id=$1",
            attempt.session_id,
        )
        with patch.object(
            credentials, "ready_for_binding", AsyncMock(return_value=current)
        ), patch.object(self.native, "send_message", send, create=True):
            with self.assertRaisesRegex(ValueError, "git_prepare_pending"):
                await anext(ns._guarded_send(binding, current.agent))
            await ns.ledger.bump_turns(attempt.session_id)
            binding = await ns.get_binding(attempt.session_id)
            self.assertEqual(binding["turns"], 1)
            with self.assertRaisesRegex(ValueError, "git_prepare_pending"):
                await anext(ns._guarded_send(binding, current.agent))
        self.assertFalse(emitted)

    async def test_pre_send_waits_for_preparation_then_sends_once(self):
        _, _, attempt = await self.task()
        binding = await ns.get_binding(attempt.session_id)
        current = self.native.sessions[binding["kagent_session_id"]]
        confirmed = current.workspace_preparation
        self.native.sessions[current.id] = replace(
            current,
            workspace_preparation=replace(confirmed, classification="uncertain"),
        )
        emitted = []

        async def send(*args, **kwargs):
            emitted.append("turn bytes")
            yield "event"

        with patch.object(self.native, "send_message", send, create=True):
            with self.assertRaisesRegex(ValueError, "git_prepare_pending"):
                await anext(ns._guarded_send(binding, current.agent))
            self.assertFalse(emitted)
            self.native.sessions[current.id] = current
            self.assertEqual(
                await anext(ns._guarded_send(binding, current.agent)), "event"
            )
            self.assertEqual(emitted, ["turn bytes"])

    async def test_prepare_marker_and_original_are_immutable_and_migration_reentrant(
        self,
    ):
        _, _, attempt = await self.task()
        await self.pool.execute(MIGRATION_SQL)
        for query in (
            "UPDATE git_enrollments SET prepare_action_id='other-action' WHERE binding_id=$1",
            "UPDATE git_enrollments SET prepare_state='requested' WHERE binding_id=$1",
            "UPDATE git_enrollments SET prepare_receipt=jsonb_set(prepare_receipt,'{original,setup_profile}','\"agent\"') WHERE binding_id=$1",
        ):
            with self.assertRaises(asyncpg.RaiseError):
                await self.pool.execute(
                    query,
                    attempt.session_id,
                )
        self.assertEqual(
            (await self.preparation_row(attempt.session_id))["prepare_state"],
            "confirmed",
        )

    async def test_preparation_flags_off_preserve_native_task_behavior(self):
        for git_enabled, push_enabled in ((False, False), (True, False), (False, True)):
            count = len(self.native.prepares)
            with patch.object(
                settings, "git_transport_enabled", git_enabled
            ), patch.object(settings, "push_gate_enabled", push_enabled):
                _, _, attempt = await self.task()
                self.assertEqual(attempt.state, "active")
                binding = await ns.get_binding(attempt.session_id)
                current = self.native.sessions[binding["kagent_session_id"]]

                async def send(*args, **kwargs):
                    yield "legacy event"

                with patch.object(self.native, "send_message", send, create=True):
                    self.assertEqual(
                        await anext(ns._guarded_send(binding, current.agent)),
                        "legacy event",
                    )
                self.assertEqual(len(self.native.prepares), count)

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

    # ---- writer plans must satisfy kagent's preparation check -----------------------

    async def unknown_default_branch(self):
        await self.pool.execute(
            "UPDATE projects SET default_branch='' WHERE id=$1", self.pid
        )
        await self.pool.execute(
            "DELETE FROM push_branch_policies WHERE project_id=$1", self.pid
        )

    def github(self, server):
        return (
            app_settings(),
            patch.object(
                github_checkout,
                "GitHubCreationClient",
                partial(
                    GitHubCreationClient, transport=httpx.MockTransport(server.handle)
                ),
            ),
        )

    async def attempt_evidence(self, attempt_id):
        from mainloop.tasks import lifecycle

        async with self.pool.acquire() as conn:
            return (await lifecycle.load_attempt(conn, attempt_id)).evidence_refs

    def assert_kagent_accepts(self, references, accepted=True):
        refs = [r if isinstance(r, dict) else r.model_dump() for r in references]
        self.assertEqual(kagent_preparation_accepts(refs), accepted)
        counts = credentials.git_reference_counts(refs)
        self.assertEqual(counts == {"read": 1, "push": 1}, accepted)

    async def test_writer_learns_unknown_default_branch_and_freezes_read_and_push(self):
        await self.unknown_default_branch()
        server = CheckoutServer()
        patchers = self.github(server)
        with patchers[0], patchers[1]:
            # The fixture checkout is an explicit SHA, so no ref resolution reads GitHub.
            _, task, attempt = await self.task(branch="mainloop/x")
        self.assertEqual(task.checkout.ref, "a" * 40)
        self.assertEqual(attempt.state, "active")
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT default_branch FROM projects WHERE id=$1", self.pid
            ),
            "trunk",
        )
        async with self.pool.acquire() as conn:
            policy = await store.load_policy(conn, self.pid)
        self.assertEqual((policy.default_branch, policy.version), ("trunk", 1))
        enrollment = await self.enrolled(attempt.session_id)
        git = [
            r
            for r in enrollment.plan.references
            if r.origin in (settings.git_read_origin, settings.git_push_origin)
        ]
        self.assertEqual(
            sorted(r.origin for r in git),
            sorted((settings.git_read_origin, settings.git_push_origin)),
        )
        for ref in git:
            self.assertEqual(
                (ref.header, ref.secret_key), ("authorization", "authorization")
            )
        self.assertIsNotNone(enrollment.plan.push_version)
        self.assertEqual(enrollment.push_state, "published")
        self.assert_kagent_accepts(enrollment.plan.references)
        # The created Session carried exactly the frozen references.
        self.assertEqual(
            tuple(asdict(r) for r in self.native.creates[-1][1][1]),
            tuple(r.model_dump() for r in enrollment.plan.references),
        )
        self.assertFalse(
            [e for e in attempt.evidence_refs if e.startswith("git-push-absent:")]
        )

    async def test_default_branch_lookup_failure_holds_admission_without_enrollment(
        self,
    ):
        await self.unknown_default_branch()
        server = CheckoutServer()
        server.repo_status = 503
        patchers = self.github(server)
        with patchers[0], patchers[1]:
            operation, _, attempt = await self.task(branch="mainloop/x")
        self.assertEqual(attempt.state, "creating")
        self.assertIn("git-hold:default_branch_unavailable", attempt.evidence_refs)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM git_enrollments WHERE binding_id=$1",
                attempt.session_id,
            ),
            0,
        )
        self.assertFalse(self.native.creates)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT default_branch FROM projects WHERE id=$1", self.pid
            ),
            "",
        )
        self.assertIsNone(
            await self.pool.fetchrow(
                "SELECT 1 FROM push_branch_policies WHERE project_id=$1", self.pid
            )
        )
        # Once GitHub answers, the held admission resumes with both references.
        server.repo_status = 200
        with patchers[0], patchers[1]:
            await self.worker.reconcile(db, operation)
        enrollment = await self.enrolled(attempt.session_id)
        self.assert_kagent_accepts(enrollment.plan.references)
        self.assertEqual(len(self.native.creates), 1)

    async def test_writer_on_default_branch_stays_read_only_with_reason(self):
        await self.unknown_default_branch()
        server = CheckoutServer()
        patchers = self.github(server)
        with patchers[0], patchers[1]:
            _, _, attempt = await self.task(branch="trunk", ready=False)
            binding = await ns.get_binding(attempt.session_id)
            await ns._create_bound_session(binding)
        enrollment = await self.enrolled(attempt.session_id)
        self.assertIsNone(enrollment.plan.push_version)
        self.assertEqual(enrollment.push_state, "absent")
        self.assertEqual(
            credentials.git_reference_counts(enrollment.plan.references),
            {"read": 1, "push": 0},
        )
        self.assert_kagent_accepts(enrollment.plan.references, accepted=False)
        self.assertIn(
            "git-push-absent:default_branch",
            await self.attempt_evidence(attempt.id),
        )

    async def test_kagent_predicate_accepts_new_plans_and_rejects_old_spelling(self):
        _, _, attempt = await self.task(branch="mainloop/x", ready=False)
        binding = await ns.get_binding(attempt.session_id)
        await ns._create_bound_session(binding)
        new = [
            r.model_dump()
            for r in (await self.enrolled(attempt.session_id)).plan.references
        ]
        self.assert_kagent_accepts(new)
        # The first gated run froze capitalized headers: read=0, push=0 for kagent.
        old = [
            (
                {**r, "header": "Authorization"}
                if r["origin"] in (settings.git_read_origin, settings.git_push_origin)
                else r
            )
            for r in new
        ]
        self.assert_kagent_accepts(old, accepted=False)
        self.assert_kagent_accepts(
            [r for r in new if r["origin"] != settings.git_push_origin], accepted=False
        )

    async def test_previously_frozen_capitalized_plan_keeps_its_references(self):
        with patch.object(credentials, "GIT_HEADER", "Authorization"):
            _, sid, runtime = await self.preparation_target()
        before = await self.preparation_row(sid)
        plan = (await self.enrolled(sid)).plan
        self.assertEqual({r.header for r in plan.references[1:]}, {"Authorization"})
        for purpose in ("git-read", "git-push"):
            self.assertEqual(
                credentials.reference(plan, purpose).header, "Authorization"
            )
        await self.ready_preparation(sid, runtime)
        after = await self.preparation_row(sid)
        self.assertEqual(
            (after["plan"], after["plan_digest"]),
            (before["plan"], before["plan_digest"]),
        )
        self.assertEqual(after["read_state"], "published")

    async def test_prepare_failure_keeps_a_bounded_reason(self):
        _, sid, runtime = await self.preparation_target()
        attempt_id = (await self.enrolled(sid)).plan.attempt_id
        self.native.prepare_error = SessionError(
            "Current prepared runtime identity is unavailable: secret-ish detail",
            grpc_status=9,
        )
        with self.assertRaisesRegex(ValueError, "git_prepare_failed"):
            await self.ready_preparation(sid, runtime)
        row = await self.preparation_row(sid)
        receipt = json.loads(row["prepare_receipt"])
        self.assertEqual(
            receipt["failure"],
            {
                "code": "git_prepare_failed",
                "grpc_status": 9,
                "git_refs": {"read": 1, "push": 1},
            },
        )
        self.assertIn("original", receipt)
        evidence = await self.attempt_evidence(attempt_id)
        self.assertIn(
            "git-prepare-failed:git_prepare_failed:grpc=9:git-refs read=1 push=1",
            evidence,
        )
        self.assertNotIn("secret-ish", row["prepare_receipt"])
        self.assertFalse([e for e in evidence if "secret-ish" in e])


def kagent_preparation_accepts(references):
    """Mirror of kagent 9c0c373 native_handoff.go:157-169 (CheckPreparationRuntime).

    Only exact lowercase ``authorization`` headers count, at the exact read/push origins,
    with Secret key ``authorization``; preparation needs exactly one of each.
    """
    read = push = 0
    for ref in references:
        if ref["header"] != "authorization" or ref["secret_key"] != "authorization":
            continue
        if ref["origin"] == settings.git_read_origin:
            read += 1
        elif ref["origin"] == settings.git_push_origin:
            push += 1
    return read == 1 and push == 1
