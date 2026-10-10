"""Real subprocess probes for the deadlines agents rely on."""

import json
import os
import signal
import subprocess  # nosec B404 - local interpreter and synthetic offline fixtures
import sys
import tempfile
import unittest
from pathlib import Path

BACKEND = Path(__file__).resolve().parents[1]


class BackendRunnerTests(unittest.TestCase):
    def probe(self, source, *, suite_seconds=5):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "probe.py").write_text(source)
            env = {
                **os.environ,
                "MAINLOOP_TEST_DATABASE_URL": "postgresql://fixture-unused/postgres",
                "PYTHONPATH": os.pathsep.join((str(root), str(BACKEND))),
            }
            result = subprocess.run(  # nosec B603 - synthetic local test module
                [
                    sys.executable,
                    "-c",
                    "import sys; from scripts import test_backend as runner; "
                    f"runner.SUITE_SECONDS = {suite_seconds!r}; "
                    "sys.argv = ['test_backend', '--timings', sys.argv[1], 'probe']; "
                    "sys.exit(runner.main())",
                    str(root / "timings.json"),
                ],
                cwd=BACKEND,
                env=env,
                capture_output=True,
                text=True,
                timeout=15,
            )
            timings = (
                json.loads((root / "timings.json").read_text())
                if (root / "timings.json").exists()
                else None
            )
            return result, timings

    def test_success_failure_and_module_fixture_timings(self):
        result, timings = self.probe(
            "import time, unittest\n"
            "def setUpModule(): time.sleep(0.02)\n"
            "class Case(unittest.TestCase):\n"
            " def test_ok(self): self.assertEqual(1, 1)\n"
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(timings["tests_run"], 1)
        self.assertGreaterEqual(timings["modules"]["probe"], 0.02)
        result, _ = self.probe(
            "import unittest\n"
            "class Case(unittest.TestCase):\n"
            " def test_bad(self): self.fail('expected probe failure')\n"
        )
        self.assertEqual(result.returncode, 1)
        self.assertIn("expected probe failure", result.stderr)

    def test_native_wait_has_named_fatal_test_deadline(self):
        result, _ = self.probe(
            "import sys, threading, unittest\n"
            "sys.modules['__main__'].TEST_SECONDS = 0.1\n"
            "class Case(unittest.TestCase):\n"
            " def test_hung(self): threading.Event().wait()\n"
        )
        self.assertEqual(result.returncode, 1)
        self.assertIn("probe.Case.test_hung", result.stderr)
        self.assertIn("Timeout (0:00:00.100000)!", result.stderr)
        self.assertIn("threading.py", result.stderr)

    def test_asyncio_debug_still_rejects_cross_thread_loop_calls(self):
        result, _ = self.probe(
            "import asyncio, unittest\n"
            "class Case(unittest.IsolatedAsyncioTestCase):\n"
            " async def test_debug_checks(self):\n"
            "  loop = asyncio.get_running_loop()\n"
            "  self.assertTrue(loop.get_debug())\n"
            "  def misuse():\n"
            "   with self.assertRaisesRegex(RuntimeError, 'Non-thread-safe'):\n"
            "    loop.call_soon(lambda: None)\n"
            "  await asyncio.to_thread(misuse)\n"
        )
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_class_fixture_has_named_fatal_deadline(self):
        result, _ = self.probe(
            "import sys, threading, unittest\n"
            "sys.modules['__main__'].TEST_SECONDS = 0.1\n"
            "class Case(unittest.TestCase):\n"
            " @classmethod\n"
            " def setUpClass(cls): threading.Event().wait()\n"
            " def test_ok(self): pass\n"
        )
        self.assertEqual(result.returncode, 1)
        self.assertIn("probe.Case setUpClass", result.stderr)
        self.assertIn("Timeout (0:00:00.100000)!", result.stderr)

    def test_whole_suite_cap_covers_discovery(self):
        result, _ = self.probe(
            "import threading; threading.Event().wait()\n", suite_seconds=2
        )
        self.assertEqual(result.returncode, 124, result.stderr)
        self.assertIn("test discovery/imports", result.stderr)
        self.assertIn("suite exceeded 2s", result.stderr)

    def test_fatal_deadline_terminates_descendant(self):
        with tempfile.TemporaryDirectory() as directory:
            pid_path = Path(directory) / "child.pid"
            result, _ = self.probe(
                "import subprocess, sys, threading, unittest\n"
                "from pathlib import Path\n"
                "sys.modules['__main__'].TEST_SECONDS = 0.2\n"
                "class Case(unittest.TestCase):\n"
                " def test_hung(self):\n"
                "  child = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(60)'])\n"
                f"  Path({str(pid_path)!r}).write_text(str(child.pid))\n"
                "  threading.Event().wait()\n"
            )
            self.assertEqual(result.returncode, 1, result.stderr)
            pid = int(pid_path.read_text())
            try:
                status = Path(f"/proc/{pid}/stat")
                if status.exists():
                    self.assertEqual(status.read_text().split()[2], "Z")
            finally:
                try:
                    os.kill(pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
