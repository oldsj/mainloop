"""Substrate transport adapter over a fake ``kubectl ate`` subprocess: no cluster, no actors,
no credentials. Uses a fake transport to test the Substrate control adapter."""

import asyncio
import json
import unittest
from pathlib import Path

from mainloop.runtime.substrate import (
    ActorFailedToStart,
    ActorHealthTimeout,
    ActorState,
    ExecResult,
    GoldenSnapshotFailed,
    GoldenSnapshotTimeout,
    GoldenState,
    IdentityOutcome,
    NoEligibleWorker,
    SubstrateControl,
    TransportError,
    WaitTimeout,
    _actor_from_json,
    _actor_template_from_json,
    reconcile_actor_identity,
    wait_for_actor_health,
    wait_for_actor_running,
    wait_for_eligible_worker,
    wait_for_golden_snapshot,
)

# Generated with kubectl-ate's PrintWorkersTo(..., "json") at pinned Substrate commit
# cdac9baef81dd319b46086d695266e6161e9e592. It is CLI printer output with sanitized fixture
# records, not a claim that a live worker was observed.
PINNED_WORKERS_OUTPUT = (
    Path(__file__).parent
    / "fixtures"
    / "substrate"
    / "kubectl-ate-workers-pinned-cdac9ba.json"
).read_text()
PINNED_WORKERS_DOCUMENT = json.loads(PINNED_WORKERS_OUTPUT)


class FakeControl(SubstrateControl):
    def __init__(self, results):
        super().__init__(
            kubeconfig="fixture-kubeconfig", context="kind-substrate-preview"
        )
        self.results = list(results)
        self.calls: list[list[str]] = []
        self.stdins: list[str | None] = []

    async def _exec(self, args, timeout=45, stdin=None):
        self.calls.append(args)
        self.stdins.append(stdin)
        if args and args[0] in {"get", "create", "resume", "suspend", "revert"}:
            if "-o" not in args or args[args.index("-o") + 1] != "json":
                return ExecResult(0, "NAME STATUS\nresource Pending\n", "")
        result = self.results.pop(0)
        if isinstance(result, Exception):
            raise result
        return result


async def fake_sleep(_seconds: float) -> None:
    return None


class FakeClock:
    """A monotonic clock that advances by a fixed step every time it's read, so a bounded
    poll loop can be driven to its deadline deterministically without a real delay."""

    def __init__(self, step: float = 1.0):
        self.value = 0.0
        self.step = step

    def __call__(self) -> float:
        current = self.value
        self.value += self.step
        return current


def run(coro):
    return asyncio.run(coro)


def actor_json(state: str, *, snapshot_uri: str | None = None, uid: str = "u-1") -> str:
    doc = {
        "metadata": {"atespace": "mainloop-workspaces", "name": "ml-abc", "uid": uid},
        "status": {"state": state},
    }
    if snapshot_uri:
        doc["status"]["externalSnapshot"] = {"snapshotUri": snapshot_uri}
    return json.dumps(doc)


def actor_template_json(
    *, error_message: str = "", golden_tag: str | None = None, uid: str = "t-1"
) -> str:
    doc = {
        "metadata": {
            "atespace": "live-agent-gate",
            "name": "live-agent-gate-v1",
            "uid": uid,
        },
        "status": {"goldenSnapshotStatus": {}},
    }
    if error_message:
        doc["status"]["goldenSnapshotStatus"]["errorMessage"] = error_message
    if golden_tag:
        doc["status"]["goldenSnapshotStatus"]["goldenTag"] = {
            "atespace": "ate-golden",
            "name": golden_tag,
        }
    return json.dumps(doc)


def worker_record(
    *,
    state="WORKER_STATE_ACTIVE",
    namespace="live-agent-gate",
    pool_label="live-agent-gate",
    sandbox_class="gvisor",
    capacity=1,
    allocated=0,
):
    record = json.loads(json.dumps(PINNED_WORKERS_DOCUMENT["workers"][0]))
    record["workerNamespace"] = namespace
    record["sandboxClass"] = sandbox_class
    record["labels"]["workload"] = pool_label
    record["status"]["state"] = state
    record["status"]["capacity"]["actors"] = capacity
    record["status"]["allocated"]["actors"] = allocated
    return record


