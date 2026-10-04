"""Offline integration and development checks; fixtures are vendored, no cluster required."""

import copy
import unittest
from pathlib import Path

import jsonschema
import yaml

ROOT = Path(__file__).resolve().parents[3]


class MCPManifestTests(unittest.TestCase):
    def setUp(self):
        self.crd = yaml.safe_load(
            (
                Path(__file__).parent / "fixtures/kagent/remotemcpserver-crd.yaml"
            ).read_text()
        )
        self.manifest = yaml.safe_load(
            (ROOT / "k8s/integrations/kagent/mainloop-mcp.yaml").read_text()
        )

    def validate(self, manifest):
        group, version = manifest["apiVersion"].split("/")
        self.assertEqual(group, self.crd["spec"]["group"])
        self.assertEqual(manifest["kind"], self.crd["spec"]["names"]["kind"])
        schema = next(
            v["schema"]["openAPIV3Schema"]
            for v in self.crd["spec"]["versions"]
            if v["served"] and v["name"] == version
        )
        jsonschema.Draft4Validator(schema).validate(manifest)

    def test_remote_mcp_manifest_matches_vendored_served_crd(self):
        self.validate(self.manifest)
        self.assertEqual(
            self.manifest["spec"]["headersFrom"],
            [{"name": "Authorization", "value": "Bearer kagent-credential-injected"}],
        )

    def test_rejects_wrong_version_and_missing_description(self):
        old = copy.deepcopy(self.manifest)
        old["apiVersion"] = "kagent.dev/v1alpha2"
        with self.assertRaises(AssertionError):
            self.validate(old)
        missing = copy.deepcopy(self.manifest)
        del missing["spec"]["description"]
        with self.assertRaises(jsonschema.ValidationError):
            self.validate(missing)
        wrong = copy.deepcopy(self.manifest)
        wrong["apiVersion"] = "api.kagent.dev/v1alpha2"
        with self.assertRaises(StopIteration):
            self.validate(wrong)

    def test_devspace_syncs_and_reloads_both_python_containers(self):
        config = yaml.safe_load((ROOT / "devspace.yaml").read_text())
        rest, mcp = config["dev"]["backend"], config["dev"]["mcp"]
        self.assertEqual(rest["container"], "backend")
        self.assertEqual(mcp["container"], "mcp")
        self.assertEqual(rest["sync"], mcp["sync"])
        self.assertEqual(
            {entry["path"] for entry in mcp["sync"]},
            {
                "./backend/src:/app/src",
                "./models:/models",
                "./backend/pyproject.toml:/app/pyproject.toml",
                "./backend/uv.lock:/app/uv.lock",
            },
        )
        for name, entry, app, port in (
            ("backend", rest, "mainloop.api:app", "8000"),
            ("mcp", mcp, "mainloop.mcp_app:app", "8002"),
        ):
            self.assertIn(app, entry["command"], name)
            self.assertIn(port, entry["command"], name)
            self.assertIn("--reload", entry["command"], name)
            self.assertIn("/app/src", entry["command"], name)
            self.assertIn("/models", entry["command"], name)
            self.assertEqual(entry["devImage"], "mainloop-backend:dev")
