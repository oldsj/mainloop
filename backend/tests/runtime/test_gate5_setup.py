"""Credential-free regressions for the gate-5 setup script's build and rerun identity."""

import json
from contextlib import redirect_stdout
from io import StringIO
import tempfile
import unittest
from dataclasses import replace
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch
from urllib.error import HTTPError

from mainloop.runtime.substrate import (
    ActorFailedToStart,
    ActorRecord,
    ActorState,
    IdentityConflict,
)

from scripts import gate5_setup

FIXTURE_KUBECONFIG = "/fixture/kubeconfig"
FIXTURE_KO = "/fixture/substrate/bin/ko"


def completed(argv, returncode=0, stdout="", stderr=""):
    return SimpleNamespace(
        args=argv, returncode=returncode, stdout=stdout, stderr=stderr
    )


class Gate5SourceAndBuildTests(unittest.TestCase):
    def test_image_manifest_preflight_checks_registry_endpoint_and_accept_types(self):
        calls = []

        class Response:
            status = 200

            def __enter__(self):
                return self

            def __exit__(self, *_args):
                return False

        def opener(request, *, timeout):
            calls.append((request, timeout))
            return Response()

        image = "localhost:5001/live-agent-gate@sha256:" + "a" * 64
        gate5_setup.verify_image_manifest(image, opener=opener)

        request, timeout = calls[0]
        self.assertEqual(
            request.full_url,
            f"http://localhost:5001/v2/live-agent-gate/manifests/sha256:{'a' * 64}",
        )
        self.assertEqual(request.get_method(), "HEAD")
        self.assertEqual(request.get_header("Accept"), gate5_setup.IMAGE_MANIFEST_ACCEPT)
        self.assertEqual(timeout, 10)

    def test_image_manifest_preflight_fails_before_template_on_missing_manifest(self):
        image = "localhost:5001/live-agent-gate@sha256:" + "b" * 64

        def missing(request, *, timeout):
            error = HTTPError(request.full_url, 404, "MANIFEST_UNKNOWN", {}, None)
            error.close()
            raise error

        with self.assertRaisesRegex(RuntimeError, "registry returned HTTP 404"):
            gate5_setup.verify_image_manifest(image, opener=missing)

    def test_image_manifest_preflight_rejects_tag_or_malformed_digest(self):
        calls = []

        def opener(*_args, **_kwargs):
            calls.append(True)

        for image in (
            "localhost:5001/live-agent-gate:latest",
            "localhost:5001/live-agent-gate@sha256:bad",
        ):
            with self.subTest(image=image), self.assertRaisesRegex(
                RuntimeError, "full sha256 digest"
            ):
                gate5_setup.verify_image_manifest(image, opener=opener)
        self.assertEqual(calls, [])

    def make_source(self, root: Path) -> Path:
        (root / ".git").mkdir(parents=True)
        (root / "go.mod").write_text("module fixture\n")
        (root / ".ko.yaml").write_text("defaultBaseImage: scratch\n")
        return root

    def test_source_requires_pinned_checkout_and_required_files(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            source = self.make_source(Path(temp_dir) / "substrate")
            calls = []

            def runner(argv, **kwargs):
                calls.append(argv)
                return completed(
                    argv, stdout=gate5_setup.PINNED_SUBSTRATE_COMMIT + "\n"
                )

            self.assertEqual(
                gate5_setup.verify_substrate_source(str(source), runner=runner),
                str(source),
            )
            self.assertEqual(calls[0][:4], ["git", "-C", str(source), "rev-parse"])

    def test_source_rejects_wrong_commit(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            source = self.make_source(Path(temp_dir) / "substrate")

            def runner(argv, **kwargs):
                return completed(argv, stdout="a" * 40)

            with self.assertRaisesRegex(RuntimeError, "must be pinned"):
                gate5_setup.verify_substrate_source(str(source), runner=runner)

    def test_source_rejects_missing_go_module_files(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            source = Path(temp_dir) / "substrate"
            source.mkdir()
            (source / ".git").mkdir()
            with self.assertRaisesRegex(RuntimeError, "missing go.mod"):
                gate5_setup.verify_substrate_source(str(source))

    def test_ko_resolve_uses_pinned_source_cwd_and_explicit_kube_target(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            source = self.make_source(Path(temp_dir) / "substrate")
            calls = []

            def runner(argv, **kwargs):
                calls.append((argv, kwargs))
                if argv[0] == "git":
                    return completed(
                        argv, stdout=gate5_setup.PINNED_SUBSTRATE_COMMIT + "\n"
                    )
                if argv[1:3] == ["resolve", "-f"]:
                    return completed(argv, stdout="resolved-yaml")
                return completed(argv)

            with patch.dict("os.environ", {"KO_DOCKER_REPO": "localhost:5001"}):
                gate5_setup.apply_worker_pool(
                    "apiVersion: v1\n",
                    kubeconfig=FIXTURE_KUBECONFIG,
                    context="kind-substrate-preview",
                    ko=FIXTURE_KO,
                    substrate_src=str(source),
                    runner=runner,
                )

            ko_argv, ko_kwargs = calls[1]
            self.assertEqual(ko_argv[:3], [FIXTURE_KO, "resolve", "-f"])
            self.assertEqual(ko_kwargs["cwd"], str(source))
            apply_argv, apply_kwargs = calls[2]
            self.assertEqual(
                apply_argv[:5],
                [
                    "kubectl",
                    "--context",
                    "kind-substrate-preview",
                    "--kubeconfig",
                    FIXTURE_KUBECONFIG,
                ],
            )
            self.assertEqual(apply_kwargs["input"], "resolved-yaml")

    def test_cluster_identity_uses_explicit_context_and_namespace_uid(self):
        calls = []
        results = [
            json.dumps(
                {
                    "contexts": [{"name": "kind-substrate-preview"}],
                    "clusters": [{"cluster": {"server": "https://127.0.0.1:45147"}}],
                }
            ),
            json.dumps({"metadata": {"uid": "kube-system-uid"}}),
        ]

        def runner(argv, **kwargs):
            calls.append(argv)
            return completed(argv, stdout=results.pop(0))

        identity = gate5_setup.get_cluster_identity(
            context="kind-substrate-preview",
            kubeconfig=FIXTURE_KUBECONFIG,
            runner=runner,
        )
        self.assertEqual(
            identity,
            {
                "api_server_url": "https://127.0.0.1:45147",
                "kube_system_namespace_uid": "kube-system-uid",
            },
        )
        self.assertTrue(
            all("--context" in call and "--kubeconfig" in call for call in calls)
        )

    def test_actor_health_uses_actor_route_and_shim_port(self):
        class FakeConnection:
            def __init__(self, _host, _port, timeout):
                self.timeout = timeout
                self.tunnel = None
                self.requested = None

            def set_tunnel(self, target, *, headers):
                self.tunnel = (target, headers)

            def request(self, method, path):
                self.requested = (method, path)

            def getresponse(self):
                return SimpleNamespace(status=200, read=lambda: b"ok")

            def close(self):
                pass

        connections = []

        def make_connection(*args, **kwargs):
            connection = FakeConnection(*args, **kwargs)
            connections.append(connection)
            return connection

        with patch.object(gate5_setup.http.client, "HTTPConnection", make_connection):
            self.assertTrue(
                gate5_setup.actor_health_check(
                    port=18091,
                    atespace="live-agent-gate",
                    actor_name="claude-gate5",
                )
            )
        self.assertEqual(
            connections[0].tunnel,
            (
                "actor-upstream:8090",
                {"ate-target-actor": "live-agent-gate/claude-gate5"},
            ),
        )
        self.assertEqual(connections[0].requested, ("GET", "/healthz"))

    def test_actor_router_tunnel_forwards_to_connect_listener(self):
        class FakeSocket:
            def __enter__(self):
                return self

            def __exit__(self, *_exc):
                return False

        class FakeProcess:
            def __init__(self):
                self.terminated = False
                self.stdout = None

            def poll(self):
                return None

            def terminate(self):
                self.terminated = True

            def wait(self, timeout):
                self.wait_timeout = timeout

        args = SimpleNamespace(
            context="kind-substrate-preview",
            kubeconfig=FIXTURE_KUBECONFIG,
            router_port=18081,
        )
        process = FakeProcess()
        with (
            patch.object(
                gate5_setup.subprocess, "Popen", return_value=process
            ) as popen,
            patch.object(
                gate5_setup.socket, "create_connection", return_value=FakeSocket()
            ),
            gate5_setup.actor_router_tunnel(args) as route_port,
        ):
            self.assertEqual(route_port, 18081)

        command = popen.call_args.args[0]
        self.assertIn("18081:8081", command)
        self.assertTrue(process.terminated)

    def test_egress_hostname_rules_are_passed_as_repeatable_flags(self):
        args = SimpleNamespace(
            egress_tool="/fixture/mainloop-egress-tool",
            kubeconfig=FIXTURE_KUBECONFIG,
            context="kind-substrate-preview",
            atespace="live-agent-gate",
            actor_name="claude-gate5",
            egress_deny_all=False,
            egress_allow_all=False,
            egress_hostnames=["api.anthropic.com", "api.openai.com"],
        )
        with patch.object(gate5_setup.subprocess, "run") as run:
            gate5_setup.run_egress_tool(args)
        self.assertEqual(
            run.call_args.args[0],
            [
                "/fixture/mainloop-egress-tool",
                "--kubeconfig",
                FIXTURE_KUBECONFIG,
                "--context",
                "kind-substrate-preview",
                "--atespace",
                "live-agent-gate",
                "--actor",
                "claude-gate5",
                "--hostname",
                "api.anthropic.com",
                "--hostname",
                "api.openai.com",
            ],
        )


class Gate5StateTests(unittest.TestCase):
    def args(self, state_file: str, **overrides):
        values = {
            "context": "kind-substrate-preview",
            "kubeconfig": FIXTURE_KUBECONFIG,
            "atespace": "live-agent-gate",
            "template_version": "v1",
            "image": "localhost:5001/live-agent-gate@sha256:" + "a" * 64,
            "actor_name": "claude-gate5",
            "state_file": state_file,
        }
        values.update(overrides)
        return SimpleNamespace(**values)

    def cluster(self, *, uid="cluster-uid"):
        return {
            "api_server_url": "https://127.0.0.1:45147",
            "kube_system_namespace_uid": uid,
        }

    def test_persists_run_intent_before_actor_or_template_uids_exist(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            path = str(Path(temp_dir) / "state.json")
            args = self.args(path)
            state = gate5_setup.prepare_run_state(args, self.cluster())
            stored = json.loads(Path(path).read_text())
            self.assertEqual(stored["run_id"], state["run_id"])
            self.assertEqual(stored["template_name"], "live-agent-gate-v1")
            self.assertEqual(stored["actor_name"], "claude-gate5")
            self.assertIsNone(stored["template_uid"])
            self.assertIsNone(stored["actor_uid"])
            self.assertEqual(stored["cluster_identity"], self.cluster())

    def test_rerun_refuses_changed_cluster_identity(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            path = str(Path(temp_dir) / "state.json")
            args = self.args(path)
            gate5_setup.prepare_run_state(args, self.cluster())
            with self.assertRaisesRegex(RuntimeError, "requested identity differs"):
                gate5_setup.prepare_run_state(args, self.cluster(uid="other-cluster"))

    def test_rerun_refuses_changed_template_request(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            path = str(Path(temp_dir) / "state.json")
            gate5_setup.prepare_run_state(self.args(path), self.cluster())
            with self.assertRaisesRegex(RuntimeError, "template_name"):
                gate5_setup.prepare_run_state(
                    self.args(path, template_version="v2"), self.cluster()
                )


class SetupControl:
    def __init__(self, actor: ActorRecord | None):
        self.actor = actor
        self.created = []
        self.resumed = 0
        self.events = []

    async def get_actor(self, _atespace, _name):
        self.events.append("get_actor")
        return self.actor

    async def create_actor(self, atespace, name, *, template):
        self.events.append("create_actor")
        self.created.append((atespace, name, template))
        self.actor = ActorRecord(
            atespace=atespace,
            name=name,
            uid="actor-new",
            state=ActorState.SUSPENDED,
            external_snapshot_uri=None,
            current_actor_template_uid="template-new",
            raw={},
        )
        return self.actor

    async def resume_actor(self, _atespace, _name):
        self.events.append("resume_actor")
        self.resumed += 1
        self.actor = replace(self.actor, state=ActorState.RUNNING)
        return self.actor


class Gate5ActorIdentityTests(unittest.TestCase):
    def state(self, path, *, actor_uid="actor-1"):
        state = {
            "run_id": "run-1",
            "actor_uid": actor_uid,
            "actor_name": "claude-gate5",
            "template_name": "live-agent-gate-v2",
            "template_uid": "template-new",
        }
        gate5_setup.save_state(path, state)
        return state

    def test_state_file_is_private_for_future_shim_token(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            path = str(Path(temp_dir) / "state.json")
            gate5_setup.save_state(path, {"shim_token": "fixture-secret"})
            self.assertEqual(Path(path).stat().st_mode & 0o777, 0o600)

    def test_shim_token_is_persisted_before_body_only_install_and_verified(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            path = str(Path(temp_dir) / "state.json")
            state = {"run_id": "run-token"}
            args = SimpleNamespace(
                state_file=path,
                atespace="live-agent-gate",
                actor_name="claude-gate5",
            )
            token = "fixture-shim-token-with-at-least-32-characters"
            statuses = [201, 401, 401, 200, 409, 200]
            calls = []

            def requester(**kwargs):
                calls.append(kwargs)
                return statuses.pop(0)

            output = StringIO()
            with (
                patch.object(gate5_setup.secrets, "token_urlsafe", return_value=token),
                redirect_stdout(output),
            ):
                gate5_setup.ensure_shim_token(
                    args=args, state=state, port=18091, requester=requester
                )

            self.assertEqual(json.loads(Path(path).read_text())["shim_token"], token)
            self.assertEqual(Path(path).stat().st_mode & 0o777, 0o600)
            self.assertEqual(calls[0]["method"], "POST")
            self.assertEqual(calls[0]["path"], "/token")
            self.assertEqual(calls[0]["body"], {"token": token})
            self.assertNotIn("token", calls[0])
            self.assertEqual(calls[1]["token"], None)
            self.assertEqual(calls[2]["token"], f"{token}x")
            self.assertEqual(calls[3]["token"], token)
            self.assertNotIn(token, output.getvalue())

    def test_shim_token_rerun_verifies_existing_token_without_rotating_it(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            path = str(Path(temp_dir) / "state.json")
            token = "existing-shim-token-with-at-least-32-characters"
            state = {"run_id": "run-token", "shim_token": token}
            gate5_setup.save_state(path, state)
            args = SimpleNamespace(
                state_file=path,
                atespace="live-agent-gate",
                actor_name="claude-gate5",
            )
            statuses = [409, 200, 401, 401, 200, 409, 200]
            calls = []

            def requester(**kwargs):
                calls.append(kwargs)
                return statuses.pop(0)

            with patch.object(gate5_setup.secrets, "token_urlsafe") as generate:
                gate5_setup.ensure_shim_token(
                    args=args, state=state, port=18091, requester=requester
                )

            generate.assert_not_called()
            self.assertEqual(calls[1]["token"], token)
            self.assertEqual(json.loads(Path(path).read_text())["shim_token"], token)

    def actor(
        self, *, uid="actor-1", template_uid="template-new", state=ActorState.RUNNING
    ):
        return ActorRecord(
            atespace="live-agent-gate",
            name="claude-gate5",
            uid=uid,
            state=state,
            external_snapshot_uri=None,
            current_actor_template_uid=template_uid,
            raw={},
        )

    def args(self, path):
        return SimpleNamespace(
            state_file=path,
            atespace="live-agent-gate",
            actor_name="claude-gate5",
            actor_timeout=5,
            worker_timeout=5,
        )

    def test_exact_old_template_collision_with_new_template_is_refused(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            path = str(Path(temp_dir) / "state.json")
            state = {
                "run_id": "run-2",
                "actor_uid": None,
                "template_uid": "template-new",
            }
            control = SetupControl(self.actor(template_uid="template-old"))
            with self.assertRaisesRegex(IdentityConflict, "no actor uid"):
                asyncio_run(
                    gate5_setup.ensure_actor(
                        control,
                        self.args(path),
                        "live-agent-gate-v2",
                        "template-new",
                        state,
                    )
                )

    def test_owned_actor_with_template_mismatch_is_refused(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            path = str(Path(temp_dir) / "state.json")
            state = self.state(path)
            control = SetupControl(self.actor(template_uid="template-old"))
            with self.assertRaisesRegex(IdentityConflict, "references template uid"):
                asyncio_run(
                    gate5_setup.ensure_actor(
                        control,
                        self.args(path),
                        "live-agent-gate-v2",
                        "template-new",
                        state,
                    )
                )

    def test_crashed_and_deleting_actors_are_refused_before_resume(self):
        for actor_state in (ActorState.CRASHED, ActorState.DELETING):
            with (
                self.subTest(actor_state=actor_state),
                tempfile.TemporaryDirectory() as temp_dir,
            ):
                path = str(Path(temp_dir) / "state.json")
                state = self.state(path)
                control = SetupControl(self.actor(state=actor_state))
                with self.assertRaises(ActorFailedToStart):
                    asyncio_run(
                        gate5_setup.ensure_actor(
                            control,
                            self.args(path),
                            "live-agent-gate-v2",
                            "template-new",
                            state,
                        )
                    )
                self.assertEqual(control.resumed, 0)

    def test_new_actor_is_resumed_and_uid_is_saved(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            path = str(Path(temp_dir) / "state.json")
            state = {
                "run_id": "run-3",
                "actor_uid": None,
                "template_uid": "template-new",
            }
            control = SetupControl(None)

            async def wait_for_worker(*_args, **_kwargs):
                control.events.append("wait_for_eligible_worker")

            with patch.object(gate5_setup, "wait_for_eligible_worker", wait_for_worker):
                asyncio_run(
                    gate5_setup.wait_for_worker_if_actor_is_absent(
                        control, self.args(path), state
                    )
                )
                uid = asyncio_run(
                    gate5_setup.ensure_actor(
                        control,
                        self.args(path),
                        "live-agent-gate-v2",
                        "template-new",
                        state,
                    )
                )
            self.assertEqual(uid, "actor-new")
            self.assertEqual(control.resumed, 1)
            self.assertLess(
                control.events.index("get_actor"),
                control.events.index("wait_for_eligible_worker"),
            )
            self.assertLess(
                control.events.index("wait_for_eligible_worker"),
                control.events.index("create_actor"),
            )
            self.assertEqual(
                json.loads(Path(path).read_text())["actor_uid"], "actor-new"
            )

    def test_owned_rerun_skips_worker_wait_when_its_actor_occupies_only_worker(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            path = str(Path(temp_dir) / "state.json")
            state = self.state(path)
            control = SetupControl(self.actor())

            async def unexpected_worker_wait(*_args, **_kwargs):
                self.fail("owned rerun must not wait for a spare worker")

            with patch.object(
                gate5_setup, "wait_for_eligible_worker", unexpected_worker_wait
            ):
                asyncio_run(
                    gate5_setup.wait_for_worker_if_actor_is_absent(
                        control, self.args(path), state
                    )
                )
                uid = asyncio_run(
                    gate5_setup.ensure_actor(
                        control,
                        self.args(path),
                        "live-agent-gate-v2",
                        "template-new",
                        state,
                    )
                )

            self.assertEqual(uid, "actor-1")
            self.assertEqual(control.created, [])
            self.assertEqual(control.resumed, 0)

    def test_unowned_actor_is_refused_before_worker_wait(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            path = str(Path(temp_dir) / "state.json")
            state = {
                "run_id": "run-4",
                "actor_uid": None,
                "actor_name": "claude-gate5",
                "template_name": "live-agent-gate-v2",
                "template_uid": "template-new",
            }
            control = SetupControl(self.actor(template_uid="template-old"))

            async def unexpected_worker_wait(*_args, **_kwargs):
                self.fail("unowned actor must be refused before worker discovery")

            with patch.object(
                gate5_setup, "wait_for_eligible_worker", unexpected_worker_wait
            ):
                with self.assertRaisesRegex(IdentityConflict, "no actor uid"):
                    asyncio_run(
                        gate5_setup.wait_for_worker_if_actor_is_absent(
                            control, self.args(path), state
                        )
                    )

            self.assertEqual(control.events, ["get_actor"])


def asyncio_run(coro):
    import asyncio

    return asyncio.run(coro)


if __name__ == "__main__":
    unittest.main()
