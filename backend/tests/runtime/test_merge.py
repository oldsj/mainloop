"""Merge paths use real PostgreSQL, real MCP/HITL seams and fake HTTP only."""

import asyncio
import copy
import json
import os
import unittest
from datetime import datetime, timedelta, timezone
from unittest.mock import patch

import asyncpg
import httpx
from fastapi.testclient import TestClient
from mainloop.db import db
from mainloop.mcp_app import create_app, invoke
from mainloop.runtime import hitl_continuation, native_sessions
from mainloop.runtime.agent_identity import token_for
from mainloop.runtime.agent_tools import AgentService
from mainloop.runtime.delegation import PgStore
from mainloop.runtime.hitl_observer import HITLObserver
from mainloop.runtime.policy import PolicyError
from mainloop.services import github_merge, merge, merge_authorization
from mainloop.services.github_creation import GitHubError
from mainloop.services.github_repo import parse_github_repo
from mainloop.tasks.projection import ci_state
from tests.runtime.github_app_fake import app_settings, app_transport
from tests.runtime.test_context_model import KINDS, FakeStore
from tests.runtime.test_hitl_observer import Gateway
from tests.runtime.test_postgres_ledger import PostgresTestCase, _init_schema

from models.hitl import (
    HITL_EXTENSION,
    HITLProjection,
    ToolApproval,
    ToolApprovalResponse,
)

SHA = "a" * 40
BASE = "b" * 40
MERGED = "c" * 40
# Current default-branch head once main has moved past the PR's recorded base.
MOVED = "f" * 40
# GitHub's test merge commit for the PR (refs/pull/N/merge).
TEST_MERGE = "9" * 40
CAPTURED_AT = datetime(2026, 10, 9, 2, tzinfo=timezone.utc)


def abandoned_suite(**changes):
    return {
        "id": 2,
        "head_sha": SHA,
        "app": {"id": 99, "slug": "cloudflare-workers-and-pages"},
        "created_at": (CAPTURED_AT - timedelta(minutes=11)).isoformat(),
        "latest_check_runs_count": 0,
        "status": "queued",
        "conclusion": None,
        **changes,
    }


def default_branch_rules():
    # oldsj/infrastructure PR 69, terraform/github/main.tf, with required REST fields.
    return [
        {"type": "deletion"},
        {"type": "non_fast_forward"},
        {
            "type": "pull_request",
            "parameters": {
                "required_approving_review_count": 0,
                "dismiss_stale_reviews_on_push": False,
                "require_code_owner_review": False,
                "require_last_push_approval": False,
                "required_review_thread_resolution": False,
            },
        },
    ]


