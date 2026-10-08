"""PR creation contracts with sanitized HTTP and PostgreSQL fakes, never live GitHub."""

import asyncio
import copy
import json
import unittest
import uuid
from datetime import UTC, datetime
from unittest.mock import AsyncMock, patch

import httpx
from fastapi import HTTPException
from mainloop.config import settings
from mainloop.db import db
from mainloop.db import tasks as task_store
from mainloop.db.postgres import PRCreationConflict
from mainloop.mcp_app import TOOLS, invoke
from mainloop.providers import registry
from mainloop.runtime import native_sessions, workspaces
from mainloop.runtime.agent_identity import token_for
from mainloop.runtime.agent_tools import AgentService, Ctx
from mainloop.runtime.delegation import PgStore
from mainloop.runtime.policy import Actor, may_call, tools_for
from mainloop.services import github_creation as creation
from mainloop.services.github_repo import parse_github_repo
from mainloop.tasks import lifecycle
from pydantic import ValidationError
from tests.runtime.test_postgres_ledger import PostgresTestCase, _init_schema

from models.agent_tools import OpenPullRequest
from models.task import Task, TaskCheckout
from models.workspace import WorkspaceManifest

SHA = "a" * 40
BASE_SHA = "b" * 40
ARGS = {
    "project_id": "project-1",
    "branch": "feature/fix",
    "expected_sha": SHA,
    "title": "Fix it",
    "body": "A sanitized fixture",
    "request_id": "request-1",
}
REPO = {"id": 123, "full_name": "owner/repo", "default_branch": "trunk"}


class FakeGitHub:
    def __init__(self):
        self.repo = copy.deepcopy(REPO)
        self.branch_sha = SHA
        self.prs = []
        self.requests = []
        self.lose_response = False
        self.refusal = None
        self.pr_changes = None
        self.on_request = None

    def pr(self):
        pr = {
            "number": 17,
            "state": "open",
            "head": {"ref": ARGS["branch"], "sha": SHA, "repo": self.repo},
            "base": {
                "ref": self.repo["default_branch"],
                "sha": BASE_SHA,
                "repo": self.repo,
            },
        }
        pr = copy.deepcopy(pr)
        if self.pr_changes:
            self.pr_changes(pr)
        return pr

    async def handle(self, request):
        self.requests.append(request)
        if request.url.host != "api.github.com":
            raise AssertionError("credential escaped fixed GitHub host")
        if self.on_request:
            await self.on_request(request)
        if self.refusal:
            return httpx.Response(
                self.refusal,
                text='{"token":"secret-fixture", "Authorization":"Basic secret-fixture"}',
                headers={"Location": "https://evil.invalid/secret-fixture"},
            )
        path = request.url.path
        if path == "/repos/owner/repo":
            return httpx.Response(200, json=self.repo)
        if path.startswith("/repos/owner/repo/branches/"):
            name = path.split("/branches/")[1]
            return httpx.Response(
                200, json={"name": name, "commit": {"sha": self.branch_sha}}
            )
        if path == "/repos/owner/repo/pulls":
            if request.method == "GET":
                return httpx.Response(200, json=self.prs)
            body = json.loads(request.content)
            if set(body) != {"head", "base", "title", "body"}:
                raise AssertionError("unexpected creation authority")
            pr = self.pr()
            self.prs.append(pr)
            if self.lose_response:
                raise httpx.ReadTimeout(
                    "Bearer secret-fixture response lost", request=request
                )
            return httpx.Response(201, json=pr)
        raise AssertionError(f"unexpected request {request.method} {path}")

    @property
    def posts(self):
        return [r for r in self.requests if r.method == "POST"]


