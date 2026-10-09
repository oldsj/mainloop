"""Offline checks of the tracked Kubernetes manifests; no cluster is contacted."""

import shutil
import subprocess  # nosec B404 - only runs the local kubectl on tracked manifests
import unittest
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[3]
BASE = ROOT / "k8s/apps/mainloop/base"
SECRET_VERBS = {
    "get",
    "list",
    "watch",
    "create",
    "update",
    "patch",
    "delete",
    "deletecollection",
    "*",
}


def build(path: Path) -> list[dict]:
    """Render a kustomization with ``kubectl kustomize`` (client-side, offline)."""
    kubectl = shutil.which("kubectl")
    if kubectl is None:
        raise unittest.SkipTest("kubectl is not installed")
    out = subprocess.run(  # nosec B603 - the resolved kubectl, run on a tracked kustomization
        [kubectl, "kustomize", str(path)],
        check=True,
        capture_output=True,
        text=True,
    ).stdout
    return [doc for doc in yaml.safe_load_all(out) if doc]


def grants_secrets(rule: dict) -> bool:
    return bool(
        {"secrets", "*"} & set(rule.get("resources", []))
        and SECRET_VERBS & set(rule.get("verbs", []))
    )


class BaseRBACTests(unittest.TestCase):
    def test_base_grants_no_cluster_scoped_secret_verbs(self):
        """The trusted publisher manages binding Secrets through a namespace Role."""
        docs = [
            doc
            for path in sorted(BASE.glob("*.yaml"))
            for doc in yaml.safe_load_all(path.read_text())
            if doc
        ]
        cluster_roles = [d for d in docs if d["kind"] == "ClusterRole"]
        for role in cluster_roles:
            for rule in role.get("rules", []):
                self.assertFalse(
                    grants_secrets(rule),
                    f"ClusterRole {role['metadata']['name']} reads Secrets cluster-wide",
                )
        # A binding to a role that is not defined here (cluster-admin, a built-in) would
        # bypass the check above.
        defined = {d["metadata"]["name"] for d in cluster_roles}
        for binding in (d for d in docs if d["kind"] == "ClusterRoleBinding"):
            self.assertIn(binding["roleRef"]["name"], defined)
            self.assertNotIn(
                binding["roleRef"]["name"], {"cluster-admin", "admin", "edit"}
            )

    def test_rendered_prod_overlay_grants_no_cluster_scoped_secret_verbs(self):
        for doc in build(ROOT / "k8s/apps/mainloop/overlays/prod"):
            if doc["kind"] == "ClusterRole":
                for rule in doc.get("rules", []):
                    self.assertFalse(grants_secrets(rule), doc["metadata"]["name"])

    def test_the_checker_catches_a_secret_reading_rule(self):
        self.assertTrue(grants_secrets({"resources": ["secrets"], "verbs": ["get"]}))
        self.assertTrue(grants_secrets({"resources": ["*"], "verbs": ["*"]}))
        self.assertTrue(grants_secrets({"resources": ["secrets"], "verbs": ["patch"]}))
        self.assertFalse(grants_secrets({"resources": ["pods"], "verbs": ["get"]}))


class KindOverlayTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.docs = build(ROOT / "k8s/apps/mainloop/overlays/kind")

    def resource(self, kind, name):
        return next(
            d for d in self.docs if d["kind"] == kind and d["metadata"]["name"] == name
        )

    def test_exposure_and_mcp_are_separate(self):
        for name, port in (("mainloop-frontend", 30300), ("mainloop-backend", 30800)):
            service = self.resource("Service", name)
            self.assertEqual(service["spec"]["type"], "NodePort")
            self.assertEqual(
                [p.get("nodePort") for p in service["spec"]["ports"]], [port]
            )
        self.assertNotEqual(
            self.resource("Service", "mainloop-mcp")["spec"].get("type"), "NodePort"
        )
        for p in self.resource("Service", "mainloop-mcp")["spec"]["ports"]:
            self.assertNotIn("nodePort", p)
        pod = self.resource("Deployment", "mainloop-backend")["spec"]["template"][
            "spec"
        ]
        self.assertEqual({c["name"] for c in pod["containers"]}, {"backend", "mcp"})
        self.assertFalse(pod.get("imagePullSecrets"))
        self.resource("StatefulSet", "mainloop-postgres")
        self.resource("NetworkPolicy", "mainloop-backend-ingress")

    def test_backend_processes_use_only_app_secret_references(self):
        for overlay in (
            BASE,
            ROOT / "k8s/apps/mainloop/overlays/kind",
            ROOT / "k8s/apps/mainloop/overlays/prod",
        ):
            deployment = next(
                doc
                for doc in build(overlay)
                if doc["kind"] == "Deployment"
                and doc["metadata"]["name"] == "mainloop-backend"
            )
            for container in deployment["spec"]["template"]["spec"]["containers"]:
                with self.subTest(overlay=overlay.name, container=container["name"]):
                    env = {entry["name"]: entry for entry in container.get("env", [])}
                    self.assertNotIn("GITHUB_TOKEN", env)
                    for name, key in (
                        ("GITHUB_APP_ID", "github-app-id"),
                        ("GITHUB_APP_PRIVATE_KEY", "github-app-private-key"),
                    ):
                        self.assertEqual(
                            env[name],
                            {
                                "name": name,
                                "valueFrom": {
                                    "secretKeyRef": {
                                        "name": "mainloop-secrets",
                                        "key": key,
                                    }
                                },
                            },
                        )

    def test_binding_secret_publisher_permissions(self):
        role = self.resource("Role", "mainloop-agent-tokens")
        self.assertEqual(
            role["rules"],
            [
                {
                    "apiGroups": [""],
                    "resources": ["secrets"],
                    "verbs": ["get", "create", "delete"],
                }
            ],
        )
        self.assertFalse(
            any(
                d["kind"] == "Secret"
                and d["metadata"]["name"] == "mainloop-agent-tokens"
                for d in self.docs
            )
        )

    def test_integration_keeps_its_namespace_and_has_no_embedded_credentials(self):
        for kind, name in (
            ("Role", "mainloop-agent-tokens"),
            ("RoleBinding", "mainloop-agent-tokens"),
            ("RemoteMCPServer", "mainloop"),
        ):
            self.assertEqual(
                self.resource(kind, name)["metadata"]["namespace"], "kagent"
            )
        for doc in self.docs:
            if doc["kind"] == "Secret":
                self.assertFalse(doc.get("data") or doc.get("stringData"))
            if doc["kind"] in ("Deployment", "StatefulSet"):
                for container in doc["spec"]["template"]["spec"]["containers"]:
                    for env in container.get("env", []):
                        if env["name"] in (
                            "DB_PASSWORD",
                            "POSTGRES_PASSWORD",
                            "AGENT_TOKEN_KEY",
                            "GITHUB_APP_ID",
                            "GITHUB_APP_PRIVATE_KEY",
                        ):
                            self.assertIn("secretKeyRef", env["valueFrom"])

    def test_workspaces_use_dedicated_nonexpiring_full_snapshot_harnesses(self):
        config = self.resource("ConfigMap", "mainloop-config")["data"]
        for kind in ("CLAUDE", "CODEX"):
            name = config[f"KAGENT_WORKSPACE_{kind}_AGENT"]
            self.assertNotEqual(name, config[f"KAGENT_{kind}_AGENT"])
            agent = self.resource("Agent", name)
            harness = self.resource("Harness", agent["spec"]["harnessRef"]["name"])[
                "spec"
            ]
            self.assertEqual(harness["sessionIdleTTL"], "0s")
            self.assertEqual(
                harness["substrate"]["snapshotPolicy"]["onQuiesce"], "Full"
            )
            self.assertEqual(harness["git"]["origins"], ["github.com"])
        self.assertEqual(
            self.resource("Harness", "mainloop-main-harness")["spec"]["sessionIdleTTL"],
            "0s",
        )


if __name__ == "__main__":
    unittest.main()