class ActorJsonParsingTests(unittest.TestCase):
    def test_parses_running_actor_with_snapshot(self):
        import json

        doc = json.loads(actor_json("ACTOR_STATE_RUNNING", snapshot_uri="gs://b/p"))
        record = _actor_from_json(doc)
        self.assertEqual(record.atespace, "mainloop-workspaces")
        self.assertEqual(record.name, "ml-abc")
        self.assertEqual(record.state, ActorState.RUNNING)
        self.assertEqual(record.external_snapshot_uri, "gs://b/p")

    def test_unrecognized_state_string_is_unspecified_not_a_crash(self):
        record = _actor_from_json(
            {"metadata": {}, "status": {"state": "SOME_FUTURE_STATE"}}
        )
        self.assertEqual(record.state, ActorState.UNSPECIFIED)

    def test_missing_status_defaults_to_unspecified(self):
        record = _actor_from_json({"metadata": {"atespace": "a", "name": "n"}})
        self.assertEqual(record.state, ActorState.UNSPECIFIED)
        self.assertIsNone(record.external_snapshot_uri)


class SubstrateControlTests(unittest.TestCase):
    def test_get_actor_parses_json_and_uses_argv_not_shell(self):
        ctl = FakeControl([ExecResult(0, actor_json("ACTOR_STATE_RUNNING"), "")])
        actor = run(ctl.get_actor("mainloop-workspaces", "ml-abc"))
        self.assertEqual(actor.state, ActorState.RUNNING)
        self.assertEqual(
            ctl.calls[0],
            [
                "get",
                "actor",
                "ml-abc",
                "--atespace",
                "mainloop-workspaces",
                "-o",
                "json",
            ],
        )

    def test_get_actor_not_found_returns_none_not_an_exception(self):
        ctl = FakeControl(
            [ExecResult(1, "", 'Error: actors.ate.dev "ml-abc" not found')]
        )
        self.assertIsNone(run(ctl.get_actor("mainloop-workspaces", "ml-abc")))

    def test_get_actor_other_failure_raises_transport_error(self):
        ctl = FakeControl([ExecResult(1, "", "connection refused")])
        with self.assertRaises(TransportError):
            run(ctl.get_actor("mainloop-workspaces", "ml-abc"))

    def test_create_actor_uses_template_flag(self):
        # A freshly created actor starts SUSPENDED (never auto-started); measured against a
        # live kind-substrate-preview cluster while building this adapter.
        ctl = FakeControl([ExecResult(0, actor_json("ACTOR_STATE_SUSPENDED"), "")])
        run(
            ctl.create_actor(
                "mainloop-workspaces", "ml-abc", template="mainloop-workspace"
            )
        )
        self.assertEqual(
            ctl.calls[0],
            [
                "create",
                "actor",
                "ml-abc",
                "--atespace",
                "mainloop-workspaces",
                "--template",
                "mainloop-workspace",
                "-o",
                "json",
            ],
        )

    def test_create_actor_falls_back_to_a_read_when_output_is_empty(self):
        ctl = FakeControl(
            [
                ExecResult(0, "", ""),
                ExecResult(0, actor_json("ACTOR_STATE_RESUMING"), ""),
            ]
        )
        actor = run(ctl.create_actor("mainloop-workspaces", "ml-abc", template="t"))
        self.assertEqual(actor.state, ActorState.RESUMING)
        self.assertEqual(ctl.calls[1][:2], ["get", "actor"])

    def test_revert_is_a_single_explicit_call_never_a_retry_loop(self):
        ctl = FakeControl([ExecResult(0, actor_json("ACTOR_STATE_SUSPENDED"), "")])
        actor = run(ctl.revert_actor("mainloop-workspaces", "ml-abc"))
        self.assertEqual(actor.state, ActorState.SUSPENDED)
        self.assertEqual(len(ctl.calls), 1)
        self.assertEqual(ctl.calls[0][:2], ["revert", "actor"])

    def test_delete_actor_tolerates_already_gone(self):
        ctl = FakeControl([ExecResult(1, "", "not found")])
        run(ctl.delete_actor("mainloop-workspaces", "ml-abc"))  # does not raise

    def test_transport_error_on_transient_failure_is_not_retried(self):
        ctl = FakeControl([TransportError("boom")])
        with self.assertRaises(TransportError):
            run(ctl.suspend_actor("mainloop-workspaces", "ml-abc"))
        self.assertEqual(len(ctl.calls), 1)


