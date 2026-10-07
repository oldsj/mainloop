"""Summary snapshots are deterministic and positive decisions bind their digest."""

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
