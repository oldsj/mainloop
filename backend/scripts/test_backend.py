"""Run offline backend discovery with fatal test deadlines and a process-group cap."""

from __future__ import annotations

import argparse
import asyncio
import faulthandler
import json
import logging
import math
import os
import re
import selectors
import signal
import subprocess  # nosec B404 - runs this runner with the current interpreter
import sys
import tempfile
import time
import traceback
import unittest
import uuid
from collections import Counter, defaultdict
from contextlib import contextmanager
from dataclasses import dataclass, field
from pathlib import Path

TEST_SECONDS = 60
SUITE_SECONDS = 540
CLEANUP_SECONDS = 5
STACK_DUMP_SECONDS = 0.5
BACKEND = Path(__file__).resolve().parents[1]
_deadlines = []
_events = None


def emit(event, **values):
    if _events is not None:
        print(json.dumps({"event": event, **values}), file=_events, flush=True)


@contextmanager
def deadline(label):
    print(f"[deadline {TEST_SECONDS}s] {label}", file=sys.stderr, flush=True)
    # A signal/asyncio deadline cannot interrupt a blocked native call reliably.
    # faulthandler prints all thread stacks and exits even in that case.
    faulthandler.dump_traceback_later(TEST_SECONDS, exit=True)
    _deadlines.append((label, time.monotonic() + TEST_SECONDS))
    try:
        yield
    finally:
        faulthandler.cancel_dump_traceback_later()
        _deadlines.pop()
        if _deadlines:
            parent, expires = _deadlines[-1]
            print(f"[deadline resumes] {parent}", file=sys.stderr, flush=True)
            faulthandler.dump_traceback_later(
                max(0.001, expires - time.monotonic()), exit=True
            )


class TimedResult(unittest.TextTestResult):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self.modules = defaultdict(float)
        self.timings = []

    def record_execution(self, test):
        emit("execution", id=test.id())

    def startTest(self, test):
        super().startTest(test)
        self.record_execution(test)


class CappedSuite(unittest.TestSuite):
    """Cap fixtures and the complete test call, including async runner close."""

    def run(self, result, debug=False):
        # Mirror unittest's fixture transitions, but bound the entire leaf call.
        # IsolatedAsyncioTestCase closes its runner after result.stopTest().
        top_level = not getattr(result, "_testRunEntered", False)
        if top_level:
            result._testRunEntered = True
        for index, test in enumerate(self):
            if result.shouldStop:
                break
            if unittest.suite._isnotsuite(test):
                self._tearDownPreviousClass(test, result)
                self._handleModuleFixture(test, result)
                self._handleClassSetUp(test, result)
                result._previousTestClass = type(test)
                if getattr(type(test), "_classSetupFailed", False) or getattr(
                    result, "_moduleSetUpFailed", False
                ):
                    # unittest reports a fixture skip using an ErrorHolder,
                    # without calling startTest for any affected leaf.
                    skipped_fixtures = {
                        f"setUpClass ({unittest.util.strclass(type(test))})",
                        f"setUpModule ({type(test).__module__})",
                    }
                    if any(
                        holder.id() in skipped_fixtures
                        for holder, _reason in result.skipped
                    ):
                        result.record_execution(test)
                    continue
                started = time.monotonic()
                with deadline(test.id()):
                    if debug:
                        test.debug()
                    else:
                        test(result)
                seconds = time.monotonic() - started
                result.modules[type(test).__module__] += seconds
                result.timings.append({"id": test.id(), "seconds": seconds})
                emit(
                    "test",
                    id=test.id(),
                    module=type(test).__module__,
                    seconds=seconds,
                    tests_run=result.testsRun,
                    failures=len(result.failures),
                    errors=len(result.errors),
                    skipped=len(result.skipped),
                )
            elif debug:
                test.run(result, debug=True)
            else:
                test(result)
            if self._cleanup:
                self._removeTestAtIndex(index)
        if top_level:
            self._tearDownPreviousClass(None, result)
            self._handleModuleTearDown(result)
            result._testRunEntered = False
        return result

    @contextmanager
    def fixture(self, result, cls, phase):
        if cls is None:
            yield
            return
        started = time.monotonic()
        accounted = sum(result.modules.values())
        with deadline(f"{cls.__module__}.{cls.__qualname__} {phase}"):
            yield
        # Module setup invokes the previous module's teardown; count it once.
        nested = sum(result.modules.values()) - accounted
        seconds = time.monotonic() - started - nested
        result.modules[cls.__module__] += seconds
        emit("fixture", module=cls.__module__, seconds=seconds)

    def _handleClassSetUp(self, test, result):
        if type(test) is getattr(result, "_previousTestClass", None):
            return super()._handleClassSetUp(test, result)
        with self.fixture(result, type(test), "setUpClass"):
            return super()._handleClassSetUp(test, result)

    def _tearDownPreviousClass(self, test, result):
        previous = getattr(result, "_previousTestClass", None)
        if type(test) is previous:
            return super()._tearDownPreviousClass(test, result)
        with self.fixture(result, previous, "tearDownClass/cleanups"):
            return super()._tearDownPreviousClass(test, result)

    def _handleModuleFixture(self, test, result):
        previous = getattr(result, "_previousTestClass", None)
        if previous and previous.__module__ == type(test).__module__:
            return super()._handleModuleFixture(test, result)
        with self.fixture(result, type(test), "setUpModule"):
            return super()._handleModuleFixture(test, result)

    def _handleModuleTearDown(self, result):
        with self.fixture(
            result,
            getattr(result, "_previousTestClass", None),
            "tearDownModule/cleanups",
        ):
            return super()._handleModuleTearDown(result)