class AtespaceTests(unittest.TestCase):
    def test_ensure_atespace_creates(self):
        ctl = FakeControl([ExecResult(0, "{}", "")])
        run(ctl.ensure_atespace("live-agent-gate"))
        self.assertEqual(
            ctl.calls[0], ["create", "atespace", "live-agent-gate", "-o", "json"]
        )

    def test_ensure_atespace_tolerates_already_exists(self):
        ctl = FakeControl(
            [
                ExecResult(
                    1, "", 'Error: atespaces.ate.dev "live-agent-gate" already exists'
                )
            ]
        )
        run(ctl.ensure_atespace("live-agent-gate"))  # does not raise

    def test_ensure_atespace_other_failure_raises(self):
        ctl = FakeControl([ExecResult(1, "", "connection refused")])
        with self.assertRaises(TransportError):
            run(ctl.ensure_atespace("live-agent-gate"))

    def test_atespace_exists_false_on_not_found(self):
        ctl = FakeControl([ExecResult(1, "", 'Error: atespaces.ate.dev "x" not found')])
        self.assertFalse(run(ctl.atespace_exists("x")))


class ActorTemplateJsonParsingTests(unittest.TestCase):
    def test_pending_when_no_tag_and_no_error(self):
        record = _actor_template_from_json(json.loads(actor_template_json()))
        self.assertEqual(record.golden_state, GoldenState.PENDING)

    def test_ready_when_golden_tag_present(self):
        record = _actor_template_from_json(
            json.loads(actor_template_json(golden_tag="golden-1"))
        )
        self.assertEqual(record.golden_state, GoldenState.READY)
        self.assertEqual(record.golden_tag, "golden-1")

    def test_failed_when_error_message_present(self):
        record = _actor_template_from_json(
            json.loads(actor_template_json(error_message="golden actor crashed"))
        )
        self.assertEqual(record.golden_state, GoldenState.FAILED)
        self.assertEqual(record.error_message, "golden actor crashed")


