"""Strict dev manifest validation and fake actor provisioning."""

import base64
import unittest
from unittest.mock import AsyncMock, Mock, patch

from mainloop.runtime.actor_provisioner import (
    FakeActorProvisioner,
    SubstrateActorProvisioner,
)
from mainloop.runtime.substrate import ActorRecord, ActorState
from pydantic import ValidationError

from models import WorkspaceManifest


def manifest(dev: dict) -> WorkspaceManifest:
    return WorkspaceManifest(
        branch="feature/dev-env",
        resource_class="default",
        dev=dev,
    )


class WorkspaceDevManifestTests(unittest.TestCase):
    def test_accepts_image_services_ports_and_timeout(self):
        result = manifest(
            {
                "image": "node:22",
                "actor_template": "sample",
                "services": [
                    {
                        "name": "postgres",
                        "image": "postgres:16",
                        "env": {"POSTGRES_DB": "workspace"},
                        "ports": [5432],
                    }
                ],
                "ports": [{"name": "app", "number": 3000, "protocol": "http"}],
                "idle_timeout_minutes": 45,
            }
        )

        self.assertEqual(result.dev.image, "node:22")
        self.assertEqual(result.dev.services[0].ports, (5432,))
        self.assertEqual(result.dev.ports[0].number, 3000)
        self.assertEqual(result.dev.idle_timeout_minutes, 45)

    def test_requires_exactly_one_image_source(self):
        for dev in ({}, {"image": "node:22", "devcontainer_ref": "ghcr.io/dev"}):
            with self.subTest(dev=dev), self.assertRaises(ValidationError):
                manifest(dev)

    def test_rejects_unknown_fields_and_duplicate_ports(self):
        with self.assertRaisesRegex(ValidationError, "extra_forbidden"):
            manifest({"image": "node:22", "surprise": True})
        with self.assertRaisesRegex(ValidationError, "unique"):
            manifest(
                {
                    "image": "node:22",
                    "ports": [
                        {"name": "app", "number": 3000},
                        {"name": "web", "number": 3000},
                    ],
                }
            )

    def test_rejects_out_of_range_timeout_and_duplicate_service_names(self):
        with self.assertRaises(ValidationError):
            manifest({"image": "node:22", "idle_timeout_minutes": 0})
        with self.assertRaisesRegex(ValidationError, "unique"):
            manifest(
                {
                    "image": "node:22",
                    "services": [
                        {"name": "db", "image": "postgres:16"},
                        {"name": "db", "image": "redis:7"},
                    ],
                }
            )

    def test_rejects_non_strict_timeout(self):
        with self.assertRaises(ValidationError):
            manifest({"image": "node:22", "idle_timeout_minutes": "30"})


class FakeProvisionerTests(unittest.IsolatedAsyncioTestCase):
    async def test_fake_creates_and_deletes_actor_and_secret(self):
        provisioner = FakeActorProvisioner()
        # Test value is a Kubernetes Secret name, not credential material.
        created = await provisioner.create(  # nosec B106
            atespace="workspaces",
            actor_name="ml-feature-1",
            template="sample-template",
            shim_token_secret_name="ml-feature-1-shim",
        )

        self.assertEqual(created.actor.name, "ml-feature-1")
        self.assertIn("ml-feature-1-shim", provisioner.secrets)
        await provisioner.delete(
            atespace="workspaces",
            actor_name="ml-feature-1",
            shim_token_secret_name=created.shim_token_secret_name,
        )
        self.assertFalse(provisioner.actors)
        self.assertFalse(provisioner.secrets)

    async def test_substrate_provisioner_stores_a_random_token_in_secret(self):
        actor = ActorRecord(
            atespace="workspaces",
            name="ml-branch-1",
            uid="actor-1",
            state=ActorState.RUNNING,
            external_snapshot_uri=None,
            current_actor_template_uid="template-1",
            raw={},
        )
        control = Mock()
        control.get_actor = AsyncMock(return_value=actor)
        control.create_actor = AsyncMock()
        core_api = Mock()
        provisioner = SubstrateActorProvisioner(control=control, core_api=core_api)

        with patch(
            "mainloop.runtime.actor_provisioner.secrets.token_urlsafe",
            return_value="private-token",
        ):
            # Test value is a Kubernetes Secret name, not credential material.
            result = await provisioner.create(  # nosec B106
                atespace="workspaces",
                actor_name="ml-branch-1",
                template="project-template",
                shim_token_secret_name="ml-branch-1-shim",
            )

        secret = core_api.create_namespaced_secret.call_args.args[1]
        self.assertEqual(base64.b64decode(secret.data["token"]), b"private-token")
        self.assertEqual(result.actor, actor)
        self.assertEqual(result.shim_token_secret_name, "ml-branch-1-shim")
        control.create_actor.assert_not_awaited()


if __name__ == "__main__":
    unittest.main()
