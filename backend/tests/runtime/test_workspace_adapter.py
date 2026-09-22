"""Pure policy and projection functions from workspace_adapter.py, tested without a database or
cluster -- the same split as test_session_status.py covers for native_sessions.py's status rules.
The DB-backed orchestration functions (ensure_workspace, resume_workspace, ...) are not yet
exercised against a live cluster; see docs/spikes and the task proof note."""

import unittest
from datetime import UTC, datetime

from mainloop.runtime.workspace_adapter import (
    CRASHED_NOTE,
    _binding_from_row,
    actor_name,
    is_crashed,
    plan_ensure,
)

from models import CapabilityState

NOW = datetime(2026, 9, 22, tzinfo=UTC)


def row(**overrides):
    base = {
        "workspace_id": "sess-1",
        "atespace": "mainloop-workspaces",
        "actor_name": "ml-sess1",
        "actor_template": "mainloop-workspace",
        "preview_route": "mainloop-workspaces/ml-sess1",
        "runtime_endpoint": "mainloop-workspaces/ml-sess1",
        "observed_state": "ready",
        "observed_at": NOW,
        "updated_at": NOW,
        "last_error": None,
    }
    base.update(overrides)
    return base


class ActorNameTests(unittest.TestCase):
    def test_stable_and_namespace_safe(self):
        name = actor_name("0123456789abcdefextra")
        self.assertEqual(name, "ml-0123456789abcdef")
        self.assertEqual(name, actor_name("0123456789abcdefextra"))  # deterministic


class PlanEnsureTests(unittest.TestCase):
    def test_first_provision_creates(self):
        self.assertEqual(plan_ensure(row_exists=False, actor_found=False), "create")

    def test_existing_actor_is_attached_whether_or_not_we_have_a_row(self):
        self.assertEqual(plan_ensure(row_exists=False, actor_found=True), "attach")
        self.assertEqual(plan_ensure(row_exists=True, actor_found=True), "attach")

    def test_a_row_with_no_matching_actor_is_surfaced_not_recreated(self):
        self.assertEqual(plan_ensure(row_exists=True, actor_found=False), "surface_gap")


class BindingProjectionTests(unittest.TestCase):
    def test_ready_row_has_no_capability_noise(self):
        binding = _binding_from_row(row())
        self.assertEqual(binding.observed_state, "ready")
        self.assertEqual(binding.capabilities, ())

    def test_crashed_row_surfaces_a_proved_actor_crashed_capability(self):
        binding = _binding_from_row(
            row(observed_state="unavailable", last_error=CRASHED_NOTE)
        )
        self.assertEqual(len(binding.capabilities), 1)
        cap = binding.capabilities[0]
        self.assertEqual(cap.capability, "actor_crashed")
        self.assertEqual(cap.state, CapabilityState.PROVED)
        self.assertEqual(cap.scope, "live")
        self.assertIn("CRASHED", cap.detail)
        self.assertIn("Substrate's actor record has no snapshot timestamp", cap.detail)

    def test_missing_actor_row_surfaces_a_distinct_capability_from_crashed(self):
        binding = _binding_from_row(
            row(
                observed_state="unavailable",
                last_error="actor not found where this row expected one; not recreated "
                "automatically (inspect and reconcile explicitly, or delete this row to allow "
                "a fresh actor)",
            )
        )
        self.assertEqual(binding.capabilities[0].capability, "actor_health")

    def test_runtime_endpoint_falls_back_to_preview_route(self):
        binding = _binding_from_row(row(runtime_endpoint=None))
        self.assertEqual(binding.runtime_endpoint, "mainloop-workspaces/ml-sess1")

    def test_runtime_endpoint_falls_back_to_unrouted_before_first_provision_observation(
        self,
    ):
        binding = _binding_from_row(row(runtime_endpoint=None, preview_route=None))
        self.assertEqual(binding.runtime_endpoint, "unrouted")


class IsCrashedTests(unittest.TestCase):
    def test_true_only_for_the_crashed_note(self):
        self.assertTrue(
            is_crashed(row(observed_state="unavailable", last_error=CRASHED_NOTE))
        )
        self.assertFalse(
            is_crashed(row(observed_state="unavailable", last_error="actor missing"))
        )
        self.assertFalse(is_crashed(row()))


if __name__ == "__main__":
    unittest.main()
