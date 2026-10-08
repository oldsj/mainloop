"""Pure contracts and fail-closed seams; no native sessions or external services."""

import unittest
from datetime import UTC, datetime

from mainloop.config import Settings
from mainloop.providers import qualify_task_profile, registry
from mainloop.runtime.policy import Actor, tools_for
from mainloop.tasks.principal import TaskPrincipal
from pydantic import ValidationError

from models.agent_tools import TaskDelegate, TaskReportInput
from models.task import Task, TaskAttempt, TaskCheckout, TaskCreate


def create(**changes):
    return TaskCreate(
        request_id="r",
        title="Implement",
        brief="Do the work",
        mode="code",
        project_id="p",
        checkout=TaskCheckout(branch="feature/task"),
        **changes
    )


class TaskContracts(unittest.TestCase):
    def test_no_caller_authority_or_legacy_aliases(self):
        payload = create().model_dump()
        for key in (
            "owner_id",
            "root_task_id",
            "parent_task_id",
            "role",
            "depth",
            "agent_ref",
            "credential_ref",
            "binding_id",
            "kind",
        ):
            with self.subTest(key=key), self.assertRaises(ValidationError):
                TaskDelegate.model_validate({**payload, key: "forged"})
        with self.assertRaises(ValidationError):
            TaskReportInput(summary="Finished")

    def test_code_and_coordination_requirements(self):
        for payload in (
            {"mode": "code"},
            {"mode": "code", "project_id": "p"},
            {"mode": "coordination", "checkout": {"branch": "f"}},
        ):
            with self.assertRaises(ValidationError):
                TaskCreate(request_id="r", title="t", brief="b", **payload)
        TaskCreate(request_id="r", title="t", brief="b", mode="coordination")
        with self.assertRaises(ValidationError):
            TaskCreate(request_id="r", title="t", brief="é" * 9000, mode="coordination")

    def test_invalid_checkout_refs(self):
        for branch in (
            "main~1",
            "refs/heads/x",
            "-x",
            "a..b",
            "a.lock",
            "a//b",
            "a b",
            "a@{b",
        ):
            with self.subTest(branch=branch), self.assertRaises(ValidationError):
                TaskCheckout(branch=branch)

    def test_role_depth_and_hierarchy_rejected(self):
        now = datetime.now(UTC)
        for role, depth in (("main", 0), ("child", 1), ("supervisor", 2)):
            with self.assertRaises(ValidationError):
                TaskAttempt(
                    id="a",
                    task_id="t",
                    number=1,
                    profile_id="claude",
                    native_provider="claude",
                    configuration_revision="v1",
                    agent_ref={"namespace": "kagent", "name": "agent"},
                    role=role,
                    depth=depth,
                    created_at=now,
                    updated_at=now,
                )
        with self.assertRaises(ValidationError):
            Task(
                id="t",
                owner_id="o",
                root_task_id="other",
                title="t",
                brief="b",
                mode="coordination",
                assigned_profile_id="claude",
                selection_source="explicit",
                created_at=now,
                updated_at=now,
            )
        for kwargs in (
            {"role": "supervisor", "depth": 1},
            {"role": "child", "depth": 1, "binding_id": "b"},
            {"role": "owner", "binding_id": "b"},
        ):
            with self.assertRaises(ValueError):
                TaskPrincipal("o", **kwargs)

    def test_task_discovery_uses_exact_roles_and_hides_unavailable_handoff(self):
        for actor in (Actor("main", 0), Actor("supervisor", 1)):
            self.assertTrue(
                {"delegate", "task_get", "task_list", "task_history", "task_cancel"}
                <= tools_for(actor)
            )
            self.assertFalse({"status", "read", "cancel", "clear"} & tools_for(actor))
            self.assertFalse({"task_retry", "task_reassign"} & tools_for(actor))
        self.assertIn("report", tools_for(Actor("child", 2)))
        self.assertNotIn("delegate", tools_for(Actor("child", 2)))
        self.assertFalse(tools_for(Actor("child", 1)))

    def test_retention_and_default_settings(self):
        self.assertEqual(Settings(_env_file=None).task_delete_after_months, 2)
        for kwargs in (
            {"task_archive_after_days": 90},
            {"task_default_provider_profile_id": "../evil"},
            {"task_max_children_global": 0},
        ):
            with self.assertRaises(ValidationError):
                Settings(_env_file=None, **kwargs)

    def test_unknown_and_fixture_evidence_do_not_qualify_production(self):
        for profile in registry().profiles:
            with self.assertRaises(ValueError):
                qualify_task_profile(profile, "supervisor", "code")


class ProfileQualification(unittest.TestCase):
    def test_fixture_qualification_is_explicit_and_snapshot_is_not_required(self):
        from mainloop.providers import (
            TASK_CODE_CAPABILITIES,
            TASK_REQUIRED_CAPABILITIES,
        )

        from models.provider import ProviderProfile

        profile = ProviderProfile(
            id="fixture",
            display_name="Fixture",
            native_provider="codex",
            configuration_revision="fixture-v1",
            agents={"supervisor": {"namespace": "kagent", "name": "fixture"}},
            capabilities=[
                {
                    "capability": name,
                    "state": "proved",
                    "scope": "fixture",
                    "evidence_ref": "fixture:qualified",
                }
                for name in sorted(TASK_REQUIRED_CAPABILITIES | TASK_CODE_CAPABILITIES)
            ],
        )
        with self.assertRaises(ValueError):
            qualify_task_profile(profile, "supervisor", "code")
        self.assertEqual(
            qualify_task_profile(profile, "supervisor", "code", allow_fixture=True),
            profile,
        )
        with self.assertRaises(ValueError):
            qualify_task_profile(profile, "child", "code", allow_fixture=True)
        with self.assertRaises(ValueError):
            qualify_task_profile(
                profile.model_copy(update={"enabled": False}),
                "supervisor",
                "code",
                allow_fixture=True,
            )
        # Imported/snapshot state is not part of committed handoff qualification.
        self.assertNotIn("workspace_import", TASK_CODE_CAPABILITIES)


class TaskEligibilityContracts(unittest.TestCase):
    def test_available_actions_need_no_blocker(self):
        from models.task import TaskEligibility

        self.assertIsNone(TaskEligibility(available=True).reason)
        self.assertEqual(
            TaskEligibility(available=True, reason=None).model_dump(),
            {"available": True, "reason": None},
        )

    def test_unavailable_actions_require_a_typed_reason(self):
        from models.task import TaskEligibility

        self.assertEqual(
            TaskEligibility(available=False, reason="handoff_unavailable").reason,
            "handoff_unavailable",
        )
        for body in (
            {},
            {"available": False},
            {"available": False, "reason": None},
            {"available": False, "reason": "invented"},
        ):
            with self.subTest(body=body), self.assertRaises(ValidationError):
                TaskEligibility.model_validate(body)
