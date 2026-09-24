"""Workspace lifecycle policy and orchestration tests; no Substrate cluster is contacted."""

import asyncio
import unittest
from contextlib import asynccontextmanager
from datetime import UTC, datetime
from unittest.mock import AsyncMock, patch

from mainloop.runtime import workspace_adapter as adapter
from mainloop.runtime.contracts import ContractError
from mainloop.runtime.substrate import ActorRecord, ActorState, TransportError

from models import (
    WorkspaceDesiredState,
    WorkspaceLifecycle,
    WorkspaceManifest,
    WorkspaceObservedState,
)

NOW = datetime(2026, 9, 24, tzinfo=UTC)


def lifecycle(
    *,
    desired: WorkspaceDesiredState = WorkspaceDesiredState.RUNNING,
    observed: WorkspaceObservedState = WorkspaceObservedState.RUNNING,
    operation_id: str | None = None,
) -> WorkspaceLifecycle:
    return WorkspaceLifecycle(
        workspace_id="session-1",
        session_id="session-1",
        desired_state=desired,
        observed_state=observed,
        manifest=WorkspaceManifest(
            repo_url="https://github.com/example/repo",
            branch="main",
            resource_class="default",
        ),
        operation_id=operation_id,
        ownership_generation=3,
        updated_at=NOW,
    )


def actor(state: ActorState, snapshot: str | None = None) -> ActorRecord:
    return ActorRecord(
        atespace="workspaces",
        name="actor-1",
        uid="uid-1",
        state=state,
        external_snapshot_uri=snapshot,
        current_actor_template_uid="template-1",
        raw={},
    )


class FakeTransaction:
    def __init__(self, connection):
        self.connection = connection

    async def __aenter__(self):
        self.connection.in_transaction = True
        self.connection.events.append("begin")
        return self.connection

    async def __aexit__(self, exc_type, exc, traceback):
        self.connection.events.append("rollback" if exc_type else "commit")
        self.connection.in_transaction = False


class FakePostgresConnection:
    def __init__(self, delivery_states=()):
        self.delivery_states = delivery_states
        self.events = []
        self.in_transaction = False
        self.lock_query = None

    def transaction(self):
        return FakeTransaction(self)

    async def fetchrow(self, query, *_args):
        if not self.in_transaction:
            raise AssertionError(
                "workspace binding lock must be inside the transaction"
            )
        self.events.append("lock")
        self.lock_query = query
        return {"ownership_generation": 3}

    async def fetch(self, query, *_args):
        if not self.in_transaction:
            raise AssertionError("delivery fence must be inside the transaction")
        if "native_deliveries" not in query:
            raise AssertionError("expected native delivery state query")
        self.events.append("delivery-check")
        return [{"state": state} for state in self.delivery_states]

    async def execute(self, query, *_args):
        if not self.in_transaction:
            raise AssertionError("workspace reservation must be inside the transaction")
        if "UPDATE workspace_bindings" in query:
            self.events.append("reserve")
        elif "UPDATE workspace_lifecycles" in query:
            self.events.append("lifecycle")
        return "UPDATE 1"


def fake_db_connection(connection):
    @asynccontextmanager
    async def connect():
        yield connection

    return connect


