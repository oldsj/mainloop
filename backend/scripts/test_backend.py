"""Run offline backend discovery with fatal test deadlines and a process-group cap."""

from __future__ import annotations

import argparse
import asyncio
import faulthandler
import json
import logging
import os
import signal
import subprocess  # nosec B404 - runs this runner with the current interpreter
import sys
import time
import traceback
import unittest
from collections import defaultdict
from contextlib import contextmanager
from pathlib import Path

TEST_SECONDS = 60
SUITE_SECONDS = 540
CLEANUP_SECONDS = 5
STACK_DUMP_SECONDS = 0.5
BACKEND = Path(__file__).resolve().parents[1]
_deadlines = []


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
        result.modules[cls.__module__] += time.monotonic() - started - nested

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


def run_tests(names, timings):
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
    started = time.monotonic()
    loader = unittest.TestLoader()
    loader.suiteClass = CappedSuite
    with deadline("test discovery/imports"):
        suite = (
            loader.loadTestsFromNames(names)
            if names
            else loader.discover("tests", top_level_dir=".")
        )
        suite = cap_suite(suite)
    # Filter only the existing console handlers. Log capture added by tests
    # must still receive HTTP records, including credential-leak assertions.
    for handler in logging.getLogger().handlers:
        handler.addFilter(ConsoleHTTPFilter())
    result = unittest.TextTestRunner(verbosity=2, resultclass=TimedResult).run(suite)
    wall_seconds = time.monotonic() - started
    print(f"Backend wall time: {wall_seconds:.3f}s", file=sys.stderr)
    print("Slowest modules (including class/module fixtures):", file=sys.stderr)
    for name, seconds in sorted(result.modules.items(), key=lambda item: -item[1])[:15]:
        print(f"  {seconds:8.3f}s {name}", file=sys.stderr)
    print("Slowest tests (including setup/teardown/cleanups):", file=sys.stderr)
    for entry in sorted(result.timings, key=lambda item: -item["seconds"])[:15]:
        print(f"  {entry['seconds']:8.3f}s {entry['id']}", file=sys.stderr)
    if timings:
        Path(timings).write_text(
            json.dumps(
                {
                    "wall_seconds": wall_seconds,
                    "modules": dict(result.modules),
                    "tests": result.timings,
                    "tests_run": result.testsRun,
                    "failures": len(result.failures),
                    "errors": len(result.errors),
                    "skipped": len(result.skipped),
                },
                indent=2,
            )
            + "\n"
        )
    return 0 if result.wasSuccessful() else 1


class Interrupted(Exception):
    def __init__(self, signum):
        self.signum = signum


def supervise(arguments):
    process = None
    pending_signal = None
    timed_out = False

    def interrupted(signum, _frame):
        nonlocal pending_signal
        pending_signal = signum
        # Popen must return the group leader's identity before cancellation can
        # unwind. A signal in the launch/assignment window is handled below.
        if process is not None:
            raise Interrupted(signum)

    try:
        for signum in (signal.SIGINT, signal.SIGTERM):
            signal.signal(signum, interrupted)
        process = subprocess.Popen(  # nosec B603 - this file, explicit interpreter/argv
            [sys.executable, str(Path(__file__).resolve()), "--worker", *arguments],
            start_new_session=True,
        )
        if pending_signal is not None:
            raise Interrupted(pending_signal)
        return process.wait(timeout=SUITE_SECONDS)
    except subprocess.TimeoutExpired:
        timed_out = True
        print(
            f"Backend suite exceeded {SUITE_SECONDS}s; terminating its process group.",
            file=sys.stderr,
            flush=True,
        )
        return 124
    except Interrupted as error:
        return 128 + error.signum
    finally:
        # Also remove subprocesses left behind after a fatal per-test deadline.
        for signum in (signal.SIGINT, signal.SIGTERM):
            signal.signal(signum, signal.SIG_IGN)
        if process is not None:
            cleanup_expires = time.monotonic() + CLEANUP_SECONDS
            if timed_out:
                try:
                    os.kill(process.pid, signal.SIGUSR1)
                    # Give faulthandler time to write actual frames before TERM.
                    # This interval is part of the five-second cleanup budget.
                    process.wait(timeout=STACK_DUMP_SECONDS)
                except (ProcessLookupError, subprocess.TimeoutExpired):
                    pass
            try:
                os.killpg(process.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            try:
                process.wait(timeout=max(0, cleanup_expires - time.monotonic()))
            except subprocess.TimeoutExpired:
                pass
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.wait()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "names", nargs="*", help="optional unittest module/class/test names"
    )
    parser.add_argument("--timings", default=os.environ.get("MAINLOOP_TEST_TIMINGS"))
    parser.add_argument("--worker", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args()
    if args.worker:
        return run_tests(args.names, args.timings)
    if not os.environ.get("MAINLOOP_TEST_DATABASE_URL"):
        parser.error(
            "set MAINLOOP_TEST_DATABASE_URL to a disposable PostgreSQL server (or use dev-postgres run make test-backend)"
        )
    return supervise(sys.argv[1:])


if __name__ == "__main__":
    sys.exit(main())