class ConsoleHTTPFilter(logging.Filter):
    def filter(self, record):
        return record.name != "httpx" or record.levelno >= logging.WARNING


def cap_suite(test):
    # load_tests hooks can return their own TestSuite, bypassing loader.suiteClass.
    if isinstance(test, unittest.TestSuite):
        suite = CappedSuite(cap_suite(child) for child in test)
        suite._cleanup = test._cleanup
        return suite
    return test


def leaves(suite):
    for test in suite:
        if isinstance(test, unittest.TestSuite):
            yield from leaves(test)
        else:
            yield test


def inventory(suite):
    return [
        {"id": test.id(), "module": type(test).__module__} for test in leaves(suite)
    ]


def verify_inventory(expected, actual, kind="inventory"):
    expected_ids = Counter(entry["id"] for entry in expected)
    actual_ids = Counter(entry["id"] for entry in actual)
    if expected_ids != actual_ids or any(count != 1 for count in actual_ids.values()):
        raise ValueError(
            f"Test {kind} mismatch: "
            f"missing={list((expected_ids - actual_ids).elements())}, "
            f"extra={list((actual_ids - expected_ids).elements())}, "
            f"duplicates={[name for name, count in actual_ids.items() if count > 1]}"
        )


def distribute(entries, workers, previous):
    """Keep modules intact; stable longest-first balancing or round-robin."""
    modules = sorted({entry["module"] for entry in entries})
    assignments = [[] for _ in range(min(workers, len(modules)) or 1)]
    weights = {
        module: seconds
        for module, seconds in previous.items()
        if module in modules
        and isinstance(seconds, (int, float))
        and math.isfinite(seconds)
        and seconds > 0
    }
    owners = {}
    if weights:
        estimate = sum(weights.values()) / len(weights)
        loads = [0.0] * len(assignments)
        for module in sorted(
            modules, key=lambda name: (-weights.get(name, estimate), name)
        ):
            worker = min(range(len(loads)), key=lambda index: (loads[index], index))
            owners[module] = worker
            loads[worker] += weights.get(module, estimate)
    else:
        owners = {
            module: index % len(assignments) for index, module in enumerate(modules)
        }
    for entry in entries:
        assignments[owners[entry["module"]]].append(entry)
    verify_inventory(entries, [entry for group in assignments for entry in group])
    return assignments


def select_suite(suite, modules):
    # Retain discovery/load_tests ordering and custom suite nesting.
    selected = CappedSuite()
    for test in suite:
        if isinstance(test, unittest.TestSuite):
            child = select_suite(test, modules)
            if child.countTestCases():
                selected.addTest(child)
        elif type(test).__module__ in modules:
            selected.addTest(test)
    selected._cleanup = suite._cleanup
    return selected


