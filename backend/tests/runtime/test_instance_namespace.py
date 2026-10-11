"""MAINLOOP_NAMESPACE: production values stay byte-identical; other namespaces get their own."""

import os
import unittest
import uuid
from contextlib import contextmanager
from unittest.mock import AsyncMock, patch

from fastapi.testclient import TestClient
from mainloop.config import Settings, settings
from mainloop.mcp_app import create_app
from mainloop.push_gate.credentials import _git_reference
from mainloop.push_gate.transport import GitApplication
from mainloop.runtime.agent_credentials import credential_reference
from mainloop.runtime.agent_tools import AgentService
from pydantic import ValidationError
from tests.runtime.test_context_model import KINDS, FakeStore

ISSUANCE = uuid.UUID("00000000-0000-4000-8000-0000000000c1")
PROD_MCP_HOST = "mainloop-mcp.mainloop.svc.cluster.local"
DEV_MCP_HOST = "mainloop-mcp.mainloop-dev.svc.cluster.local"


def load(**values) -> Settings:
    with patch.dict(os.environ, {}, clear=False):
        os.environ.pop("MAINLOOP_NAMESPACE", None)
        os.environ.pop("GIT_READ_ORIGIN", None)
        os.environ.pop("GIT_PUSH_ORIGIN", None)
        return Settings(_env_file=None, **values)


@contextmanager
def instance(namespace: str):
    """Point the shared settings object at one namespace, as the env would at startup."""
    loaded = load(mainloop_namespace=namespace)
    with patch.multiple(
        settings,
        mainloop_namespace=loaded.mainloop_namespace,
        git_read_origin=loaded.git_read_origin,
        git_push_origin=loaded.git_push_origin,
    ):
        yield


class ProductionPinTests(unittest.TestCase):
    """With no new settings, every value is the one production used before the setting."""

    def test_settings(self):
        loaded = load()
        self.assertEqual(loaded.mainloop_namespace, "mainloop")
        self.assertEqual(
            loaded.mcp_origin, "http://mainloop-mcp.mainloop.svc.cluster.local"
        )
        self.assertEqual(loaded.mcp_host, PROD_MCP_HOST)
        self.assertEqual(
            loaded.git_read_origin,
            "http://mainloop-git-read.mainloop.svc.cluster.local",
        )
        self.assertEqual(
            loaded.git_push_origin,
            "http://mainloop-git-push.mainloop.svc.cluster.local",
        )
        self.assertEqual(loaded.credential_secret_prefix, "mainloop")

    def test_references_and_hosts(self):
        with instance("mainloop"):
            ref = credential_reference("binding-id")
            self.assertEqual(
                (ref.origin, ref.header, ref.secret_name, ref.secret_key),
                (
                    "http://mainloop-mcp.mainloop.svc.cluster.local",
                    "Authorization",
                    "mainloop-mcp-binding-id",
                    "authorization",
                ),
            )
            for purpose in ("read", "push"):
                ref = _git_reference(f"git-{purpose}", ISSUANCE)
                self.assertEqual(
                    ref.origin,
                    f"http://mainloop-git-{purpose}.mainloop.svc.cluster.local",
                )
                self.assertEqual(ref.secret_name, f"mainloop-git-{purpose}-{ISSUANCE}")
                self.assertEqual(
                    GitApplication(f"git-{purpose}").hosts,
                    (f"mainloop-git-{purpose}.mainloop.svc.cluster.local",),
                )
            self.assertEqual(mcp_status(PROD_MCP_HOST), 200)
            self.assertEqual(mcp_status(f"{PROD_MCP_HOST}:80"), 200)
            self.assertEqual(mcp_status(DEV_MCP_HOST), 421)