class InputTests(unittest.TestCase):
    def test_server_project_and_workspace_authority(self):
        project = {
            "owner": "owner",
            "name": "repo",
            "full_name": "owner/repo",
            "html_url": "https://github.com/owner/repo",
            "role": "main",
            "mcp_grant_kind": "coordination",
        }
        body = OpenPullRequest.model_validate(ARGS)
        self.assertEqual(creation._project_repo(project, body), "owner/repo")
        child = {
            **project,
            "role": "child",
            "mcp_grant_kind": "coordination",
            "session_project_id": "project-1",
            "session_repo": "https://github.com/owner/repo",
            "workspace_repo": "https://github.com/owner/repo",
            "workspace_branch": "feature/fix",
        }
        self.assertEqual(creation._project_repo(child, body), "owner/repo")
        for key, value in (
            ("session_project_id", "another"),
            ("session_repo", "https://github.com/fork/repo"),
            ("workspace_repo", None),
            ("workspace_branch", "other"),
            ("role", "unknown"),
            ("html_url", "https://evil.invalid/owner/repo"),
            ("owner", "other"),
        ):
            with self.subTest(key=key), self.assertRaises(creation.PolicyError):
                creation._project_repo({**child, key: value}, body)

        workspace = {
            **project,
            "role": "agent",
            "mcp_grant_kind": "workspace",
            "session_project_id": "project-1",
            "session_repo": "https://github.com/owner/repo",
            "session_branch": "feature/fix",
            "workspace_repo": "https://github.com/owner/repo",
            "workspace_branch": "feature/fix",
            "kagent_session_id": "runtime-1",
        }
        self.assertEqual(creation._project_repo(workspace, body), "owner/repo")
        for key, value in (
            ("session_project_id", "another"),
            ("session_repo", "https://github.com/fork/repo"),
            ("workspace_repo", None),
            ("workspace_branch", "feature/other"),
            ("session_branch", "feature/other"),
            ("workspace_branch", "invalid..branch"),
            ("kagent_session_id", None),
        ):
            with self.subTest(workspace_key=key, workspace_value=value):
                with self.assertRaises(creation.PolicyError):
                    creation._project_repo({**workspace, key: value}, body)

    def test_branch_refusal_reasons_preserve_private_scope(self):
        from mainloop.services.workspace_authority import (
            ScopeUnavailable,
            repository_scope,
        )

        authority = {
            "owner": "owner",
            "name": "repo",
            "full_name": "owner/repo",
            "html_url": "https://github.com/owner/repo",
            "default_branch": "trunk",
            "role": "agent",
            "mcp_grant_kind": "workspace",
            "session_project_id": "project-1",
            "session_repo": "https://github.com/owner/repo",
            "workspace_repo": "https://github.com/owner/repo",
            "session_branch": "feature/fix",
            "workspace_branch": "feature/fix",
            "kagent_session_id": "runtime-1",
        }
        for branch, reason in (
            ("trunk", "default branch is not allowed"),
            ("other", "branch does not match this workspace"),
        ):
            with self.subTest(branch=branch):
                with self.assertRaisesRegex(creation.PolicyError, reason):
                    repository_scope(
                        authority,
                        project_id="project-1",
                        branch=branch,
                        require_runtime=True,
                    )
                with self.assertRaises(ScopeUnavailable):
                    repository_scope(
                        authority, project_id="other-project", branch=branch
                    )
                with self.assertRaises(ScopeUnavailable):
                    repository_scope(
                        {**authority, "kagent_session_id": None},
                        project_id="project-1",
                        branch=branch,
                    )

    def test_branch_sha_and_authority_arguments(self):
        for branch in (
            "main:feature",
            "../other",
            "refs/heads/x",
            "a..b",
            "x@{1}",
            "a.lock",
            ".hidden/x",
            "feature x",
            "-x",
            "a/",
            "a//b",
            "a\\b",
            "a?b",
            "@",
            "a\x00b",
            "a\x7fb",
        ):
            with self.subTest(branch=branch), self.assertRaises(ValidationError):
                OpenPullRequest.model_validate({**ARGS, "branch": branch})
        for field, value in (
            ("expected_sha", "abc"),
            ("project_id", ""),
            ("title", " "),
            ("request_id", ""),
            ("base", "main"),
            ("host", "evil.invalid"),
            ("token", "secret-fixture"),
            ("repo_url", "https://evil.invalid/a/b"),
        ):
            with self.subTest(field=field), self.assertRaises(ValidationError):
                OpenPullRequest.model_validate({**ARGS, field: value})
        for branch in ("feature/fix", "feature.v2", "topic_123"):
            self.assertEqual(
                OpenPullRequest.model_validate({**ARGS, "branch": branch}).branch,
                branch,
            )


