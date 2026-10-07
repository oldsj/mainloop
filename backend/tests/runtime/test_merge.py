"""Merge paths use real PostgreSQL, real MCP/HITL seams and fake HTTP only."""

import asyncio
import copy
import json
import os
import unittest
from unittest.mock import patch

import asyncpg
import httpx
from fastapi.testclient import TestClient
from mainloop.config import settings
from mainloop.db import db
from mainloop.mcp_app import create_app, invoke
from mainloop.runtime import hitl_continuation
from mainloop.runtime.agent_identity import token_for
from mainloop.runtime.agent_tools import AgentService
from mainloop.runtime.delegation import PgStore
from mainloop.runtime.hitl_observer import HITLObserver
from mainloop.runtime.policy import PolicyError
from mainloop.services import github_merge, merge
from mainloop.services.github_creation import GitHubError
from mainloop.services.github_repo import parse_github_repo
from tests.runtime.test_context_model import KINDS, FakeStore
from tests.runtime.test_hitl_observer import Gateway
from tests.runtime.test_postgres_ledger import PostgresTestCase, _init_schema

from models.hitl import HITLProjection, ToolApproval, ToolApprovalResponse

SHA = "a" * 40
BASE = "b" * 40
MERGED = "c" * 40


class GitHub:
    def __init__(self):
        self.repo = {
            "id": 123,
            "full_name": "owner/repo",
            "default_branch": "main",
            "allow_squash_merge": True,
        }
        self.pr = {
            "number": 17,
            "state": "open",
            "draft": False,
            "changed_files": 1,
            "mergeable": True,
            "mergeable_state": "clean",
            "merged": False,
            "head": {"ref": "feature", "sha": SHA, "repo": self.repo},
            "base": {"ref": "main", "sha": BASE, "repo": self.repo},
        }
        self.files = [{"filename": "src/app.py", "status": "modified"}]
        self.suites = [
            {
                "id": 1,
                "head_sha": SHA,
                "created_at": "2026-10-01T00:00:00Z",
                "status": "completed",
                "conclusion": "success",
            }
        ]
        self.runs = [self.run(10)]
        self.statuses = []
        self.rules = []
        self.protection = {"required_status_checks": None}
        self.calls = []
        self.lose = False
        self.fail_before_put = False
        self.hook = None

    def run(self, id, **changes):
        return {
            "id": id,
            "name": "build",
            "head_sha": SHA,
            "app": {"id": 4},
            "status": "completed",
            "conclusion": "success",
            "check_suite": {"id": 1},
            **changes,
        }

    @property
    def puts(self):
        return [r for r in self.calls if r.method == "PUT"]

    async def handle(self, req):
        self.calls.append(req)
        if req.url.host != "api.github.com":
            raise AssertionError("unexpected upstream origin")
        if self.hook:
            await self.hook(req)
        path = req.url.path.removeprefix("/repos/owner/repo")
        if req.method == "PUT":
            if json.loads(req.content) != {"sha": SHA, "merge_method": "squash"}:
                raise AssertionError("merge must pin SHA and squash")
            if self.fail_before_put:
                raise httpx.ConnectError("fixture", request=req)
            self.pr.update(merged=True, state="closed", merge_commit_sha=MERGED)
            if self.lose:
                raise httpx.ReadTimeout("secret-fixture", request=req)
            return httpx.Response(200, json={"merged": True, "sha": MERGED})
        values = {
            "": self.repo,
            "/pulls/17": self.pr,
            "/pulls/17/files": self.files,
            "/branches/main": {
                "name": "main",
                "commit": {"sha": self.pr["base"]["sha"]},
            },
            "/branches/main/protection": self.protection,
            "/rules/branches/main": self.rules,
            f"/commits/{SHA}/statuses": self.statuses,
        }
        for endpoint, key, items in [
            ("check-runs", "check_runs", self.runs),
            ("check-suites", "check_suites", self.suites),
        ]:
            if path == f"/commits/{SHA}/{endpoint}":
                page = int(req.url.params.get("page", 1))
                return httpx.Response(
                    200,
                    json={
                        "total_count": len(items),
                        key: items[(page - 1) * 100 : page * 100],
                    },
                )
        if path in values:
            value = copy.deepcopy(values[path])
            if isinstance(value, list):
                page = int(req.url.params.get("page", 1))
                value = value[(page - 1) * 100 : page * 100]
            return httpx.Response(200, json=value)
        raise AssertionError(path)


class EvidenceTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.fake = GitHub()
        p = patch.object(settings, "github_token", "fixture")
        p.start()
        self.addCleanup(p.stop)

    async def evidence(self):
        async with github_merge.GitHubMergeClient(
            transport=httpx.MockTransport(self.fake.handle)
        ) as client:
            return await client.evidence("owner/repo", 17, SHA)

    async def test_green_rerun_namespaces_and_required_app(self):
        self.assertTrue((await self.evidence())["ci"]["green"])
        self.fake.runs.append(self.fake.run(11, status="queued", conclusion=None))
        self.assertFalse((await self.evidence())["ci"]["green"])
        self.fake.runs[-1].update(status="completed", conclusion="success")
        self.fake.statuses = [
            {
                "id": 1,
                "context": "build",
                "state": "pending",
                "created_at": "2026-10-01T00:00:00Z",
            }
        ]
        self.assertFalse((await self.evidence())["ci"]["green"])
        self.fake.statuses.append(
            {
                "id": 2,
                "context": "build",
                "state": "success",
                "created_at": "2026-10-01T00:00:00Z",
            }
        )
        self.assertTrue((await self.evidence())["ci"]["green"])
        self.fake.rules = [
            {
                "type": "required_status_checks",
                "parameters": {
                    "required_status_checks": [
                        {"context": "build", "integration_id": 99}
                    ]
                },
            }
        ]
        self.assertFalse((await self.evidence())["ci"]["green"])
        self.fake.rules[0]["parameters"]["required_status_checks"][0][
            "integration_id"
        ] = 4
        self.assertTrue((await self.evidence())["ci"]["green"])

    async def test_non_success_empty_and_unknown_mergeability(self):
        for conclusion in (
            "neutral",
            "skipped",
            "failure",
            "cancelled",
            "timed_out",
            "action_required",
            None,
            "unknown",
        ):
            self.fake.runs[0]["conclusion"] = conclusion
            self.assertFalse((await self.evidence())["ci"]["green"])
        self.fake.runs.clear()
        self.assertFalse((await self.evidence())["ci"]["green"])
        self.fake.pr["mergeable"] = None
        self.assertFalse((await self.evidence())["mergeable"])

    async def test_complete_files_protected_renames_and_deletions(self):
        self.fake.files = [
            {"filename": "src/a", "status": "renamed", "previous_filename": "k8s/a"}
        ]
        self.assertEqual((await self.evidence())["protected_matches"], ["k8s/a"])
        self.fake.files = [{"filename": "migrations/001.sql", "status": "removed"}]
        self.assertEqual(
            (await self.evidence())["protected_matches"], ["migrations/001.sql"]
        )
        self.fake.pr["changed_files"] = 2
        with self.assertRaises(GitHubError):
            await self.evidence()

    async def test_caps_duplicates_and_unsupported_inventory(self):
        self.fake.suites *= 1000
        with self.assertRaises(GitHubError):
            await self.evidence()
        self.fake.suites = self.fake.suites[:1]
        self.fake.runs *= 2
        with self.assertRaises(GitHubError):
            await self.evidence()
        self.fake.runs = self.fake.runs[:1]
        self.fake.rules = [{"type": "unknown"}]
        with self.assertRaises(PolicyError):
            await self.evidence()
        self.fake.rules = []
        self.fake.protection = {}
        with self.assertRaises((GitHubError, ValueError)):
            await self.evidence()

    async def test_identity_change_between_file_reads(self):
        count = 0

        async def hook(req):
            nonlocal count
            if req.url.path.endswith("/pulls/17"):
                count += 1
                if count == 2:
                    self.fake.pr["base"]["sha"] = "d" * 40

        self.fake.hook = hook
        with self.assertRaises(PolicyError):
            await self.evidence()

    async def test_multiple_pages_and_pending_suite_without_run(self):
        self.fake.runs = [self.fake.run(i, name=f"check-{i}") for i in range(1, 102)]
        self.assertTrue((await self.evidence())["ci"]["green"])
        self.assertTrue(any(r.url.params.get("page") == "2" for r in self.fake.calls))
        self.fake.suites.append(
            {"id": 2, "head_sha": SHA, "status": "queued", "conclusion": None}
        )
        self.assertFalse((await self.evidence())["ci"]["green"])
        self.fake.runs = [self.fake.run(i) for i in range(1, 1002)]
        with self.assertRaises(GitHubError):
            await self.evidence()

    async def test_draft_fork_head_base_and_squash_gates(self):
        original = copy.deepcopy(self.fake.pr)
        for field, value in (
            ("draft", True),
            ("state", "closed"),
            ("changed_files", 3001),
        ):
            self.fake.pr = {**copy.deepcopy(original), field: value}
            with self.assertRaises((PolicyError, ValueError)):
                await self.evidence()
        self.fake.pr = copy.deepcopy(original)
        self.fake.pr["head"]["sha"] = "e" * 40
        with self.assertRaises(PolicyError):
            await self.evidence()
        self.fake.pr = copy.deepcopy(original)
        self.fake.pr["head"]["repo"]["id"] = 999
        with self.assertRaises(PolicyError):
            await self.evidence()
        self.fake.pr = copy.deepcopy(original)
        self.fake.repo["allow_squash_merge"] = False
        with self.assertRaises(PolicyError):
            await self.evidence()

    async def test_classic_required_context_missing_and_unreadable(self):
        self.fake.protection = {
            "required_status_checks": {
                "checks": [{"context": "missing", "app_id": 4}],
                "contexts": [],
            }
        }
        self.assertFalse((await self.evidence())["ci"]["green"])
        self.fake.protection = {
            "required_status_checks": None,
            "restrictions": {"users": []},
        }
        with self.assertRaises(PolicyError):
            await self.evidence()

    async def test_every_completed_suite_must_succeed_with_or_without_runs(self):
        for conclusion in (
            "cancelled",
            "failure",
            "neutral",
            "skipped",
            "timed_out",
            "action_required",
            "stale",
            "startup_failure",
            None,
            "unknown",
        ):
            for with_runs in (False, True):
                with self.subTest(conclusion=conclusion, with_runs=with_runs):
                    self.fake.suites = [
                        {
                            "id": 1,
                            "head_sha": SHA,
                            "status": "completed",
                            "conclusion": conclusion,
                        },
                        {
                            "id": 2,
                            "head_sha": SHA,
                            "status": "completed",
                            "conclusion": "success",
                        },
                    ]
                    # Even a newer success for the same app/name cannot hide the
                    # older unsuccessful suite. In the runless case it is the
                    # only run, supplying otherwise green, nonempty evidence.
                    self.fake.runs = [self.fake.run(11, check_suite={"id": 2})]
                    if with_runs:
                        self.fake.runs.insert(
                            0, self.fake.run(10, conclusion=conclusion)
                        )
                    evidence = (await self.evidence())["ci"]
                    self.assertFalse(evidence["green"])
                    self.assertFalse(evidence["pending"])
                    self.assertEqual(evidence["suites"][0]["conclusion"], conclusion)
        self.fake.suites[0]["conclusion"] = "success"
        self.assertTrue((await self.evidence())["ci"]["green"])

    async def test_unknown_run_or_suite_status_is_not_pending(self):
        self.fake.runs.append(self.fake.run(11, status="future-state", conclusion=None))
        evidence = (await self.evidence())["ci"]
        self.assertFalse(evidence["green"])
        self.assertFalse(evidence["pending"])
        self.fake.runs = [self.fake.run(10)]
        self.fake.suites[0].update(status="future-state", conclusion=None)
        evidence = (await self.evidence())["ci"]
        self.assertFalse(evidence["green"])
        self.assertFalse(evidence["pending"])