def run_tests(names, plan=None, inventory_only=False):
    os.chdir(BACKEND)
    sys.path.insert(0, str(BACKEND))
    faulthandler.register(signal.SIGUSR1, all_threads=True)
    # IsolatedAsyncioTestCase keeps debug checks enabled, but capturing ten
    # creation frames for every IO callback/future is costly in a sandbox.
    # This only shortens diagnostic provenance; exception tracebacks stay full.
    depth = int(os.environ.get("MAINLOOP_TEST_DEBUG_STACK_DEPTH", "1"))
    if depth < 1:
        raise ValueError("MAINLOOP_TEST_DEBUG_STACK_DEPTH must be positive")
    asyncio.constants.DEBUG_STACK_DEPTH = depth
    # CPython's accelerated Future/Task caches extract_stack itself. Adjust its
    # default rather than replacing the function, so those stacks are bounded
    # too. Explicit limits and exception/timeout traceback formatting are intact.
    traceback.extract_stack.__defaults__ = (None, depth)
    loader = unittest.TestLoader()
    loader.suiteClass = CappedSuite
    with deadline("test discovery/imports"):
        suite = (
            loader.loadTestsFromNames(names)
            if names
            else loader.discover("tests", top_level_dir=".")
        )
        suite = cap_suite(suite)
        entries = inventory(suite)
        if plan:
            expected = json.loads(Path(plan).read_text())
            suite = select_suite(suite, {entry["module"] for entry in expected})
            entries = inventory(suite)
            verify_inventory(expected, entries)
        else:
            verify_inventory(entries, entries)
        emit("inventory", tests=entries)
    if inventory_only:
        return 0
    # Filter only the existing console handlers. Log capture added by tests
    # must still receive HTTP records, including credential-leak assertions.
    for handler in logging.getLogger().handlers:
        handler.addFilter(ConsoleHTTPFilter())
    result = unittest.TextTestRunner(verbosity=2, resultclass=TimedResult).run(suite)
    emit(
        "result",
        tests_run=result.testsRun,
        failures=[(test.id(), detail) for test, detail in result.failures],
        errors=[(test.id(), detail) for test, detail in result.errors],
        skipped=len(result.skipped),
    )
    return 0 if result.wasSuccessful() else 1


def report(timings):
    print(f"Backend wall time: {timings['wall_seconds']:.3f}s", file=sys.stderr)
    print(
        f"Ran {timings['tests_run']} tests across {timings['workers']} workers; "
        f"failures={timings['failures']}, errors={timings['errors']}, "
        f"skipped={timings['skipped']}",
        file=sys.stderr,
    )
    print("Slowest modules (including class/module fixtures):", file=sys.stderr)
    for name, seconds in sorted(timings["modules"].items(), key=lambda item: -item[1])[
        :15
    ]:
        print(f"  {seconds:8.3f}s {name}", file=sys.stderr)
    print("Slowest tests (including setup/teardown/cleanups):", file=sys.stderr)
    for entry in sorted(timings["tests"], key=lambda item: -item["seconds"])[:15]:
        print(f"  {entry['seconds']:8.3f}s {entry['id']}", file=sys.stderr)


class Interrupted(Exception):
    def __init__(self, signum):
        self.signum = signum


def kill_group(process, signum):
    try:
        os.killpg(process.pid, signum)
    except ProcessLookupError:
        pass


def cleanup_budget():
    seconds = CLEANUP_SECONDS
    inherited_grace = os.environ.get("MAINLOOP_TIMEOUT_GRACE_MS")
    if inherited_grace is not None:
        parent_seconds = float(inherited_grace) / 1000
        seconds = min(seconds, max(0, parent_seconds - min(1, parent_seconds / 4)))
    return seconds


def cleanup(processes, timed_out, expires, signum=signal.SIGTERM):
    """One cleanup budget for all groups, including descendants of exited workers."""
    if timed_out:
        for process in processes:
            if process.poll() is None:
                try:
                    os.kill(process.pid, signal.SIGUSR1)
                except ProcessLookupError:
                    pass
        # Let every worker dump stacks concurrently before terminating groups.
        time.sleep(min(STACK_DUMP_SECONDS, max(0, expires - time.monotonic())))
    for process in processes:
        kill_group(process, signum)
    # Leave time in the same budget to reclaim databases after killing groups.
    graceful_expires = time.monotonic() + min(1, max(0, expires - time.monotonic()) / 2)
    for process in processes:
        try:
            process.wait(timeout=max(0, graceful_expires - time.monotonic()))
        except subprocess.TimeoutExpired:
            pass
    for process in processes:
        kill_group(process, signal.SIGKILL)
    for process in processes:
        process.wait(timeout=max(0, expires - time.monotonic()))