class ActorTemplateControlTests(unittest.TestCase):
    def test_get_actor_template_uses_atespace_flag_not_a_composite_name(self):
        # The prior harness bug: `get actor-template "<atespace>/<name>"` as a single
        # positional argument, which the CLI requires as separate `-a <atespace> <name>`.
        ctl = FakeControl([ExecResult(0, actor_template_json(golden_tag="g1"), "")])
        run(ctl.get_actor_template("live-agent-gate", "live-agent-gate-v1"))
        self.assertEqual(
            ctl.calls[0],
            [
                "get",
                "actor-template",
                "live-agent-gate-v1",
                "-a",
                "live-agent-gate",
                "-o",
                "json",
            ],
        )

    def test_get_actor_template_not_found_returns_none(self):
        ctl = FakeControl(
            [ExecResult(1, "", 'Error: actortemplates.ate.dev "x" not found')]
        )
        self.assertIsNone(run(ctl.get_actor_template("live-agent-gate", "x")))

    def test_get_actor_template_other_failure_raises_transport_error(self):
        ctl = FakeControl([ExecResult(1, "", "connection refused")])
        with self.assertRaises(TransportError):
            run(ctl.get_actor_template("live-agent-gate", "x"))

    def test_create_actor_template_requests_json_and_passes_manifest_over_stdin(self):
        ctl = FakeControl([ExecResult(0, actor_template_json(), "")])
        run(
            ctl.create_actor_template(
                "live-agent-gate", "live-agent-gate-v1", "metadata:\n  name: x\n"
            )
        )
        self.assertEqual(
            ctl.calls[0],
            ["create", "actor-template", "-f", "-", "-o", "json"],
        )
        self.assertEqual(ctl.stdins[0], "metadata:\n  name: x\n")

    def test_uncertain_template_create_reads_before_returning_success(self):
        ctl = FakeControl(
            [
                TransportError("request timed out after send"),
                ExecResult(0, actor_template_json(golden_tag="g1"), ""),
            ]
        )
        record = run(
            ctl.create_actor_template(
                "live-agent-gate", "live-agent-gate-v1", "manifest"
            )
        )
        self.assertEqual(record.uid, "t-1")
        self.assertEqual(len(ctl.calls), 2)
        self.assertEqual(
            ctl.calls[1][:3], ["get", "actor-template", "live-agent-gate-v1"]
        )

    def test_uncertain_template_create_reads_before_reporting_absent(self):
        ctl = FakeControl(
            [
                TransportError("request timed out after send"),
                ExecResult(1, "", 'Error: actortemplates.ate.dev "x" not found'),
            ]
        )
        with self.assertRaises(TransportError):
            run(ctl.create_actor_template("live-agent-gate", "x", "manifest"))
        self.assertEqual(
            len(ctl.calls), 2
        )  # read-back happened before any caller retry


class WaitForGoldenSnapshotTests(unittest.TestCase):
    def test_returns_once_ready(self):
        ctl = FakeControl(
            [
                ExecResult(0, actor_template_json(), ""),  # pending
                ExecResult(0, actor_template_json(golden_tag="g1"), ""),  # ready
            ]
        )
        record = run(
            wait_for_golden_snapshot(
                ctl,
                "live-agent-gate",
                "live-agent-gate-v1",
                timeout_s=30,
                poll_interval_s=0,
                sleep=fake_sleep,
                clock=FakeClock(step=1),
            )
        )
        self.assertEqual(record.golden_state, GoldenState.READY)

    def test_raises_on_reported_failure_not_just_returns(self):
        ctl = FakeControl(
            [ExecResult(0, actor_template_json(error_message="boom"), "")]
        )
        with self.assertRaises(GoldenSnapshotFailed):
            run(
                wait_for_golden_snapshot(
                    ctl,
                    "live-agent-gate",
                    "live-agent-gate-v1",
                    timeout_s=30,
                    sleep=fake_sleep,
                    clock=FakeClock(step=1),
                )
            )

    def test_raises_timeout_rather_than_claiming_success(self):
        # Every poll is still pending; the fake clock advances past the deadline. This is
        # the "polling loop prints success after timeout" bug from the recovery review --
        # here, timing out must raise, never return.
        ctl = FakeControl([ExecResult(0, actor_template_json(), "") for _ in range(50)])
        with self.assertRaises(GoldenSnapshotTimeout):
            run(
                wait_for_golden_snapshot(
                    ctl,
                    "live-agent-gate",
                    "live-agent-gate-v1",
                    timeout_s=5,
                    poll_interval_s=0,
                    sleep=fake_sleep,
                    clock=FakeClock(step=1),
                )
            )

    def test_disappearing_template_is_a_transport_error_not_pending(self):
        ctl = FakeControl(
            [ExecResult(1, "", 'Error: actortemplates.ate.dev "x" not found')]
        )
        with self.assertRaises(TransportError):
            run(
                wait_for_golden_snapshot(
                    ctl,
                    "live-agent-gate",
                    "live-agent-gate-v1",
                    timeout_s=30,
                    sleep=fake_sleep,
                    clock=FakeClock(step=1),
                )
            )