class SurfaceTests(unittest.TestCase):
    def test_both_routes_list_and_call_filter_and_default_off(self):
        for enabled in ("false", "true"):
            with patch.dict(
                os.environ, MAINLOOP_MERGE_TOOLS_ENABLED=enabled
            ), TestClient(create_app(AgentService(FakeStore(), KINDS))) as client:
                for path, protected in [("/mcp", False), ("/mcp/merge-approval", True)]:

                    def rpc(method, params=None, credential="tok-main", path=path):
                        return client.post(
                            path,
                            headers={
                                "Authorization": f"Bearer {credential}",
                                "Accept": "application/json, text/event-stream",
                            },
                            json={
                                "jsonrpc": "2.0",
                                "id": 1,
                                "method": method,
                                "params": params or {},
                            },
                        )

                    rpc(
                        "initialize",
                        {
                            "protocolVersion": "2025-06-18",
                            "capabilities": {},
                            "clientInfo": {"name": "fixture", "version": "1"},
                        },
                    )
                    tools = {
                        t["name"] for t in rpc("tools/list").json()["result"]["tools"]
                    }
                    if protected:
                        self.assertEqual(
                            tools,
                            (
                                {"merge_pull_request_with_approval"}
                                if enabled == "true"
                                else set()
                            ),
                        )
                    else:
                        self.assertNotIn("merge_pull_request_with_approval", tools)
                        self.assertEqual(
                            "merge_pull_request" in tools, enabled == "true"
                        )
                    refused = (
                        "whoami" if protected else "merge_pull_request_with_approval"
                    )
                    self.assertTrue(
                        rpc("tools/call", {"name": refused, "arguments": {}}).json()[
                            "result"
                        ]["isError"]
                    )
                    self.assertEqual(
                        rpc("tools/list", credential="nope").status_code, 401
                    )