async def cleanup_databases(namespaces, expires):
    """Drop only this run's class/template names, inside its shared budget."""
    if not namespaces:
        return
    import asyncpg

    pattern = (
        r"^mainloop_(test|template)_("
        + "|".join(re.escape(namespace) for namespace in namespaces)
        + r")_[a-f0-9]{12}$"
    )
    connection = None
    async with asyncio.timeout(max(0, expires - time.monotonic())):
        try:
            connection = await asyncpg.connect(
                os.environ["MAINLOOP_TEST_DATABASE_URL"],
                timeout=max(0.001, expires - time.monotonic()),
            )
            databases = await connection.fetch(
                "SELECT datname FROM pg_database WHERE datname ~ $1", pattern
            )
            for row in databases:
                # Names passed the exact fixture grammar, including UUID suffix.
                await connection.execute(
                    f'DROP DATABASE IF EXISTS "{row["datname"]}" WITH (FORCE)'
                )
        finally:
            if connection is not None:
                connection.terminate()


@dataclass
class Worker:
    name: str
    process: subprocess.Popen
    log: Path
    buffer: bytes = b""
    inventory: list = field(default_factory=list)
    plan: list | None = None
    executed: list = field(default_factory=list)
    counts: dict = field(default_factory=dict)
    result: dict | None = None
    group_cleaned: bool = False