class GetEligibleWorkersTests(unittest.TestCase):
    def get_workers(self, workers):
        return FakeControl([ExecResult(0, json.dumps({"workers": workers}), "")])

    def test_parses_pinned_cli_json_and_uses_pool_filters(self):
        ctl = FakeControl([ExecResult(0, PINNED_WORKERS_OUTPUT, "")])
        self.assertEqual(
            run(
                ctl.get_eligible_workers(
                    "live-agent-gate", "workload=live-agent-gate", "gvisor"
                )
            ),
            1,
        )
        self.assertEqual(
            ctl.calls[0],
            [
                "get",
                "workers",
                "-n",
                "live-agent-gate",
                "-l",
                "workload=live-agent-gate",
                "--sandbox-class",
                "gvisor",
                "-o",
                "json",
            ],
        )

    def test_excludes_occupied_worker_using_capacity_minus_allocated(self):
        ctl = self.get_workers([worker_record(capacity=1, allocated=1)])
        self.assertEqual(
            run(
                ctl.get_eligible_workers(
                    "live-agent-gate", "workload=live-agent-gate", "gvisor"
                )
            ),
            0,
        )

    def test_excludes_draining_worker(self):
        ctl = self.get_workers([worker_record(state="WORKER_STATE_DRAINING")])
        self.assertEqual(
            run(
                ctl.get_eligible_workers(
                    "live-agent-gate", "workload=live-agent-gate", "gvisor"
                )
            ),
            0,
        )

    def test_excludes_wrong_pool_worker(self):
        ctl = self.get_workers([worker_record(pool_label="other")])
        self.assertEqual(
            run(
                ctl.get_eligible_workers(
                    "live-agent-gate", "workload=live-agent-gate", "gvisor"
                )
            ),
            0,
        )

    def test_empty_workers_list_returns_zero(self):
        ctl = self.get_workers([])
        self.assertEqual(
            run(
                ctl.get_eligible_workers(
                    "live-agent-gate", "workload=live-agent-gate", "gvisor"
                )
            ),
            0,
        )

    def test_empty_object_matches_pinned_cli_empty_result(self):
        ctl = FakeControl([ExecResult(0, "{}", "")])
        self.assertEqual(
            run(
                ctl.get_eligible_workers(
                    "live-agent-gate", "workload=live-agent-gate", "gvisor"
                )
            ),
            0,
        )

    def test_missing_workers_field_is_a_contract_error(self):
        ctl = FakeControl([ExecResult(0, json.dumps({"items": []}), "")])
        with self.assertRaisesRegex(TransportError, "missing its 'workers' list"):
            run(
                ctl.get_eligible_workers(
                    "live-agent-gate", "workload=live-agent-gate", "gvisor"
                )
            )


class WaitForEligibleWorkerTests(unittest.TestCase):
    def test_returns_once_a_worker_is_eligible(self):
        ctl = FakeControl(
            [
                ExecResult(0, json.dumps({"workers": []}), ""),
                ExecResult(0, json.dumps({"workers": [worker_record()]}), ""),
            ]
        )
        count = run(
            wait_for_eligible_worker(
                ctl,
                "live-agent-gate",
                "workload=live-agent-gate",
                "gvisor",
                timeout_s=30,
                poll_interval_s=0,
                sleep=fake_sleep,
                clock=FakeClock(step=1),
            )
        )
        self.assertEqual(count, 1)

    def test_raises_no_eligible_worker_on_timeout(self):
        ctl = FakeControl(
            [ExecResult(0, json.dumps({"workers": []}), "") for _ in range(50)]
        )
        with self.assertRaises(NoEligibleWorker):
            run(
                wait_for_eligible_worker(
                    ctl,
                    "live-agent-gate",
                    "workload=live-agent-gate",
                    "gvisor",
                    timeout_s=5,
                    poll_interval_s=0,
                    sleep=fake_sleep,
                    clock=FakeClock(step=1),
                )
            )


