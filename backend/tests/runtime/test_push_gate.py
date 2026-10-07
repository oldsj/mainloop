"""Scope fixtures; no Git listener, provider or agent calls."""

import unittest

from mainloop.config import Settings
from mainloop.push_gate.authorization import ZERO_OID, authorize

from models.push_gate import ProtectedBranchPolicy, PushGrant, RefUpdate

GRANT = PushGrant(
    id="g",
    owner_id="u",
    project_id="p",
    repository="Owner/Repo",
    branch="Feature",
    workspace_id="s",
    session_id="s",
    runtime_identity="runtime",
)
POLICY = ProtectedBranchPolicy(
    project_id="p", version=1, default_branch="main", patterns=("release/*",)
)
UPDATE = RefUpdate(ref="refs/heads/Feature", old_oid="a" * 40, new_oid="b" * 40)


class PushAuthorizationTests(unittest.TestCase):
    def test_scope_matrix(self):
        cases = [
            ({}, None),
            ({"update": UPDATE.model_copy(update={"old_oid": ZERO_OID})}, None),
            (
                {"update": UPDATE.model_copy(update={"ref": "refs/heads/main"})},
                "default_branch",
            ),
            (
                {"update": UPDATE.model_copy(update={"ref": "refs/heads/release/1"})},
                "protected_branch",
            ),
            (
                {"update": UPDATE.model_copy(update={"ref": "refs/heads/feature"})},
                "branch_mismatch",
            ),
            (
                {"update": UPDATE.model_copy(update={"ref": "refs/tags/v1"})},
                "ref_namespace",
            ),
            (
                {"update": UPDATE.model_copy(update={"ref": "refs/notes/x"})},
                "ref_namespace",
            ),
            ({"update": UPDATE.model_copy(update={"new_oid": ZERO_OID})}, "deletion"),
            ({"repository": "owner/other"}, "repository_mismatch"),
            ({"repository": "OWNER/REPO"}, None),
            (
                {"repository": "https://evil.invalid/owner/repo"},
                "repository_unavailable",
            ),
            ({"updates": [UPDATE, UPDATE]}, "single_ref_required"),
            ({"updates": []}, "single_ref_required"),
            ({"ancestor": False}, "non_fast_forward"),
            ({"ancestor": None}, "ancestry_unavailable"),
        ]
        for args, expected in cases:
            with self.subTest(args=args):
                self.assertEqual(
                    authorize(
                        GRANT,
                        POLICY,
                        args.get("repository", "owner/repo"),
                        args.get("updates", [args.get("update", UPDATE)]),
                        lambda old, new, args=args: args.get("ancestor", True),
                    ),
                    expected,
                )

    def test_authority_matrix(self):
        for changes, reason in [
            ({"active": False}, "grant_revoked"),
            ({"archived": True}, "session_archived"),
            ({"terminal": True}, "session_terminal"),
            ({"role": "main"}, "grant_kind"),
            ({"role": "child"}, "grant_kind"),
            ({"grant_kind": "coordination"}, "grant_kind"),
            ({"runtime_identity": ""}, "binding_unavailable"),
            ({"project_id": "other"}, "policy_mismatch"),
            ({"branch": "refs/heads/x"}, "invalid_branch"),
        ]:
            with self.subTest(changes=changes):
                self.assertEqual(
                    authorize(
                        GRANT.model_copy(update=changes),
                        POLICY,
                        "owner/repo",
                        [UPDATE],
                        lambda *_: True,
                    ),
                    reason,
                )

    def test_protected_workspace_and_missing_metadata(self):
        for branch in ("main", "release/1"):
            reason = "default_branch" if branch == "main" else "protected_branch"
            self.assertEqual(
                authorize(
                    GRANT.model_copy(update={"branch": branch}),
                    POLICY,
                    "owner/repo",
                    [UPDATE.model_copy(update={"ref": "refs/heads/" + branch})],
                    lambda *_: True,
                ),
                reason,
            )
        self.assertEqual(
            authorize(
                GRANT,
                POLICY.model_copy(update={"default_branch": ""}),
                "owner/repo",
                [UPDATE],
                lambda *_: True,
            ),
            "metadata_unavailable",
        )

    def test_creation_does_not_need_ancestry(self):
        def forbidden(*_):
            self.fail("creation queried ancestry")

        self.assertIsNone(
            authorize(
                GRANT,
                POLICY,
                "owner/repo",
                [UPDATE.model_copy(update={"old_oid": ZERO_OID})],
                forbidden,
            )
        )

    def test_disabled_by_default(self):
        self.assertFalse(Settings(_env_file=None).push_gate_enabled)