class MergeTests(PostgresTestCase):
    async def asyncSetUp(self):
        await super().asyncSetUp()
        self.project = await db.get_or_create_project(
            self.user, parse_github_repo("owner/repo")
        )
        self.sid, _ = await self.bound_session(role="main")
        await self.pool.execute(
            "UPDATE native_bindings SET kagent_session_id=$2 WHERE session_id=$1",
            self.sid,
            f"runtime-{self.sid}",
        )
        self.binding = await PgStore().get_binding(self.sid)
        self.service = AgentService(PgStore())
        self.ctx = await self.service.authenticate(token_for(self.sid))
        self.args = {
            "project_id": self.project.id,
            "pr_number": 17,
            "expected_sha": SHA,
            "request_id": "prepare-1",
        }
        self.fake = GitHub()
        cls = github_merge.GitHubMergeClient
        for p in (
            patch.dict(os.environ, MAINLOOP_MERGE_TOOLS_ENABLED="true"),
            patch.object(settings, "github_token", "fixture"),
            patch.object(
                merge,
                "GitHubMergeClient",
                lambda: cls(transport=httpx.MockTransport(self.fake.handle)),
            ),
        ):
            p.start()
            self.addCleanup(p.stop)

    async def prepare(self, **changes):
        return await merge.prepare(self.binding, {**self.args, **changes})

    async def execute(self, p, request="invoke-1", approved=False):
        return await merge.execute(
            self.binding,
            {"proposal_id": p["proposal_id"], "request_id": request},
            approved=approved,
        )

    async def pause(self, p, *, provider="claude", tool=None, request="invoke-1"):
        await self.pool.execute(
            "UPDATE native_bindings SET kind=$2 WHERE session_id=$1", self.sid, provider
        )
        gateway = Gateway()
        name = tool or (
            "mcp__mainloop-merge-approval__merge_pull_request_with_approval"
            if provider == "claude"
            else "mainloop-merge-approval.merge_pull_request_with_approval"
        )
        session, task = gateway.add(
            self.binding["kagent_session_id"],
            {
                "type": "tool_approval_request",
                "tools": [
                    {
                        "id": "call",
                        "call_id": "native",
                        "name": name,
                        "args": {
                            "proposal_id": p["proposal_id"],
                            "request_id": request,
                        },
                    }
                ],
            },
        )
        observer = HITLObserver(
            gateway,
            gateway=f"https://gateway/{self.user}",
            owner=self.user,
            creator="gateway-owner",
        )
        config = {
            "owner_id": self.user,
            "binding_id": self.sid,
            "runtime_session_id": session.id,
            "provider": provider,
            "prepared_revision": session.prepared_revision,
            "evidence_reference": "fixture://verified-config",
            "mappings": [
                {
                    "provider": provider,
                    "prepared_revision": session.prepared_revision,
                    "compiled_alias": "mainloop-merge-approval",
                    "remote_server_id": "kagent/mainloop-merge-approval",
                    "endpoint": "http://mainloop-mcp.mainloop.svc.cluster.local/mcp/merge-approval",
                    "tool": "merge_pull_request_with_approval",
                    "require_approval": True,
                }
            ],
        }
        patcher = patch.dict(
            os.environ, MAINLOOP_MERGE_CONFIGURATIONS=json.dumps([config])
        )
        patcher.start()
        self.addCleanup(patcher.stop)
        await observer.once()
        raw = await self.pool.fetchval(
            "SELECT snapshot FROM native_hitl_requests WHERE owner_id=$1", self.user
        )
        projection = HITLProjection.model_validate(merge.decode(raw))
        return observer, projection, gateway

    async def decide(self, observer, projection, approved=True):
        return await hitl_continuation.submit(
            self.user,
            projection.id,
            "decision-1",
            ToolApprovalResponse(
                type="tool_approval_response",
                approvals=(ToolApproval(id="call", approved=approved),),
            ),
            service=observer,
        )

    async def test_auto_no_human_card_pinned_squash_and_notification_dedup(self):
        results = await asyncio.gather(
            *(merge.auto_merge(self.binding, self.args) for _ in range(2))
        )
        self.assertTrue(all(r["state"] == "merged" for r in results))
        self.assertEqual(len(self.fake.puts), 1)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_hitl_requests WHERE owner_id=$1", self.user
            ),
            0,
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM queue_items WHERE user_id=$1 AND id LIKE 'merge-outcome:%'",
                self.user,
            ),
            1,
        )
        self.assertEqual(
            (await merge.auto_merge(self.binding, self.args))["state"], "merged"
        )
        self.assertEqual(len(self.fake.puts), 1)

    async def test_protected_and_approval_route_do_not_create_card(self):
        for protected in (True, False):
            if protected:
                self.fake.files[0]["filename"] = "k8s/app.yaml"
            else:
                self.fake.files[0]["filename"] = "src/app.py"
                await self.pool.execute(
                    "UPDATE projects SET merge_policy='approval',merge_policy_version=merge_policy_version+1 WHERE id=$1",
                    self.project.id,
                )
            result = await merge.auto_merge(
                self.binding, {**self.args, "request_id": f"prepare-{protected}"}
            )
            self.assertEqual(result["state"], "approval_required")
            with self.assertRaises(PolicyError):
                await self.execute(result)
            with self.assertRaises(PolicyError):
                await self.execute(result, approved=True)
        self.assertEqual(len(self.fake.puts), 0)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_hitl_requests WHERE owner_id=$1", self.user
            ),
            0,
        )

    async def test_recorded_consent_before_runtime_acceptance_and_provider_names(self):
        await self.pool.execute(
            "UPDATE projects SET merge_policy='approval',merge_policy_version=2 WHERE id=$1",
            self.project.id,
        )
        p = await self.prepare()
        observer, projection, gateway = await self.pause(p)
        # Record via the real route but leave dispatch unaccepted. Merge lookup must
        # not wait on the resumed task's own completion or transport acknowledgment.
        from unittest.mock import AsyncMock

        with patch.object(hitl_continuation, "dispatch", AsyncMock()):
            await self.decide(observer, projection)
        self.assertEqual((await self.execute(p, approved=True))["state"], "merged")
        self.assertEqual((await self.execute(p, approved=True))["state"], "merged")
        self.assertEqual((await self.execute(p, approved=False))["state"], "merged")
        self.assertEqual(len(self.fake.puts), 1)
        self.assertEqual(gateway.sent, [])
        async with db.connection() as conn:
            view = await hitl_continuation.view(conn, projection)
        self.assertEqual(view["merge_enrichment"][0]["head_sha"], SHA)

    async def test_codex_exact_receipt_and_wrong_invocation_denied(self):
        self.fake.files[0]["filename"] = "k8s/protected.yaml"
        p = await self.prepare()
        observer, projection, _ = await self.pause(p, provider="codex")
        await self.decide(observer, projection)
        with self.assertRaises(PolicyError):
            await self.execute(p, request="wrong", approved=True)
        self.assertEqual((await self.execute(p, approved=True))["state"], "merged")

    async def test_unrelated_approval_and_unknown_mapping_never_authorize(self):
        p = await self.prepare()
        observer, projection, _ = await self.pause(
            p, tool="mcp__other__merge_pull_request_with_approval"
        )
        await self.decide(observer, projection)
        with self.assertRaises(PolicyError):
            await self.execute(p, approved=True)
        self.assertEqual(len(self.fake.puts), 0)

    async def test_rejection_bars_auto_and_new_request_ids(self):
        p = await self.prepare()
        observer, projection, _ = await self.pause(p)
        await self.decide(observer, projection, False)
        with self.assertRaises(PolicyError):
            await self.execute(p)
        with self.assertRaises(PolicyError):
            await self.prepare(request_id="another")
        self.assertEqual(len(self.fake.puts), 0)

    async def test_stale_base_policy_and_replacement_refuse_approval(self):
        p = await self.prepare()
        observer, projection, _ = await self.pause(p)
        self.fake.pr["base"]["sha"] = "d" * 40
        with self.assertRaises(ValueError):
            await self.decide(observer, projection)
        self.fake.pr["base"]["sha"] = BASE
        replacement = await self.prepare(request_id="replacement")
        self.assertNotEqual(p["proposal_id"], replacement["proposal_id"])
        with self.assertRaises(ValueError):
            await self.decide(observer, projection)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_hitl_responses WHERE owner_id=$1",
                self.user,
            ),
            0,
        )

    async def test_policy_change_before_claim_refuses_auto(self):
        p = await self.prepare()
        await self.pool.execute(
            "UPDATE projects SET merge_policy='approval',merge_policy_version=2 WHERE id=$1",
            self.project.id,
        )
        with self.assertRaises(PolicyError):
            await self.execute(p)
        self.assertEqual(len(self.fake.puts), 0)

    async def test_ci_failure_explicit_same_head_retry_and_immutable_proposal(self):
        p = await self.prepare()
        self.fake.runs[0]["conclusion"] = "failure"
        self.assertEqual((await self.execute(p))["state"], "blocked")
        self.fake.runs[0]["conclusion"] = "success"
        self.assertEqual((await self.execute(p))["state"], "blocked")
        newer = await self.prepare(request_id="retry")
        self.assertEqual((await self.execute(p))["state"], "blocked")
        self.assertEqual(
            (await self.execute(newer, request="retry-invoke"))["state"], "merged"
        )
        async with db.connection() as conn:
            with self.assertRaises(asyncpg.RaiseError):
                await conn.execute(
                    "UPDATE merge_proposals SET facts='{}' WHERE id=$1",
                    p["proposal_id"],
                )

    async def test_lost_response_and_crash_before_dispatch_never_repeat(self):
        p = await self.prepare()
        self.fake.lose = True
        self.assertEqual((await self.execute(p))["state"], "uncertain")
        self.assertEqual((await self.execute(p))["state"], "merged")
        self.assertEqual(len(self.fake.puts), 1)

    async def test_crash_before_put_stays_uncertain_across_duplicate_ids(self):
        p = await self.prepare()
        self.fake.fail_before_put = True
        self.assertEqual((await self.execute(p))["state"], "uncertain")
        self.fake.fail_before_put = False
        self.assertEqual((await self.execute(p, request="other"))["state"], "uncertain")
        with self.assertRaises(PolicyError):
            await self.prepare(request_id="other")
        self.assertEqual(len(self.fake.puts), 1)

    async def test_runtime_replacement_and_revocation_denied(self):
        p = await self.prepare()
        await self.pool.execute(
            "UPDATE native_bindings SET kagent_session_id='replacement' WHERE session_id=$1",
            self.sid,
        )
        with self.assertRaises(PolicyError):
            await self.execute(p)
        self.assertEqual(len(self.fake.puts), 0)

    async def test_schema_idempotent(self):
        await _init_schema(self.url)
        p = await self.prepare()
        await _init_schema(self.url)
        self.assertEqual((await self.prepare())["proposal_id"], p["proposal_id"])

    async def test_queued_rerun_deadline_persists_and_explicit_retry(self):
        p = await self.prepare()
        self.fake.runs.append(self.fake.run(11, status="queued", conclusion=None))
        self.assertEqual((await self.execute(p))["state"], "evaluating")
        deadline = await self.pool.fetchval(
            "SELECT deadline FROM merge_requests WHERE owner_id=$1", self.user
        )
        self.assertEqual((await self.execute(p))["state"], "evaluating")
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT deadline FROM merge_requests WHERE owner_id=$1", self.user
            ),
            deadline,
        )
        with self.assertRaises(PolicyError):
            await self.prepare(request_id="bypass")
        await self.pool.execute(
            "UPDATE merge_requests SET deadline=now()-interval '1 second' WHERE owner_id=$1",
            self.user,
        )
        self.assertEqual((await self.execute(p))["state"], "expired")
        newer = await self.prepare(request_id="explicit-retry")
        self.fake.runs[-1].update(status="completed", conclusion="success")
        self.assertEqual(
            (await self.execute(newer, request="new-attempt"))["state"], "merged"
        )

    async def test_long_human_wait_does_not_start_evaluation(self):
        p = await self.prepare()
        observer, projection, _ = await self.pause(p)
        self.assertIsNone(
            await self.pool.fetchval(
                "SELECT deadline FROM merge_requests WHERE owner_id=$1", self.user
            )
        )
        # Proposal creation timestamps have no validity timeout; no native retention proof.
        from datetime import datetime, timedelta, timezone

        with patch.object(merge, "datetime") as clock:
            clock.now.return_value = datetime.now(timezone.utc) + timedelta(days=7)
            await self.decide(observer, projection)
        self.assertIsNone(
            await self.pool.fetchval(
                "SELECT deadline FROM merge_requests WHERE owner_id=$1", self.user
            )
        )
        self.assertEqual((await self.execute(p, approved=True))["state"], "merged")

    async def test_unknown_configuration_and_spoofed_metadata_mint_no_receipt(self):
        p = await self.prepare()
        observer, projection, gateway = await self.pause(p)
        with patch.dict(os.environ, MAINLOOP_MERGE_CONFIGURATIONS="[]"):
            for task in gateway.tasks.values():
                task.status.message.metadata["merge_approved"] = True
                task.status.message.metadata["prepared_revision"] = "forged"
            await self.decide(observer, projection)
        self.assertIsNone(
            await self.pool.fetchval(
                "SELECT call_snapshot->>'merge_key' FROM native_hitl_response_members WHERE owner_id=$1",
                self.user,
            )
        )
        with self.assertRaises(PolicyError):
            await self.execute(p, approved=True)

    async def test_sibling_and_changed_payload_cannot_use_receipt(self):
        p = await self.prepare()
        observer, projection, _ = await self.pause(p)
        await self.decide(observer, projection)
        sibling, _ = await self.bound_session(role="main")
        await self.pool.execute(
            "UPDATE native_bindings SET kagent_session_id=$2 WHERE session_id=$1",
            sibling,
            f"runtime-{sibling}",
        )
        binding = await PgStore().get_binding(sibling)
        with self.assertRaises(PolicyError):
            await merge.execute(
                binding,
                {"proposal_id": p["proposal_id"], "request_id": "invoke-1"},
                approved=True,
            )
        replacement = await self.prepare(request_id="new-proposal")
        with self.assertRaises(PolicyError):
            await self.execute(replacement, approved=True)
        with self.assertRaises(PolicyError):
            await self.execute(p, approved=True)
        self.assertEqual(len(self.fake.puts), 0)

    async def test_policy_and_rejection_win_before_claim(self):
        p = await self.prepare()
        observer, projection, _ = await self.pause(p)
        reached, resume = asyncio.Event(), asyncio.Event()

        async def hook(req):
            if req.url.path.endswith("/check-runs"):
                reached.set()
                await resume.wait()

        self.fake.hook = hook
        task = asyncio.create_task(self.execute(p))
        await asyncio.wait_for(reached.wait(), 3)
        await self.decide(observer, projection, False)
        resume.set()
        with self.assertRaises(PolicyError):
            await task
        self.assertEqual(len(self.fake.puts), 0)

    async def test_claim_wins_before_later_rejection_and_policy_edit(self):
        p = await self.prepare()
        observer, projection, _ = await self.pause(p)
        reached, resume = asyncio.Event(), asyncio.Event()

        async def hook(req):
            if req.method == "PUT":
                reached.set()
                await resume.wait()

        self.fake.hook = hook
        task = asyncio.create_task(self.execute(p))
        await asyncio.wait_for(reached.wait(), 3)
        with self.assertRaises(ValueError):
            await self.decide(observer, projection, False)
        from models.merge_policy import MergePolicyUpdate

        await db.update_merge_policy(
            self.project.id,
            self.user,
            MergePolicyUpdate(merge_policy="approval", expected_version=1),
        )
        resume.set()
        self.assertEqual((await task)["state"], "merged")
        self.assertEqual((await self.execute(p))["state"], "merged")
        self.assertEqual(len(self.fake.puts), 1)

    async def test_verified_outer_parent_authorizes_only_leaf(self):
        from mainloop.db import hitl as store
        from mainloop.runtime.hitl_correlation import task_identity

        from models.hitl import VerifiedAssociation

        p = await self.prepare()
        observer, child, gateway = await self.pause(p)
        leaf_task = gateway.tasks[child.outer.task_id]
        parent, task = gateway.add(
            "outer-parent",
            {
                "type": "tool_approval_request",
                "tools": [
                    {
                        "id": "parent-call",
                        "call_id": "parent-native",
                        "name": "delegate",
                        "args": {},
                    }
                ],
                "nested": {
                    "subagent_name": "hint",
                    "task_id": leaf_task.id,
                    "context_id": leaf_task.context_id,
                    "tools": leaf_task.status.message.metadata[
                        "https://kagent.dev/extensions/hitl/v1"
                    ]["tools"],
                },
            },
        )
        await self.pool.execute(
            "UPDATE native_hitl_inventory_state SET next_sweep=now() WHERE owner_id=$1",
            self.user,
        )
        await observer.once()
        raw = await self.pool.fetchval(
            "SELECT snapshot FROM native_hitl_requests WHERE owner_id=$1 AND snapshot->'outer'->>'task_id'=$2",
            self.user,
            task.id,
        )
        outer = HITLProjection.model_validate(merge.decode(raw))
        async with self.pool.acquire() as conn:
            self.assertTrue((await hitl_continuation.view(conn, child))["answerable"])
            await store.save_association(
                conn,
                VerifiedAssociation(
                    owner_id=self.user,
                    outer=task_identity(outer.outer),
                    leaf=task_identity(child.outer),
                    evidence_source="gateway_continuation",
                    evidence_reference="fixture:trusted",
                ),
            )
        await observer.once()
        await observer.once()
        raw = await self.pool.fetchval(
            "SELECT snapshot FROM native_hitl_requests WHERE owner_id=$1 AND snapshot->'outer'->>'task_id'=$2",
            self.user,
            task.id,
        )
        outer = HITLProjection.model_validate(merge.decode(raw))
        await self.decide(observer, outer)
        self.assertEqual(gateway.sent[0][1]["task_id"], task.id)
        receipt = merge.decode(
            await self.pool.fetchval(
                "SELECT snapshot FROM native_hitl_responses WHERE owner_id=$1",
                self.user,
            )
        )
        self.assertEqual(receipt["outer"]["runtime_session_id"], parent.id)
        self.assertEqual(receipt["calls"][0]["merge_key"]["leaf_binding_id"], self.sid)
        self.assertEqual((await self.execute(p, approved=True))["state"], "merged")

    async def test_raw_mcp_invocation_requires_receipt(self):
        p = await self.prepare()
        args = {"proposal_id": p["proposal_id"], "request_id": "invoke-1"}
        result = await invoke(
            self.service,
            self.ctx,
            "merge_pull_request_with_approval",
            args,
            surface="approval",
        )
        self.assertTrue(result.isError)
        self.assertIn("[consent]", result.content[0].text)
        observer, projection, _ = await self.pause(p)
        await self.decide(observer, projection)
        result = await invoke(
            self.service,
            self.ctx,
            "merge_pull_request_with_approval",
            args,
            surface="approval",
        )
        self.assertFalse(result.isError)
        self.assertEqual(result.structuredContent["state"], "merged")
        self.assertEqual(len(self.fake.puts), 1)

    async def test_policy_change_during_refresh_wins_claim(self):
        p = await self.prepare()
        changed = False

        async def hook(req):
            nonlocal changed
            if not changed and req.url.path.endswith("/check-runs"):
                changed = True
                from models.merge_policy import MergePolicyUpdate

                await db.update_merge_policy(
                    self.project.id,
                    self.user,
                    MergePolicyUpdate(merge_policy="approval", expected_version=1),
                )

        self.fake.hook = hook
        self.assertEqual((await self.execute(p))["state"], "blocked")
        self.assertEqual(len(self.fake.puts), 0)

    async def test_changed_head_and_diff_after_consent_refused(self):
        p = await self.prepare()
        observer, projection, _ = await self.pause(p)
        await self.decide(observer, projection)
        self.fake.files[0]["filename"] = "k8s/changed.yaml"
        self.assertEqual((await self.execute(p, approved=True))["state"], "blocked")
        self.assertEqual(len(self.fake.puts), 0)

    async def test_intent_survives_expired_deadline_and_unusable_response(self):
        p = await self.prepare()
        self.fake.fail_before_put = True
        self.assertEqual((await self.execute(p))["state"], "uncertain")
        await self.pool.execute(
            "UPDATE merge_requests SET deadline=now()-interval '1 day' WHERE owner_id=$1",
            self.user,
        )
        self.fake.fail_before_put = False
        self.assertEqual((await self.execute(p))["state"], "uncertain")
        self.assertEqual(len(self.fake.puts), 1)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM queue_items WHERE user_id=$1 AND id LIKE 'merge-outcome:%'",
                self.user,
            ),
            0,
        )

    async def test_crash_after_intent_before_any_put(self):
        p = await self.prepare()
        factory = merge.GitHubMergeClient
        calls = 0

        def crash_on_dispatch():
            nonlocal calls
            calls += 1
            if calls == 2:
                raise asyncio.CancelledError
            return factory()

        with patch.object(merge, "GitHubMergeClient", crash_on_dispatch):
            with self.assertRaises(asyncio.CancelledError):
                await self.execute(p)
        self.assertEqual(len(self.fake.puts), 0)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT state FROM merge_requests WHERE owner_id=$1", self.user
            ),
            "uncertain",
        )
        self.assertEqual((await self.execute(p))["state"], "uncertain")
        self.assertEqual(len(self.fake.puts), 0)

    async def assert_runless_suite_blocks(self, conclusion, *, approved):
        if approved:
            await self.pool.execute(
                "UPDATE projects SET merge_policy='approval',merge_policy_version=2 WHERE id=$1",
                self.project.id,
            )
        p = await self.prepare()
        if approved:
            observer, projection, _ = await self.pause(p)
            await self.decide(observer, projection)
        self.fake.suites.append(
            {"id": 2, "head_sha": SHA, "status": "completed", "conclusion": conclusion}
        )
        self.fake.pr["mergeable_state"] = "unstable"
        result = (
            await self.execute(p, approved=True)
            if approved
            else await merge.auto_merge(self.binding, self.args)
        )
        self.assertEqual(result["state"], "blocked")
        self.assertEqual(
            (
                await self.execute(
                    p,
                    request="invoke-1" if approved else self.args["request_id"],
                    approved=approved,
                )
            )["state"],
            "blocked",
        )
        self.assertEqual(len(self.fake.puts), 0)
        self.assertIsNone(
            await self.pool.fetchval(
                "SELECT intent_id FROM merge_requests WHERE owner_id=$1", self.user
            )
        )

    async def test_runless_cancelled_suite_blocks_auto(self):
        await self.assert_runless_suite_blocks("cancelled", approved=False)

    async def test_runless_failed_suite_blocks_auto(self):
        await self.assert_runless_suite_blocks("failure", approved=False)

    async def test_runless_cancelled_suite_blocks_approved(self):
        await self.assert_runless_suite_blocks("cancelled", approved=True)

    async def test_runless_failed_suite_blocks_approved(self):
        await self.assert_runless_suite_blocks("failure", approved=True)

    async def assert_pending_read_race_resumes(self, *, approved):
        if approved:
            await self.pool.execute(
                "UPDATE projects SET merge_policy='approval',merge_policy_version=2 WHERE id=$1",
                self.project.id,
            )
        p = await self.prepare()
        if approved:
            observer, projection, _ = await self.pause(p)
            await self.decide(observer, projection)
        deadline = None
        for index, status in enumerate(
            ("queued", "in_progress", "pending", "waiting", "requested")
        ):
            with self.subTest(status=status, approved=approved):
                self.fake.suites[0].update(status="completed", conclusion="success")
                observed = False

                async def introduce_run(req, status=status, run_id=11 + index):
                    nonlocal observed
                    if req.url.path.endswith("/check-runs") and not observed:
                        # The completed suite snapshot has already been returned.
                        observed = True
                        self.fake.suites[0].update(
                            status="in_progress", conclusion=None
                        )
                        self.fake.runs.append(
                            self.fake.run(run_id, status=status, conclusion=None)
                        )

                self.fake.hook = introduce_run
                self.assertEqual(
                    (await self.execute(p, approved=approved))["state"], "evaluating"
                )
                self.assertTrue(observed)
                current = await self.pool.fetchval(
                    "SELECT deadline FROM merge_requests WHERE owner_id=$1", self.user
                )
                if deadline is None:
                    deadline = current
                self.assertEqual(current, deadline)
                self.assertEqual(len(self.fake.puts), 0)
                self.assertEqual(
                    await self.pool.fetchval(
                        "SELECT count(*) FROM merge_proposal_results WHERE proposal_id=$1",
                        p["proposal_id"],
                    ),
                    0,
                )
                self.fake.runs[-1].update(status="completed", conclusion="success")
        self.fake.hook = None
        self.fake.suites[0].update(status="completed", conclusion="success")
        self.assertEqual((await self.execute(p, approved=approved))["state"], "merged")
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT deadline FROM merge_requests WHERE owner_id=$1", self.user
            ),
            deadline,
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM merge_proposals WHERE owner_id=$1", self.user
            ),
            1,
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_hitl_responses WHERE owner_id=$1",
                self.user,
            ),
            int(approved),
        )
        self.assertEqual(len(self.fake.puts), 1)

    async def test_auto_pending_suite_read_races_resume_same_attempt(self):
        await self.assert_pending_read_race_resumes(approved=False)

    async def test_approved_pending_suite_read_races_resume_same_attempt(self):
        await self.assert_pending_read_race_resumes(approved=True)
