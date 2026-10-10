"""Small publication decision fixtures; no external services."""

import sys
import time
import unittest
from contextlib import redirect_stdout
from io import StringIO
from types import SimpleNamespace
from unittest.mock import patch

import smoke_live
from smoke_live import completed, stage


class SmokeDecisions(unittest.TestCase):
    def setUp(self):
        self.view = {
            "task": {"status": "completed", "checkout": {"branch": "smoke/fixture"}},
            "projection": {
                "pr_number": 2,
                "pr_head_sha": "abc",
                "ci_head_sha": "abc",
                "ci_state": "success",
                "merge_state": "merged",
            },
        }
        self.facts = {"push_confirmed": True}
        self.pr = {
            "merged": True,
            "merged_by": {"login": "test-app[bot]"},
            "head": {
                "ref": "smoke/fixture",
                "sha": "abc",
                "repo": {"full_name": "fixture/repo"},
            },
            "base": {"repo": {"full_name": "fixture/repo"}},
        }

    def test_requires_every_fact(self):
        self.assertTrue(
            completed(self.view, self.facts, self.pr, "test-app[bot]", "fixture/repo")
        )
        for field, bad in (("status", "running"),):
            self.view["task"][field] = bad
            self.assertFalse(
                completed(
                    self.view, self.facts, self.pr, "test-app[bot]", "fixture/repo"
                )
            )
        self.view["task"]["status"] = "completed"
        self.assertFalse(
            completed(self.view, self.facts, self.pr, "other[bot]", "fixture/repo")
        )
        self.assertFalse(
            completed(
                self.view,
                {"push_confirmed": False},
                self.pr,
                "test-app[bot]",
                "fixture/repo",
            )
        )
        self.view["projection"]["merge_state"] = "pending"
        self.assertFalse(
            completed(self.view, self.facts, self.pr, "test-app[bot]", "fixture/repo")
        )

    def test_first_unproven_step(self):
        self.assertEqual(stage(None, self.facts, {}), "delegation")
        self.assertEqual(stage(self.view, {"push_confirmed": False}, {}), "push")
        self.view["projection"]["pr_number"] = None
        self.assertEqual(stage(self.view, self.facts, {}), "pull_request")
        self.view["projection"].update(pr_number=2, ci_state="failure")
        self.assertEqual(stage(self.view, self.facts, {}), "ci")
        self.view["projection"]["ci_state"] = "success"
        self.assertEqual(stage(self.view, self.facts, {}), "merge")

    def test_unrelated_pr_cannot_pass(self):
        for field, value in (
            ("ref", "other"),
            ("sha", "other"),
            ("repo", {"full_name": "other/repo"}),
        ):
            with self.subTest(field=field):
                pr = {**self.pr, "head": {**self.pr["head"], field: value}}
                self.assertFalse(
                    completed(
                        self.view, self.facts, pr, "test-app[bot]", "fixture/repo"
                    )
                )

    def test_blank_context_has_no_side_effects(self):
        for value in ("", " ", "\t\n"):
            argv = [
                "smoke",
                "--context",
                value,
                "--namespace",
                "fixture",
                "--project-id",
                "fixture",
                "--repo",
                "fixture/repo",
                "--app-login",
                "app[bot]",
                "--provider",
                "codex",
            ]
            with patch.object(sys, "argv", argv), patch.object(
                smoke_live, "forward"
            ) as forward:
                with self.assertRaises(SystemExit) as error:
                    smoke_live.main()
                self.assertEqual(error.exception.code, 2)
                forward.assert_not_called()

    def test_wall_deadline_interrupts_complete_operation(self):
        started = time.monotonic()
        with self.assertRaisesRegex(RuntimeError, "operation deadline"):
            with smoke_live.wall_deadline(0.03):
                for _ in range(100):
                    time.sleep(0.01)
        self.assertLess(time.monotonic() - started, 0.3)

    def test_uncertain_submission_discovers_late_task_and_ci(self):
        import json

        calls = []
        args = SimpleNamespace(
            deadline=1,
            step_deadline=1,
            poll_interval=0.01,
            project_id="fixture",
            repo="fixture/repo",
            provider="codex",
            app_login="app[bot]",
        )
        facts = {
            "deliveries_busy": False,
            "deliveries": [],
            "capacity_holders": [],
            "parent_capacity": 3,
            "global_capacity_available": True,
            "push_confirmed": False,
        }

        def urlopen(request, timeout):
            calls.append(request.full_url)
            if request.data:
                raise TimeoutError("accepted but response lost")
            if request.full_url.endswith("/health"):
                value = {}
            elif request.full_url.endswith("/projects/fixture"):
                value = {"full_name": "fixture/repo"}
            elif "/tasks?" in request.full_url:
                value = [
                    {
                        "task": {
                            "id": "late",
                            "status": "running",
                            "checkout": {"branch": "smoke/codex-fixed"},
                        },
                        "projection": {"pr_number": 2},
                    }
                ]
            else:
                value = facts
            from io import BytesIO

            return BytesIO(json.dumps(value).encode())

        gh_calls = []

        def github(repo, path, timeout):
            gh_calls.append(path)
            if path.startswith("pulls?"):
                return []
            if path.startswith("pulls/"):
                return {"head": {"sha": "abc"}, "merged": False}
            return {"check_runs": []}

        with patch.object(
            smoke_live.uuid, "uuid4", return_value=SimpleNamespace(hex="fixed")
        ), patch.object(
            smoke_live.urllib.request, "urlopen", side_effect=urlopen
        ), patch.object(
            smoke_live, "gh", side_effect=github
        ), redirect_stdout(
            StringIO()
        ):
            self.assertEqual(smoke_live.run(args, "http://127.0.0.1:1"), 1)
        self.assertEqual(sum(url.endswith("/chat") for url in calls), 1)
        self.assertIn("commits/abc/check-runs", gh_calls)

    def test_streaming_body_cannot_outlast_deadline(self):
        import json
        import threading
        from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200)
                self.end_headers()
                try:
                    for byte in b'{"slow": "response"}':
                        self.wfile.write(bytes([byte]))
                        self.wfile.flush()
                        time.sleep(0.01)
                except (BrokenPipeError, ConnectionResetError):
                    pass

            def log_message(self, *args):
                pass

        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        started = time.monotonic()
        try:
            with self.assertRaisesRegex(RuntimeError, "operation deadline"):
                with smoke_live.wall_deadline(0.05):
                    with smoke_live.urllib.request.urlopen(
                        f"http://127.0.0.1:{server.server_port}", timeout=0.05
                    ) as response:  # nosec B310
                        json.load(response)
            self.assertLess(time.monotonic() - started, 0.2)
        finally:
            server.shutdown()
            server.server_close()
            thread.join()

    def test_poll_sleep_stops_at_step_deadline(self):
        import json
        from io import BytesIO

        clock = [0.0]
        sleeps = []
        args = SimpleNamespace(
            deadline=100,
            step_deadline=10,
            poll_interval=90,
            project_id="fixture",
            repo="fixture/repo",
            provider="codex",
            app_login="app[bot]",
        )
        facts = {
            "deliveries_busy": False,
            "deliveries": [],
            "capacity_holders": [],
            "parent_capacity": 3,
            "global_capacity_available": True,
            "push_confirmed": False,
        }

        def urlopen(request, timeout):
            if request.full_url.endswith("/projects/fixture"):
                value = {"full_name": "fixture/repo"}
            elif "/tasks?" in request.full_url:
                value = []
            else:
                value = facts
            return BytesIO(json.dumps(value).encode())

        def sleep(seconds):
            sleeps.append(seconds)
            clock[0] += seconds

        with patch.object(
            smoke_live.time, "monotonic", side_effect=lambda: clock[0]
        ), patch.object(smoke_live.time, "sleep", side_effect=sleep), patch.object(
            smoke_live.urllib.request, "urlopen", side_effect=urlopen
        ), patch.object(
            smoke_live, "gh", return_value=[]
        ), redirect_stdout(
            StringIO()
        ):
            self.assertEqual(smoke_live.run(args, "http://127.0.0.1:1"), 1)
        self.assertEqual(sleeps, [10])


if __name__ == "__main__":
    unittest.main()