class ToolTests(unittest.IsolatedAsyncioTestCase):
    async def test_discovery_and_invocation_share_roles(self):
        service = AgentService(PgStore())
        with patch(
            "mainloop.services.github_creation.open_pull_request",
            new=AsyncMock(return_value={"text": "fixture"}),
        ) as run:
            for role, depth, grant, allowed in (
                ("main", 0, "coordination", True),
                ("supervisor", 1, "workspace", True),
                ("child", 2, "workspace", True),
                ("agent", 0, "workspace", True),
                ("supervisor", 1, "coordination", False),
                ("child", 2, "coordination", False),
                ("child", 0, "coordination", False),
                ("child", 1, "workspace", False),
                ("supervisor", 0, "workspace", False),
                ("main", 1, "coordination", False),
                ("agent", 0, "none", False),
                ("unknown", 0, "coordination", False),
            ):
                with self.subTest(role=role, depth=depth, grant=grant):
                    actor = Actor(role, depth, grant)
                    ctx = Ctx({"role": role, "mcp_grant_kind": grant}, actor)
                    exposed = "open_pull_request" in tools_for(actor)
                    result = await invoke(service, ctx, "open_pull_request", ARGS)
                    self.assertEqual(exposed, allowed)
                    self.assertEqual(result.isError, not exposed)
                    if exposed:
                        may_call(actor, "open_pull_request")
            self.assertEqual(run.await_count, 4)
        self.assertIs(TOOLS["open_pull_request"][0], OpenPullRequest)

    async def test_client_errors_are_opaque_and_redirects_not_followed(self):
        for status in (301, 302, 307, 401, 403, 429, 500):
            fake = FakeGitHub()
            fake.refusal = status
            with patch.object(settings, "github_token", "secret-fixture"):
                async with creation.GitHubCreationClient(
                    transport=httpx.MockTransport(fake.handle)
                ) as client:
                    with self.assertRaises(creation.GitHubError) as error:
                        await client.repo("owner/repo")
                    self.assertNotIn("secret-fixture", str(error.exception))
            self.assertEqual(len(fake.requests), 1)
            self.assertEqual(
                fake.requests[0].headers["Authorization"], "Bearer secret-fixture"
            )

    async def test_invalid_upstream_evidence_and_pagination_are_bounded(self):
        count = 0

        async def handle(request):
            nonlocal count
            count += 1
            return httpx.Response(
                200,
                json=[FakeGitHub().pr()] * 100,
                headers={"Link": '<https://evil.invalid/>; rel="next"'},
            )

        with patch.object(settings, "github_token", "fixture"):
            async with creation.GitHubCreationClient(
                transport=httpx.MockTransport(handle)
            ) as client:
                with self.assertRaises(creation.GitHubError):
                    await client.find("owner/repo", "feature/fix", "trunk")
        self.assertEqual(count, 10)

    async def test_missing_token_refuses_before_http(self):
        with patch.object(settings, "github_token", ""):
            with self.assertRaises(creation.PolicyError) as error:
                creation.GitHubCreationClient()
            self.assertEqual(error.exception.code, "configuration")

    async def test_response_size_is_bounded(self):
        with patch.object(settings, "github_token", "fixture"):
            async with creation.GitHubCreationClient(
                transport=httpx.MockTransport(
                    lambda _: httpx.Response(200, content=b"x" * 2_000_001)
                )
            ) as client:
                with self.assertRaises(creation.GitHubError):
                    await client.repo("owner/repo")

    async def test_total_deadline_bounds_a_slow_response(self):
        class SlowStream(httpx.AsyncByteStream):
            async def __aiter__(self):
                await asyncio.sleep(0.05)
                yield json.dumps(REPO).encode()

        with patch.object(settings, "github_token", "fixture"), patch.object(
            creation, "REQUEST_TIMEOUT_SECONDS", 0.01
        ):
            async with creation.GitHubCreationClient(
                transport=httpx.MockTransport(
                    lambda _: httpx.Response(200, stream=SlowStream())
                )
            ) as client:
                with self.assertRaises(creation.GitHubError):
                    await client.repo("owner/repo")


