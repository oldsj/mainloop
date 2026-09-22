"""Substrate transport adapter over a fake ``kubectl ate`` subprocess: no cluster, no actors,
no credentials. Mirrors test_herdr.py's fake-transport pattern for the Herdr adapter."""

import asyncio
import unittest

from mainloop.runtime.substrate import (
    ActorState,
    ExecResult,
    SubstrateControl,
    TransportError,
    _actor_from_json,
)


class FakeControl(SubstrateControl):
    def __init__(self, results):
        super().__init__(
            kubeconfig="fixture-kubeconfig", context="kind-substrate-preview"
        )
        self.results = list(results)
        self.calls: list[list[str]] = []

    async def _exec(self, args, timeout=45):
        self.calls.append(args)
        result = self.results.pop(0)
        if isinstance(result, Exception):
            raise result
        return result


def run(coro):
    return asyncio.run(coro)


def actor_json(state: str, *, snapshot_uri: str | None = None) -> str:
    doc = {
        "metadata": {"atespace": "mainloop-workspaces", "name": "ml-abc", "uid": "u-1"},
        "status": {"state": state},
    }
    if snapshot_uri:
        doc["status"]["externalSnapshot"] = {"snapshotUri": snapshot_uri}
    import json

    return json.dumps(doc)


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


if __name__ == "__main__":
    unittest.main()