class WaitForActorRunningTests(unittest.TestCase):
    def test_returns_once_running(self):
        ctl = FakeControl(
            [
                ExecResult(0, actor_json("ACTOR_STATE_RESUMING"), ""),
                ExecResult(0, actor_json("ACTOR_STATE_RUNNING"), ""),
            ]
        )
        actor = run(
            wait_for_actor_running(
                ctl,
                "mainloop-workspaces",
                "ml-abc",
                timeout_s=30,
                poll_interval_s=0,
                sleep=fake_sleep,
                clock=FakeClock(step=1),
            )
        )
        self.assertEqual(actor.state, ActorState.RUNNING)

    def test_raises_actor_failed_to_start_on_crash_not_timeout(self):
        ctl = FakeControl([ExecResult(0, actor_json("ACTOR_STATE_CRASHED"), "")])
        with self.assertRaises(ActorFailedToStart):
            run(
                wait_for_actor_running(
                    ctl,
                    "mainloop-workspaces",
                    "ml-abc",
                    timeout_s=30,
                    sleep=fake_sleep,
                    clock=FakeClock(step=1),
                )
            )

    def test_raises_wait_timeout_when_stuck_resuming(self):
        ctl = FakeControl(
            [ExecResult(0, actor_json("ACTOR_STATE_RESUMING"), "") for _ in range(50)]
        )
        with self.assertRaises(WaitTimeout):
            run(
                wait_for_actor_running(
                    ctl,
                    "mainloop-workspaces",
                    "ml-abc",
                    timeout_s=5,
                    poll_interval_s=0,
                    sleep=fake_sleep,
                    clock=FakeClock(step=1),
                )
            )


class WaitForActorHealthTests(unittest.TestCase):
    def test_slow_start_waits_until_health_route_returns_200(self):
        checks = iter([False, False, True])
        run(
            wait_for_actor_health(
                lambda: next(checks),
                timeout_s=10,
                poll_interval_s=0,
                sleep=fake_sleep,
                clock=FakeClock(step=1),
            )
        )

    def test_failed_pane_or_shim_times_out_while_health_stays_non_200(self):
        with self.assertRaises(ActorHealthTimeout):
            run(
                wait_for_actor_health(
                    lambda: False,
                    timeout_s=3,
                    poll_interval_s=0,
                    sleep=fake_sleep,
                    clock=FakeClock(step=1),
                )
            )

    def test_restored_actor_passes_without_replaying_boot_marker(self):
        # Restore resumes processes; current health, not a repeated startup log, is the
        # readiness contract.
        run(
            wait_for_actor_health(
                lambda: True,
                timeout_s=3,
                sleep=fake_sleep,
                clock=FakeClock(step=1),
            )
        )


class ReconcileActorIdentityTests(unittest.TestCase):
    def test_absent_when_no_live_actor(self):
        self.assertEqual(reconcile_actor_identity("u-1", None), IdentityOutcome.ABSENT)

    def test_matches_when_uids_equal(self):
        live = _actor_from_json(
            json.loads(actor_json("ACTOR_STATE_RUNNING", uid="u-1"))
        )
        self.assertEqual(reconcile_actor_identity("u-1", live), IdentityOutcome.MATCHES)

    def test_unowned_when_no_actor_uid_was_persisted(self):
        live = _actor_from_json(
            json.loads(actor_json("ACTOR_STATE_RUNNING", uid="u-1"))
        )
        self.assertEqual(reconcile_actor_identity(None, live), IdentityOutcome.UNOWNED)

    def test_diverged_when_uids_differ(self):
        # A rerun must not silently resume or recreate an actor that turned out to belong
        # to a different run under the same name.
        live = _actor_from_json(
            json.loads(actor_json("ACTOR_STATE_RUNNING", uid="u-2"))
        )
        self.assertEqual(
            reconcile_actor_identity("u-1", live), IdentityOutcome.DIVERGED
        )


if __name__ == "__main__":
    unittest.main()
