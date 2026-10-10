"""Summary snapshots are deterministic and positive decisions bind their digest."""

import copy
import unittest

from mainloop.services.merge_summary import (
    build_summary,
    canonical_digest,
    validate_reviewed_context,
)


def facts():
    files = [
        {
            "filename": "k8s/overlays/prod.yaml",
            "status": "modified",
            "previous_filename": None,
            "additions": 38,
            "deletions": 11,
        }
    ]
    return {
        "repository": "owner/repo",
        "pr_number": 17,
        "title": "Update workspace deployment defaults",
        "description": "Describes the deployment default changes.",
        "description_truncated": False,
        "description_digest": "d" * 64,
        "description_length": 41,
        "changed_files_count": 1,
        "additions": 38,
        "deletions": 11,
        "files": files,
        "files_digest": canonical_digest(files),
        "protected_matches": ["k8s/overlays/prod.yaml"],
        "policy": "auto",
        "policy_version": 4,
        "globs_version": 1,
        "head": "feature/deployment-defaults",
        "head_sha": "a" * 40,
        "base": "main",
        "base_sha": "b" * 40,
        "ci": {
            "green": True,
            "pending": False,
            "suites": [
                {
                    "id": 1,
                    "head_sha": "a" * 40,
                    "status": "completed",
                    "conclusion": "success",
                }
            ],
            "checks": [
                {
                    "id": 2,
                    "name": "build",
                    "status": "completed",
                    "conclusion": "success",
                }
            ],
            "statuses": [],
            "required": [["build", 12]],
            "captured_at": "2026-10-07T10:00:00Z",
            "complete": True,
        },
    }