class WorkspaceStateMappingTests(unittest.TestCase):
    def test_actor_states_map_without_guessing_transitions(self):
        expected = {
            ActorState.RUNNING: WorkspaceObservedState.RUNNING,
            ActorState.SUSPENDING: WorkspaceObservedState.SUSPENDING,
            ActorState.SUSPENDED: WorkspaceObservedState.SUSPENDED,
            ActorState.RESUMING: WorkspaceObservedState.RESUMING,
            ActorState.CRASHED: WorkspaceObservedState.FAILED,
            ActorState.DELETING: WorkspaceObservedState.FAILED,
            ActorState.PAUSED: WorkspaceObservedState.UNKNOWN,
            ActorState.PAUSING: WorkspaceObservedState.UNKNOWN,
            ActorState.REVERTING: WorkspaceObservedState.UNKNOWN,
            ActorState.UNSPECIFIED: WorkspaceObservedState.UNKNOWN,
        }
        for actor_state, workspace_state in expected.items():
            with self.subTest(actor_state=actor_state):
                self.assertEqual(adapter.lifecycle_state(actor_state), workspace_state)

    def test_suspend_fence_blocks_open_and_uncertain_deliveries(self):
        self.assertIsNone(adapter.suspend_fence_reason(set()))
        self.assertEqual(adapter.suspend_fence_reason({"recorded"}), "DeliveryRecorded")
        self.assertEqual(adapter.suspend_fence_reason({"sending"}), "TurnInFlight")
        self.assertEqual(adapter.suspend_fence_reason({"delivered"}), "TurnInFlight")
        self.assertEqual(
            adapter.suspend_fence_reason({"uncertain"}), "DeliveryUncertain"
        )
        self.assertEqual(adapter.suspend_fence_reason({"queued"}), "DeliveryQueued")


