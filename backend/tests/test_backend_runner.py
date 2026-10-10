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
    def probe(self, source, *, suite_seconds=5, supervisor_setup=""):
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
                    "import sys\nfrom scripts import test_backend as runner\n"
                    f"runner.SUITE_SECONDS = {suite_seconds!r}\n"
                    + supervisor_setup
                    + "\nsys.argv = ['test_backend', '--timings', sys.argv[1], 'probe']\n"
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
        self.assertIn("threading.py", result.stderr)
        self.assertIn('File "', result.stderr)
        self.assertIn("in wait", result.stderr)

    def test_load_tests_suites_have_fixture_deadlines(self):
        fixtures = {
            "setUpModule": "def setUpModule(): threading.Event().wait()\n",
            "setUpClass": (
                "Case.setUpClass = classmethod(lambda cls: threading.Event().wait())\n"
            ),
            "tearDownClass/cleanups": (
                "Case.setUpClass = classmethod(lambda cls: cls.addClassCleanup(threading.Event().wait))\n"
            ),
            "tearDownModule/cleanups": (
                "def setUpModule(): unittest.addModuleCleanup(threading.Event().wait)\n"
            ),
        }
        for phase, fixture in fixtures.items():
            with self.subTest(phase=phase):
                result, _ = self.probe(
                    "import sys, threading, unittest\n"
                    "sys.modules['__main__'].TEST_SECONDS = 0.1\n"
                    "class Case(unittest.TestCase):\n"
                    " def test_ok(self): pass\n"
                    + fixture
                    + "def load_tests(loader, tests, pattern):\n"
                    " return unittest.TestSuite([unittest.TestSuite([Case('test_ok')])])\n",
                    suite_seconds=2,
                )
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertIn(f"probe.Case {phase}", result.stderr)
                self.assertIn("Timeout (0:00:00.100000)!", result.stderr)

    def test_load_tests_fixture_timings_include_setup_and_cleanup(self):
        result, timings = self.probe(
            "import time, unittest\n"
            "def setUpModule(): time.sleep(0.02)\n"
            "class Case(unittest.TestCase):\n"
            " @classmethod\n"
            " def setUpClass(cls): cls.addClassCleanup(time.sleep, 0.02)\n"
            " def test_ok(self): pass\n"
            "def load_tests(loader, tests, pattern):\n"
            " return unittest.TestSuite([unittest.TestSuite([Case('test_ok')])])\n"
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(timings["tests_run"], 1)
        self.assertGreaterEqual(timings["modules"]["probe"], 0.04)

    def test_async_runner_shutdown_has_test_deadline(self):
        shutdown_cases = {
            "pending task": (
                "  async def pending():\n"
                "   try: await asyncio.Event().wait()\n"
                "   except asyncio.CancelledError: threading.Event().wait()\n"
                "  asyncio.create_task(pending())\n"
            ),
            "default executor": (
                "  asyncio.create_task(asyncio.to_thread(threading.Event().wait))\n"
            ),
        }
        for label, source in shutdown_cases.items():
            with self.subTest(shutdown=label):
                result, _ = self.probe(
                    "import asyncio, sys, threading, unittest\n"
                    "sys.modules['__main__'].TEST_SECONDS = 0.1\n"
                    "class Case(unittest.IsolatedAsyncioTestCase):\n"
                    " async def test_ok(self):\n"
                    + source
                    + "  await asyncio.sleep(0.02)\n",
                    suite_seconds=2,
                )
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertIn("probe.Case.test_ok", result.stderr)
                self.assertIn("Timeout (0:00:00.100000)!", result.stderr)
                self.assertIn("_tearDownAsyncioRunner", result.stderr)

    def test_async_runner_shutdown_is_included_in_test_timing(self):
        result, timings = self.probe(
            "import time, unittest\n"
            "class Case(unittest.IsolatedAsyncioTestCase):\n"
            " def _tearDownAsyncioRunner(self):\n"
            "  if not getattr(self, 'closed', False):\n"
            "   time.sleep(0.05)\n"
            "   self.closed = True\n"
            "  super()._tearDownAsyncioRunner()\n"
            " async def test_ok(self): pass\n"
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertGreaterEqual(timings["tests"][0]["seconds"], 0.05)

    def test_cancellation_during_launch_cleans_worker_and_descendant(self):
        for signum in (signal.SIGINT, signal.SIGTERM):
            with self.subTest(
                signal=signum
            ), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                worker_path = root / "worker.pid"
                child_path = root / "child.pid"
                try:
                    result, _ = self.probe(
                        "import subprocess, sys, threading, unittest\n"
                        "from pathlib import Path\n"
                        "class Case(unittest.TestCase):\n"
                        " def test_hung(self):\n"
                        "  child = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(60)'])\n"
                        f"  Path({str(child_path)!r}).write_text(str(child.pid))\n"
                        "  threading.Event().wait()\n",
                        supervisor_setup=(
                            "import signal, time\nfrom pathlib import Path\n"
                            "native_launch = runner.subprocess.Popen\n"
                            "def interrupted_launch(*args, **kwargs):\n"
                            " process = native_launch(*args, **kwargs)\n"
                            f" Path({str(worker_path)!r}).write_text(str(process.pid))\n"
                            " expires = time.monotonic() + 5\n"
                            f" while not Path({str(child_path)!r}).exists():\n"
                            "  if time.monotonic() >= expires: raise RuntimeError('child did not start')\n"
                            "  time.sleep(0.01)\n"
                            f" signal.raise_signal({int(signum)})\n"
                            " return process\n"
                            "runner.subprocess.Popen = interrupted_launch\n"
                        ),
                    )
                    self.assertEqual(result.returncode, 128 + signum, result.stderr)
                    for path in (worker_path, child_path):
                        status = Path(f"/proc/{int(path.read_text())}/stat")
                        if status.exists():
                            self.assertEqual(status.read_text().split()[2], "Z")
                finally:
                    if worker_path.exists():
                        try:
                            os.killpg(int(worker_path.read_text()), signal.SIGKILL)
                        except ProcessLookupError:
                            pass

    def test_contributor_commands_identify_pending_workspace_qualification(self):
        text = " ".join((BACKEND.parent / "CONTRIBUTING.md").read_text().split())
        self.assertIn(
            "gVisor/arm64 workspace qualification of `make test-backend` is pending.",
            text,
        )
        self.assertIn("60 seconds/test, 9 minutes/suite", text)
        self.assertNotIn(
            "exact commands verified to work in a Mainloop workspace", text
        )

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