class MergeSummaryTests(unittest.TestCase):
    def test_ignored_suites_are_hashed_but_excluded_from_active_counts(self):
        proposal_facts = facts()
        suite = {
            "id": 3,
            "head_sha": "a" * 40,
            "status": "queued",
            "conclusion": None,
            "app": {"id": 99, "slug": "cloudflare-workers-and-pages"},
            "created_at": "2026-10-07T09:00:00Z",
            "latest_check_runs_count": 0,
        }
        proposal_facts["ci"]["suites"].append(suite)
        proposal_facts["ci"]["ignored_suites"] = [
            {**suite, "reason": "queued_without_runs_past_grace_and_no_required_app"}
        ]
        summary, digest = build_summary(proposal_facts, "proposal-17")
        self.assertEqual(summary["availability"], "ready")
        self.assertEqual(summary["ci"]["ignored_suite_count"], 1)
        self.assertEqual(summary["ci"]["result_count"], 2)
        self.assertEqual(summary["ci"]["passed_count"], 2)
        self.assertEqual(summary["ci"]["pending_count"], 0)
        self.assertEqual(summary["ci"]["failed_count"], 0)
        validate_reviewed_context(
            proposal_facts, "proposal-17", digest, summary, digest
        )
        for field, value in (
            ("reason", "changed reason"),
            ("created_at", "2026-10-07T08:00:00Z"),
            ("app", {"id": 98}),
        ):
            with self.subTest(field=field):
                changed = copy.deepcopy(proposal_facts)
                changed["ci"]["ignored_suites"][0][field] = value
                if field != "reason":
                    changed["ci"]["suites"][-1][field] = value
                changed_summary, changed_digest = build_summary(changed, "proposal-17")
                self.assertNotEqual(changed_digest, digest)
                self.assertNotEqual(
                    changed_summary["ci"]["inventory_digest"],
                    summary["ci"]["inventory_digest"],
                )
                with self.assertRaisesRegex(ValueError, "changed or is unavailable"):
                    validate_reviewed_context(
                        changed, "proposal-17", digest, summary, digest
                    )

    def test_old_proposals_without_ignored_suites_keep_their_digest(self):
        proposal_facts = facts()
        summary, _ = build_summary(proposal_facts, "proposal-17")
        self.assertEqual(
            summary["ci"]["inventory_digest"], canonical_digest(proposal_facts["ci"])
        )
        self.assertNotIn("ignored_suite_count", summary["ci"])

    def test_malformed_ignored_inventory_makes_summary_unavailable(self):
        for ignored in (
            None,
            {},
            [None],
            [
                {
                    "id": 3,
                    "reason": "not in suites",
                    "status": "queued",
                    "conclusion": None,
                }
            ],
        ):
            with self.subTest(ignored=ignored):
                proposal_facts = facts()
                proposal_facts["ci"]["ignored_suites"] = ignored
                summary, _ = build_summary(proposal_facts, "proposal-17")
                self.assertEqual(summary["availability"], "unavailable")

    def test_digest_covers_proposal_facts_and_deterministic_reasons(self):
        proposal_id = "proposal-17"
        presentation, digest = build_summary(facts(), proposal_id)
        duplicate, duplicate_digest = build_summary(facts(), proposal_id)

        self.assertEqual(presentation, duplicate)
        self.assertEqual(digest, duplicate_digest)
        self.assertEqual(len(digest), 64)
        self.assertEqual(
            presentation["approval_reasons"],
            [
                {
                    "type": "protected_path",
                    "glob": "k8s/**",
                    "path": "k8s/overlays/prod.yaml",
                }
            ],
        )
        self.assertEqual(presentation["availability"], "ready")
        self.assertEqual(presentation["ci"]["result_count"], 2)
        validate_reviewed_context(facts(), proposal_id, digest, presentation, digest)

    def test_policy_and_protected_path_reasons_are_both_shown(self):
        proposal_id = "proposal-17"
        proposal_facts = facts()
        proposal_facts["policy"] = "approval"
        presentation, _ = build_summary(proposal_facts, proposal_id)
        self.assertEqual(
            presentation["approval_reasons"],
            [
                {"type": "project_policy"},
                {
                    "type": "protected_path",
                    "glob": "k8s/**",
                    "path": "k8s/overlays/prod.yaml",
                },
            ],
        )

    def test_merge_only_protected_paths_are_reasons_and_previewed_first(self):
        proposal_facts = facts()
        proposal_facts["files"][0]["filename"] = "src/build.yml"
        proposal_facts["files_digest"] = canonical_digest(proposal_facts["files"])
        merge_files = [
            dict(proposal_facts["files"][0]),
            {
                "filename": ".github/workflows/build.yml",
                "status": "renamed",
                "previous_filename": "ci/build.yml",
                "additions": 1,
                "deletions": 1,
            },
        ]
        proposal_facts.update(
            merge_files=merge_files,
            merge_files_digest=canonical_digest(merge_files),
            protected_matches=[".github/workflows/build.yml"],
        )
        summary, digest = build_summary(proposal_facts, "proposal-17")
        self.assertEqual(summary["availability"], "ready")
        self.assertEqual(
            summary["approval_reasons"],
            [
                {
                    "type": "protected_path",
                    "glob": ".github/**",
                    "path": ".github/workflows/build.yml",
                }
            ],
        )
        self.assertEqual(
            summary["paths_preview"],
            [
                "ci/build.yml → .github/workflows/build.yml (merge result only)",
                "src/build.yml",
            ],
        )
        self.assertFalse(summary["paths_preview_truncated"])
        self.assertEqual(summary["merge_file_count"], 2)
        self.assertEqual(summary["merge_only_file_count"], 1)
        self.assertEqual(
            summary["merge_paths_digest"], proposal_facts["merge_files_digest"]
        )
        validate_reviewed_context(
            proposal_facts, "proposal-17", digest, summary, digest
        )
        changed = copy.deepcopy(proposal_facts)
        changed["merge_files"][1]["filename"] = ".github/workflows/other.yml"
        with self.assertRaises(ValueError):
            validate_reviewed_context(changed, "proposal-17", digest, summary, digest)

    def test_malformed_merge_inventory_cannot_be_approved(self):
        for change in (
            {"merge_files": None},
            {"merge_files": [None]},
            {"merge_files_digest": None},
            {"merge_files": [{"filename": "a", "status": "added"}] * 2},
        ):
            with self.subTest(change=change):
                proposal_facts = facts()
                proposal_facts.update(merge_files=[], merge_files_digest="e" * 64)
                proposal_facts.update(change)
                summary, _ = build_summary(proposal_facts, "proposal-17")
                self.assertEqual(summary["availability"], "unavailable")
                self.assertIn(
                    "Complete merge-result file details are unavailable",
                    summary["unavailable_reasons"],
                )

    def test_missing_description_or_incomplete_diff_cannot_be_approved(self):
        proposal_id = "proposal-17"
        proposal_facts = facts()
        proposal_facts["description"] = ""
        summary, digest = build_summary(proposal_facts, proposal_id)
        self.assertEqual(summary["availability"], "unavailable")
        with self.assertRaisesRegex(ValueError, "unavailable"):
            validate_reviewed_context(
                proposal_facts, proposal_id, digest, summary, digest
            )

        incomplete = facts()
        incomplete["changed_files_count"] = 2
        summary, _ = build_summary(incomplete, proposal_id)
        self.assertIn(
            "Complete changed-file details are unavailable",
            summary["unavailable_reasons"],
        )

    def test_changed_summary_digest_is_rejected(self):
        proposal_id = "proposal-17"
        presentation, digest = build_summary(facts(), proposal_id)
        with self.assertRaisesRegex(ValueError, "changed or is unavailable"):
            validate_reviewed_context(
                facts(), proposal_id, "0" * 64, presentation, digest
            )

    def test_approval_snapshot_binds_template_mapping_evidence(self):
        proposal_id = "proposal-17"
        proposal_facts = facts()
        proposal_facts["route"] = "approval"
        proposal_facts["mapping_evidence"] = {
            "template_name": "claude-workspace",
            "provider": "claude",
            "config_digest": "c" * 64,
        }
        presentation, digest = build_summary(proposal_facts, proposal_id)
        self.assertEqual(
            presentation["mapping_evidence"], proposal_facts["mapping_evidence"]
        )
        changed = {
            **proposal_facts,
            "mapping_evidence": {
                **proposal_facts["mapping_evidence"],
                "config_digest": "d" * 64,
            },
        }
        with self.assertRaisesRegex(ValueError, "changed or is unavailable"):
            validate_reviewed_context(
                changed, proposal_id, digest, presentation, digest
            )

    def test_approval_snapshot_without_mapping_evidence_is_unavailable(self):
        proposal_facts = facts()
        proposal_facts["route"] = "approval"
        presentation, _ = build_summary(proposal_facts, "proposal-17")
        self.assertEqual(presentation["availability"], "unavailable")
        self.assertIn(
            "Reviewed template mapping evidence is unavailable",
            presentation["unavailable_reasons"],
        )


if __name__ == "__main__":
    unittest.main()