class WorkspaceOperationTests(unittest.IsolatedAsyncioTestCase):
    async def test_resume_is_idempotent_when_actor_is_already_running(self):
        stored = lifecycle(
            desired=WorkspaceDesiredState.SUSPENDED,
            observed=WorkspaceObservedState.SUSPENDED,
        )
        resumed = lifecycle()
        control = AsyncMock()
        control.get_actor.return_value = actor(ActorState.RUNNING)

        with (
            patch.object(adapter, "_lock", return_value=asyncio.Lock()),
            patch.object(
                adapter,
                "ensure_workspace_lifecycle",
                new=AsyncMock(return_value=stored),
            ),
            patch.object(
                adapter,
                "get_workspace",
                new=AsyncMock(
                    return_value={
                        "atespace": "workspaces",
                        "actor_name": "actor-1",
                    }
                ),
            ),
            patch.object(
                adapter, "_reserve_operation", new=AsyncMock(return_value=stored)
            ),
            patch.object(
                adapter, "_record_observation", new=AsyncMock(return_value=resumed)
            ),
        ):
            result = await adapter.resume_workspace("session-1", control=control)

        self.assertEqual(result.observed_state, WorkspaceObservedState.RUNNING)
        control.get_actor.assert_awaited_once_with("workspaces", "actor-1")
        control.resume_actor.assert_not_awaited()

    async def test_recorded_delivery_refuses_suspend_before_control_call(self):
        stored = lifecycle()
        control = AsyncMock()
        connection = FakePostgresConnection({"recorded"})
        with (
            patch.object(adapter, "_lock", return_value=asyncio.Lock()),
            patch.object(adapter.db, "connection", new=fake_db_connection(connection)),
            patch.object(
                adapter,
                "ensure_workspace_lifecycle",
                new=AsyncMock(return_value=stored),
            ),
            patch.object(
                adapter,
                "get_workspace",
                new=AsyncMock(
                    return_value={
                        "atespace": "workspaces",
                        "actor_name": "actor-1",
                    }
                ),
            ),
        ):
            with self.assertRaisesRegex(ContractError, "recorded delivery"):
                await adapter.suspend_workspace("session-1", control=control)

        self.assertIn("FOR UPDATE", connection.lock_query)
        self.assertEqual(
            connection.events,
            ["begin", "lock", "delivery-check", "lifecycle", "commit"],
        )
        control.get_actor.assert_not_awaited()
        control.suspend_actor.assert_not_awaited()

    async def test_reservation_and_delivery_fence_share_the_locked_transaction(self):
        connection = FakePostgresConnection()
        with (
            patch.object(adapter.db, "connection", new=fake_db_connection(connection)),
            patch.object(
                adapter,
                "get_workspace_lifecycle",
                new=AsyncMock(return_value=None),
            ),
        ):
            result = await adapter._reserve_operation(
                lifecycle(), WorkspaceDesiredState.SUSPENDED
            )

        self.assertEqual(result.desired_state, WorkspaceDesiredState.SUSPENDED)
        self.assertIn("FOR UPDATE", connection.lock_query)
        self.assertEqual(
            connection.events,
            ["begin", "lock", "delivery-check", "reserve", "lifecycle", "commit"],
        )

    async def test_delivery_recorded_after_reservation_blocks_suspend_call(self):
        stored = lifecycle()
        reserved = lifecycle(
            desired=WorkspaceDesiredState.SUSPENDED,
            observed=WorkspaceObservedState.SUSPENDING,
            operation_id="op-1",
        )
        control = AsyncMock()
        events = []
        delivery_recorded = False

        async def reserve_operation(_previous, _desired_state):
            nonlocal delivery_recorded
            events.append("reserved")
            delivery_recorded = True
            return reserved

        async def inspect_actor(*_args):
            events.append("actor-inspected")
            return actor(ActorState.RUNNING)

        async def delivery_states(_workspace_id, **_kwargs):
            events.append("pre-suspend-fence")
            return {"recorded"} if delivery_recorded else set()

        control.get_actor.side_effect = inspect_actor
        record_observation = AsyncMock(return_value=stored)
        with (
            patch.object(adapter, "_lock", return_value=asyncio.Lock()),
            patch.object(
                adapter,
                "ensure_workspace_lifecycle",
                new=AsyncMock(return_value=stored),
            ),
            patch.object(
                adapter,
                "get_workspace",
                new=AsyncMock(
                    return_value={
                        "atespace": "workspaces",
                        "actor_name": "actor-1",
                    }
                ),
            ),
            patch.object(
                adapter,
                "_reserve_operation",
                new=AsyncMock(side_effect=reserve_operation),
            ),
            patch.object(
                adapter,
                "_delivery_states",
                new=AsyncMock(side_effect=delivery_states),
            ),
            patch.object(adapter, "_record_observation", new=record_observation),
        ):
            with self.assertRaisesRegex(ContractError, "recorded delivery"):
                await adapter.suspend_workspace("session-1", control=control)

        self.assertEqual(
            events,
            ["reserved", "actor-inspected", "pre-suspend-fence"],
        )
        self.assertEqual(
            record_observation.await_args.kwargs["operation_reason"],
            "DeliveryRecorded",
        )
        control.suspend_actor.assert_not_awaited()

    async def test_suspend_timeout_persists_unknown_and_keeps_operation_id(self):
        stored = lifecycle()
        reserved = lifecycle(
            desired=WorkspaceDesiredState.SUSPENDED,
            observed=WorkspaceObservedState.SUSPENDING,
            operation_id="op-1",
        )
        uncertain = lifecycle(
            desired=WorkspaceDesiredState.SUSPENDED,
            observed=WorkspaceObservedState.UNKNOWN,
            operation_id="op-1",
        )
        control = AsyncMock()
        control.get_actor.return_value = actor(ActorState.RUNNING)
        control.suspend_actor.side_effect = TransportError("timed out")

        with (
            patch.object(adapter, "_lock", return_value=asyncio.Lock()),
            patch.object(
                adapter,
                "ensure_workspace_lifecycle",
                new=AsyncMock(return_value=stored),
            ),
            patch.object(
                adapter,
                "get_workspace",
                new=AsyncMock(
                    return_value={
                        "atespace": "workspaces",
                        "actor_name": "actor-1",
                    }
                ),
            ),
            patch.object(
                adapter, "_delivery_states", new=AsyncMock(return_value=set())
            ),
            patch.object(
                adapter, "_reserve_operation", new=AsyncMock(return_value=reserved)
            ),
            patch.object(
                adapter,
                "_record_operation_failure",
                new=AsyncMock(return_value=uncertain),
            ) as record_failure,
        ):
            result = await adapter.suspend_workspace("session-1", control=control)

        self.assertEqual(result.observed_state, WorkspaceObservedState.UNKNOWN)
        self.assertEqual(result.operation_id, "op-1")
        record_failure.assert_awaited_once()
        self.assertTrue(record_failure.await_args.kwargs["uncertain"])
        control.suspend_actor.assert_awaited_once_with("workspaces", "actor-1")


if __name__ == "__main__":
    unittest.main()
