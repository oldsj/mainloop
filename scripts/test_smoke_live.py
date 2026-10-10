"""Small publication decision fixtures; no external services."""

import unittest

from smoke_live import completed, stage


class SmokeDecisions(unittest.TestCase):
    def setUp(self):
        self.view = {
            "task": {"status": "completed"},
            "projection": {
                "pr_number": 2,
                "ci_state": "success",
                "merge_state": "merged",
            },
        }
        self.facts = {"pushes": [{"state": "confirmed"}]}
        self.pr = {"merged": True, "merged_by": {"login": "test-app[bot]"}}

    def test_requires_every_fact(self):
        self.assertTrue(completed(self.view, self.facts, self.pr, "test-app[bot]"))
        for field, bad in (("status", "running"),):
            self.view["task"][field] = bad
            self.assertFalse(completed(self.view, self.facts, self.pr, "test-app[bot]"))
        self.view["task"]["status"] = "completed"
        self.assertFalse(completed(self.view, self.facts, self.pr, "other[bot]"))
        self.assertFalse(
            completed(
                self.view, {"pushes": [{"state": "unknown"}]}, self.pr, "test-app[bot]"
            )
        )
        self.view["projection"]["merge_state"] = "pending"
        self.assertFalse(completed(self.view, self.facts, self.pr, "test-app[bot]"))

    def test_first_unproven_step(self):
        self.assertEqual(stage(None, self.facts, {}), "delegation")
        self.assertEqual(stage(self.view, {"pushes": []}, {}), "push")
        self.view["projection"]["pr_number"] = None
        self.assertEqual(stage(self.view, self.facts, {}), "pull_request")
        self.view["projection"].update(pr_number=2, ci_state="failure")
        self.assertEqual(stage(self.view, self.facts, {}), "ci")
        self.view["projection"]["ci_state"] = "success"
        self.assertEqual(stage(self.view, self.facts, {}), "merge")


if __name__ == "__main__":
    unittest.main()
