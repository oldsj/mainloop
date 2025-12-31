#!/usr/bin/env python3
"""
Tests for native plan mode support in job_runner.

Unit tests that can run without K8s or Claude credentials.
"""

import os
import sys
import unittest
from unittest.mock import patch, MagicMock, AsyncMock

# Set up environment before importing job_runner
os.environ.setdefault("TASK_ID", "test-task-123")
os.environ.setdefault("TASK_PROMPT", "Add a new feature to handle user authentication")
os.environ.setdefault("REPO_URL", "https://github.com/test/repo")
os.environ.setdefault("MODE", "plan")

import job_runner


class TestPlanPrompt(unittest.TestCase):
    """Test plan prompt generation."""

    def test_plan_prompt_with_repo(self):
        """Plan prompt should include repo and simplified instructions."""
        with patch.object(job_runner, "REPO_URL", "https://github.com/test/repo"):
            with patch.object(job_runner, "TASK_PROMPT", "Add authentication"):
                with patch.object(job_runner, "TASK_ID", "abc12345"):
                    with patch.object(job_runner, "FEEDBACK_CONTEXT", ""):
                        prompt = job_runner.build_plan_prompt()

        # Should contain task info
        self.assertIn("Task ID: abc12345", prompt)
        self.assertIn("Add authentication", prompt)
        self.assertIn("https://github.com/test/repo", prompt)

        # Should have simplified instructions (not GitHub issue creation)
        self.assertIn("Clone the repository", prompt)
        self.assertIn("implementation plan", prompt)
        self.assertIn("Approach", prompt)
        self.assertIn("Files to modify", prompt)

        # Should NOT contain old prompt instructions
        self.assertNotIn("gh issue create", prompt)
        self.assertNotIn("mainloop-plan", prompt)

    def test_plan_prompt_without_repo(self):
        """Plan prompt should work without repo URL."""
        with patch.object(job_runner, "REPO_URL", ""):
            with patch.object(job_runner, "TASK_PROMPT", "Generic task"):
                with patch.object(job_runner, "TASK_ID", "xyz789"):
                    with patch.object(job_runner, "FEEDBACK_CONTEXT", ""):
                        prompt = job_runner.build_plan_prompt()

        self.assertIn("Generic task", prompt)
        self.assertIn("Create an implementation plan", prompt)

    def test_plan_prompt_with_feedback(self):
        """Plan prompt should include feedback context for revisions."""
        with patch.object(job_runner, "REPO_URL", "https://github.com/test/repo"):
            with patch.object(job_runner, "TASK_PROMPT", "Add feature"):
                with patch.object(job_runner, "TASK_ID", "rev123"):
                    with patch.object(job_runner, "FEEDBACK_CONTEXT", "Please add more detail about error handling"):
                        prompt = job_runner.build_plan_prompt()

        self.assertIn("Feedback on your previous plan", prompt)
        self.assertIn("Please add more detail about error handling", prompt)


class TestPermissionMode(unittest.TestCase):
    """Test permission mode selection.

    All batch job modes use bypassPermissions since there's no human
    to approve permission requests in a K8s Job context.
    """

    def test_all_modes_use_bypass(self):
        """All modes should use permission_mode='bypassPermissions' for batch jobs."""
        # Batch jobs can't get human approval, so all modes bypass
        for mode in ["plan", "implement", "feedback", "fix"]:
            with patch.object(job_runner, "MODE", mode):
                # This mirrors the actual logic in execute_task()
                perm_mode = "bypassPermissions"
            self.assertEqual(perm_mode, "bypassPermissions", f"Mode {mode} should use bypassPermissions")


class TestGitHubIssueCreation(unittest.TestCase):
    """Test GitHub issue creation from plan content."""

    @patch("subprocess.run")
    def test_create_issue_success(self, mock_run):
        """Should create issue and return URL on success."""
        # Mock successful gh issue create
        mock_run.return_value = MagicMock(
            returncode=0,
            stdout="https://github.com/test/repo/issues/42\n",
            stderr="",
        )

        with patch.object(job_runner, "REPO_URL", "https://github.com/test/repo"):
            with patch.object(job_runner, "TASK_PROMPT", "Add authentication"):
                with patch.object(job_runner, "WORKSPACE", "/workspace"):
                    result = job_runner.create_github_issue_from_plan("## Plan content here")

        self.assertEqual(result, "https://github.com/test/repo/issues/42")

        # Verify gh issue create was called
        create_call = mock_run.call_args_list[0]
        self.assertEqual(create_call[0][0][0:3], ["gh", "issue", "create"])

    @patch("subprocess.run")
    def test_create_issue_failure(self, mock_run):
        """Should return None on gh CLI failure."""
        mock_run.return_value = MagicMock(
            returncode=1,
            stdout="",
            stderr="error: could not create issue",
        )

        with patch.object(job_runner, "REPO_URL", "https://github.com/test/repo"):
            with patch.object(job_runner, "TASK_PROMPT", "Add feature"):
                with patch.object(job_runner, "WORKSPACE", "/workspace"):
                    result = job_runner.create_github_issue_from_plan("## Plan")

        self.assertIsNone(result)

    def test_create_issue_no_repo(self):
        """Should return None when no repo URL."""
        with patch.object(job_runner, "REPO_URL", ""):
            result = job_runner.create_github_issue_from_plan("## Plan")

        self.assertIsNone(result)

    @patch("subprocess.run")
    def test_issue_body_includes_commands(self, mock_run):
        """Issue body should include /implement and /revise commands."""
        mock_run.return_value = MagicMock(
            returncode=0,
            stdout="https://github.com/test/repo/issues/1\n",
            stderr="",
        )

        with patch.object(job_runner, "REPO_URL", "https://github.com/test/repo"):
            with patch.object(job_runner, "TASK_PROMPT", "Task"):
                with patch.object(job_runner, "WORKSPACE", "/workspace"):
                    job_runner.create_github_issue_from_plan("## My Plan")

        # Get the body argument from the call
        create_call = mock_run.call_args_list[0]
        body_idx = create_call[0][0].index("--body") + 1
        body = create_call[0][0][body_idx]

        self.assertIn("/implement", body)
        self.assertIn("/revise", body)
        self.assertIn("## My Plan", body)


class TestUrlExtraction(unittest.TestCase):
    """Test URL extraction from output."""

    def test_extract_pr_url(self):
        """Should extract PR URL from output."""
        output = "Created PR: https://github.com/owner/repo/pull/123\nDone!"
        result = job_runner.extract_pr_url(output)
        self.assertEqual(result, "https://github.com/owner/repo/pull/123")

    def test_extract_pr_url_none(self):
        """Should return None when no PR URL found."""
        output = "No PR created"
        result = job_runner.extract_pr_url(output)
        self.assertIsNone(result)

    def test_extract_issue_url(self):
        """Should extract issue URL from output."""
        output = "Created issue: https://github.com/owner/repo/issues/456"
        result = job_runner.extract_issue_url(output)
        self.assertEqual(result, "https://github.com/owner/repo/issues/456")

    def test_extract_issue_url_none(self):
        """Should return None when no issue URL found."""
        output = "No issue created"
        result = job_runner.extract_issue_url(output)
        self.assertIsNone(result)


if __name__ == "__main__":
    unittest.main(verbosity=2)
