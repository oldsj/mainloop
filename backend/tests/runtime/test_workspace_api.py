"""Workspace API projection and ownership behavior with adapter fakes."""

import unittest
from datetime import UTC, datetime
from unittest.mock import AsyncMock, patch

from fastapi import HTTPException
from mainloop.runtime import workspace_api
from mainloop.runtime.contracts import ContractError

from models import (
    WorkspaceAgentKind,
    WorkspaceDesiredState,
    WorkspaceLifecycle,
    WorkspaceManifest,
    WorkspaceObservedState,
)


def lifecycle() -> WorkspaceLifecycle:
    return WorkspaceLifecycle(
        workspace_id="session-1",
        session_id="session-1",
        desired_state=WorkspaceDesiredState.RUNNING,
        observed_state=WorkspaceObservedState.RUNNING,
        manifest=WorkspaceManifest(
            repo_url="https://github.com/example/repo",
            branch="main",
            agent_kinds=(WorkspaceAgentKind.CODEX,),
            skills=("skill://review",),
            mcp_servers=("mcp://docs",),
            egress_allowlist=("github.com",),
            resource_class="small",
        ),
        ownership_generation=4,
        updated_at=datetime(2026, 9, 24, tzinfo=UTC),
    )


class WorkspaceApiTests(unittest.IsolatedAsyncioTestCase):
    def test_routes_expose_the_workspace_lifecycle_api(self):
        route_methods = {
            (route.path, method)
            for route in workspace_api.router.routes
            for method in route.methods or ()
        }
        self.assertTrue(
            {
                ("/workspaces", "GET"),
                ("/workspaces/{workspace_id}", "GET"),
                ("/workspaces/{workspace_id}/suspend", "POST"),
                ("/workspaces/{workspace_id}/resume", "POST"),
                ("/workspaces/{workspace_id}/refresh", "POST"),
            }.issubset(route_methods)
        )

    async def test_list_returns_lifecycle_and_manifest(self):
        with patch.object(
            workspace_api.workspace_adapter,
            "list_workspace_lifecycles",
            new=AsyncMock(return_value=[lifecycle()]),
        ) as list_workspaces:
            response = await workspace_api.list_workspaces(user_id="owner-1")

        self.assertEqual(response[0].observed_state, WorkspaceObservedState.RUNNING)
        self.assertEqual(response[0].manifest.agent_kinds, (WorkspaceAgentKind.CODEX,))
        list_workspaces.assert_awaited_once_with("owner-1")

    async def test_get_detail_and_lifecycle_actions_publish_the_lifecycle(self):
        current = lifecycle()
        with (
            patch.object(workspace_api, "_require_owned_workspace", new=AsyncMock()),
            patch.object(
                workspace_api.workspace_adapter,
                "ensure_workspace_lifecycle",
                new=AsyncMock(return_value=current),
            ),
            patch.object(
                workspace_api.workspace_adapter,
                "suspend_workspace",
                new=AsyncMock(return_value=current),
            ),
            patch.object(
                workspace_api.workspace_adapter,
                "resume_workspace",
                new=AsyncMock(return_value=current),
            ),
            patch.object(
                workspace_api.workspace_adapter,
                "refresh_workspace_lifecycle",
                new=AsyncMock(return_value=current),
            ),
            patch.object(workspace_api, "_publish", new=AsyncMock()) as publish,
        ):
            detail = await workspace_api.get_workspace("session-1", user_id="owner-1")
            suspended = await workspace_api.suspend_workspace(
                "session-1", user_id="owner-1"
            )
            resumed = await workspace_api.resume_workspace(
                "session-1", user_id="owner-1"
            )
            refreshed = await workspace_api.refresh_workspace(
                "session-1", user_id="owner-1"
            )

        self.assertEqual(detail.workspace_id, "session-1")
        self.assertEqual(suspended.workspace_id, "session-1")
        self.assertEqual(resumed.workspace_id, "session-1")
        self.assertEqual(refreshed.workspace_id, "session-1")
        self.assertEqual(suspended.manifest.resource_class, "small")
        self.assertEqual(publish.await_count, 3)

    async def test_fence_error_is_a_conflict_and_preserves_reason(self):
        current = lifecycle()
        with (
            patch.object(workspace_api, "_require_owned_workspace", new=AsyncMock()),
            patch.object(
                workspace_api.workspace_adapter,
                "suspend_workspace",
                new=AsyncMock(
                    side_effect=ContractError("A recorded delivery is still open.")
                ),
            ),
            patch.object(
                workspace_api.workspace_adapter,
                "get_workspace_lifecycle",
                new=AsyncMock(return_value=current),
            ),
            patch.object(workspace_api, "_publish", new=AsyncMock()),
        ):
            with self.assertRaises(HTTPException) as raised:
                await workspace_api.suspend_workspace("session-1", user_id="owner-1")

        self.assertEqual(raised.exception.status_code, 409)
        self.assertEqual(raised.exception.detail, "A recorded delivery is still open.")

    async def test_owner_check_hides_other_users_workspaces(self):
        with patch.object(
            workspace_api,
            "_require_owned_workspace",
            new=AsyncMock(side_effect=HTTPException(404, "Workspace not found")),
        ):
            with self.assertRaises(HTTPException) as raised:
                await workspace_api.get_workspace("session-1", user_id="other")

        self.assertEqual(raised.exception.status_code, 404)


if __name__ == "__main__":
    unittest.main()