class PRPostgresTests(PostgresTestCase):
    async def asyncSetUp(self):
        await super().asyncSetUp()
        # Each method owns its task capacity in this class's scratch database.
        # Retained attempts from earlier fixtures must not consume later admission.
        await self.pool.execute(
            "TRUNCATE tasks,task_attempts,workspace_writer_claims,task_events,task_operations CASCADE"
        )
        self.project = await db.get_or_create_project(
            self.user, parse_github_repo("owner/repo")
        )
        await db.update_project_metadata(self.project.id, default_branch="main")
        self.project = await db.get_project(self.project.id)
        self.sid, _ = await self.bound_session(role="main")
        self.binding = await PgStore().get_binding(self.sid)
        self.args = {**ARGS, "project_id": self.project.id}
        self.fake = FakeGitHub()
        self.service = AgentService(PgStore())
        self.ctx = await self.service.authenticate(token_for(self.sid))
        client_class = creation.GitHubCreationClient
        self.token_patch = patch.object(settings, "github_token", "fixture-only")
        self.token_patch.start()
        self.addCleanup(self.token_patch.stop)
        self.client_patch = patch.object(
            creation,
            "GitHubCreationClient",
            side_effect=lambda: client_class(
                transport=httpx.MockTransport(self.fake.handle)
            ),
        )
        self.client_patch.start()
        self.addCleanup(self.client_patch.stop)

    async def call(self, **changes):
        return await invoke(
            self.service, self.ctx, "open_pull_request", {**self.args, **changes}
        )

    async def workspace(self, branch="feature/fix"):
        sid, _ = await self.bound_session(
            role="agent", mcp_grant_kind="workspace", status="active"
        )
        await self.pool.execute(
            """UPDATE sessions SET project_id=$2,repo_url=$3,branch_name=$4
               WHERE id=$1""",
            sid,
            self.project.id,
            "https://github.com/owner/repo",
            branch,
        )
        await self.pool.execute(
            "INSERT INTO workspaces(session_id,repo,branch) VALUES($1,$2,$3)",
            sid,
            "https://github.com/owner/repo",
            branch,
        )
        await self.pool.execute(
            "UPDATE native_bindings SET kagent_session_id=$2 WHERE session_id=$1",
            sid,
            f"runtime-{sid}",
        )
        self.ctx = await self.service.authenticate(token_for(sid))
        self.args = {**ARGS, "project_id": self.project.id}
        return sid

    async def test_workspace_identity_and_pr_scope_are_resolved_server_side(self):
        sid = await self.workspace()
        self.assertEqual(
            tools_for(self.ctx.actor), frozenset({"whoami", "open_pull_request"})
        )
        identity = await invoke(self.service, self.ctx, "whoami", {})
        self.assertFalse(identity.isError)
        self.assertEqual(identity.structuredContent["grant_status"], "active")
        self.assertEqual(identity.structuredContent["scope_status"], "available")
        self.assertEqual(identity.structuredContent["project_id"], self.project.id)
        self.assertEqual(identity.structuredContent["workspace_id"], sid)
        self.assertEqual(identity.structuredContent["repository"], "owner/repo")
        self.assertEqual(identity.structuredContent["branch"], ARGS["branch"])
        self.assertNotIn("token", str(identity.structuredContent).lower())

        accepted = await self.call()
        self.assertFalse(accepted.isError, accepted.content)
        self.assertEqual(len(self.fake.posts), 1)

        other_project = await db.get_or_create_project(
            self.user, parse_github_repo("owner/other")
        )
        for changes in (
            {"project_id": other_project.id},
            {"branch": "feature/other"},
        ):
            result = await self.call(**changes)
            self.assertTrue(result.isError, changes)
        self.assertEqual(len(self.fake.posts), 1)

        mismatch = await self.call(branch="feature/other")
        self.assertIn("branch does not match this workspace", mismatch.content[0].text)
        await self.pool.execute(
            "UPDATE projects SET default_branch='trunk' WHERE id=$1", self.project.id
        )
        default_mismatch = await self.call(branch="trunk")
        self.assertIn("default branch is not allowed", default_mismatch.content[0].text)
        await self.workspace("trunk")
        default_head = await self.call(branch="trunk", request_id="default-head")
        self.assertTrue(default_head.isError)
        self.assertIn("default branch is not allowed", default_head.content[0].text)
        self.assertEqual(len(self.fake.posts), 1)

    async def test_sibling_workspace_identity_cannot_publish_another_branch(self):
        owner = await self.workspace("feature/owner")
        self.args["branch"] = "feature/owner"
        owner_ctx = self.ctx
        sibling = await self.workspace("feature/sibling")
        sibling_ctx = self.ctx

        self.ctx = owner_ctx
        self.args["branch"] = "feature/sibling"
        self.assertTrue((await self.call()).isError)
        self.assertEqual(len(self.fake.posts), 0)

        self.ctx = sibling_ctx
        self.args["branch"] = "feature/sibling"
        result = await self.call()
        self.assertFalse(result.isError, result.content)
        self.assertEqual(len(self.fake.posts), 1)
        self.assertNotEqual(owner, sibling)

    async def test_legacy_agent_binding_remains_unenrolled(self):
        sid, _ = await self.bound_session(role="agent")
        binding = await native_sessions.get_binding(sid)
        self.assertEqual(binding["mcp_grant_kind"], "none")
        self.assertIsNone(binding["token_hash"])
        with self.assertRaises(HTTPException) as denied:
            await self.service.authenticate(token_for(sid))
        self.assertEqual(denied.exception.status_code, 401)

    async def child(self):
        # Offline authority fixture: enroll persisted S0/S1 facts, no live runtime proof.
        now = datetime.now(UTC)
        root_id = uuid.uuid4().hex
        parent_binding = self.sid
        async with self.pool.acquire() as conn, conn.transaction():
            for role, depth, mode in (
                ("supervisor", 1, "coordination"),
                ("child", 2, "code"),
            ):
                task_id = root_id if depth == 1 else uuid.uuid4().hex
                task = Task(
                    id=task_id,
                    owner_id=self.user,
                    project_id=self.project.id,
                    parent_task_id=None if depth == 1 else root_id,
                    root_task_id=root_id,
                    creator_binding_id=parent_binding,
                    title="Offline PR scope fixture",
                    brief="Persisted authority fixture; no live runtime qualification",
                    mode=mode,
                    assigned_profile_id="codex",
                    selection_source="explicit",
                    checkout=(
                        TaskCheckout(branch=ARGS["branch"], ref=SHA)
                        if mode == "code"
                        else None
                    ),
                    created_at=now,
                    updated_at=now,
                )
                await task_store.insert_task(conn, task)
                task, attempt = await task_store.admit_attempt(
                    conn,
                    task,
                    registry().resolve("codex", role),
                    role=role,
                    depth=depth,
                )
                enrolled = await workspaces.enroll_session(
                    conn,
                    user_id=self.user,
                    kind="codex",
                    role=role,
                    mcp_grant_kind="workspace" if mode == "code" else "coordination",
                    manifest=(
                        WorkspaceManifest(
                            repo_url="https://github.com/owner/repo",
                            branch=ARGS["branch"],
                            ref=SHA,
                            agent_kind="codex",
                        )
                        if mode == "code"
                        else None
                    ),
                    project_id=self.project.id,
                    session_id=attempt.id,
                    parent_session_id=parent_binding,
                    title="Offline PR scope fixture",
                    description="Offline enrollment, not live provisioning evidence",
                    prompt="Fixture",
                    environment=None,
                    claim_branch=False,
                )
                await lifecycle.save_attempt(
                    conn,
                    attempt.model_copy(
                        update={
                            "binding_id": enrolled.workspace_id,
                            "session_id": enrolled.workspace_id,
                            "workspace_id": enrolled.workspace_id,
                            "state": "active",
                        }
                    ),
                )
                # Synthetic runtime identity satisfies repository authority, never calls kagent.
                await conn.execute(
                    "UPDATE native_bindings SET kagent_session_id=$2 WHERE session_id=$1",
                    enrolled.workspace_id,
                    "offline-pr-fixture-" + attempt.id,
                )
                parent_binding = enrolled.workspace_id
        self.ctx = await self.service.authenticate(token_for(parent_binding))
        return parent_binding

    async def test_create_uses_live_default_branch_and_returns_canonical_link(self):
        result = await self.call()
        self.assertFalse(result.isError, result.content)
        self.assertEqual(result.structuredContent["base"], "trunk")
        self.assertEqual(result.structuredContent["head_sha"], SHA)
        self.assertEqual(
            result.structuredContent["url"], "https://github.com/owner/repo/pull/17"
        )
        body = json.loads(self.fake.posts[0].content)
        self.assertEqual(body["base"], "trunk")
        self.assertEqual(body["head"], ARGS["branch"])
        self.assertEqual(self.project.default_branch, "main")
        row = await self.pool.fetchrow(
            "SELECT * FROM pr_creations WHERE user_id=$1", self.user
        )
        self.assertEqual(row["state"], "created")
        self.assertNotIn("fixture-only", str(dict(row)))

    async def test_competing_duplicate_requests_and_distinct_ids_send_one_post(self):
        responses = await asyncio.gather(
            *(self.call(request_id=f"request-{n % 3}") for n in range(12))
        )
        self.assertTrue(all(not r.isError for r in responses), responses)
        self.assertEqual(len(self.fake.posts), 1)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM pr_creations WHERE user_id=$1", self.user
            ),
            1,
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM pr_creation_requests WHERE user_id=$1", self.user
            ),
            3,
        )
        self.assertEqual(
            (await self.call(request_id="request-0")).structuredContent["state"],
            "created",
        )

    async def test_lost_response_reconciles_after_restart_without_second_post(self):
        self.fake.lose_response = True
        first = await self.call()
        self.assertEqual(first.structuredContent["state"], "uncertain")
        self.service = AgentService(PgStore())
        self.ctx = await self.service.authenticate(token_for(self.sid))
        before = len(self.fake.requests)
        second = await self.call()
        self.assertEqual(second.structuredContent["state"], "created")
        self.assertEqual(len(self.fake.posts), 1)
        self.assertTrue(
            any(
                r.method == "GET" and r.url.path.endswith("/pulls")
                for r in self.fake.requests[before:]
            )
        )

    async def test_no_match_after_lost_response_stays_uncertain(self):
        self.fake.lose_response = True
        await self.call()
        self.fake.prs = []
        self.assertEqual((await self.call()).structuredContent["state"], "uncertain")
        self.assertEqual(len(self.fake.posts), 1)

    async def test_request_hash_and_tuple_conflicts_rollback(self):
        await self.call()
        for changes in (
            {"title": "changed"},
            {"body": "changed"},
            {"request_id": "new", "title": "changed"},
        ):
            result = await self.call(**changes)
            self.assertTrue(result.isError)
            self.assertIn("[conflict]", result.content[0].text)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM pr_creation_requests WHERE user_id=$1", self.user
            ),
            1,
        )
        self.assertEqual(len(self.fake.posts), 1)

    async def test_completed_request_returns_recorded_result_without_github(self):
        first = await self.call()
        self.fake.requests.clear()
        self.fake.branch_sha = "c" * 40
        self.fake.refusal = 500
        second = await self.call()
        self.assertEqual(second.structuredContent, first.structuredContent)
        self.assertEqual(self.fake.requests, [])
        conflict = await self.call(expected_sha="c" * 40)
        self.assertTrue(conflict.isError)
        self.assertIn("[conflict]", conflict.content[0].text)
        self.assertEqual(self.fake.requests, [])

    async def test_crash_after_intent_before_post_never_replays(self):
        body = OpenPullRequest.model_validate(self.args)
        payload_hash = creation.hashlib.sha256(
            json.dumps(body.model_dump(exclude={"request_id"}), sort_keys=True).encode()
        ).hexdigest()
        await db.claim_pr_creation(
            user_id=self.user,
            project_id=self.project.id,
            request_id=self.args["request_id"],
            payload_hash=payload_hash,
            repo_id=123,
            head=ARGS["branch"],
            base="trunk",
            expected_sha=SHA,
        )
        self.assertEqual((await self.call()).structuredContent["state"], "uncertain")
        self.assertEqual(len(self.fake.posts), 0)

    async def test_default_branch_change_before_dispatch_never_retargets(self):
        original = self.fake.handle
        reads = 0

        async def handle(request):
            nonlocal reads
            if request.url.path == "/repos/owner/repo":
                reads += 1
                if reads == 2:
                    self.fake.repo["default_branch"] = "new-default"
            return await original(request)

        self.fake.handle = handle
        result = await self.call()
        self.assertTrue(result.isError)
        self.assertIn("[base]", result.content[0].text)
        self.assertEqual(len(self.fake.posts), 0)

    async def test_invalid_post_identity_stays_uncertain_without_replay(self):
        self.fake.pr_changes = lambda pr: pr["head"]["repo"].update(
            id=456, full_name="fork/repo"
        )
        result = await self.call()
        self.assertEqual(result.structuredContent["state"], "uncertain")
        self.assertTrue((await self.call()).isError)
        self.assertEqual(len(self.fake.posts), 1)

    async def test_competing_payload_conflicts_have_one_winner(self):
        for same_id in (True, False):
            await self.pool.execute(
                "DELETE FROM pr_creation_requests WHERE user_id=$1", self.user
            )
            await self.pool.execute(
                "DELETE FROM pr_creations WHERE user_id=$1", self.user
            )
            claims = [
                {
                    "user_id": self.user,
                    "project_id": self.project.id,
                    "request_id": "one" if same_id else f"r-{i}",
                    "payload_hash": f"hash-{i}",
                    "repo_id": 123,
                    "head": "feature/fix",
                    "base": "trunk",
                    "expected_sha": SHA,
                }
                for i in range(2)
            ]
            results = await asyncio.gather(
                *(db.claim_pr_creation(**c) for c in claims), return_exceptions=True
            )
            self.assertEqual(sum(isinstance(r, PRCreationConflict) for r in results), 1)
            self.assertEqual(sum(isinstance(r, tuple) and r[1] for r in results), 1)

    async def test_cross_owner_project_default_head_and_wrong_sha_refused(self):
        other = await db.get_or_create_project(
            "different-owner", parse_github_repo("owner/repo")
        )
        for changes in (
            {"project_id": other.id},
            {"project_id": "unknown"},
            {"branch": "trunk"},
            {"expected_sha": "c" * 40},
            {"branch": "fork:feature"},
        ):
            result = await self.call(**changes)
            self.assertTrue(result.isError)
        self.assertEqual(len(self.fake.posts), 0)

    async def test_child_requires_matching_project_workspace_and_branch(self):
        sid = await self.child()
        result = await self.call()
        self.assertFalse(result.isError, result.content)
        other = await db.get_or_create_project(
            self.user, parse_github_repo("owner/other")
        )
        self.assertTrue((await self.call(project_id=other.id)).isError)
        for query, value, restore in (
            (
                "UPDATE workspaces SET repo=$2 WHERE session_id=$1",
                "https://github.com/fork/repo",
                "https://github.com/owner/repo",
            ),
            (
                "UPDATE workspaces SET branch=$2 WHERE session_id=$1",
                "other",
                ARGS["branch"],
            ),
        ):
            await self.pool.execute(query, sid, value)
            self.assertTrue((await self.call()).isError)
            await self.pool.execute(query, sid, restore)
        await self.pool.execute("DELETE FROM workspaces WHERE session_id=$1", sid)
        self.assertTrue((await self.call()).isError)
        self.assertEqual(len(self.fake.posts), 1)

    async def test_binding_revocation_cases_refused_after_auth(self):
        # Authenticate before each server-side change so invocation uses a stale context.
        cases = (
            (
                "revoked token",
                "UPDATE native_bindings SET token_hash=NULL WHERE session_id=$1",
            ),
            (
                "unknown role",
                "UPDATE native_bindings SET role='agent' WHERE session_id=$1",
            ),
            (
                "child role without workspace",
                "UPDATE native_bindings SET role='child' WHERE session_id=$1",
            ),
            ("archived", "UPDATE sessions SET archived_at=NOW() WHERE id=$1"),
            (
                "runtime deleted",
                "UPDATE native_bindings SET kagent_deleted_at=NOW() WHERE session_id=$1",
            ),
            ("binding deleted", "DELETE FROM native_bindings WHERE session_id=$1"),
            ("completed", "UPDATE sessions SET status='completed' WHERE id=$1"),
            ("failed", "UPDATE sessions SET status='failed' WHERE id=$1"),
            ("cancelled", "UPDATE sessions SET status='cancelled' WHERE id=$1"),
        )
        lifecycle_denials = {
            "revoked token": "[403] binding_revoked",
            "archived": "[403] session_terminal",
            "runtime deleted": "[403] binding_revoked",
            "completed": "[403] session_terminal",
            "failed": "[403] session_terminal",
            "cancelled": "[403] session_terminal",
        }
        for name, query in cases:
            with self.subTest(state=name):
                sid, _ = await self.bound_session(role="main")
                self.ctx = await self.service.authenticate(token_for(sid))
                await self.pool.execute(query, sid)
                result = await self.call()
                self.assertTrue(result.isError)
                if name in lifecycle_denials:
                    self.assertEqual(result.content[0].text, lifecycle_denials[name])
                else:
                    self.assertIn("[ownership]", result.content[0].text)
                self.assertEqual(self.fake.requests, [])
                self.assertEqual(self.fake.posts, [])

    async def test_revocation_during_github_preflight_prevents_post(self):
        async def revoke(request):
            if request.method == "GET" and request.url.path.endswith("/pulls"):
                await self.pool.execute(
                    "UPDATE native_bindings SET token_hash=NULL WHERE session_id=$1",
                    self.sid,
                )
                self.fake.on_request = None

        self.fake.on_request = revoke
        with patch.object(
            db, "pr_project_authority", new=AsyncMock(wraps=db.pr_project_authority)
        ) as authority:
            result = await self.call()
            self.assertEqual(authority.await_count, 2)
        self.assertTrue(result.isError)
        self.assertEqual(
            result.content[0].text,
            "[ownership] project binding changed before PR creation",
        )
        self.assertIsNone(self.fake.on_request)
        self.assertEqual(self.fake.posts, [])

    async def test_repository_redirect_identity_and_fork_pr_refused(self):
        self.fake.repo["full_name"] = "fork/repo"
        self.assertTrue((await self.call()).isError)
        self.fake.repo = copy.deepcopy(REPO)
        pr = self.fake.pr()
        pr["head"]["repo"] = {**REPO, "id": 456, "full_name": "fork/repo"}
        self.fake.prs = [pr]
        self.assertTrue((await self.call()).isError)
        self.assertEqual(len(self.fake.posts), 0)

    async def test_ambiguous_listing_does_not_create(self):
        self.fake.prs = [self.fake.pr(), {**self.fake.pr(), "number": 18}]
        self.assertEqual((await self.call()).structuredContent["state"], "uncertain")
        self.assertEqual(len(self.fake.posts), 0)

    async def test_upstream_error_response_and_transport_secrets_never_reach_mcp(self):
        for status in (302, 401, 403, 429, 500):
            self.fake.refusal = status
            result = await self.call()
            self.assertTrue(result.isError)
            self.assertNotIn("secret-fixture", str(result))
            self.assertNotIn("fixture-only", str(result))
        self.fake.refusal = None
        self.fake.lose_response = True
        result = await self.call()
        self.assertNotIn("secret-fixture", str(result))
        self.assertNotIn("fixture-only", str(result))

    async def test_migration_is_repeatable(self):
        await self.call()
        await _init_schema(self.url)
        self.assertEqual((await self.call()).structuredContent["state"], "created")
        self.assertEqual(len(self.fake.posts), 1)

    async def test_delegated_pr_result_links_exact_attempt_and_event_once(self):
        sid = await self.child()
        first = await self.call()
        self.assertFalse(first.isError, first.content)
        async with self.pool.acquire() as conn:
            row = await conn.fetchrow(
                "SELECT id,task_id FROM task_attempts WHERE binding_id=$1", sid
            )
            attempt = await lifecycle.load_attempt(conn, row["id"])
            task = await lifecycle.load_task(conn, row["task_id"])
            projection = await task_store.projection(conn, task.id)
            refs = [
                ref for ref in attempt.evidence_refs if ref.startswith("pr-creation:")
            ]
        self.assertEqual(len(refs), 1)
        intent = await self.pool.fetchrow(
            "SELECT * FROM pr_creations WHERE id=$1",
            refs[0].removeprefix("pr-creation:"),
        )
        self.assertEqual(
            (intent["user_id"], intent["project_id"], intent["head"]),
            (self.user, self.project.id, task.checkout.branch),
        )
        self.assertEqual(
            (projection.repository, projection.pr_number, projection.pr_head_sha),
            ("owner/repo", 17, SHA),
        )
        self.assertEqual(
            (projection.pr_state, projection.ci_state), ("unknown", "unknown")
        )
        replay = await self.call()
        self.assertFalse(replay.isError, replay.content)
        self.assertEqual(replay.structuredContent, first.structuredContent)
        self.assertEqual(len(self.fake.posts), 1)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM task_events WHERE task_id=$1 AND event_key LIKE 'pr-created:%'",
                task.id,
            ),
            1,
        )
        self.assertEqual(
            await self.pool.fetchval("SELECT version FROM tasks WHERE id=$1", task.id),
            task.version,
        )

    async def test_delegated_lost_pr_response_reconciles_link_without_redispatch(self):
        sid = await self.child()
        self.fake.lose_response = True
        first = await self.call()
        self.assertEqual(first.structuredContent["state"], "uncertain")
        second = await self.call()
        self.assertFalse(second.isError, second.content)
        self.assertEqual(second.structuredContent["state"], "created")
        self.assertEqual(len(self.fake.posts), 1)
        task_id = await self.pool.fetchval(
            "SELECT task_id FROM task_attempts WHERE binding_id=$1", sid
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT projection->>'pr_head_sha' FROM tasks WHERE id=$1", task_id
            ),
            SHA,
        )

    async def test_unassociated_delegated_intent_cannot_be_adopted(self):
        await self.child()
        body = OpenPullRequest.model_validate(self.args)
        payload_hash = creation.hashlib.sha256(
            json.dumps(body.model_dump(exclude={"request_id"}), sort_keys=True).encode()
        ).hexdigest()
        await db.claim_pr_creation(
            user_id=self.user,
            project_id=self.project.id,
            request_id=body.request_id,
            payload_hash=payload_hash,
            repo_id=123,
            head=body.branch,
            base="trunk",
            expected_sha=SHA,
        )
        result = await self.call()
        self.assertTrue(result.isError)
        self.assertIn("[ownership]", result.content[0].text)
        self.assertEqual(self.fake.posts, [])