def live_mainloop_rules():
    # gh api repos/oldsj/mainloop/rules/branches/main (2026-10-10), ruleset metadata
    # dropped. Commits on PRs from the Mainloop App are App/agent-authored.
    return [
        {"type": "deletion"},
        {"type": "non_fast_forward"},
        {
            "type": "pull_request",
            "parameters": {
                "required_approving_review_count": 0,
                "dismiss_stale_reviews_on_push": False,
                "required_reviewers": [],
                "require_code_owner_review": False,
                "require_last_push_approval": False,
                "required_review_thread_resolution": False,
                "require_extra_approval_for_unattributed_changes": True,
                "allowed_merge_methods": ["squash"],
            },
        },
        {
            "type": "required_status_checks",
            "parameters": {
                "strict_required_status_checks_policy": False,
                "do_not_enforce_on_create": False,
                "required_status_checks": [
                    {"context": "Lint", "integration_id": 15368}
                ],
            },
        },
    ]


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
            "title": "Update workspace deployment defaults",
            "body": "Updates the defaults used by workspace deployments.",
            "draft": False,
            "changed_files": 1,
            "additions": 3,
            "deletions": 1,
            "mergeable": True,
            "mergeable_state": "clean",
            "merged": False,
            "merge_commit_sha": TEST_MERGE,
            "head": {"ref": "feature", "sha": SHA, "repo": self.repo},
            "base": {"ref": "main", "sha": BASE, "repo": self.repo},
        }
        self.files = [
            {
                "filename": "src/app.py",
                "status": "modified",
                "additions": 3,
                "deletions": 1,
            }
        ]
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
        self.errors = {}
        self.calls = []
        self.lose = False
        self.fail_before_put = False
        self.hook = None
        # GitHub keeps pr.base.sha at the base of the last synchronization.
        self.main = BASE
        # Test-merge inventory against main and its parents; None mirrors the
        # PR files and [main, head].
        self.merge_files = None
        self.merge_parents = None

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
        path = req.url.path.removeprefix(f"/repos/{self.repo['full_name']}")
        if path in self.errors:
            error = self.errors[path]
            if isinstance(error, tuple):
                return httpx.Response(error[0], json=error[1])
            return httpx.Response(error, json={"message": "fixture"})
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
                "commit": {"sha": self.main},
            },
            "/branches/main/protection": self.protection,
            "/rules/branches/main": self.rules,
            f"/commits/{SHA}/statuses": self.statuses,
        }
        merge_sha = self.pr.get("merge_commit_sha")
        if merge_sha and path == f"/commits/{merge_sha}":
            files = self.files if self.merge_files is None else self.merge_files
            page = int(req.url.params.get("page", 1))
            parents = self.merge_parents or [self.main, self.pr["head"]["sha"]]
            additions = sum(item["additions"] for item in files)
            deletions = sum(item["deletions"] for item in files)
            return httpx.Response(
                200,
                json={
                    "sha": merge_sha,
                    "parents": [{"sha": parent} for parent in parents],
                    "stats": {
                        "additions": additions,
                        "deletions": deletions,
                        "total": additions + deletions,
                    },
                    "files": copy.deepcopy(files[(page - 1) * 100 : page * 100]),
                },
            )
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
        p = app_settings()
        p.start()
        self.addCleanup(p.stop)

    async def evidence(self):
        async with github_merge.GitHubMergeClient(
            "owner/repo", transport=app_transport(self.fake.handle)
        ) as client:
            return await client.evidence("owner/repo", 17, SHA)

    async def captured_ci(self):
        with patch.object(github_merge, "datetime", wraps=datetime) as clock:
            clock.now.return_value = CAPTURED_AT
            ci = (await self.evidence())["ci"]
            clock.now.assert_called_once_with(timezone.utc)
            return ci

    async def test_abandoned_suite_is_ignored_and_auditable(self):
        suite = abandoned_suite()
        self.fake.suites.append(suite)
        ci = await self.captured_ci()
        self.assertTrue(ci["green"])
        self.assertFalse(ci["pending"])
        self.assertFalse(ci["blocked"])
        self.assertEqual(ci["captured_at"], "2026-10-09T02:00:00Z")
        self.assertEqual(
            ci["ignored_suites"],
            [
                {
                    **suite,
                    "reason": "queued_without_runs_past_grace_and_no_required_app",
                }
            ],
        )
        self.assertEqual(ci["suites"][1], suite)

    async def test_collection_crossing_grace_does_not_ignore_pre_grace_suite(self):
        for boundary in ("/branches/main/protection", "/rules/branches/main"):
            with self.subTest(boundary=boundary):
                self.fake = GitHub()
                self.fake.suites.append(
                    abandoned_suite(
                        created_at=(CAPTURED_AT - timedelta(minutes=10)).isoformat()
                    )
                )
                before = CAPTURED_AT - timedelta(seconds=1)
                after = CAPTURED_AT + timedelta(seconds=1)
                with patch.object(github_merge, "datetime", wraps=datetime) as clock:
                    clock.now.return_value = before

                    async def hook(req, boundary=boundary, after=after):
                        if req.url.path.endswith(boundary):
                            clock.now.return_value = after
                            self.fake.runs.append(
                                self.fake.run(
                                    11,
                                    name="deployment",
                                    app={"id": 99},
                                    check_suite={"id": 2},
                                    status="in_progress",
                                    conclusion=None,
                                )
                            )
                            self.fake.suites[-1].update(
                                status="in_progress", latest_check_runs_count=1
                            )

                    self.fake.hook = hook
                    first = (await self.evidence())["ci"]
                    self.fake.hook = None
                    second = (await self.evidence())["ci"]
                    self.assertEqual(clock.now.call_count, 2)

                # The first inventory predates both grace expiry and the new run.
                self.assertFalse(first["green"])
                self.assertTrue(first["pending"])
                self.assertEqual(first["captured_at"], "2026-10-09T01:59:59Z")
                self.assertEqual(first["ignored_suites"], [])
                self.assertEqual([run["id"] for run in first["checks"]], [10])
                self.assertEqual(ci_state(first, SHA, now=after), "pending")
                # A fresh collection must observe the new run and remain pending.
                self.assertFalse(second["green"])
                self.assertTrue(second["pending"])
                self.assertEqual(second["captured_at"], "2026-10-09T02:00:01Z")
                self.assertEqual(second["ignored_suites"], [])
                self.assertEqual([run["id"] for run in second["checks"]], [10, 11])

    async def test_suite_grace_boundary_and_invalid_creation_times_stay_pending(self):
        for created_at in (
            None,
            "unparseable",
            "2026-10-09T01:00:00",  # No timezone: age cannot be proved.
            (CAPTURED_AT + timedelta(seconds=1)).isoformat(),
            (CAPTURED_AT - timedelta(minutes=9)).isoformat(),
            (CAPTURED_AT - github_merge.ABANDONED_SUITE_GRACE).isoformat(),
        ):
            with self.subTest(created_at=created_at):
                self.fake.suites = [
                    self.fake.suites[0],
                    abandoned_suite(created_at=created_at),
                ]
                ci = await self.captured_ci()
                self.assertFalse(ci["green"])
                self.assertTrue(ci["pending"])
                self.assertEqual(ci["ignored_suites"], [])
        del self.fake.suites[-1]["created_at"]
        self.assertTrue((await self.captured_ci())["pending"])

    async def test_suite_age_uses_creation_time_and_timezone_not_update_time(self):
        self.fake.suites.append(
            abandoned_suite(
                created_at="2026-10-08T17:49:59-08:00",
                updated_at=CAPTURED_AT.isoformat(),
            )
        )
        self.assertTrue((await self.captured_ci())["green"])

    async def test_reported_suite_run_count_must_be_zero_or_absent(self):
        for count in (1, None):
            with self.subTest(count=count):
                self.fake.suites = [
                    self.fake.suites[0],
                    abandoned_suite(latest_check_runs_count=count),
                ]
                ci = await self.captured_ci()
                self.assertTrue(ci["pending"])
                self.assertEqual(ci["ignored_suites"], [])
        del self.fake.suites[-1]["latest_check_runs_count"]
        self.assertTrue((await self.captured_ci())["green"])

    async def test_suite_with_any_run_cannot_be_ignored_even_if_rerun_is_newer(self):
        self.fake.suites.append(abandoned_suite())
        self.fake.runs.insert(0, self.fake.run(9, check_suite={"id": 2}))
        ci = await self.captured_ci()
        self.assertFalse(ci["green"])
        self.assertTrue(ci["pending"])
        self.assertEqual(ci["ignored_suites"], [])

    async def test_required_app_prevents_ignoring_suite_for_classic_and_rulesets(self):
        self.fake.suites.append(abandoned_suite())
        for source in ("classic", "ruleset"):
            with self.subTest(source=source):
                self.fake.protection = {"required_status_checks": None}
                self.fake.rules = []
                if source == "classic":
                    self.fake.protection["required_status_checks"] = {
                        "checks": [{"context": "build", "app_id": 99}],
                        "contexts": [],
                    }
                else:
                    self.fake.rules = [
                        {
                            "type": "required_status_checks",
                            "parameters": {
                                "strict_required_status_checks_policy": False,
                                "required_status_checks": [
                                    {"context": "build", "integration_id": 99}
                                ],
                            },
                        }
                    ]
                ci = await self.captured_ci()
                self.assertFalse(ci["green"])
                self.assertTrue(ci["pending"])
                self.assertEqual(ci["ignored_suites"], [])

    async def test_unbound_required_context_needs_an_existing_green_run_or_status(self):
        self.fake.suites.append(abandoned_suite())
        for app in (None, -1):
            for source in ("run", "status", "missing"):
                with self.subTest(app=app, source=source):
                    self.fake.protection = {
                        "required_status_checks": {
                            "checks": [{"context": "required", "app_id": app}],
                            "contexts": ["required"],
                        }
                    }
                    self.fake.runs = [self.fake.run(10)]
                    self.fake.statuses = []
                    if source == "run":
                        self.fake.runs.append(self.fake.run(11, name="required"))
                    elif source == "status":
                        self.fake.statuses = [
                            {
                                "id": 1,
                                "context": "required",
                                "state": "success",
                                "created_at": "2026-10-09T01:00:00Z",
                            }
                        ]
                    ci = await self.captured_ci()
                    self.assertEqual(ci["green"], source != "missing")
                    self.assertEqual(len(ci["ignored_suites"]), 1)

    async def test_missing_suite_app_identity_stays_pending(self):
        for app in (None, "absent"):
            with self.subTest(app=app):
                suite = abandoned_suite(app=app)
                if app == "absent":
                    del suite["app"]
                self.fake.suites = [self.fake.suites[0], suite]
                self.assertTrue((await self.captured_ci())["pending"])

    async def test_suite_fields_used_for_ignoring_are_strictly_validated(self):
        for field, value in (
            ("app", {"id": "99"}),
            ("app", {"id": True}),
            ("app", {"id": 99, "slug": 123}),
            ("created_at", 0),
            ("latest_check_runs_count", "0"),
            ("latest_check_runs_count", False),
            ("latest_check_runs_count", -1),
        ):
            with self.subTest(field=field, value=value):
                self.fake.suites = [
                    self.fake.suites[0],
                    abandoned_suite(**{field: value}),
                ]
                with self.assertRaises(ValueError):
                    await self.captured_ci()

    async def test_non_queued_suite_and_non_null_conclusion_cannot_be_ignored(self):
        for status, conclusion, blocked in (
            ("in_progress", None, False),
            ("queued", "success", False),
            ("completed", "failure", True),
        ):
            with self.subTest(status=status, conclusion=conclusion):
                self.fake.suites = [
                    self.fake.suites[0],
                    abandoned_suite(status=status, conclusion=conclusion),
                ]
                ci = await self.captured_ci()
                self.assertFalse(ci["green"])
                self.assertEqual(ci["pending"], not blocked)
                self.assertEqual(ci["blocked"], blocked)
                self.assertEqual(ci["ignored_suites"], [])

    async def test_only_ignored_suite_is_not_green_evidence(self):
        self.fake.suites = [abandoned_suite()]
        self.fake.runs = []
        ci = await self.captured_ci()
        self.assertFalse(ci["green"])
        self.assertFalse(ci["pending"])
        self.assertEqual(len(ci["ignored_suites"]), 1)

    async def test_ignoring_suite_does_not_hide_unsuccessful_selected_runs_or_statuses(
        self,
    ):
        self.fake.suites.append(abandoned_suite())
        for status, conclusion, blocked in (
            ("queued", None, False),
            ("completed", "failure", True),
        ):
            with self.subTest(status=status, conclusion=conclusion):
                self.fake.runs = [
                    self.fake.run(11, status=status, conclusion=conclusion),
                    self.fake.run(10),
                ]
                ci = await self.captured_ci()
                self.assertFalse(ci["green"])
                self.assertEqual(ci["pending"], not blocked)
                self.assertEqual(ci["blocked"], blocked)
                self.assertEqual(len(ci["checks"]), 1)
        self.fake.runs = [self.fake.run(10)]
        self.fake.statuses = [
            {
                "id": 1,
                "context": "deploy",
                "state": "failure",
                "created_at": "2026-10-09T01:00:00Z",
            }
        ]
        ci = await self.captured_ci()
        self.assertFalse(ci["green"])
        self.assertTrue(ci["blocked"])

    async def test_no_classic_protection_with_or_without_rulesets(self):
        self.fake.errors["/branches/main/protection"] = 404
        for rules in ([], default_branch_rules()):
            with self.subTest(rules=rules):
                self.fake.rules = rules
                self.assertTrue((await self.evidence())["ci"]["green"])
        self.assertFalse(self.fake.puts)

    async def test_plan_unavailable_preserves_mainloop_checks_and_evidence(self):
        endpoints = {
            "protection": "/branches/main/protection",
            "rules": "/rules/branches/main",
        }
        message = "Upgrade to GitHub Pro or make this repository public to enable this feature."
        for missing in (("protection",), ("rules",), ("protection", "rules")):
            with self.subTest(missing=missing):
                self.fake.errors = {
                    endpoints[key]: (403, {"message": message}) for key in missing
                }
                self.fake.runs = [self.fake.run(10)]
                facts = await self.evidence()
                self.assertTrue(facts["ci"]["green"])
                self.assertEqual(
                    facts["ci"]["github_rules_unavailable_on_plan"], list(missing)
                )
                self.fake.runs = [self.fake.run(10, conclusion="failure")]
                self.assertFalse((await self.evidence())["ci"]["green"])
                self.fake.runs = []
                self.assertFalse((await self.evidence())["ci"]["green"])
        self.assertFalse(self.fake.puts)

    async def observe(self):
        async with github_merge.GitHubMergeClient(
            "owner/repo", transport=app_transport(self.fake.handle)
        ) as client:
            return await client.observation("owner/repo", 17)

    async def test_base_behind_main_is_evaluated_against_current_main(self):
        self.fake.main = MOVED
        self.fake.rules = live_mainloop_rules()
        self.fake.runs = [self.fake.run(10, name="Lint", app={"id": 15368})]
        facts = await self.evidence()
        self.assertEqual(facts["base_sha"], MOVED)
        self.assertTrue(facts["ci"]["green"])
        self.assertTrue(facts["mergeable"])

    async def test_protected_paths_unchanged_when_base_behind_main(self):
        self.fake.main = MOVED
        self.fake.files[0]["filename"] = "k8s/deploy.yaml"
        self.assertEqual(
            (await self.evidence())["protected_matches"], ["k8s/deploy.yaml"]
        )

    async def test_main_moving_during_evidence_is_stale(self):
        reads = 0

        async def hook(req):
            nonlocal reads
            if req.url.path.endswith("/pulls/17"):
                reads += 1
                if reads == 2:
                    self.fake.main = MOVED

        self.fake.hook = hook
        with self.assertRaisesRegex(PolicyError, "default branch moved"):
            await self.evidence()

    async def test_conflicting_pr_is_refused_with_next_step(self):
        for mergeable, state in ((False, "dirty"), (None, "dirty"), (False, "unknown")):
            with self.subTest(mergeable=mergeable, state=state):
                self.fake.pr.update(mergeable=mergeable, mergeable_state=state)
                with self.assertRaisesRegex(
                    PolicyError, "merge conflicts with main; merge or rebase main"
                ) as raised:
                    await self.evidence()
                self.assertEqual(raised.exception.code, "merge_conflict")

    async def test_strict_policy_behind_reports_out_of_date_branch(self):
        self.fake.main = MOVED
        self.fake.pr["mergeable_state"] = "behind"
        strict = live_mainloop_rules()
        strict[-1]["parameters"]["strict_required_status_checks_policy"] = True
        classic = {
            "required_status_checks": {"strict": True, "checks": [], "contexts": []}
        }
        for rules, protection in (
            (strict, {"required_status_checks": None}),
            ([], classic),
            ([], {"required_status_checks": None}),
        ):
            with self.subTest(rules=bool(rules), protection=protection):
                self.fake.rules, self.fake.protection = rules, protection
                with self.assertRaisesRegex(PolicyError, "out of date") as raised:
                    await self.evidence()
                self.assertEqual(raised.exception.code, "rules")
        self.fake.rules = strict
        self.fake.protection = {"required_status_checks": None}
        _, ci = await self.observe()
        self.assertEqual(ci["policy_rejections"], [github_merge.OUT_OF_DATE])
        # Up to date under strict checks remains unsupported, as before.
        self.fake.pr["mergeable_state"] = "clean"
        with self.assertRaisesRegex(
            PolicyError, "unsupported strict_required_status_checks_policy"
        ):
            await self.evidence()

    async def test_base_side_rename_into_protected_path_is_matched(self):
        # Real git (review): main renames src/build.yml to
        # .github/workflows/build.yml; the feature edits src/build.yml. The
        # three-dot PR list shows only src/build.yml, the squash changes the
        # workflow.
        self.fake.main = MOVED
        self.fake.pr.update(additions=1, deletions=1)
        self.fake.files = [
            dict(filename="src/build.yml", status="modified", additions=1, deletions=1)
        ]
        self.fake.merge_files = [
            dict(
                filename=".github/workflows/build.yml",
                status="modified",
                additions=1,
                deletions=1,
            )
        ]
        facts = await self.evidence()
        self.assertEqual(facts["protected_matches"], [".github/workflows/build.yml"])
        self.assertEqual(
            [item["filename"] for item in facts["merge_files"]],
            [".github/workflows/build.yml"],
        )
        self.assertEqual(facts["merge_commit_sha"], TEST_MERGE)

    async def test_merge_result_rename_keeps_both_sides(self):
        self.fake.merge_files = [
            dict(
                filename="src/app.py",
                status="renamed",
                previous_filename="k8s/app.py",
                additions=3,
                deletions=1,
            )
        ]
        self.assertEqual((await self.evidence())["protected_matches"], ["k8s/app.py"])
        self.fake.merge_files[0].pop("previous_filename")
        with self.assertRaises(ValueError):
            await self.evidence()

    async def test_unverifiable_merge_result_never_falls_back_to_pr_files(self):
        self.fake.main = MOVED
        for parents in ([BASE, SHA], [MOVED, "e" * 40], [SHA, MOVED], [MOVED]):
            with self.subTest(parents=parents):
                self.fake.merge_parents = parents
                with self.assertRaises(github_merge.MergeResultPending):
                    await self.evidence()
        self.fake.merge_parents = None
        self.fake.pr["merge_commit_sha"] = None
        with self.assertRaises(github_merge.MergeResultPending):
            await self.evidence()
        self.fake.pr["merge_commit_sha"] = TEST_MERGE
        self.fake.merge_files = [
            dict(filename=f"f{i}", status="modified", additions=0, deletions=0)
            for i in range(3000)
        ]
        with self.assertRaisesRegex(PolicyError, "too many") as raised:
            await self.evidence()
        self.assertEqual(raised.exception.code, "merge_result")
        self.fake.merge_files = [
            dict(filename="src/app.py", status="modified", additions=9, deletions=1)
        ]
        self.fake.merge_files += [dict(self.fake.merge_files[0])]
        with self.assertRaises(GitHubError):
            await self.evidence()

    async def test_default_head_refusal_is_precise(self):
        self.fake.pr["head"]["ref"] = "main"
        with self.assertRaisesRegex(PolicyError, "default branch is not allowed"):
            await self.evidence()
        self.assertFalse(self.fake.puts)

    async def test_malformed_plan_refusal_fails_closed(self):
        for endpoint in ("/branches/main/protection", "/rules/branches/main"):

            async def handle(req, endpoint=endpoint):
                if req.url.path.endswith(endpoint):
                    return httpx.Response(403, text="not JSON")
                return await self.fake.handle(req)

            with self.subTest(endpoint=endpoint):
                async with github_merge.GitHubMergeClient(
                    "owner/repo", transport=app_transport(handle)
                ) as client:
                    with self.assertRaises(GitHubError):
                        await client.evidence("owner/repo", 17, SHA)

    async def test_plan_refusal_after_partial_rules_fails_closed(self):
        async def handle(req):
            if "/rules/branches/" in req.url.path:
                if req.url.params.get("page") == "1":
                    return httpx.Response(200, json=[{"type": "deletion"}] * 100)
                return httpx.Response(
                    403,
                    json={
                        "message": "Upgrade to GitHub Pro or make this repository public to enable this feature."
                    },
                )
            return await self.fake.handle(req)

        async with github_merge.GitHubMergeClient(
            "owner/repo", transport=app_transport(handle)
        ) as client:
            with self.assertRaises(GitHubError):
                await client.evidence("owner/repo", 17, SHA)

    async def test_plan_refusal_on_other_endpoints_fails_closed(self):
        self.fake.errors["/pulls/17"] = (
            403,
            {
                "message": "Upgrade to GitHub Pro or make this repository public to enable this feature."
            },
        )
        with self.assertRaises(GitHubError):
            await self.evidence()

    async def test_ambiguous_plan_errors_fail_closed(self):
        message = "Upgrade to GitHub Pro or make this repository public to enable this feature."
        for endpoint in ("/branches/main/protection", "/rules/branches/main"):
            for error in (
                (403, {"message": "Resource not accessible by integration"}),
                (403, {"message": message + " extra"}),
                (403, {"documentation_url": "https://docs.github.com/rest"}),
                (403, [message]),
                (401, {"message": message}),
                (500, {"message": message}),
            ):
                with self.subTest(endpoint=endpoint, error=error):
                    self.fake.errors = {endpoint: error}
                    with self.assertRaises(GitHubError):
                        await self.evidence()

    async def test_available_rules_still_enforced_when_other_endpoint_unavailable(self):
        message = "Upgrade to GitHub Pro or make this repository public to enable this feature."
        self.fake.errors = {"/branches/main/protection": (403, {"message": message})}
        self.fake.rules = [{"type": "required_signatures"}]
        with self.assertRaises(PolicyError):
            await self.evidence()
        self.fake.errors = {"/rules/branches/main": (403, {"message": message})}
        self.fake.protection = {
            "required_status_checks": {"checks": [], "contexts": ["absent"]}
        }
        self.assertFalse((await self.evidence())["ci"]["green"])

    async def test_zero_approval_and_linear_history(self):
        self.fake.errors["/branches/main/protection"] = 404
        self.fake.rules = default_branch_rules()
        self.fake.rules[-1]["parameters"].update(allowed_merge_methods=["squash"])
        self.fake.rules.append({"type": "required_linear_history"})
        self.assertTrue((await self.evidence())["ci"]["green"])

    async def test_live_mainloop_ruleset_is_supported(self):
        self.fake.errors["/branches/main/protection"] = 404
        self.fake.rules = live_mainloop_rules()
        # The ruleset's required check binds to its app, so it must be present.
        self.assertFalse((await self.evidence())["ci"]["green"])
        self.fake.runs.append(self.fake.run(11, name="Lint", app={"id": 15368}))
        ci = (await self.evidence())["ci"]
        self.assertTrue(ci["green"])
        self.assertEqual(ci["required"], [("Lint", 15368)])
        self.assertNotIn("policy_rejections", ci)
        self.assertFalse(self.fake.puts)

    async def test_unknown_pr_parameters_are_refused_whatever_their_value(self):
        # Synthetic future fields: even false/null/[] may mean a restriction
        # (an inverse flag, a default policy, an empty allowlist).
        self.fake.errors["/branches/main/protection"] = 404
        for field, value in (
            ("allow_unreviewed_merge", False),
            ("approval_policy", None),
            ("allowed_merge_strategies", []),
            ("future_requirement", True),
            ("future_requirement", 0),
            ("future_requirement", "on"),
            ("future_requirement", {"id": 1}),
            ("", True),
            ("", False),
        ):
            with self.subTest(field=field, value=value):
                self.fake.rules = live_mainloop_rules()
                self.fake.rules[2]["parameters"][field] = value
                with self.assertRaises(PolicyError) as refused:
                    await self.evidence()
                self.assertEqual(
                    refused.exception.message,
                    "unsupported pull_request requirement: unknown_pull_request_parameter",
                )
        self.assertFalse(self.fake.puts)

    async def test_allowlisted_pr_parameters_only_with_inactive_values(self):
        self.fake.errors["/branches/main/protection"] = 404
        self.fake.runs.append(self.fake.run(11, name="Lint", app={"id": 15368}))
        for value in (True, False):
            with self.subTest(value=value):
                self.fake.rules = live_mainloop_rules()
                parameters = self.fake.rules[2]["parameters"]
                parameters["require_extra_approval_for_unattributed_changes"] = value
                self.assertTrue((await self.evidence())["ci"]["green"])
        for field, value in (
            ("required_reviewers", None),
            ("required_reviewers", [{}]),
            ("require_extra_approval_for_unattributed_changes", None),
        ):
            with self.subTest(field=field, value=value):
                self.fake.rules = live_mainloop_rules()
                self.fake.rules[2]["parameters"][field] = value
                with self.assertRaisesRegex(PolicyError, "unsupported pull_request"):
                    await self.evidence()

    async def test_unknown_rule_type_is_not_copied_into_reason(self):
        self.fake.errors["/branches/main/protection"] = 404
        for rule_type, reason in (
            ("SYNTHETIC_OPAQUE_MARKER", "unknown_branch_rule"),
            ("required_signatures", "required_signatures"),
        ):
            with self.subTest(rule_type=rule_type):
                self.fake.rules = [{"type": rule_type}]
                with self.assertRaises(PolicyError) as refused:
                    await self.evidence()
                self.assertEqual(
                    refused.exception.message,
                    f"unsupported active branch rule: {reason}",
                )

    async def test_required_reviewers_and_extra_approval_with_nonzero_count(self):
        self.fake.errors["/branches/main/protection"] = 404
        reviewer = {
            "minimum_approvals": 1,
            "file_patterns": ["*"],
            "reviewer": {"id": 1, "type": "Team"},
        }
        for changes, reason in (
            ({"required_reviewers": [reviewer]}, "required_reviewers"),
            (
                {
                    "required_approving_review_count": 1,
                    "require_extra_approval_for_unattributed_changes": True,
                },
                "required_approving_review_count",
            ),
        ):
            with self.subTest(reason=reason):
                self.fake.rules = live_mainloop_rules()
                self.fake.rules[2]["parameters"].update(changes)
                with self.assertRaisesRegex(
                    PolicyError, f"unsupported pull_request requirement: {reason}"
                ):
                    await self.evidence()
        self.assertFalse(self.fake.puts)

    async def test_unsupported_pr_requirements_fail_closed(self):
        for field, value in (
            ("required_approving_review_count", 1),
            ("required_approving_review_count", "0"),
            ("dismiss_stale_reviews_on_push", True),
            ("require_code_owner_review", True),
            ("require_last_push_approval", True),
            ("required_review_thread_resolution", True),
            ("allowed_merge_methods", ["rebase"]),
            ("required_reviewers", [{"reviewer": {"id": 1, "type": "Team"}}]),
            ("required_reviewers", "team"),
            ("require_extra_approval_for_unattributed_changes", "true"),
            ("future_requirement", True),
        ):
            with self.subTest(field=field, value=value):
                self.fake.rules = default_branch_rules()
                self.fake.rules[-1]["parameters"][field] = value
                with self.assertRaisesRegex(PolicyError, "unsupported pull_request"):
                    await self.evidence()
        self.fake.rules = [{"type": "pull_request"}]
        with self.assertRaisesRegex(PolicyError, "unsupported pull_request"):
            await self.evidence()

    async def test_only_protection_404_is_supported(self):
        for code in (401, 403, 429, 500):
            self.fake.errors["/branches/main/protection"] = code
            with self.assertRaises(GitHubError):
                await self.evidence()
        self.fake.errors["/branches/main/protection"] = 404
        for path in ("/rules/branches/main", "/branches/main", "/pulls/17", ""):
            self.fake.errors[path] = 404
            with self.assertRaises(GitHubError):
                await self.evidence()
            del self.fake.errors[path]
        self.fake.errors.clear()
        self.fake.protection = None
        with self.assertRaises((GitHubError, ValueError)):
            await self.evidence()

    async def test_pr_rule_required_fields_cannot_be_omitted(self):
        for field in default_branch_rules()[-1]["parameters"]:
            with self.subTest(field=field):
                self.fake.rules = default_branch_rules()
                del self.fake.rules[-1]["parameters"][field]
                with self.assertRaisesRegex(
                    PolicyError, "unsupported pull_request parameters"
                ):
                    await self.evidence()

    async def test_ruleset_strict_checks_require_explicit_false(self):
        baseline = {
            "required_status_checks": [{"context": "build", "integration_id": 4}],
            "strict_required_status_checks_policy": False,
        }
        for value in (False, True, None, "false", "missing"):
            with self.subTest(value=value):
                parameters = copy.deepcopy(baseline)
                if value == "missing":
                    del parameters["strict_required_status_checks_policy"]
                else:
                    parameters["strict_required_status_checks_policy"] = value
                self.fake.rules = [
                    {"type": "required_status_checks", "parameters": parameters}
                ]
                if value is False:
                    self.assertTrue((await self.evidence())["ci"]["green"])
                else:
                    with self.assertRaises(PolicyError):
                        await self.evidence()

    async def test_classic_unsupported_and_unknown_protection_fields(self):
        for field in (
            "required_conversation_resolution",
            "required_signatures",
            "lock_branch",
            "block_creations",
        ):
            for value in (True, False):
                with self.subTest(field=field, value=value):
                    self.fake.protection = {
                        "required_status_checks": None,
                        field: {"enabled": value},
                    }
                    if value:
                        with self.assertRaisesRegex(PolicyError, field):
                            await self.evidence()
                    else:
                        self.assertTrue((await self.evidence())["ci"]["green"])
        for field, value in (
            ("future_requirement", {"enabled": True}),
            ("required_signatures", {}),
            ("required_signatures", None),
            ("required_signatures", {"enabled": "false"}),
            ("required_signatures", {"enabled": False, "future_requirement": True}),
            ("required_pull_request_reviews", {}),
            ("restrictions", {}),
        ):
            with self.subTest(field=field, value=value):
                self.fake.protection = {"required_status_checks": None, field: value}
                with self.assertRaisesRegex(PolicyError, "unsupported classic"):
                    await self.evidence()

    async def test_classic_compatible_flags(self):
        self.fake.protection.update(
            {
                "url": "https://api.github.com/repos/owner/repo/branches/main/protection",
                "enabled": True,
                "enforce_admins": {
                    "enabled": True,
                    "url": "https://api.github.com/fixture",
                },
                "required_linear_history": {"enabled": True},
                "allow_force_pushes": {"enabled": False},
                "allow_deletions": {"enabled": False},
                "allow_fork_syncing": {"enabled": True},
            }
        )
        self.assertTrue((await self.evidence())["ci"]["green"])

    async def test_classic_check_app_id_required_but_nullable(self):
        for app in (None, 4, 99, "missing"):
            with self.subTest(app=app):
                check = {"context": "build", "app_id": app}
                if app == "missing":
                    del check["app_id"]
                self.fake.protection = {
                    "required_status_checks": {
                        "checks": [check],
                        "contexts": ["build"],
                        "strict": False,
                    }
                }
                if app == "missing":
                    with self.assertRaisesRegex(PolicyError, "unsupported classic"):
                        await self.evidence()
                else:
                    self.assertEqual((await self.evidence())["ci"]["green"], app != 99)
        self.fake.protection["required_status_checks"]["checks"] = [
            {"context": "build", "app_id": 4}
        ]
        self.fake.protection["required_status_checks"]["strict"] = True
        with self.assertRaisesRegex(PolicyError, "strict status checks"):
            await self.evidence()
        self.fake.protection["required_status_checks"]["strict"] = False
        del self.fake.protection["required_status_checks"]["contexts"]
        with self.assertRaisesRegex(PolicyError, "unsupported classic"):
            await self.evidence()

    async def test_ruleset_checks_and_unknown_rule_without_classic_protection(self):
        self.fake.errors["/branches/main/protection"] = 404
        self.fake.rules = default_branch_rules() + [
            {
                "type": "required_status_checks",
                "parameters": {
                    "strict_required_status_checks_policy": False,
                    "required_status_checks": [
                        {"context": "build", "integration_id": 4}
                    ],
                },
            }
        ]
        self.assertTrue((await self.evidence())["ci"]["green"])
        self.fake.rules[-1]["parameters"]["required_status_checks"][0][
            "integration_id"
        ] = 99
        self.assertFalse((await self.evidence())["ci"]["green"])
        self.fake.rules.append({"type": "future_rule", "parameters": {"unknown": True}})
        with self.assertRaisesRegex(PolicyError, "unsupported active branch rule"):
            await self.evidence()

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
                    "strict_required_status_checks_policy": False,
                    "required_status_checks": [
                        {"context": "build", "integration_id": 99}
                    ],
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
        with self.assertRaises(github_merge.MergeResultPending):
            await self.evidence()

    async def test_complete_files_protected_renames_and_deletions(self):
        self.fake.files = [
            {
                "filename": "src/a",
                "status": "renamed",
                "previous_filename": "k8s/a",
                "additions": 3,
                "deletions": 1,
            }
        ]
        self.assertEqual((await self.evidence())["protected_matches"], ["k8s/a"])
        self.fake.files = [
            {
                "filename": "migrations/001.sql",
                "status": "removed",
                "additions": 3,
                "deletions": 1,
            }
        ]
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
        with self.assertRaises((GitHubError, ValueError, PolicyError)):
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
        self.fake.protection["required_status_checks"]["checks"][0]["context"] = "build"
        self.assertTrue((await self.evidence())["ci"]["green"])
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


