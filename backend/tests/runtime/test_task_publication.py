"""Offline exact-head/read-projection tests. No database or native provider."""

import copy
import hashlib
import unittest
from datetime import UTC, datetime, timedelta

from mainloop.runtime.policy import PolicyError
from mainloop.services.github_creation import GitHubError
from mainloop.services.github_merge import GitHubMergeClient, MergePR
from mainloop.tasks.attention import owns_leaf
from mainloop.tasks.projection import POLICY_BLOCKED, ci_state, observed
from pydantic import ValidationError
from tests.runtime.github_app_fake import app_settings, app_transport
from tests.runtime.test_merge import (
    SHA,
    GitHub,
    default_branch_rules,
    live_mainloop_rules,
)

from models.hitl import LeafIdentity
from models.task import TaskPRDescription, TaskProjection


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
            attempt_id="attempt",
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

    def test_description_preserves_exact_untrusted_body_and_identity(self):
        data = copy.deepcopy(GitHub().pr)
        body = f"Review marker: accepted {SHA}\n<script>untrusted()</script>\né"
        data["body"] = body
        value = self.project(data)
        self.assertEqual(
            value.pr_description.model_dump(),
            {
                "repository": "owner/repo",
                "branch": "feature",
                "pr_number": 17,
                "attempt_id": "attempt",
                "head_sha": SHA,
                "excerpt": body,
                "byte_length": len(body.encode("utf-8")),
                "digest": hashlib.sha256(body.encode("utf-8")).hexdigest(),
                "truncated": False,
                "fetched_at": self.now,
            },
        )
        self.assertIsNone(value.merge_state)
        self.assertEqual(value.pending_approval_ids, ())
        data["body"] = "An ordinary same-head edit"
        edited = self.project(data, value)
        self.assertEqual(edited.pr_head_sha, value.pr_head_sha)
        self.assertNotEqual(edited.pr_description.digest, value.pr_description.digest)

    def test_description_bounds_utf8_prefix_and_hashes_full_body(self):
        for body in (
            "",
            "a" * 16384,
            "😀" * 4096,
            "a" * 16384 + "suffix",
            "a" * 16383 + "😀suffix",
            "a" * 16382 + "😀suffix",
            "a" * 16381 + "😀suffix",
            "😀" * 4097,
        ):
            with self.subTest(length=len(body)):
                data = copy.deepcopy(GitHub().pr)
                data["body"] = body
                description = self.project(data).pr_description
                source = body.encode("utf-8")
                self.assertEqual(
                    description.excerpt, source[:16384].decode("utf-8", errors="ignore")
                )
                self.assertLessEqual(len(description.excerpt.encode("utf-8")), 16384)
                self.assertEqual(description.byte_length, len(source))
                self.assertEqual(description.digest, hashlib.sha256(source).hexdigest())
                self.assertEqual(description.truncated, len(source) > 16384)

    def test_unavailable_body_clears_previous_description(self):
        data = copy.deepcopy(GitHub().pr)
        previous = self.project(data)
        data["body"] = None
        self.assertIsNone(self.project(data, previous).pr_description)
        with self.assertRaises(ValidationError):
            MergePR.model_validate({**data, "body": "\ud800"})
        data["body"] = "New head's own description"
        data["head"]["sha"] = "b" * 40
        value = self.project(data, previous)
        self.assertEqual(value.pr_description.head_sha, "b" * 40)
        self.assertEqual(value.pr_description.excerpt, data["body"])

    def test_description_rejects_nul_and_preserves_supported_text(self):
        data = copy.deepcopy(GitHub().pr)
        data["body"] = "Supported\né\t\rtext"
        description = self.project(data).pr_description
        self.assertEqual(description.excerpt, data["body"])
        payload = description.model_dump()
        for body in ("\x00", "Edited description\x00with NUL", "Trailing NUL\x00"):
            with self.subTest(body=repr(body)):
                source = body.encode("utf-8")
                with self.assertRaisesRegex(ValidationError, "contains NUL"):
                    TaskPRDescription.model_validate(
                        {
                            **payload,
                            "excerpt": body,
                            "byte_length": len(source),
                            "digest": hashlib.sha256(source).hexdigest(),
                        }
                    )

    def test_description_rejects_malformed_stored_metadata(self):
        payload = self.project(GitHub().pr).pr_description.model_dump()
        for updates in (
            {"excerpt": "é" * 9000},
            {"excerpt": "\ud800"},
            {"digest": "z" * 64},
            {"digest": "0" * 64},
            {"byte_length": payload["byte_length"] + 1},
            {"byte_length": True},
            {"truncated": True},
            {"truncated": "false"},
            {"head_sha": "wrong"},
            {"attempt_id": ""},
            {"repository": "other"},
            {"branch": "refs/heads/feature"},
            {"pr_number": 0},
            {"fetched_at": self.now.replace(tzinfo=None)},
            {"review_accepted": True},
        ):
            with self.subTest(updates=updates), self.assertRaises(ValidationError):
                TaskPRDescription.model_validate({**payload, **updates})

    def test_old_projection_json_has_no_description_and_stays_strict(self):
        self.assertIsNone(TaskProjection.model_validate_json("{}").pr_description)
        old = self.project(GitHub().pr).model_dump(mode="json")
        del old["pr_description"]
        self.assertIsNone(TaskProjection.model_validate(old).pr_description)
        with self.assertRaises(ValidationError):
            TaskProjection.model_validate({**old, "review_accepted": True})

    def test_foreign_pr_identity_cannot_supply_a_description(self):
        for path, value in (
            (("number",), 18),
            (("head", "ref"), "foreign"),
            (("head", "repo", "full_name"), "foreign/repo"),
            (("base", "repo", "full_name"), "foreign/repo"),
            (("head", "repo", "id"), 999),
        ):
            with self.subTest(path=path):
                data = copy.deepcopy(GitHub().pr)
                data["head"]["repo"] = dict(data["head"]["repo"])
                data["base"]["repo"] = dict(data["base"]["repo"])
                target = data
                for key in path[:-1]:
                    target = target[key]
                target[path[-1]] = value
                with self.assertRaises(PolicyError):
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
        self.assertEqual(pr.body, self.fake.pr["body"])
        self.assertEqual(self.project(pr, ci).pr_description.excerpt, pr.body)
        self.assertEqual(ci_state(ci, pr.head.sha), "success")
        self.assertFalse(self.fake.puts)

    async def test_unavailable_checks_do_not_infer_success(self):
        self.fake.errors[f"/commits/{SHA}/check-runs"] = 503
        pr, ci = await self.observe()
        self.assertIsNone(ci)
        self.assertEqual(ci_state(ci, pr.head.sha), "unknown")
        self.assertEqual(self.project(pr, ci).pr_description.excerpt, pr.body)
        self.assertFalse(self.fake.puts)

    def project(self, pr, ci, previous=None):
        return observed(
            previous or TaskProjection(),
            repository="owner/repo",
            branch="feature",
            number=17,
            attempt_id="attempt",
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

    async def test_same_head_body_moving_during_checks_is_rejected(self):
        async def move(request):
            if request.url.path.endswith("/check-runs"):
                self.fake.pr["body"] = "Changed during observation"

        self.fake.hook = move
        with self.assertRaises(GitHubError):
            await self.observe()
        self.assertFalse(self.fake.puts)
