"""Offline exact-head/read-projection tests. No database or native provider."""

import copy
import unittest
from datetime import UTC, datetime, timedelta

from mainloop.runtime.policy import PolicyError
from mainloop.services.github_creation import GitHubError
from mainloop.services.github_merge import GitHubMergeClient, MergePR
from mainloop.tasks.attention import owns_leaf
from mainloop.tasks.projection import POLICY_BLOCKED, ci_state, observed
from tests.runtime.github_app_fake import app_settings, app_transport
from tests.runtime.test_merge import (
    SHA,
    GitHub,
    default_branch_rules,
    live_mainloop_rules,
)

from models.hitl import LeafIdentity
from models.task import TaskProjection


class PublicationFactsTests(unittest.TestCase):
    def setUp(self):
        self.now = datetime.now(UTC)
        self.ci = {
            "head_sha": SHA,
            "captured_at": self.now.isoformat(),
            "complete": True,
            "green": True,
            "pending": False,
            "blocked": False,
        }

    def test_exact_head_ci_states(self):
        cases = [
            (self.ci, "success"),
            ({**self.ci, "green": False, "blocked": True}, "failure"),
            ({**self.ci, "green": False, "pending": True}, "pending"),
            ({**self.ci, "green": False}, "unknown"),
            ({**self.ci, "complete": False}, "unknown"),
            ({**self.ci, "head_sha": "b" * 40}, "unknown"),
            (
                {
                    **self.ci,
                    "captured_at": (self.now - timedelta(minutes=6)).isoformat(),
                },
                "unknown",
            ),
            (
                {
                    **self.ci,
                    "captured_at": (self.now + timedelta(seconds=1)).isoformat(),
                },
                "unknown",
            ),
            (
                {**self.ci, "captured_at": self.now.replace(tzinfo=None).isoformat()},
                "unknown",
            ),
            ({**self.ci, "captured_at": None}, "unknown"),
            (None, "unknown"),
            ({}, "unknown"),
        ]
        for evidence, expected in cases:
            with self.subTest(evidence=evidence):
                self.assertEqual(ci_state(evidence, SHA, now=self.now), expected)

    def project(self, data, previous=None, ci=None):
        return observed(
            previous or TaskProjection(),
            repository="owner/repo",
            branch="feature",
            number=17,
            pr=MergePR.model_validate(data),
            ci=ci or self.ci,
            observed_at=self.now,
        )

    def test_head_change_invalidates_merge_but_preserves_native_attention(self):
        previous = TaskProjection(
            pr_head_sha=SHA,
            merge_proposal_id="proposal",
            merge_state="prepared",
            pending_approval_ids=("card",),
        )
        data = copy.deepcopy(GitHub().pr)
        data["head"]["sha"] = "b" * 40
        value = self.project(data, previous)
        self.assertEqual(value.ci_state, "unknown")
        self.assertIsNone(value.merge_proposal_id)
        self.assertEqual(value.pending_approval_ids, ("card",))

    def test_closed_unmerged_is_separate_and_observation_is_not_settlement(self):
        data = copy.deepcopy(GitHub().pr)
        data.update(state="closed", merged=False)
        value = self.project(data)
        self.assertEqual(value.pr_state, "closed")
        self.assertIsNone(value.merge_state)
        data.update(merged=True, merge_commit_sha="c" * 40)
        value = self.project(data)
        self.assertEqual(value.pr_state, "merged")
        self.assertEqual(value.merge_state, "merged")

    def test_merged_observation_overrides_stale_proposal_and_ci_readiness(self):
        data = copy.deepcopy(GitHub().pr)
        data.update(state="closed", merged=True, merge_commit_sha="c" * 40)
        for state in (None, "prepared", "evaluating", "uncertain", POLICY_BLOCKED):
            with self.subTest(state=state):
                previous = TaskProjection(pr_head_sha=SHA, merge_state=state)
                value = self.project(data, previous, ci={"head_sha": SHA})
                self.assertEqual(
                    (value.pr_state, value.merge_state), ("merged", "merged")
                )
                self.assertEqual(value.ci_state, "unknown")

    def test_sibling_or_parent_branch_is_not_publication_evidence(self):
        data = copy.deepcopy(GitHub().pr)
        data["head"]["ref"] = "other-task"
        with self.assertRaisesRegex(PolicyError, "PR observation"):
            self.project(data)

    def test_rollup_does_not_transfer_receipt_ownership(self):
        leaf = LeafIdentity(
            owner_id="owner",
            binding_id="child",
            gateway="gateway",
            endpoint="/agents/ns/child",
            runtime_session_id="runtime-child",
            context_id="context",
            task_id="native-task",
            pending_request_id="pending",
            request_hash="a" * 64,
        )
        for owner, binding, runtime, expected in (
            ("owner", "child", "runtime-child", True),
            ("owner", "parent", "runtime-child", False),
            ("owner", "sibling", "runtime-sibling", False),
            ("other-owner", "child", "runtime-child", False),
        ):
            self.assertEqual(
                owns_leaf(
                    leaf, owner_id=owner, binding_id=binding, runtime_session_id=runtime
                ),
                expected,
            )


class GitHubObservationTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.fake = GitHub()
        self.patcher = app_settings()
        self.patcher.start()
        self.addCleanup(self.patcher.stop)

    async def observe(self):
        async with GitHubMergeClient(
            "owner/repo", transport=app_transport(self.fake.handle)
        ) as client:
            return await client.observation("owner/repo", 17)

    async def test_observation_has_fresh_exact_ci_and_never_writes(self):
        pr, ci = await self.observe()
        self.assertEqual(ci_state(ci, pr.head.sha), "success")
        self.assertFalse(self.fake.puts)

    async def test_unavailable_checks_do_not_infer_success(self):
        self.fake.errors[f"/commits/{SHA}/check-runs"] = 503
        pr, ci = await self.observe()
        self.assertIsNone(ci)
        self.assertEqual(ci_state(ci, pr.head.sha), "unknown")
        self.assertFalse(self.fake.puts)

    def project(self, pr, ci, previous=None):
        return observed(
            previous or TaskProjection(),
            repository="owner/repo",
            branch="feature",
            number=17,
            pr=pr,
            ci=ci,
            observed_at=datetime.now(UTC),
        )

    async def test_policy_rejected_rule_keeps_pr_and_exact_head_ci(self):
        self.fake.errors["/branches/main/protection"] = 404
        self.fake.rules = live_mainloop_rules()
        self.fake.rules[2]["parameters"]["required_reviewers"] = [
            {"reviewer": {"id": 1, "type": "Team"}}
        ]
        self.fake.runs.append(self.fake.run(11, name="Lint", app={"id": 15368}))
        with self.assertLogs("mainloop.services.github_merge", "INFO") as logs:
            pr, ci = await self.observe()
        self.assertIn("required_reviewers", logs.output[0])
        # An understood review requirement does not hide any required check.
        self.assertFalse(ci["required_checks_incomplete"])
        self.assertEqual(
            ci["policy_rejections"],
            ["unsupported pull_request requirement: required_reviewers"],
        )
        value = self.project(pr, ci, TaskProjection(merge_state="prepared"))
        self.assertEqual(
            (value.pr_state, value.pr_head_sha, value.ci_state, value.ci_head_sha),
            ("open", SHA, "success", SHA),
        )
        self.assertEqual(value.merge_state, POLICY_BLOCKED)
        # The owner fixes the ruleset: readiness no longer carries the block.
        del self.fake.rules[2]["parameters"]["required_reviewers"]
        pr, ci = await self.observe()
        self.assertEqual(ci["policy_rejections"], [])
        self.assertIsNone(self.project(pr, ci, value).merge_state)
        # Merged outcomes are never relabelled as blocked.
        merged = TaskProjection(pr_head_sha=SHA, merge_state="merged")
        self.fake.rules[2]["parameters"]["future_requirement"] = True
        pr, ci = await self.observe()
        self.assertEqual(self.project(pr, ci, merged).merge_state, "merged")
        self.assertFalse(self.fake.puts)

    async def test_refused_required_check_source_never_shows_green(self):
        self.fake.errors["/branches/main/protection"] = 404
        for rules in (
            [{"type": "workflows", "parameters": {"workflows": []}}],
            [
                {
                    "type": "required_status_checks",
                    "parameters": {"required_status_checks": [{"context": "x"}]},
                }
            ],
            [{"type": 7}],
            [
                {
                    "type": "pull_request",
                    "parameters": {
                        **default_branch_rules()[-1]["parameters"],
                        "required_check_contexts": ["review-policy"],
                    },
                }
            ],
            [
                {
                    "type": "pull_request",
                    "parameters": {"required_status_checks": ["unseen"]},
                }
            ],
        ):
            with self.subTest(rules=rules):
                self.fake.rules = rules
                pr, ci = await self.observe()
                self.assertTrue(ci["required_checks_incomplete"])
                self.assertTrue(ci["policy_rejections"])
                self.assertFalse(ci["green"])
                value = self.project(pr, ci)
                self.assertEqual((value.pr_state, value.ci_state), ("open", "unknown"))
                self.assertEqual(value.merge_state, POLICY_BLOCKED)
        self.assertFalse(self.fake.puts)

    async def test_unknown_identifiers_are_not_logged(self):
        marker = "SYNTHETIC_OPAQUE_MARKER"
        parameters = {**default_branch_rules()[-1]["parameters"], marker: True}
        for rules in (
            [{"type": marker}],
            [{"type": "pull_request", "parameters": parameters}],
        ):
            with self.subTest(rules=rules):
                self.fake.rules = rules
                with self.assertLogs("mainloop.services.github_merge", "INFO") as logs:
                    await self.observe()
                self.assertNotIn(marker, "\n".join(logs.output))
                self.assertRegex(
                    logs.output[0], "unknown_branch_rule|unknown_pull_request_parameter"
                )

    async def test_policy_rejection_still_reports_failing_ci(self):
        self.fake.rules = [{"type": "required_signatures"}]
        self.fake.runs = [self.fake.run(10, conclusion="failure")]
        pr, ci = await self.observe()
        self.assertEqual(self.project(pr, ci).ci_state, "failure")

    async def test_head_moving_during_checks_is_rejected(self):
        async def move(request):
            if request.url.path.endswith("/check-runs"):
                self.fake.pr["head"]["sha"] = "b" * 40

        self.fake.hook = move
        with self.assertRaises(GitHubError):
            await self.observe()
        self.assertFalse(self.fake.puts)