class Supervisor:
    def __init__(self, args, root):
        self.args = args
        self.root = root
        self.started = time.monotonic()
        self.expires = self.started + SUITE_SECONDS
        self.namespace = uuid.uuid4().hex[:12]
        self.workers = []
        self.selector = selectors.DefaultSelector()
        self.pending_signal = None
        self.launching = False
        self.timings = {
            "wall_seconds": 0,
            "workers": args.workers,
            "status": "running",
            "modules": {},
            "tests": [],
            "tests_run": 0,
            "failures": 0,
            "errors": 0,
            "skipped": 0,
            "inventory": [],
            "worker_inventories": {},
            "worker_executions": {},
            "failure_summary": [],
        }

    def interrupted(self, signum, _frame):
        self.pending_signal = signum
        # Defer at every Popen boundary until the new group is registered.
        if not self.launching:
            raise Interrupted(signum)

    def remaining(self):
        seconds = self.expires - time.monotonic()
        if seconds <= 0:
            raise subprocess.TimeoutExpired("backend suite", SUITE_SECONDS)
        return seconds

    def launch(self, name, *, plan=None, inventory_only=False):
        self.remaining()
        arguments = [sys.executable, str(Path(__file__).resolve()), "--worker"]
        if inventory_only:
            arguments.append("--inventory-only")
        if plan is not None:
            path = self.root / f"{name}.plan.json"
            path.write_text(json.dumps(plan))
            arguments.extend(("--plan", str(path)))
        arguments.extend(self.args.names)
        log = self.root / f"{name}.log"
        with log.open("w") as stream:
            self.launching = True
            try:
                process = subprocess.Popen(  # nosec B603 - this runner, explicit argv
                    arguments,
                    start_new_session=True,
                    stdout=subprocess.PIPE,
                    stderr=stream,
                    env={
                        **os.environ,
                        "MAINLOOP_TEST_NAMESPACE": f"{self.namespace}_w{name}",
                    },
                )
                worker = Worker(name, process, log, plan=plan)
                self.workers.append(worker)
                self.selector.register(process.stdout, selectors.EVENT_READ, worker)
            finally:
                self.launching = False
        if self.pending_signal is not None:
            raise Interrupted(self.pending_signal)
        return worker

    def save(self):
        self.timings["wall_seconds"] = time.monotonic() - self.started
        for key in ("tests_run", "failures", "errors", "skipped"):
            self.timings[key] = sum(
                worker.counts.get(key, 0) for worker in self.workers
            )
        if self.args.timings:
            path = Path(self.args.timings)
            temporary = path.with_name(f".{path.name}.{os.getpid()}.tmp")
            temporary.write_text(json.dumps(self.timings, indent=2) + "\n")
            temporary.replace(path)

    def event(self, worker, event):
        kind = event.pop("event")
        if kind == "inventory":
            worker.inventory = event["tests"]
            if worker.name != "discovery":
                self.timings["worker_inventories"][worker.name] = [
                    entry["id"] for entry in worker.inventory
                ]
            if self.args.workers == 1:
                self.timings["inventory"] = [entry["id"] for entry in worker.inventory]
        elif kind == "execution":
            worker.executed.append({"id": event["id"]})
            self.timings["worker_executions"][worker.name] = [
                entry["id"] for entry in worker.executed
            ]
        elif kind in ("test", "fixture"):
            module = event["module"]
            self.timings["modules"][module] = (
                self.timings["modules"].get(module, 0) + event["seconds"]
            )
            if kind == "test":
                self.timings["tests"].append(
                    {
                        "id": event["id"],
                        "seconds": event["seconds"],
                        "worker": worker.name,
                    }
                )
                worker.counts = {
                    key: event[key]
                    for key in ("tests_run", "failures", "errors", "skipped")
                }
        elif kind == "result":
            worker.result = event
            worker.counts = {
                "tests_run": event["tests_run"],
                "failures": len(event["failures"]),
                "errors": len(event["errors"]),
                "skipped": event["skipped"],
            }
            for key in ("failures", "errors"):
                self.timings["failure_summary"].extend(
                    {"worker": worker.name, "kind": key, "id": name, "detail": detail}
                    for name, detail in event[key]
                )
        self.save()

    def collect(self):
        # Read only ready pipes: a hung worker cannot block progress from others.
        while self.selector.get_map():
            for key, _ in self.selector.select(timeout=min(0.1, self.remaining())):
                worker = key.data
                chunk = os.read(key.fd, 65536)
                if not chunk:
                    self.selector.unregister(key.fileobj)
                    key.fileobj.close()
                    continue
                worker.buffer += chunk
                while b"\n" in worker.buffer:
                    line, worker.buffer = worker.buffer.split(b"\n", 1)
                    self.event(worker, json.loads(line))
            for worker in self.workers:
                if worker.process.poll() is not None and not worker.group_cleaned:
                    # Children holding a pipe/group open must not outlive tests.
                    kill_group(worker.process, signal.SIGKILL)
                    worker.group_cleaned = True
        for worker in self.workers:
            worker.process.wait(timeout=self.remaining())
            if not worker.group_cleaned:
                kill_group(worker.process, signal.SIGKILL)
                worker.group_cleaned = True

    def run(self, previous):
        self.save()
        if self.args.workers == 1:
            worker = self.launch("0")
            self.collect()
            expected = worker.inventory
        else:
            discovery = self.launch("discovery", inventory_only=True)
            self.collect()
            if discovery.process.returncode != 0:
                return 1
            plans = distribute(discovery.inventory, self.args.workers, previous)
            expected = discovery.inventory
            self.timings["inventory"] = [entry["id"] for entry in discovery.inventory]
            self.timings["workers"] = len(plans)
            print(
                f"Backend inventory: {len(discovery.inventory)} tests; {len(plans)} workers",
                file=sys.stderr,
                flush=True,
            )
            for index, plan in enumerate(plans):
                self.launch(str(index), plan=plan)
            self.collect()
            verify_inventory(
                discovery.inventory,
                [entry for worker in self.workers[1:] for entry in worker.inventory],
            )
        execution_workers = [
            worker for worker in self.workers if worker.name != "discovery"
        ]
        for worker in execution_workers:
            verify_inventory(
                worker.plan if worker.plan is not None else worker.inventory,
                worker.executed,
                kind=f"execution (worker {worker.name})",
            )
        verify_inventory(
            expected,
            [entry for worker in execution_workers for entry in worker.executed],
            kind="execution (combined)",
        )
        return int(
            any(
                worker.process.returncode != 0
                or (worker.name != "discovery" and worker.result is None)
                for worker in self.workers
            )
        )


