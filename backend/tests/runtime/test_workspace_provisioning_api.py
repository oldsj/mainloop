"""Project ownership and actor provisioning orchestration use fakes."""

import unittest
from contextlib import asynccontextmanager
from datetime import UTC, datetime
from unittest.mock import AsyncMock, patch

from fastapi import HTTPException
from mainloop.runtime import workspace_api
from mainloop.runtime.actor_provisioner import FakeActorProvisioner, ProvisionedActor
from mainloop.runtime.substrate import ActorRecord, ActorState

from models import (
    WorkspaceDesiredState,
    WorkspaceLifecycle,
    WorkspaceManifest,
    WorkspaceObservedState,
)


class FakeConnection:
    def __init__(self, *, project=True):
        self.project = project
        self.statements = []
        self.workspace_row = {
            "atespace": "mainloop-workspaces",
            "actor_name": "ml-workspace",
            "shim_token_secret_name": "ml-workspace-shim",
            "desired_state": "active",
            "conversation_id": "conversation-1",
        }

    @asynccontextmanager
    async def transaction(self):
        yield

    async def fetchrow(self, query, *_args):
        if "FROM projects" in query:
            return (
                {"id": "project-1", "html_url": "https://github.com/example/repo"}
                if self.project
                else None
            )
        if "FROM main_threads" in query:
            return None
        if "FROM workspace_bindings" in query:
            return self.workspace_row
        raise AssertionError(f"unexpected query: {query}")

    async def execute(self, query, *args):
        self.statements.append((query, args))


def fake_connection(connection):
    @asynccontextmanager
    async def connect():
        yield connection

    return connect


def lifecycle(workspace_id: str, manifest: WorkspaceManifest) -> WorkspaceLifecycle:
    return WorkspaceLifecycle(
        workspace_id=workspace_id,
        session_id=workspace_id,
        desired_state=WorkspaceDesiredState.RUNNING,
        observed_state=WorkspaceObservedState.RUNNING,
        manifest=manifest,
        updated_at=datetime.now(UTC),
    )


class WorkspaceProvisioningApiTests(unittest.IsolatedAsyncioTestCase):
    async def test_create_persists_identity_and_provisions_one_actor(self):
        connection = FakeConnection()
        provisioner = FakeActorProvisioner()
        manifest = WorkspaceManifest(
            repo_url="https://github.com/example/repo",
            branch="feature/one",
            resource_class="default",
            dev={"image": "node:22", "actor_template": "project-template"},
        )
        actor = ActorRecord(
            atespace="mainloop-workspaces",
            name="ml-workspace",
            uid="actor-1",
            state=ActorState.RUNNING,
            external_snapshot_uri=None,
            current_actor_template_uid="template-1",
            raw={},
        )
        fake_create = AsyncMock(
            return_value=ProvisionedActor(actor, "ml-workspace-shim")
        )
        provisioner.create = fake_create
        with (
            patch.object(
                workspace_api.db, "connection", new=fake_connection(connection)
            ),
            patch.object(
                workspace_api, "get_actor_provisioner", return_value=provisioner
            ),
            patch.object(
                workspace_api.workspace_adapter,
                "_record_observation",
                new=AsyncMock(
                    side_effect=lambda workspace_id, **_kwargs: lifecycle(
                        workspace_id, manifest
                    )
                ),
            ),
            patch.object(workspace_api, "_publish", new=AsyncMock()),
        ):
            result = await workspace_api.create_workspace(
                workspace_api.CreateWorkspaceRequest(
                    project_id="project-1",
                    branch="feature/one",
                    dev={"image": "node:22", "actor_template": "project-template"},
                ),
                user_id="owner-1",
            )

        self.assertEqual(result.manifest.branch, "feature/one")
        self.assertEqual(result.observed_state, WorkspaceObservedState.RUNNING)
        self.assertTrue(
            any(
                "INSERT INTO workspace_bindings" in query
                for query, _ in connection.statements
            )
        )
        self.assertTrue(
            any(
                "INSERT INTO workspace_lifecycles" in query
                for query, _ in connection.statements
            )
        )
        kwargs = fake_create.await_args.kwargs
        self.assertEqual(kwargs["template"], "project-template")
        self.assertEqual(
            kwargs["shim_token_secret_name"],
            workspace_api.settings.shim_token_secret_name(
                workspace_api.settings.substrate_atespace, kwargs["actor_name"]
            ),
        )

    async def test_create_hides_projects_owned_by_another_user(self):
        connection = FakeConnection(project=False)
        with patch.object(
            workspace_api.db, "connection", new=fake_connection(connection)
        ):
            with self.assertRaises(HTTPException) as raised:
                await workspace_api.create_workspace(
                    workspace_api.CreateWorkspaceRequest(
                        project_id="project-1", branch="main", dev={"image": "node:22"}
                    ),
                    user_id="other-owner",
                )

        self.assertEqual(raised.exception.status_code, 404)
        self.assertFalse(connection.statements)

    async def test_delete_removes_actor_secret_and_workspace_records(self):
        connection = FakeConnection()
        provisioner = FakeActorProvisioner()
        provisioner.actors[("mainloop-workspaces", "ml-workspace")] = ActorRecord(
            atespace="mainloop-workspaces",
            name="ml-workspace",
            uid="actor-1",
            state=ActorState.RUNNING,
            external_snapshot_uri=None,
            current_actor_template_uid="template-1",
            raw={},
        )
        provisioner.secrets.add("ml-workspace-shim")
        with (
            patch.object(workspace_api, "_require_owned_workspace", new=AsyncMock()),
            patch.object(
                workspace_api.db, "connection", new=fake_connection(connection)
            ),
            patch.object(
                workspace_api.workspace_adapter,
                "_delivery_states",
                new=AsyncMock(return_value=set()),
            ),
            patch.object(
                workspace_api, "get_actor_provisioner", return_value=provisioner
            ),
        ):
            response = await workspace_api.delete_workspace(
                "workspace-1", user_id="owner-1"
            )

        self.assertEqual(response.status_code, 204)
        self.assertFalse(provisioner.actors)
        self.assertFalse(provisioner.secrets)
        self.assertTrue(
            any(
                "DELETE FROM workspace_lifecycles" in query
                for query, _ in connection.statements
            )
        )
        self.assertTrue(
            any(
                "DELETE FROM workspace_bindings" in query
                for query, _ in connection.statements
            )
        )
        self.assertTrue(
            any(
                "DELETE FROM native_deliveries" in query
                for query, _ in connection.statements
            )
        )
        self.assertTrue(
            any("DELETE FROM messages" in query for query, _ in connection.statements)
        )

    async def test_delete_refuses_open_deliveries_before_actor_mutation(self):
        connection = FakeConnection()
        provisioner = FakeActorProvisioner()
        with (
            patch.object(workspace_api, "_require_owned_workspace", new=AsyncMock()),
            patch.object(
                workspace_api.db, "connection", new=fake_connection(connection)
            ),
            patch.object(
                workspace_api.workspace_adapter,
                "_delivery_states",
                new=AsyncMock(return_value={"sending"}),
            ),
            patch.object(
                workspace_api, "get_actor_provisioner", return_value=provisioner
            ),
        ):
            with self.assertRaises(HTTPException) as raised:
                await workspace_api.delete_workspace("workspace-1", user_id="owner-1")

        self.assertEqual(raised.exception.status_code, 409)
        self.assertFalse(provisioner.actors)


if __name__ == "__main__":
    unittest.main()
