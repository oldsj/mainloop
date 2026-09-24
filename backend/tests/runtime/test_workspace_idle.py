"""Durable activity touches and the periodic idle policy remain fake-backed."""

import asyncio
import unittest
from contextlib import asynccontextmanager
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch

from mainloop.runtime import native_sessions
from mainloop.runtime import workspace_adapter as adapter
from mainloop.runtime.contracts import ContractError

from models import (
    WorkspaceDesiredState,
    WorkspaceLifecycle,
    WorkspaceManifest,
    WorkspaceObservedState,
)


def lifecycle(state=WorkspaceObservedState.SUSPENDED):
    from datetime import UTC, datetime

    return WorkspaceLifecycle(
        workspace_id="workspace-1",
        session_id="workspace-1",
        desired_state=WorkspaceDesiredState.SUSPENDED,
        observed_state=state,
        manifest=WorkspaceManifest(
            branch="feature/sample",
            resource_class="default",
            dev={"image": "node:22"},
        ),
        updated_at=datetime.now(UTC),
    )


class FakeConnection:
    def __init__(self, rows=()):
        self.rows = list(rows)
        self.executed = []
        self.query = ""

    async def execute(self, query, *args):
        self.executed.append((query, args))

    @asynccontextmanager
    async def transaction(self):
        yield self

    async def fetchrow(self, query, *_args):
        if "FROM workspace_bindings" in query:
            return {"workspace_id": "workspace-1", "ownership_generation": 3}
        if "FROM workspace_lifecycles" in query and "last_activity_at" in query:
            from datetime import UTC, datetime

            return {
                "last_activity_at": datetime.now(UTC),
                "last_delivery_at": None,
            }
        if "FROM workspace_lifecycles" in query:
            return {"desired_state": "suspended", "observed_state": "suspended"}
        raise AssertionError(f"unexpected query: {query}")

    async def fetch(self, query, *args):
        self.query = query
        return self.rows


def fake_connection(connection):
    @asynccontextmanager
    async def connect():
        yield connection

    return connect


class WorkspaceIdleTests(unittest.IsolatedAsyncioTestCase):
    async def test_native_turn_touches_a_branch_workspace_before_recording(self):
        with (
            patch.object(
                native_sessions.db,
                "get_session",
                new=AsyncMock(
                    return_value=SimpleNamespace(status="active", conversation_id="c")
                ),
            ),
            patch.object(
                adapter,
                "get_workspace",
                new=AsyncMock(return_value={"actor_name": "ml-ws"}),
            ),
            patch.object(adapter, "touch_workspace", new=AsyncMock()) as touch,
            patch.object(native_sessions, "_lock", return_value=asyncio.Lock()),
            patch.object(native_sessions, "_open_count", new=AsyncMock(return_value=1)),
            patch.object(
                native_sessions,
                "_record_delivery_message",
                new=AsyncMock(return_value="message-1"),
            ),
        ):
            message_id = await native_sessions.submit_message(
                "workspace-1", "work", source="report"
            )

        self.assertEqual(message_id, "message-1")
        touch.assert_awaited_once_with("workspace-1", reason="turn")

    async def test_turn_touch_records_activity_and_wakes_a_parked_workspace(self):
        connection = FakeConnection()
        current = lifecycle()
        resumed = lifecycle(WorkspaceObservedState.RUNNING)
        with (
            patch.object(
                adapter,
                "ensure_workspace_lifecycle",
                new=AsyncMock(return_value=current),
            ),
            patch.object(adapter.db, "connection", new=fake_connection(connection)),
            patch.object(
                adapter, "resume_workspace", new=AsyncMock(return_value=resumed)
            ) as wake,
        ):
            result = await adapter.touch_workspace("workspace-1", reason="turn")

        self.assertEqual(result.observed_state, WorkspaceObservedState.RUNNING)
        self.assertIn("last_activity_at=NOW()", connection.executed[0][0])
        wake.assert_awaited_once_with("workspace-1")

    async def test_idle_reservation_rechecks_activity_under_the_binding_lock(self):
        current = lifecycle(WorkspaceObservedState.RUNNING)
        current = current.model_copy(
            update={
                "desired_state": WorkspaceDesiredState.RUNNING,
                "ownership_generation": 3,
            }
        )
        connection = FakeConnection()
        with patch.object(adapter.db, "connection", new=fake_connection(connection)):
            result = await adapter._reserve_operation(
                current,
                WorkspaceDesiredState.SUSPENDED,
                only_if_idle=True,
            )

        self.assertIsNone(result)
        self.assertFalse(connection.executed)

    async def test_touch_rejects_unknown_activity_reasons(self):
        with self.assertRaisesRegex(ValueError, "reason must be"):
            await adapter.touch_workspace("workspace-1", reason="browser")

    async def test_idle_scan_uses_durable_activity_and_fenced_suspend(self):
        connection = FakeConnection(
            [{"workspace_id": "workspace-1"}, {"workspace_id": "workspace-2"}]
        )
        suspend = AsyncMock(
            side_effect=[lifecycle(), ContractError("delivery still open")]
        )
        with (
            patch.object(adapter.db, "connection", new=fake_connection(connection)),
            patch.object(adapter, "suspend_workspace_if_idle", new=suspend),
        ):
            count = await adapter.suspend_idle_workspaces()

        self.assertIn("last_activity_at", connection.query)
        self.assertIn("idle_timeout_minutes", connection.query)
        self.assertEqual(count, 1)
        self.assertEqual(suspend.await_count, 2)


if __name__ == "__main__":
    unittest.main()