class MergeFixture(PostgresTestCase):
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
        self.gateway = Gateway()
        self.gateway.add(f"runtime-{self.sid}")
        self.hitl_service = HITLObserver(
            self.gateway,
            gateway=f"https://gateway/{self.user}",
            owner=self.user,
            creator="gateway-owner",
        )
        patcher = patch.object(
            merge_authorization, "observer", return_value=self.hitl_service
        )
        patcher.start()
        self.addCleanup(patcher.stop)
        configurations = [
            {
                "template_name": "merge-template",
                "provider": provider,
                "compiled_alias": "mainloop-merge-approval",
                "endpoint": "http://mainloop-mcp.mainloop.svc.cluster.local/mcp/merge-approval",
                "tool": "merge_pull_request_with_approval",
                "require_approval": True,
                "operation": "mainloop.merge_pull_request_with_approval.v1",
            }
            for provider in ("claude", "codex")
        ]
        patcher = patch.dict(
            os.environ, MAINLOOP_MERGE_CONFIGURATIONS=json.dumps(configurations)
        )
        patcher.start()
        self.addCleanup(patcher.stop)
        cls = github_merge.GitHubMergeClient
        for p in (
            patch.dict(os.environ, MAINLOOP_MERGE_TOOLS_ENABLED="true"),
            app_settings(),
            patch.object(
                merge,
                "GitHubMergeClient",
                lambda repository: cls(
                    repository, transport=app_transport(self.fake.handle)
                ),
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
        gateway = self.gateway
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
            "template_name": "merge-template",
            "provider": provider,
            "compiled_alias": "mainloop-merge-approval",
            "endpoint": "http://mainloop-mcp.mainloop.svc.cluster.local/mcp/merge-approval",
            "tool": "merge_pull_request_with_approval",
            "require_approval": True,
            "operation": "mainloop.merge_pull_request_with_approval.v1",
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

    async def decide(self, observer, projection, approved=True, reviewed_context=None):
        if approved and reviewed_context is None:
            tools = (
                projection.payload.nested.tools
                if projection.payload.nested
                else projection.payload.tools
            )
            merge_tool = next(
                tool
                for tool in tools
                if tool.name.endswith("merge_pull_request_with_approval")
            )
            proposal_id = merge_tool.args["proposal_id"]
            reviewed_context = {
                merge_tool.id: await self.pool.fetchval(
                    "SELECT summary_digest FROM merge_proposals WHERE id=$1",
                    proposal_id,
                )
            }
        reviewed_context = reviewed_context or {}
        return await hitl_continuation.submit(
            self.user,
            projection.id,
            "decision-1",
            ToolApprovalResponse(
                type="tool_approval_response",
                approvals=(ToolApproval(id="call", approved=approved),),
                reviewed_context=reviewed_context,
            ),
            service=observer,
        )


class MergeTests(MergeFixture):
    async def test_workspace_merge_access_uses_the_same_persisted_scope(self):
        sid, _ = await self.session("active")
        await self.pool.execute(
            """UPDATE sessions SET project_id=$2,repo_url=$3,branch_name=$4
               WHERE id=$1""",
            sid,
            self.project.id,
            "https://github.com/owner/repo",
            "feature",
        )
        await self.pool.execute(
            "INSERT INTO workspaces(session_id,repo,branch) VALUES($1,$2,$3)",
            sid,
            "https://github.com/owner/repo",
            "feature",
        )
        await native_sessions.create_binding(sid, "claude", mcp_grant_kind="workspace")
        await self.pool.execute(
            "UPDATE native_bindings SET kagent_session_id=$2 WHERE session_id=$1",
            sid,
            f"runtime-{sid}",
        )
        service = AgentService(PgStore())
        ctx = await service.authenticate(token_for(sid))
        arguments = {
            "project_id": self.project.id,
            "pr_number": 17,
            "expected_sha": SHA,
            "request_id": "workspace-prepare",
        }
        exposed = await invoke(service, ctx, "prepare_pull_request_merge", arguments)
        self.assertFalse(exposed.isError, exposed.content)
        self.assertEqual(exposed.structuredContent["state"], "prepared")

        await self.pool.execute(
            "UPDATE workspaces SET branch='feature/other' WHERE session_id=$1", sid
        )
        await self.pool.execute(
            "UPDATE sessions SET branch_name='feature/other' WHERE id=$1", sid
        )
        denied = await invoke(
            service,
            ctx,
            "prepare_pull_request_merge",
            {**arguments, "request_id": "workspace-wrong-head"},
        )
        self.assertTrue(denied.isError)
        self.assertIn("branch does not match this workspace", denied.content[0].text)
        self.assertEqual(len(self.fake.puts), 0)

        wrong_project = await db.get_or_create_project(
            self.user, parse_github_repo("owner/other")
        )
        denied_project = await invoke(
            service,
            ctx,
            "prepare_pull_request_merge",
            {
                **arguments,
                "project_id": wrong_project.id,
                "request_id": "workspace-wrong-project",
            },
        )
        self.assertTrue(denied_project.isError)

        await self.pool.execute(
            "UPDATE workspaces SET repo='https://github.com/fork/repo' WHERE session_id=$1",
            sid,
        )
        denied_repo = await invoke(
            service,
            ctx,
            "prepare_pull_request_merge",
            {**arguments, "request_id": "workspace-wrong-repo"},
        )
        self.assertTrue(denied_repo.isError)

        await self.pool.execute(
            "UPDATE workspaces SET repo='https://github.com/owner/repo',branch='main' WHERE session_id=$1",
            sid,
        )
        await self.pool.execute(
            "UPDATE sessions SET branch_name='main' WHERE id=$1", sid
        )
        self.fake.pr["head"]["ref"] = "main"
        denied_default = await invoke(
            service,
            ctx,
            "prepare_pull_request_merge",
            {**arguments, "request_id": "workspace-default-head"},
        )
        self.assertTrue(denied_default.isError)
        self.assertIn("default branch is not allowed", denied_default.content[0].text)
        self.assertEqual(len(self.fake.puts), 0)

    async def test_prepare_records_plan_unavailability(self):
        message = "Upgrade to GitHub Pro or make this repository public to enable this feature."
        self.fake.errors = {
            "/branches/main/protection": (403, {"message": message}),
            "/rules/branches/main": (403, {"message": message}),
        }
        proposal = await self.prepare()
        self.assertEqual(
            proposal["ci"]["github_rules_unavailable_on_plan"], ["protection", "rules"]
        )
        self.assertTrue(proposal["ci"]["green"])
        self.assertFalse(self.fake.puts)

    async def test_prepare_captures_immutable_bounded_presentation(self):
        proposal = await self.prepare()
        self.assertEqual(proposal["summary"]["title"], self.fake.pr["title"])
        self.assertEqual(proposal["summary"]["availability"], "ready")
        self.assertEqual(len(proposal["summary_digest"]), 64)
        self.assertNotIn("files", proposal)
        self.assertNotIn("description", proposal)
        stored = await self.pool.fetchrow(
            "SELECT presentation,summary_digest FROM merge_proposals WHERE id=$1",
            proposal["proposal_id"],
        )
        self.assertEqual(merge.decode(stored["presentation"]), proposal["summary"])
        self.assertEqual(stored["summary_digest"], proposal["summary_digest"])

    async def test_positive_owner_decision_requires_exact_summary_digest(self):
        proposal = await self.prepare()
        observer, projection, gateway = await self.pause(proposal)
        for reviewed_context, message in (
            ({}, "Approve using the current merge summary"),
            ({"call": "0" * 64}, "changed or is unavailable"),
        ):
            with self.assertRaisesRegex(ValueError, message):
                await self.decide(
                    observer,
                    projection,
                    reviewed_context=reviewed_context,
                )
            self.assertEqual(
                await self.pool.fetchval(
                    "SELECT count(*) FROM native_hitl_responses WHERE owner_id=$1",
                    self.user,
                ),
                0,
            )
        result = await self.decide(observer, projection)
        self.assertEqual(result["transport_state"], "accepted")
        native_payload = (
            gateway.tasks[projection.outer.task_id].history[-1].metadata[HITL_EXTENSION]
        )
        self.assertNotIn("reviewed_context", native_payload)

    async def test_incomplete_summary_blocks_approval_but_allows_rejection(self):
        self.fake.pr["body"] = ""
        proposal = await self.prepare()
        self.assertEqual(proposal["summary"]["availability"], "unavailable")
        observer, projection, _ = await self.pause(proposal)
        with self.assertRaisesRegex(ValueError, "changed or is unavailable"):
            await self.decide(observer, projection)
        result = await self.decide(observer, projection, approved=False)
        self.assertFalse(result["response"]["response"]["approvals"][0]["approved"])

    async def test_ruleset_only_pinned_squash_through_pr_api(self):
        self.fake.errors["/branches/main/protection"] = 404
        self.fake.rules = default_branch_rules()
        result = await merge.auto_merge(self.binding, self.args)
        self.assertEqual(result["state"], "merged")
        writes = [r for r in self.fake.calls if r.method != "GET"]
        self.assertEqual(len(writes), 1)
        self.assertEqual(writes[0].method, "PUT")
        self.assertEqual(writes[0].url.path, "/repos/owner/repo/pulls/17/merge")
        self.assertEqual(
            json.loads(writes[0].content), {"sha": SHA, "merge_method": "squash"}
        )

    async def test_ignored_suite_evidence_is_persisted_and_pinned_merge_can_succeed(
        self,
    ):
        self.fake.suites.append(
            abandoned_suite(
                created_at=(
                    datetime.now(timezone.utc) - timedelta(minutes=11)
                ).isoformat()
            )
        )
        proposal = await self.prepare()
        self.assertEqual(proposal["summary"]["ci"]["ignored_suite_count"], 1)
        self.assertEqual(proposal["summary"]["ci"]["pending_count"], 0)
        saved = json.loads(
            await self.pool.fetchval(
                "SELECT facts FROM merge_proposals WHERE id=$1", proposal["proposal_id"]
            )
        )
        self.assertEqual(
            saved["ci"]["ignored_suites"], proposal["ci"]["ignored_suites"]
        )
        self.assertEqual((await self.execute(proposal))["state"], "merged")
        self.assertEqual(len(self.fake.puts), 1)

    async def test_suite_can_age_past_grace_during_same_merge_evaluation(self):
        captured_at = datetime.now(timezone.utc)
        self.fake.suites.append(
            abandoned_suite(
                created_at=(captured_at - timedelta(minutes=11)).isoformat()
            )
        )
        with patch.object(github_merge, "datetime", wraps=datetime) as clock:
            clock.now.return_value = captured_at - timedelta(minutes=2)
            proposal = await self.prepare()
            self.assertEqual(proposal["ci"]["ignored_suites"], [])
            self.assertEqual((await self.execute(proposal))["state"], "evaluating")
        self.assertEqual(len(self.fake.puts), 0)
        self.assertEqual((await self.execute(proposal))["state"], "merged")
        self.assertEqual(len(self.fake.puts), 1)

    async def test_ignored_suite_is_preserved_in_uncertain_intent(self):
        self.fake.suites.append(
            abandoned_suite(
                created_at=(
                    datetime.now(timezone.utc) - timedelta(minutes=11)
                ).isoformat()
            )
        )
        proposal = await self.prepare()
        self.fake.lose = True
        self.assertEqual((await self.execute(proposal))["state"], "uncertain")
        intent = json.loads(
            await self.pool.fetchval(
                "SELECT result FROM merge_requests WHERE owner_id=$1", self.user
            )
        )
        self.assertEqual(
            intent["claim_evidence"]["ci"]["ignored_suites"],
            proposal["ci"]["ignored_suites"],
        )
        self.assertEqual(intent["claim_evidence"]["ci_state"], "success")

    async def test_fresh_run_prevents_previously_ignored_suite_from_authorizing_merge(
        self,
    ):
        self.fake.suites.append(
            abandoned_suite(
                created_at=(
                    datetime.now(timezone.utc) - timedelta(minutes=11)
                ).isoformat()
            )
        )
        proposal = await self.prepare()
        self.assertTrue(proposal["ci"]["green"])
        self.fake.runs.append(self.fake.run(11, check_suite={"id": 2}))
        self.assertEqual((await self.execute(proposal))["state"], "evaluating")
        self.assertEqual(len(self.fake.puts), 0)

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
        await self.pool.execute(
            "UPDATE native_bindings SET kind='codex' WHERE session_id=$1", self.sid
        )
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
        with self.assertRaisesRegex(ValueError, "context is unavailable"):
            await self.decide(observer, projection, reviewed_context={})
        with self.assertRaises(PolicyError):
            await self.execute(p, approved=True)
        self.assertEqual(len(self.fake.puts), 0)

    async def test_mapping_changed_while_card_open_blocks_approval_but_allows_rejection(
        self,
    ):
        self.fake.files[0]["filename"] = "k8s/protected.yaml"
        p = await self.prepare()
        observer, projection, _ = await self.pause(p)
        changed = {
            "template_name": "merge-template",
            "provider": "claude",
            "compiled_alias": "mainloop-merge-approval",
            "endpoint": "http://replacement.mainloop.svc/mcp/merge-approval",
            "tool": "merge_pull_request_with_approval",
            "require_approval": True,
            "operation": "mainloop.merge_pull_request_with_approval.v1",
        }
        with patch.dict(
            os.environ, MAINLOOP_MERGE_CONFIGURATIONS=json.dumps([changed])
        ):
            async with db.connection() as conn:
                current = await hitl_continuation.view(
                    conn, projection, service=observer
                )
            self.assertTrue(current["merge_enrichment"][0]["mapping_unavailable"])
            with self.assertRaisesRegex(ValueError, "changed or is unavailable"):
                await self.decide(observer, projection)
            rejected = await self.decide(
                observer, projection, approved=False, reviewed_context={}
            )
            self.assertEqual(rejected["transport_state"], "accepted")
        self.assertTrue(
            await self.pool.fetchval(
                "SELECT 1 FROM native_hitl_response_members m JOIN merge_proposals p ON p.id=m.call_snapshot->'merge_key'->>'proposal_id' WHERE p.id=$1 AND m.call_snapshot->>'approved'='false'",
                p["proposal_id"],
            )
        )

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
        self.fake.main = "d" * 40
        with self.assertRaises(ValueError):
            await self.decide(observer, projection)
        self.fake.main = BASE
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

    async def test_base_behind_main_prepares_and_merges_against_current_main(self):
        self.fake.main = MOVED
        p = await self.prepare()
        self.assertEqual(p["base_sha"], MOVED)
        self.assertEqual((await self.execute(p))["state"], "merged")
        self.assertEqual(len(self.fake.puts), 1)

    async def test_main_moving_after_prepare_refuses_then_reprepare_merges(self):
        p = await self.prepare()
        self.fake.main = MOVED
        result = await self.execute(p)
        self.assertEqual(result["state"], "blocked")
        self.assertIn("default branch moved since preparation", result["text"])
        self.assertEqual(len(self.fake.puts), 0)
        newer = await self.prepare(request_id="after-main-moved")
        self.assertEqual(newer["base_sha"], MOVED)
        self.assertEqual(
            (await self.execute(newer, request="invoke-2"))["state"], "merged"
        )
        self.assertEqual(len(self.fake.puts), 1)

    async def test_base_side_rename_routes_to_approval(self):
        self.fake.main = MOVED
        self.fake.pr.update(additions=1, deletions=1)
        self.fake.files = [
            dict(filename="src/build.yml", status="modified", additions=1, deletions=1)
        ]
        self.fake.merge_files = [
            dict(
                filename=".github/workflows/build.yml",
                status="modified",
                additions=1,
                deletions=1,
            )
        ]
        p = await self.prepare()
        self.assertEqual(p["route"], "approval")
        self.assertEqual(p["protected_matches"], [".github/workflows/build.yml"])
        with self.assertRaisesRegex(PolicyError, "protected merge tool"):
            await self.execute(p)
        auto = await merge.auto_merge(self.binding, {**self.args, "request_id": "auto"})
        self.assertEqual(auto["state"], "approval_required")
        self.assertEqual(len(self.fake.puts), 0)

    async def test_missing_or_mismatched_test_merge_never_auto_merges(self):
        self.fake.main = MOVED
        self.fake.merge_parents = [BASE, SHA]
        with self.assertRaisesRegex(PolicyError, "does not match"):
            await self.prepare()
        self.fake.merge_parents = None
        p = await self.prepare(request_id="ready")
        # GitHub recomputes the test merge: execution waits, sends nothing.
        self.fake.merge_parents = [BASE, SHA]
        self.assertEqual((await self.execute(p))["state"], "evaluating")
        self.fake.merge_parents = None
        self.fake.pr["merge_commit_sha"] = None
        self.assertEqual((await self.execute(p))["state"], "evaluating")
        self.assertEqual(len(self.fake.puts), 0)
        # A recreated test merge commit with the same parents and inventory
        # does not invalidate the proposal.
        self.fake.pr["merge_commit_sha"] = "8" * 40
        self.assertEqual((await self.execute(p))["state"], "merged")
        self.assertEqual(len(self.fake.puts), 1)

    async def test_conflict_refuses_prepare_and_blocks_execution(self):
        p = await self.prepare()
        self.fake.pr.update(mergeable=False, mergeable_state="dirty")
        result = await self.execute(p)
        self.assertEqual(result["state"], "blocked")
        self.assertIn("merge conflicts with main", result["text"])
        with self.assertRaisesRegex(PolicyError, "merge conflicts with main"):
            await self.prepare(request_id="conflicting")
        self.assertEqual(len(self.fake.puts), 0)

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
            with self.assertRaisesRegex(ValueError, "context is unavailable"):
                await self.decide(observer, projection, reviewed_context={})
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

        def crash_on_dispatch(repository):
            nonlocal calls
            calls += 1
            if calls == 2:
                raise asyncio.CancelledError
            return factory(repository)

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
                # Exercise a fresh server pass; public retries only read pending state.
                result = await merge.execute_once(
                    self.binding,
                    {"proposal_id": p["proposal_id"], "request_id": "invoke-1"},
                    approved=approved,
                    reevaluate=True,
                )
                self.assertEqual(result["state"], "evaluating")
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
        if approved:
            await merge.reconcile_approved_merges()
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