def supervise(args):
    previous = {}
    if args.timings and Path(args.timings).exists():
        try:
            previous = json.loads(Path(args.timings).read_text()).get("modules", {})
            if not isinstance(previous, dict):
                previous = {}
        except (ValueError, OSError, AttributeError) as error:
            print(f"Ignoring previous timings: {error}", file=sys.stderr)
    cache = Path.home() / ".cache"
    cache.mkdir(exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="mainloop-tests-", dir=cache) as directory:
        supervisor = Supervisor(args, Path(directory))
        handlers = {
            signum: signal.signal(signum, supervisor.interrupted)
            for signum in (signal.SIGINT, signal.SIGQUIT, signal.SIGTERM)
        }
        code = 1
        try:
            code = supervisor.run(previous)
            supervisor.timings["status"] = "complete" if code == 0 else "failed"
        except subprocess.TimeoutExpired:
            code = 124
            supervisor.timings["status"] = "timed_out"
            print(
                f"Backend suite exceeded {SUITE_SECONDS}s; terminating every worker process group.",
                file=sys.stderr,
                flush=True,
            )
        except Interrupted as error:
            code = 128 + error.signum
            supervisor.timings["status"] = "cancelled"
        except (ValueError, OSError) as error:
            supervisor.timings["status"] = "failed"
            print(str(error), file=sys.stderr, flush=True)
        finally:
            for signum in handlers:
                signal.signal(signum, signal.SIG_IGN)
            try:
                cleanup_expires = time.monotonic() + cleanup_budget()
                try:
                    cleanup(
                        [
                            worker.process
                            for worker in supervisor.workers
                            if not worker.group_cleaned
                        ],
                        code == 124,
                        cleanup_expires,
                        supervisor.pending_signal or signal.SIGTERM,
                    )
                    asyncio.run(
                        cleanup_databases(
                            [
                                f"{supervisor.namespace}_w{worker.name}"
                                for worker in supervisor.workers
                            ],
                            cleanup_expires,
                        )
                    )
                except Exception as error:
                    print(f"Backend cleanup failed: {error}", file=sys.stderr)
                    if code == 0:
                        code = 1
                        supervisor.timings["status"] = "failed"
                supervisor.selector.close()
                for worker in supervisor.workers:
                    if worker.process.stdout and not worker.process.stdout.closed:
                        worker.process.stdout.close()
                    if worker.process.returncode != 0 or worker.result is None:
                        if (
                            worker.name == "discovery"
                            and worker.process.returncode == 0
                        ):
                            continue
                        print(
                            f"\nWorker {worker.name} (exit {worker.process.returncode}):",
                            file=sys.stderr,
                        )
                        print(worker.log.read_text(), file=sys.stderr)
                supervisor.save()
                report(supervisor.timings)
                if supervisor.timings["failure_summary"]:
                    print("\nMerged failure summary:", file=sys.stderr)
                    for failure in supervisor.timings["failure_summary"]:
                        print(
                            f"  worker {failure['worker']}: {failure['id']} ({failure['kind']})\n"
                            + failure["detail"],
                            file=sys.stderr,
                        )
            finally:
                for signum, handler in handlers.items():
                    signal.signal(signum, handler)
        return code


def main():
    global _events
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "names", nargs="*", help="optional unittest module/class/test names"
    )
    parser.add_argument("--timings", default=os.environ.get("MAINLOOP_TEST_TIMINGS"))
    parser.add_argument("--worker", action="store_true", help=argparse.SUPPRESS)
    parser.add_argument("--inventory-only", action="store_true", help=argparse.SUPPRESS)
    parser.add_argument("--plan", help=argparse.SUPPRESS)
    parser.add_argument(
        "--workers",
        type=int,
        default=os.environ.get("MAINLOOP_TEST_WORKERS", min(os.cpu_count() or 1, 6)),
    )
    args = parser.parse_args()
    if args.worker:
        # Reserve the pipe for events; test output goes into the worker's log.
        _events = sys.stdout
        sys.stdout = sys.stderr
        return run_tests(args.names, args.plan, args.inventory_only)
    if args.workers < 1:
        parser.error("MAINLOOP_TEST_WORKERS must be positive")
    if not os.environ.get("MAINLOOP_TEST_DATABASE_URL"):
        parser.error(
            "set MAINLOOP_TEST_DATABASE_URL to a disposable PostgreSQL server (or use dev-postgres run make test-backend)"
        )
    return supervise(args)


if __name__ == "__main__":
    sys.exit(main())
