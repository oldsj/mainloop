"""Manifest contract for the preview backend's direct Substrate API access."""

from __future__ import annotations

import unittest
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[3]
OVERLAY = ROOT / "k8s/apps/mainloop/overlays/substrate-preview"


def load_documents(path: Path) -> list[dict]:
    return [doc for doc in yaml.safe_load_all(path.read_text()) if doc]


class SubstratePreviewManifestTests(unittest.TestCase):
    def test_backend_mounts_audience_scoped_token_for_the_direct_endpoint(self):
        deployment = load_documents(OVERLAY / "backend.yaml")[0]
        configmap = next(
            doc
            for doc in load_documents(OVERLAY / "configmap.yaml")
            if doc["kind"] == "ConfigMap"
        )
        pod = deployment["spec"]["template"]["spec"]
        container = pod["containers"][0]
        volume = next(
            volume
            for volume in pod["volumes"]
            if volume["name"] == "substrate-api-token"
        )
        token_projection = volume["projected"]["sources"][0]["serviceAccountToken"]

        self.assertEqual(pod["serviceAccountName"], "mainloop-backend")
        self.assertEqual(
            token_projection,
            {
                "audience": "api.ate-system.svc",
                "expirationSeconds": 3600,
                "path": "token",
            },
        )
        self.assertIn(
            {
                "name": "substrate-api-token",
                "mountPath": "/var/run/secrets/tokens/substrate-api",
                "readOnly": True,
            },
            container["volumeMounts"],
        )
        self.assertEqual(
            configmap["data"]["SUBSTRATE_ENDPOINT"], "api.ate-system.svc:443"
        )
        self.assertEqual(
            configmap["data"]["SUBSTRATE_TOKEN_FILE"],
            "/var/run/secrets/tokens/substrate-api/token",
        )

    def test_cluster_role_only_lists_substrate_trust_bundles(self):
        documents = load_documents(OVERLAY / "backend-rbac.yaml")
        cluster_role = next(doc for doc in documents if doc["kind"] == "ClusterRole")
        binding = next(doc for doc in documents if doc["kind"] == "ClusterRoleBinding")

        self.assertEqual(
            cluster_role["rules"],
            [
                {
                    "apiGroups": ["certificates.k8s.io"],
                    "resources": ["clustertrustbundles"],
                    "verbs": ["list"],
                }
            ],
        )
        self.assertEqual(
            binding["subjects"],
            [
                {
                    "kind": "ServiceAccount",
                    "name": "mainloop-backend",
                    "namespace": "mainloop-control",
                }
            ],
        )


if __name__ == "__main__":
    unittest.main()
