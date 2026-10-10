"""Real subprocess probes for the deadlines agents rely on."""

import json
import os
import signal
import subprocess  # nosec B404 - local interpreter and synthetic offline fixtures
import sys
import tempfile
import unittest
from pathlib import Path

from scripts import test_backend as runner

BACKEND = Path(__file__).resolve().parents[1]


class BackendRunnerTests(unittest.TestCase):
    def probe(
        self,
        source,
        *,
        suite_seconds=5,
        supervisor_setup="",
        workers=1,
        database_url="postgresql://fixture-unused/postgres",
        previous=None,
    ):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            sources = {"probe": source} if isinstance(source, str) else source
            for name, text in sources.items():
                (root / f"{name}.py").write_text(text)
            if previous is not None:
                (root / "timings.json").write_text(json.dumps(previous))
            env = {
                **os.environ,
                "MAINLOOP_TEST_DATABASE_URL": database_url,
                "MAINLOOP_TEST_WORKERS": str(workers),
                "PYTHONPATH": os.pathsep.join((str(root), str(BACKEND))),
            }
            result = subprocess.run(  # nosec B603 - synthetic local test module
                [
                    sys.executable,
                    "-c",
                    "import sys\nfrom scripts import test_backend as runner\n"
                    f"runner.SUITE_SECONDS = {suite_seconds!r}\n"
                    + supervisor_setup
                    + "\nsys.argv = ['test_backend', '--timings', sys.argv[1], *sys.argv[2:]]\n"
                    "sys.exit(runner.main())",
                    str(root / "timings.json"),
                    *sources,
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

    def test_distribution_is_deterministic_and_uses_recorded_module_times(self):
        entries = [
            {"id": f"{module}.Case.test_{index}", "module": module}
            for module in ("a", "b", "c", "d")
            for index in range(2)
        ]
        fallback = runner.distribute(entries, 2, {})
        self.assertEqual(
            [{entry["module"] for entry in group} for group in fallback],
            [{"a", "c"}, {"b", "d"}],
        )
        weights = {"a": 10, "b": 9, "c": 2, "d": 1}
        balanced = runner.distribute(entries, 2, weights)
        self.assertEqual(
            balanced,
            runner.distribute(entries, 2, dict(reversed(list(weights.items())))),
        )
        self.assertEqual(
            [{entry["module"] for entry in group} for group in balanced],
            [{"a", "d"}, {"b", "c"}],
        )
        runner.verify_inventory(
            entries, [entry for group in balanced for entry in group]
        )
        for invalid in (entries[:-1], entries + entries[:1]):
            with self.assertRaisesRegex(ValueError, "inventory mismatch"):
                runner.verify_inventory(entries, invalid)

    def test_parallel_inventory_matches_serial_discovery_and_load_tests(self):
        sources = {
            "probe_a": (
                "import unittest\n"
                "class Case(unittest.TestCase):\n"
                " def test_a(self): pass\n"
                " def test_b(self): pass\n"
                "def load_tests(loader, tests, pattern):\n"
                " return unittest.TestSuite([unittest.TestSuite([Case('test_b')])])\n"
            ),
            "probe_b": "import unittest\nclass Case(unittest.TestCase):\n def test_c(self): pass\n",
        }
        serial, expected = self.probe(sources)
        parallel, actual = self.probe(sources, workers=2)
        self.assertEqual(serial.returncode, 0, serial.stderr)
        self.assertEqual(parallel.returncode, 0, parallel.stderr)
        self.assertEqual(actual["workers"], 2)
        self.assertEqual(actual["inventory"], expected["inventory"])
        self.assertCountEqual(
            [entry["id"] for entry in actual["tests"]], actual["inventory"]
        )
        self.assertCountEqual(
            [name for names in actual["worker_inventories"].values() for name in names],
            expected["inventory"],
        )
        self.assertEqual(actual["tests_run"], 2)

    def test_worker_inventory_drift_is_rejected(self):
        result, timings = self.probe(
            {
                "probe": (
                    "import os, unittest\n"
                    "class Case(unittest.TestCase):\n"
                    " def test_ok(self): pass\n"
                    "def load_tests(loader, tests, pattern):\n"
                    " return unittest.TestSuite([]) if os.environ['MAINLOOP_TEST_NAMESPACE'].endswith('w0') else tests\n"
                )
            },
            workers=2,
        )
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn("inventory mismatch", result.stderr)
        self.assertEqual(timings["status"], "failed")

    def test_one_worker_failure_preserves_other_worker_results(self):
        result, timings = self.probe(
            {
                "probe_bad": "import unittest\nclass Case(unittest.TestCase):\n def test_bad(self): self.fail('parallel probe failure')\n",
                "probe_good": "import time, unittest\nclass Case(unittest.TestCase):\n def test_ok(self): time.sleep(0.1)\n",
            },
            workers=2,
        )
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertEqual(timings["tests_run"], 2)
        self.assertEqual(timings["failures"], 1)
        self.assertEqual(timings["failure_summary"][0]["id"], "probe_bad.Case.test_bad")
        self.assertIn("Merged failure summary", result.stderr)
        self.assertIn("parallel probe failure", result.stderr)

    def test_finished_timings_are_visible_before_later_test_returns(self):
        # The second test reads the public JSON while the worker is still running.
        result, timings = self.probe(
            "import json, os, time, unittest\n"
            "from pathlib import Path\n"
            "class Case(unittest.TestCase):\n"
            " def test_a_done(self): pass\n"
            " def test_b_read_timings(self):\n"
            "  path = Path(os.environ['PROBE_TIMINGS'])\n"
            "  expires = time.monotonic() + 2\n"
            "  while time.monotonic() < expires:\n"
            "   data = json.loads(path.read_text())\n"
            "   if data['tests']: break\n"
            "   time.sleep(0.01)\n"
            "  self.assertEqual(data['status'], 'running')\n"
            "  self.assertEqual(data['tests'][0]['id'], 'probe.Case.test_a_done')\n",
            supervisor_setup="import os\nos.environ['PROBE_TIMINGS'] = sys.argv[1]\n",
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(timings["tests_run"], 2)

    def test_incremental_timings_survive_global_timeout(self):
        result, timings = self.probe(
            {
                "probe_fast": "import unittest\nclass Case(unittest.TestCase):\n def test_done(self): pass\n",
                "probe_slow": "import threading, unittest\nclass Case(unittest.TestCase):\n def test_hung(self): threading.Event().wait()\n",
            },
            suite_seconds=2,
            workers=2,
        )
        self.assertEqual(result.returncode, 124, result.stderr)
        self.assertEqual(timings["status"], "timed_out")
        self.assertEqual(timings["tests_run"], 1)
        self.assertEqual(timings["tests"][0]["id"], "probe_fast.Case.test_done")
        self.assertIn("probe_fast", timings["modules"])
        self.assertIn("threading.py", result.stderr)

    def test_global_timeout_and_cancellation_kill_all_worker_groups(self):
        for signum in (None, signal.SIGINT, signal.SIGTERM):
            with self.subTest(
                signal=signum
            ), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                sources = {}
                for name in ("probe_a", "probe_b"):
                    sources[name] = (
                        "import os, signal, subprocess, sys, threading, unittest\n"
                        "from pathlib import Path\n"
                        "class Case(unittest.TestCase):\n"
                        " def test_hung(self):\n"
                        "  child = subprocess.Popen([sys.executable, '-c', 'import signal, time; signal.signal(signal.SIGTERM, signal.SIG_IGN); time.sleep(60)'])\n"
                        f"  Path({str(root / (name + '.pid'))!r}).write_text(str(os.getpid()) + ' ' + str(child.pid))\n"
                        "  threading.Event().wait()\n"
                    )
                setup = ""
                if signum is not None:
                    setup = (
                        "import os, signal, threading, time\nfrom pathlib import Path\n"
                        "def cancel():\n"
                        " expires = time.monotonic() + 5\n"
                        f" while not all(path.exists() for path in Path({str(root)!r}).glob('*.pid')) or len(list(Path({str(root)!r}).glob('*.pid'))) != 2:\n"
                        "  if time.monotonic() >= expires: return\n"
                        "  time.sleep(0.01)\n"
                        f" os.kill(os.getpid(), {int(signum)})\n"
                        "threading.Thread(target=cancel, daemon=True).start()\n"
                    )
                try:
                    result, _ = self.probe(
                        sources,
                        workers=2,
                        suite_seconds=2 if signum is None else 6,
                        supervisor_setup=setup,
                    )
                    self.assertEqual(
                        result.returncode,
                        124 if signum is None else 128 + signum,
                        result.stderr,
                    )
                    for name in sources:
                        path = root / (name + ".pid")
                        self.assertTrue(path.exists(), result.stderr)
                        for pid in path.read_text().split():
                            status = Path(f"/proc/{pid}/stat")
                            if status.exists():
                                self.assertEqual(status.read_text().split()[2], "Z")
                finally:
                    for path in root.glob("*.pid"):
                        worker, child = map(int, path.read_text().split())
                        for pid in (worker, child):
                            try:
                                os.kill(pid, signal.SIGKILL)
                            except ProcessLookupError:
                                pass

    @unittest.skipUnless(
        os.environ.get("MAINLOOP_TEST_DATABASE_URL"), "scratch PostgreSQL required"
    )
    def test_parallel_worker_database_and_template_isolation(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            sources = {}
            for name in ("probe_a", "probe_b"):
                sources[name] = (
                    "import asyncio, os\nfrom pathlib import Path\n"
                    "from tests.runtime import test_postgres_ledger as ledger\n"
                    "class Case(ledger.PostgresTestCase):\n"
                    " async def test_isolated(self):\n"
                    "  namespace = os.environ['MAINLOOP_TEST_NAMESPACE']\n"
                    "  self.assertTrue(self.database.startswith('mainloop_test_' + namespace + '_'))\n"
                    "  self.assertTrue(ledger._template_database.startswith('mainloop_template_' + namespace + '_'))\n"
                    "  await self.pool.execute('CREATE TABLE worker_probe (value text)')\n"
                    "  await self.pool.execute('INSERT INTO worker_probe VALUES ($1)', namespace)\n"
                    "  root = Path(os.environ['PROBE_ROOT'])\n"
                    "  (root / __name__).write_text(self.database + '\\n' + ledger._template_database)\n"
                    "  async with asyncio.timeout(5):\n"
                    "   while len(list(root.iterdir())) < 2: await asyncio.sleep(0.01)\n"
                    "  self.assertEqual(await self.pool.fetchval('SELECT value FROM worker_probe'), namespace)\n"
                )
            result, timings = self.probe(
                sources,
                workers=2,
                suite_seconds=10,
                database_url=os.environ["MAINLOOP_TEST_DATABASE_URL"],
                supervisor_setup=f"import os\nos.environ['PROBE_ROOT'] = {str(root)!r}\n",
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(timings["tests_run"], 2)
            names = [path.read_text().splitlines() for path in root.iterdir()]
            self.assertEqual(len(names), 2)
            self.assertEqual(len({name for pair in names for name in pair}), 4)

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
