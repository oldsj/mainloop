"""Workspace creation is refused for now; deletion of existing workspaces uses fakes."""

import unittest
from contextlib import asynccontextmanager
from unittest.mock import AsyncMock, patch

from fastapi import FastAPI, HTTPException
from fastapi.testclient import TestClient
from mainloop.runtime import workspace_api
from mainloop.runtime.actor_provisioner import FakeActorProvisioner
from mainloop.runtime.substrate import ActorRecord, ActorState


class FakeConnection:
    def __init__(self, *, project=True):
        self.project = project
        self.statements = []
        self.native_binding = None
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
        if "FROM native_bindings" in query:
            return self.native_binding
        raise AssertionError(f"unexpected query: {query}")

    async def execute(self, query, *args):
        self.statements.append((query, args))
        if "INSERT INTO workspace_bindings" in query:
            self.workspace_row = {
                "atespace": args[1],
                "actor_name": args[2],
                "shim_token_secret_name": args[4],
            }
        if "INSERT INTO native_bindings" in query:
            self.native_binding = {
                "session_id": args[0],
                "kind": args[1],
            }


def fake_connection(connection):
    @asynccontextmanager
    async def connect():
        yield connection

    return connect


class WorkspaceProvisioningApiTests(unittest.IsolatedAsyncioTestCase):
    def test_create_is_refused_until_workspaces_move_to_kagent(self):
        connection = FakeConnection()
        provisioner = FakeActorProvisioner()
        app = FastAPI()
        app.include_router(workspace_api.router)
        with (
            patch.object(
                workspace_api.db, "connection", new=fake_connection(connection)
            ),
            patch.object(
                workspace_api, "get_actor_provisioner", return_value=provisioner
            ),
            TestClient(app) as client,
        ):
            for body in (
                {"project_id": "project-1", "branch": "main", "dev": {"image": "x"}},
                {},
            ):
                response = client.post(
                    "/workspaces", json=body, headers={"X-User-ID": "owner-1"}
                )
                self.assertEqual(response.status_code, 409)
                self.assertEqual(
                    response.json(),
                    {"detail": "workspaces move to kagent in a later slice"},
                )
        self.assertEqual(connection.statements, [])
        self.assertEqual(provisioner.actors, {})
        self.assertEqual(provisioner.secrets, set())

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
