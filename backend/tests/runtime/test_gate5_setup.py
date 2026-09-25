"""Credential-free regressions for the gate-5 setup script's build and rerun identity."""

import json
import os
import subprocess  # nosec B404 - fixed shell calls exercise cleanup with fake commands.
import tempfile
import unittest
from contextlib import redirect_stdout
from dataclasses import replace
from io import StringIO
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
    def parse_cli_args(self, manifest: Path, *extra: str):
        argv = [
            "gate5_setup.py",
            "--context",
            "kind-substrate-preview",
            "--kubeconfig",
            FIXTURE_KUBECONFIG,
            "--substrate-src",
            "/fixture/substrate",
            "--atespace",
            "nonroot-check",
            "--template-version",
            "v3",
            "--image",
            "localhost:5001/live-agent-gate@sha256:" + "a" * 64,
            "--manifest",
            str(manifest),
            "--state-file",
            "gate5-test-state.json",
            "--egress-deny-all",
            *extra,
        ]
        with patch("sys.argv", argv):
            return gate5_setup.parse_args()

    def test_worker_pool_option_drives_product_template_selector_and_labels(self):
        manifest = (
            Path(__file__).resolve().parents[3]
            / "spikes/substrate-workspace-adapter/k8s/actor-template.yaml.tmpl"
        )
        args = self.parse_cli_args(manifest, "--worker-pool", "isolated-pool")
        worker_pool, actor_template = gate5_setup.render_gate_manifest(args)

        self.assertEqual(args.atespace, "nonroot-check")
        self.assertEqual(args.worker_pool, "isolated-pool")
        self.assertIn("name: isolated-pool", worker_pool)
        self.assertIn("workload: isolated-pool", worker_pool)
        self.assertIn("name: live-agent-gate-v3", actor_template)
        self.assertIn("workload: isolated-pool", actor_template)
        self.assertIn(
            "name: MAINLOOP_API, value: "
            "http://mainloop-backend.mainloop-control.svc.cluster.local:8000",
            actor_template,
        )
        self.assertIn("mountPath: /work", actor_template)
        self.assertIn("value: /work/repo", actor_template)
        self.assertIn("durableDir: {}", actor_template)
        self.assertIn("path: /healthz, port: 8090", actor_template)
        self.assertNotIn("/workspace", actor_template)

    def test_worker_pool_defaults_to_the_atespace_name(self):
        manifest = (
            Path(__file__).resolve().parents[3]
            / "spikes/substrate-workspace-adapter/k8s/actor-template.yaml.tmpl"
        )

        args = self.parse_cli_args(manifest)

        self.assertEqual(args.worker_pool, "nonroot-check")

    def test_existing_worker_image_is_pinned_and_skips_ko_reference(self):
        manifest = (
            Path(__file__).resolve().parents[3]
            / "spikes/substrate-workspace-adapter/k8s/actor-template.yaml.tmpl"
        )
        worker_image = "localhost:5001/ateom-gvisor@sha256:" + "b" * 64
        args = self.parse_cli_args(manifest, "--worker-image", worker_image)

        worker_pool, _ = gate5_setup.render_gate_manifest(args)

        self.assertIn(f"workerImage: {worker_image}", worker_pool)
        self.assertNotIn("ko://", worker_pool)

    def test_existing_worker_image_rejects_tags_and_short_digests(self):
        manifest = (
            Path(__file__).resolve().parents[3]
            / "spikes/substrate-workspace-adapter/k8s/actor-template.yaml.tmpl"
        )
        for image in (
            "localhost:5001/ateom-gvisor:latest",
            "localhost:5001/ateom-gvisor@sha256:bad",
        ):
            with (
                self.subTest(image=image),
                self.assertRaisesRegex(RuntimeError, "full sha256 digest"),
            ):
                args = self.parse_cli_args(manifest, "--worker-image", image)
                gate5_setup.render_gate_manifest(args)

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
        self.assertEqual(
            request.get_header("Accept"), gate5_setup.IMAGE_MANIFEST_ACCEPT
        )
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
            with (
                self.subTest(image=image),
                self.assertRaisesRegex(RuntimeError, "full sha256 digest"),
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
                return completed(argv, stdout=gate5_setup.SUBSTRATE_FORK_COMMIT + "\n")

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
                        argv, stdout=gate5_setup.SUBSTRATE_FORK_COMMIT + "\n"
                    )
                if argv[1:3] == ["resolve", "-f"]:
                    return completed(argv, stdout="resolved-yaml")
                return completed(argv)

            with patch.dict("os.environ", {"KO_DOCKER_REPO": "localhost:5001"}):
                gate5_setup.apply_worker_pool(
                    (
                        "apiVersion: v1\n"
                        "workerImage: ko://github.com/agent-substrate/substrate/cmd/ateom-gvisor\n"
                    ),
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

    def test_existing_worker_image_applies_without_ko_or_source_checkout(self):
        calls = []

        def runner(argv, **kwargs):
            calls.append((argv, kwargs))
            return completed(argv)

        gate5_setup.apply_worker_pool(
            "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: lane-a\n",
            kubeconfig=FIXTURE_KUBECONFIG,
            context="kind-substrate-preview",
            ko=FIXTURE_KO,
            substrate_src=None,
            runner=runner,
        )

        self.assertEqual(len(calls), 1)
        apply_argv, apply_kwargs = calls[0]
        self.assertEqual(
            apply_argv,
            [
                "kubectl",
                "--context",
                "kind-substrate-preview",
                "--kubeconfig",
                FIXTURE_KUBECONFIG,
                "apply",
                "-f",
                "-",
            ],
        )
        self.assertIn("kind: Namespace", apply_kwargs["input"])

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

    def egress_args(self, **mode):
        base = dict(
            ate_cli="/fixture/kubectl-ate",
            kubeconfig=FIXTURE_KUBECONFIG,
            context="kind-substrate-preview",
            atespace="live-agent-gate",
            actor_name="claude-gate5",
            egress_deny_all=False,
            egress_allow_all=False,
            egress_hostnames=None,
            egress_cidr=None,
        )
        base.update(mode)
        return SimpleNamespace(**base)

    def test_egress_policy_manifest_has_one_rule_per_mode(self):
        cases = [
            ({"egress_deny_all": True}, []),
            ({"egress_allow_all": True}, [{"all": {}}]),
            (
                {"egress_hostnames": ["api.anthropic.com", "api.openai.com"]},
                [{"hostnames": {"patterns": ["api.anthropic.com", "api.openai.com"]}}],
            ),
            ({"egress_cidr": "192.0.2.1/32"}, [{"cidrs": {"cidrs": ["192.0.2.1/32"]}}]),
        ]
        for mode, rules in cases:
            with self.subTest(mode=mode):
                manifest = json.loads(
                    gate5_setup.egress_policy_manifest(self.egress_args(**mode))
                )
                self.assertEqual(manifest, {"rules": rules})

    def test_egress_policy_manifest_refuses_no_mode(self):
        with self.assertRaises(ValueError):
            gate5_setup.egress_policy_manifest(self.egress_args())

    def expected_egress_argv(self, verb):
        return [
            "/fixture/kubectl-ate",
            "--kubeconfig",
            FIXTURE_KUBECONFIG,
            "--context",
            "kind-substrate-preview",
            verb,
            "egress-policy",
            "claude-gate5",
            "--atespace",
            "live-agent-gate",
            "--filename",
            "-",
        ]

    def expected_get_egress_argv(self):
        return [
            "/fixture/kubectl-ate",
            "--kubeconfig",
            FIXTURE_KUBECONFIG,
            "--context",
            "kind-substrate-preview",
            "get",
            "egress-policy",
            "claude-gate5",
            "--atespace",
            "live-agent-gate",
            "-o",
            "json",
        ]

    def test_apply_egress_policy_creates_through_explicit_kube_target(self):
        args = self.egress_args(egress_deny_all=True)
        with patch.object(
            gate5_setup.subprocess,
            "run",
            side_effect=lambda argv, **_: completed(argv),
        ) as run:
            gate5_setup.apply_egress_policy(args)
        self.assertEqual(run.call_count, 1)
        self.assertEqual(run.call_args.args[0], self.expected_egress_argv("create"))
        self.assertEqual(run.call_args.kwargs["input"], '{"rules": []}')

    def test_apply_egress_policy_updates_an_existing_policy(self):
        args = self.egress_args(egress_hostnames=["api.openai.com"])
        results = iter(
            [
                completed(
                    self.expected_egress_argv("create"),
                    returncode=1,
                    stderr="rpc error: code = AlreadyExists desc = EgressPolicy already exists",
                ),
                completed(
                    self.expected_get_egress_argv(),
                    stdout=json.dumps(
                        {"metadata": {"uid": "policy-uid-1", "version": "12"}}
                    ),
                ),
                completed(self.expected_egress_argv("update")),
            ]
        )

        def fake_run(argv, **_):
            result = next(results)
            self.assertEqual(argv, result.args)
            return result

        with patch.object(gate5_setup.subprocess, "run", side_effect=fake_run) as run:
            gate5_setup.apply_egress_policy(args)
        self.assertEqual(
            [call.args[0] for call in run.call_args_list],
            [
                self.expected_egress_argv("create"),
                self.expected_get_egress_argv(),
                self.expected_egress_argv("update"),
            ],
        )
        self.assertEqual(
            json.loads(run.call_args_list[0].kwargs["input"]),
            {"rules": [{"hostnames": {"patterns": ["api.openai.com"]}}]},
        )
        self.assertIsNone(run.call_args_list[1].kwargs.get("input"))
        self.assertEqual(
            json.loads(run.call_args_list[2].kwargs["input"]),
            {
                "metadata": {"uid": "policy-uid-1", "version": "12"},
                "rules": [{"hostnames": {"patterns": ["api.openai.com"]}}],
            },
        )

    def test_apply_egress_policy_refuses_update_without_valid_preconditions(self):
        args = self.egress_args(egress_deny_all=True)
        results = iter(
            [
                completed(
                    self.expected_egress_argv("create"),
                    returncode=1,
                    stderr="rpc error: code = AlreadyExists desc = EgressPolicy already exists",
                ),
                completed(
                    self.expected_get_egress_argv(),
                    stdout=json.dumps(
                        {"metadata": {"uid": "policy-uid-1", "version": "0"}}
                    ),
                ),
            ]
        )

        def fake_run(argv, **_):
            result = next(results)
            self.assertEqual(argv, result.args)
            return result

        with (
            patch.object(gate5_setup.subprocess, "run", side_effect=fake_run) as run,
            self.assertRaisesRegex(
                RuntimeError, "valid metadata.uid and metadata.version"
            ),
        ):
            gate5_setup.apply_egress_policy(args)
        self.assertEqual(run.call_count, 2)

    def test_apply_egress_policy_does_not_update_after_other_create_failures(self):
        args = self.egress_args(egress_deny_all=True)
        with (
            patch.object(
                gate5_setup.subprocess,
                "run",
                side_effect=lambda argv, **_: completed(
                    argv,
                    returncode=1,
                    stderr="rpc error: code = FailedPrecondition desc = parent Actor does not exist",
                ),
            ) as run,
            self.assertRaisesRegex(RuntimeError, "create egress-policy failed"),
        ):
            gate5_setup.apply_egress_policy(args)
        self.assertEqual(run.call_count, 1)

    def args(self, state_file: str, **overrides):
        values = {
            "context": "kind-substrate-preview",
            "kubeconfig": FIXTURE_KUBECONFIG,
            "atespace": "live-agent-gate",
            "worker_pool": "live-agent-gate",
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
            self.assertEqual(stored["worker_pool"], "live-agent-gate")
            self.assertEqual(stored["actor_name"], "claude-gate5")
            self.assertIsNone(stored["namespace_uid"])
            self.assertIsNone(stored["template_uid"])
            self.assertIsNone(stored["actor_uid"])
            self.assertEqual(stored["cluster_identity"], self.cluster())

    def test_creates_namespace_and_persists_returned_uid(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            path = str(Path(temp_dir) / "state.json")
            args = self.args(path)
            state = gate5_setup.prepare_run_state(args, self.cluster())
            calls = []

            def runner(command, **kwargs):
                calls.append((command, kwargs))
                return completed(
                    command, stdout='{"metadata":{"uid":"lane-namespace-uid"}}'
                )

            namespace_doc = "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: lane\n"
            uid = gate5_setup.create_namespace_and_record_uid(
                args, state, namespace_doc, runner=runner
            )

            self.assertEqual(uid, "lane-namespace-uid")
            self.assertEqual(json.loads(Path(path).read_text())["namespace_uid"], uid)
            self.assertEqual(Path(path).stat().st_mode & 0o777, 0o600)
            self.assertEqual(calls[0][0][-5:], ["create", "-f", "-", "-o", "json"])
            self.assertEqual(calls[0][1]["input"], namespace_doc)

    def test_resume_refuses_state_without_namespace_uid(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            path = str(Path(temp_dir) / "state.json")
            args = self.args(path)
            state = gate5_setup.prepare_run_state(args, self.cluster())
            state.pop("namespace_uid")
            gate5_setup.save_state(path, state)

            with self.assertRaisesRegex(RuntimeError, "namespace_uid"):
                gate5_setup.prepare_run_state(args, self.cluster())

    def test_resume_refuses_empty_namespace_uid(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            path = str(Path(temp_dir) / "state.json")
            args = self.args(path)
            state = gate5_setup.prepare_run_state(args, self.cluster())
            gate5_setup.save_state(path, state)

            with self.assertRaisesRegex(RuntimeError, "namespace_uid"):
                gate5_setup.prepare_run_state(args, self.cluster())

    def test_namespace_uid_rerun_refuses_to_adopt_replacement(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            path = str(Path(temp_dir) / "state.json")
            args = self.args(path)
            state = gate5_setup.prepare_run_state(args, self.cluster())
            state["namespace_uid"] = "original-namespace-uid"
            gate5_setup.save_state(path, state)

            def runner(argv, **_):
                return completed(
                    argv, stdout='{"metadata":{"uid":"replacement-namespace-uid"}}'
                )

            with self.assertRaisesRegex(RuntimeError, "refusing to adopt"):
                gate5_setup.verify_namespace_uid(args, state, runner=runner)

            self.assertEqual(
                json.loads(Path(path).read_text())["namespace_uid"],
                "original-namespace-uid",
            )

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

    def test_rerun_refuses_changed_worker_pool(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            path = str(Path(temp_dir) / "state.json")
            gate5_setup.prepare_run_state(self.args(path), self.cluster())
            with self.assertRaisesRegex(RuntimeError, "worker_pool"):
                gate5_setup.prepare_run_state(
                    self.args(path, worker_pool="isolated-pool"), self.cluster()
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
            token = "fixture-shim-" + "v" * 40
            statuses = [201, 401, 401, 404, 409, 200]
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
            self.assertEqual(
                calls[1]["path"], "/turn/00000000-0000-4000-8000-000000000000"
            )
            self.assertEqual(calls[1]["token"], None)
            self.assertEqual(calls[2]["token"], f"{token}x")
            self.assertEqual(calls[3]["token"], token)
            self.assertNotIn(token, output.getvalue())

    def test_shim_token_rerun_verifies_existing_token_without_rotating_it(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            path = str(Path(temp_dir) / "state.json")
            token = "existing-shim-" + "v" * 40
            state = {"run_id": "run-token", "shim_token": token}
            gate5_setup.save_state(path, state)
            args = SimpleNamespace(
                state_file=path,
                atespace="live-agent-gate",
                actor_name="claude-gate5",
            )
            statuses = [409, 404, 401, 401, 404, 409, 200]
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
            worker_pool="live-agent-gate",
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

    def test_worker_wait_uses_the_selected_atespace_namespace(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            path = str(Path(temp_dir) / "state.json")
            args = self.args(path)
            args.atespace = "native-codex"
            args.worker_pool = "native-codex"
            control = SetupControl(None)
            observed = {}

            async def wait_for_worker(
                _control, namespace, worker_selector, *_args, **_kwargs
            ):
                observed["namespace"] = namespace
                observed["worker_selector"] = worker_selector

            with patch.object(gate5_setup, "wait_for_eligible_worker", wait_for_worker):
                asyncio_run(
                    gate5_setup.wait_for_worker_if_actor_is_absent(
                        control,
                        args,
                        {
                            "run_id": "run-codex",
                            "actor_uid": None,
                            "template_uid": "template-codex",
                        },
                    )
                )

            self.assertEqual(observed["namespace"], "native-codex")
            self.assertEqual(observed["worker_selector"], "workload=native-codex")

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


class LaneAProviderCleanupTests(unittest.TestCase):
    def make_cleanup_environment(
        self,
        temp_dir: str,
        *,
        unowned_secret: bool = False,
        change_secret_uid: bool = False,
    ) -> tuple[dict[str, str], Path]:
        root = Path(temp_dir)
        state_root = root / "state"
        state_root.mkdir()
        fake_bin = root / "bin"
        fake_bin.mkdir()
        substrate_bin = root / "substrate" / "bin"
        substrate_bin.mkdir(parents=True)

        state_file = state_root / "claude-gate5-state.json"
        state_file.write_text(
            json.dumps(
                {
                    "context": "kind-substrate-preview",
                    "atespace": "lane-a-claude-20260924",
                    "worker_pool": "lane-a-claude-20260924",
                    "template_name": "live-agent-gate-lane-a-claude-20260924",
                    "actor_name": "claude-live-proof",
                    "namespace_uid": "lane-namespace-uid",
                }
            )
        )

        kubectl = fake_bin / "kubectl"
        kubectl.write_text(
            """#!/bin/bash
set -eu
printf '%s\\n' "$*" >> "${FAKE_KUBECTL_LOG}"
args=" $* "
if [[ ${args} == *" get namespace lane-a-claude-20260924 "* ]]; then
  printf '%s\\n' '{"metadata":{"uid":"lane-namespace-uid"}}'
  exit 0
fi
if [[ ${args} == *" get workerpool lane-a-claude-20260924 "* ]]; then
  printf '%s\\n' 'Error from server (NotFound): workerpool not found' >&2
  exit 1
fi
if [[ ${args} == *" get secret claude-oauth "* ]]; then
  if [[ ${FAKE_UNOWNED_SECRET:-0} == 1 ]]; then
    printf '%s\\n' 'secret/claude-oauth'
    exit 0
  fi
  count_file=${FAKE_KUBECTL_LOG}.secret-count
  count=0
  if [[ -f ${count_file} ]]; then read -r count < "${count_file}"; fi
  count=$((count + 1))
  printf '%s\\n' "${count}" > "${count_file}"
  uid=secret-uid
  if [[ ${FAKE_CHANGE_SECRET_UID:-0} == 1 && ${count} -gt 1 ]]; then
    uid=replacement-secret-uid
  fi
  printf '{"metadata":{"uid":"%s","labels":{"proof.mainloop.dev/lane":"lane-a-live-proof"}}}\\n' "${uid}"
  exit 0
fi
if [[ ${args} == *" get service credprovider "* ]]; then
  printf '%s\\n' '{"metadata":{"uid":"service-uid","labels":{"proof.mainloop.dev/lane":"lane-a-live-proof"}}}'
  exit 0
fi
if [[ ${args} == *" get deployment round3-claude-provider "* ]]; then
  printf '%s\\n' '{"metadata":{"uid":"deployment-uid","labels":{"proof.mainloop.dev/lane":"lane-a-live-proof"}}}'
  exit 0
fi
if [[ ${args} == *" get serviceaccount round3-claude-provider "* ]]; then
  printf '%s\\n' '{"metadata":{"uid":"serviceaccount-uid","labels":{"proof.mainloop.dev/lane":"lane-a-live-proof"}}}'
  exit 0
fi
if [[ ${args} == *" get networkpolicy round3-claude-provider "* ]]; then
  printf '%s\\n' '{"metadata":{"uid":"networkpolicy-uid","labels":{"proof.mainloop.dev/lane":"lane-a-live-proof"}}}'
  exit 0
fi
if [[ ${args} == *" delete --raw=/api/v1/namespaces/mainloop-control/"* || ${args} == *" delete --raw=/apis/"* ]]; then
  printf '%s\\n' "$*" >> "${FAKE_PROVIDER_DELETE_LOG}"
  cat >> "${FAKE_PROVIDER_DELETE_PAYLOADS}"
  printf '%s\\n' '---' >> "${FAKE_PROVIDER_DELETE_PAYLOADS}"
  exit 0
fi
if [[ ${args} == *" delete --raw="* ]]; then
  cat >/dev/null
  exit 0
fi
exit 0
"""
        )
        kubectl.chmod(0o755)

        ate = substrate_bin / "kubectl-ate"
        ate.write_text(
            """#!/bin/bash
set -eu
args=" $* "
if [[ ${args} == *" get actor "* || ${args} == *" get actor-template "* ]]; then
  printf '%s\\n' 'NotFound' >&2
  exit 1
fi
exit 0
"""
        )
        ate.chmod(0o755)

        env = os.environ.copy()
        env.update(
            {
                "PATH": f"{fake_bin}{os.pathsep}{env['PATH']}",
                "LIVE_PROOF_STATE_DIR": str(state_root),
                "SUBSTRATE_SRC": str(root / "substrate"),
                "FAKE_KUBECTL_LOG": str(root / "kubectl.log"),
                "FAKE_PROVIDER_DELETE_LOG": str(root / "provider-deletes.log"),
                "FAKE_PROVIDER_DELETE_PAYLOADS": str(
                    root / "provider-delete-payloads.jsonl"
                ),
                "FAKE_UNOWNED_SECRET": "1" if unowned_secret else "0",
                "FAKE_CHANGE_SECRET_UID": "1" if change_secret_uid else "0",
            }
        )
        return env, state_file

    def run_cleanup(self, env: dict[str, str]) -> subprocess.CompletedProcess[str]:
        script = (
            Path(__file__).resolve().parents[3]
            / "spikes/substrate-workspace-adapter/live/cleanup-lane-a.sh"
        )
        return subprocess.run(  # nosec B603 - fixed script path; kubectl resolves to a temp fake.
            ["/bin/bash", str(script), "claude"],
            env=env,
            capture_output=True,
            check=False,
            text=True,
        )

    def test_unowned_provider_preflight_then_failure_fallback_skips_deletes(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            env, _ = self.make_cleanup_environment(temp_dir, unowned_secret=True)
            live_dir = (
                Path(__file__).resolve().parents[3]
                / "spikes/substrate-workspace-adapter/live"
            )
            env["LIVE_DIR"] = str(live_dir)
            proof_script = (live_dir / "claude_turn_proof.sh").read_text()
            cleanup_start = proof_script.index("cleanup_provider() {")
            cleanup_end = proof_script.index("\nfinish() {", cleanup_start)
            cleanup_function = proof_script[cleanup_start:cleanup_end]
            finish_start = proof_script.index("finish() {")
            finish_end = proof_script.index("\ntrap finish EXIT INT TERM", finish_start)
            finish_function = proof_script[finish_start:finish_end]
            harness = "\n".join(
                (
                    "set -euo pipefail",
                    'source "${LIVE_DIR}/common.sh"',
                    "SCRIPT_DIR=${LIVE_DIR}",
                    "LANE_STARTED=1",
                    "COMPLETE=0",
                    "PROVIDER_READY=0",
                    "declare -A PROVIDER_UIDS=()",
                    "STATE_FILE=${LIVE_PROOF_STATE_DIR}/claude-gate5-state.json",
                    "stop_router() { :; }",
                    cleanup_function,
                    finish_function,
                    "trap finish EXIT INT TERM",
                    "preflight_provider_resources_absent",
                )
            )
            preflight_and_trap = (
                subprocess.run(  # nosec B603 - fixed trap/preflight with fake kubectl.
                    ["/bin/bash", "-c", harness],
                    env=env,
                    capture_output=True,
                    check=False,
                    text=True,
                )
            )
            self.assertNotEqual(preflight_and_trap.returncode, 0)
            self.assertIn(
                "secret/claude-oauth already exists", preflight_and_trap.stderr
            )
            self.assertIn("CLEANUP=PASS", preflight_and_trap.stdout)
            calls = Path(env["FAKE_KUBECTL_LOG"]).read_text().splitlines()
            provider_mutations = [
                call
                for call in calls
                if " delete " in f" {call} "
                and any(
                    token in call
                    for token in (
                        "claude-oauth",
                        "credprovider",
                        "round3-claude-provider",
                    )
                )
            ]
            self.assertEqual(provider_mutations, [])

    def test_provider_uid_change_aborts_cleanup_before_any_provider_delete(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            env, _ = self.make_cleanup_environment(temp_dir, change_secret_uid=True)
            cleanup = self.run_cleanup(env)

            self.assertNotEqual(cleanup.returncode, 0)
            self.assertIn(
                "provider cleanup skipped secret/claude-oauth", cleanup.stderr
            )
            self.assertNotIn("CLEANUP=PASS", cleanup.stdout)
            calls = Path(env["FAKE_KUBECTL_LOG"]).read_text().splitlines()
            provider_deletes = [
                call
                for call in calls
                if " delete --raw=/api/v1/namespaces/mainloop-control/" in call
                or " delete --raw=/apis/" in call
            ]
            self.assertEqual(provider_deletes, [])
            delete_options = Path(env["FAKE_KUBECTL_LOG"] + ".secret-count")
            self.assertEqual(delete_options.read_text(), "2\n")

    def test_owned_provider_deletes_use_the_verified_uid_precondition(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            env, _ = self.make_cleanup_environment(temp_dir)
            cleanup = self.run_cleanup(env)

            self.assertEqual(cleanup.returncode, 0, cleanup.stderr)
            provider_deletes = (
                Path(env["FAKE_PROVIDER_DELETE_LOG"]).read_text().splitlines()
            )
            self.assertEqual(len(provider_deletes), 5)
            payloads = [
                json.loads(payload)
                for payload in Path(env["FAKE_PROVIDER_DELETE_PAYLOADS"])
                .read_text()
                .split("---")
                if payload.strip()
            ]
            self.assertEqual(len(payloads), 5)
            self.assertEqual(
                {payload["preconditions"]["uid"] for payload in payloads},
                {
                    "secret-uid",
                    "service-uid",
                    "deployment-uid",
                    "serviceaccount-uid",
                    "networkpolicy-uid",
                },
            )


def asyncio_run(coro):
    import asyncio

    return asyncio.run(coro)


if __name__ == "__main__":
    unittest.main()
