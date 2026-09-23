"""Fake-backed checks for the private, actor-targeted credential delivery path."""

import json
import os
from contextlib import redirect_stdout
from io import StringIO
from pathlib import Path
import tempfile
import unittest
from types import SimpleNamespace

from scripts import gate5_deliver_credentials


class Gate5CredentialDeliveryTests(unittest.TestCase):
    def setUp(self):
        self.tempdir = tempfile.TemporaryDirectory()
        self.addCleanup(self.tempdir.cleanup)
        self.root = Path(self.tempdir.name)
        self.credential_path = self.root / "private-provider-file"
        self.credential_value = b"fixture-provider-credential-never-for-argv-or-logs"
        self.credential_path.write_bytes(self.credential_value)
        self.state_path = self.root / "private-state.json"
        self.state_path.write_text(
            json.dumps(
                {
                    "context": "kind-substrate-preview",
                    "atespace": "native-claude",
                    "actor_name": "claude-final",
                    "actor_uid": "actor-uid-fixture",
                    "shim_token": "fixture-shim-token-with-at-least-32-characters",
                }
            ),
            encoding="utf-8",
        )
        self.handover_path = self.root / "shared-cluster.md"
        self.handover_path.write_text("**Handover:** done — 2026-09-23 17:00 UTC\n", encoding="utf-8")
        self.args = SimpleNamespace(
            context="kind-substrate-preview",
            kubeconfig="/fixture/kubeconfig",
            actor_namespace="native-claude",
            actor_name="claude-final",
            state_file=str(self.state_path),
            credential="claude",
            claude_token_file=str(self.credential_path),
            codex_auth_file="/fixture/not-used",
            image=gate5_deliver_credentials.DEFAULT_IMAGE,
        )

    def fake_runner(self, calls, data_seen):
        def runner(argv, **kwargs):
            calls.append((argv, kwargs))
            if "create" in argv and "secret" in argv:
                fds = kwargs.get("pass_fds", ())
                self.assertEqual(len(fds), 1)
                data_seen.append(os.pread(fds[0], 1024 * 1024, 0))
                return SimpleNamespace(stdout=b'{"apiVersion":"v1","kind":"Secret"}')
            return SimpleNamespace(stdout=b"completed")

        return runner

    def test_credentials_flow_through_control_secret_and_authenticated_job_without_path_or_value_in_argv(self):
        calls = []
        data_seen = []
        output = StringIO()
        with redirect_stdout(output):
            gate5_deliver_credentials.deliver_credentials(
                self.args,
                runner=self.fake_runner(calls, data_seen),
                handover_note=self.handover_path,
            )

        self.assertEqual(data_seen[0], self.credential_value)
        self.assertNotIn(self.args.state_file.encode(), b"".join(data_seen))
        self.assertEqual(len(calls), 8)
        for argv, _kwargs in calls:
            self.assertEqual(argv[0], "kubectl")
            self.assertIn("--context", argv)
            self.assertIn("kind-substrate-preview", argv)
            self.assertIn("--kubeconfig", argv)
            self.assertIn("/fixture/kubeconfig", argv)
            joined = " ".join(map(str, argv))
            self.assertNotIn(str(self.credential_path), joined)
            self.assertNotIn(self.credential_value.decode(), joined)
            self.assertNotIn(self.args.state_file, joined)

        secret_argv = calls[0][0]
        from_file = next(part for part in secret_argv if part.startswith("--from-file="))
        self.assertRegex(from_file, r"^--from-file=credential=/proc/self/fd/\d+$")
        job_manifest = json.loads(calls[5][1]["input"])
        self.assertEqual(job_manifest["kind"], "Job")
        self.assertEqual(job_manifest["metadata"]["namespace"], "mainloop-control")
        pod_spec = job_manifest["spec"]["template"]["spec"]
        self.assertEqual(pod_spec["securityContext"]["fsGroup"], 10001)
        self.assertEqual(pod_spec["volumes"][1]["secret"]["defaultMode"], 0o440)
        container = pod_spec["containers"][0]
        self.assertEqual(container["env"][0], {"name": "CREDENTIAL_KIND", "value": "claude"})
        self.assertEqual(container["env"][1], {"name": "ACTOR_NAMESPACE", "value": "native-claude"})
        self.assertNotIn(self.credential_value.decode(), str(job_manifest))
        self.assertNotIn(str(self.credential_path), str(job_manifest))
        self.assertEqual(
            output.getvalue().strip(),
            "credential delivered for claude to native-claude/claude-final",
        )

    def test_pending_handover_refuses_before_any_kubectl_call(self):
        calls = []
        self.handover_path.write_text("**Handover:** pending\n", encoding="utf-8")
        with self.assertRaisesRegex(RuntimeError, "handover is not done"):
            gate5_deliver_credentials.deliver_credentials(
                self.args,
                runner=self.fake_runner(calls, []),
                handover_note=self.handover_path,
            )
        self.assertEqual(calls, [])

    def test_actor_ownership_mismatch_refuses_before_any_kubectl_call(self):
        calls = []
        state = json.loads(self.state_path.read_text(encoding="utf-8"))
        state["atespace"] = "native-codex"
        self.state_path.write_text(json.dumps(state), encoding="utf-8")
        with self.assertRaisesRegex(RuntimeError, "does not own"):
            gate5_deliver_credentials.deliver_credentials(
                self.args,
                runner=self.fake_runner(calls, []),
                handover_note=self.handover_path,
            )
        self.assertEqual(calls, [])

    def test_credentials_are_not_wired_into_the_golden_setup_flow(self):
        setup_source = Path(gate5_deliver_credentials.__file__).with_name("gate5_setup.py")
        source = setup_source.read_text(encoding="utf-8")
        self.assertNotIn("gate5_deliver_credentials", source)
        self.assertNotIn("deliver_credentials(", source)


if __name__ == "__main__":
    unittest.main()