class DevNamespaceTests(unittest.TestCase):
    def test_settings(self):
        loaded = load(mainloop_namespace="mainloop-dev")
        self.assertEqual(
            loaded.mcp_origin, "http://mainloop-mcp.mainloop-dev.svc.cluster.local"
        )
        self.assertEqual(
            loaded.git_read_origin,
            "http://mainloop-git-read.mainloop-dev.svc.cluster.local",
        )
        self.assertEqual(
            loaded.git_push_origin,
            "http://mainloop-git-push.mainloop-dev.svc.cluster.local",
        )
        self.assertEqual(loaded.credential_secret_prefix, "mainloop-dev")

    def test_references_and_hosts(self):
        with instance("mainloop-dev"):
            ref = credential_reference("binding-id")
            self.assertEqual(
                ref.origin, "http://mainloop-mcp.mainloop-dev.svc.cluster.local"
            )
            self.assertEqual(ref.secret_name, "mainloop-dev-mcp-binding-id")
            for purpose in ("read", "push"):
                ref = _git_reference(f"git-{purpose}", ISSUANCE)
                self.assertEqual(
                    ref.origin,
                    f"http://mainloop-git-{purpose}.mainloop-dev.svc.cluster.local",
                )
                self.assertEqual(
                    ref.secret_name, f"mainloop-dev-git-{purpose}-{ISSUANCE}"
                )
                self.assertEqual(
                    GitApplication(f"git-{purpose}").hosts,
                    (f"mainloop-git-{purpose}.mainloop-dev.svc.cluster.local",),
                )
            self.assertEqual(mcp_status(DEV_MCP_HOST), 200)
            self.assertEqual(mcp_status(f"{DEV_MCP_HOST}:80"), 200)
            self.assertEqual(mcp_status(PROD_MCP_HOST), 421)
            self.assertEqual(mcp_status(f"{PROD_MCP_HOST}:80"), 421)


class ValidatorTests(unittest.TestCase):
    def test_git_origins_outside_the_configured_namespace_are_rejected(self):
        for namespace, origin in (
            ("mainloop", "http://mainloop-git-read.mainloop-dev.svc.cluster.local"),
            ("mainloop-dev", "http://mainloop-git-read.mainloop.svc.cluster.local"),
            ("mainloop-dev", "http://mainloop-git-push.mainloop-dev.svc.cluster.local"),
            (
                "mainloop-dev",
                "https://mainloop-git-read.mainloop-dev.svc.cluster.local",
            ),
            ("mainloop-dev", "http://evil.example"),
        ):
            with self.subTest(namespace=namespace, origin=origin):
                with self.assertRaises(ValidationError):
                    load(mainloop_namespace=namespace, git_read_origin=origin)

    def test_explicit_matching_origins_are_accepted(self):
        loaded = load(
            mainloop_namespace="mainloop-dev",
            git_read_origin="http://mainloop-git-read.mainloop-dev.svc.cluster.local",
            git_push_origin="http://mainloop-git-push.mainloop-dev.svc.cluster.local",
        )
        self.assertEqual(loaded.mainloop_namespace, "mainloop-dev")

    def test_namespace_must_be_a_label_that_cannot_shadow_production(self):
        for namespace in (
            "",
            "Mainloop",
            "mainloop.svc",
            "a/b",
            "-x",
            "x" * 64,
            "mainloop-mcp",
            "mainloop-git-read",
        ):
            with self.subTest(namespace=namespace):
                with self.assertRaises(ValidationError):
                    load(mainloop_namespace=namespace)

    def test_env_sets_the_namespace(self):
        with patch.dict(os.environ, {"MAINLOOP_NAMESPACE": "mainloop-dev"}):
            self.assertEqual(
                Settings(_env_file=None).credential_secret_prefix, "mainloop-dev"
            )


def mcp_status(host: str) -> int:
    with patch(
        "mainloop.tasks.lifecycle.authenticate_session", AsyncMock(return_value=None)
    ):
        with TestClient(create_app(AgentService(FakeStore(), KINDS))) as client:
            response = client.post(
                "/mcp",
                headers={
                    "Host": host,
                    "Authorization": "Bearer tok-main",
                    "Accept": "application/json, text/event-stream",
                },
                json={
                    "jsonrpc": "2.0",
                    "id": 1,
                    "method": "initialize",
                    "params": {
                        "protocolVersion": "2025-06-18",
                        "capabilities": {},
                        "clientInfo": {"name": "fixture", "version": "1"},
                    },
                },
            )
    return response.status_code


if __name__ == "__main__":
    unittest.main()
