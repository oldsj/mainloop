"""Branch workspaces on kagent: suspend racing a turn start (K5), idle-out, create and
replacement, archive delete, lifecycle mapping.

Fixture-backed: a fake kagent gateway and the in-memory ledger of the native-session tests. No
database, network or live kagent. The race tests below prove the ordering Mainloop enforces
with its per-session lock against the fake; whether a real SuspendSession racing a real turn
start in kagent behaves the same is an owed live check.
"""

from __future__ import annotations

import asyncio
import unittest
from datetime import UTC, datetime, timedelta
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch

import httpx
from mainloop.config import settings
from mainloop.runtime import native_sessions as ns
from mainloop.runtime import workspaces
from mainloop.runtime.agent_identity import hash_token
from mainloop.runtime.kagent_client import (
    KagentClient,
    KagentSession,
    RuntimeOperation,
    RuntimeState,
    SessionCredential,
    SessionError,
    SessionWorkspace,
    Unreachable,
)
from tests.runtime.kagent_fake import CONTEXT_ID, FakeKagent
from tests.runtime.test_native_sessions import SESSION, MemoryLedger, memory_connection
from tests.runtime.test_task_provisioning import ordinary_guard

from models import SessionStatus, WorkspaceObservedState

WORKSPACE = SessionWorkspace(
    repo="https://github.com/example/app", ref="main", branch="feature/x", depth=1
)


class GatedKagent(FakeKagent):
    """FakeKagent whose SuspendSession can be held in flight until the test releases it."""

    def __init__(self):
        super().__init__()
        self.hold_suspend = False
        self.suspend_started = asyncio.Event()
        self.release_suspend = asyncio.Event()

    def transport(self) -> httpx.MockTransport:
        async def handler(request: httpx.Request) -> httpx.Response:
            if self.hold_suspend and request.url.path.endswith("/SuspendSession"):
                self.suspend_started.set()
                await self.release_suspend.wait()
            return self.handle(request)

        return httpx.MockTransport(handler)

    def calls(self) -> list[str]:
        """Kagent calls in order (SessionService and A2A method names), reads left out."""
        out = []
        for _, path, body in self.requests:
            out.append(
                body["method"] if isinstance(body, dict) else path.rsplit("/", 1)[1]
            )
        return [name for name in out if name != "GetSession"]


class WorkspaceTestCase(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.fake = GatedKagent()
        self.ledger = MemoryLedger()
        self.ledger.workspace = WORKSPACE
        self.ledger.binding.update(
            mcp_grant_kind="workspace",
            token_hash=hash_token("sanitized-fixture"),
            credential_ref={
                "origin": "http://mainloop-mcp.mainloop.svc.cluster.local",
                "header": "Authorization",
                "secret_name": f"mainloop-mcp-{SESSION}",
                "secret_key": "authorization",
            },
        )
        self.session = SimpleNamespace(
            id=SESSION,
            user_id="user-1",
            conversation_id="conv-1",
            status=SessionStatus.ACTIVE,
            archived_at=None,
        )

        async def notify_message(user_id, session_id, message_id, role):
            pass

        http = httpx.AsyncClient(
            transport=self.fake.transport(), base_url="http://kagent.test"
        )
        ns._client = KagentClient("http://kagent.test", user_id="mainloop", client=http)
        ns._streaming.clear()
        ns._locks.clear()
        for patcher in (
            patch.object(ns.db, "connection", memory_connection),
            patch("mainloop.tasks.lifecycle.guard", ordinary_guard),
            patch("mainloop.tasks.lifecycle.check_session", AsyncMock()),
            patch.object(ns, "attempt_row", AsyncMock(return_value=None)),
            patch(
                "mainloop.runtime.agent_credentials.publish_for_binding",
                AsyncMock(
                    return_value=SessionCredential(
                        "http://mainloop-mcp.mainloop.svc.cluster.local",
                        "Authorization",
                        f"mainloop-mcp-{SESSION}",
                        "authorization",
                    )
                ),
            ),
            patch("mainloop.runtime.agent_credentials.revoke", AsyncMock()),
            patch.object(ns, "ledger", self.ledger),
            patch.object(ns.db, "get_session", AsyncMock(return_value=self.session)),
            patch.object(ns.db, "update_session", AsyncMock()),
            patch.object(ns, "notify_session_message", notify_message),
        ):
            patcher.start()
            self.addCleanup(patcher.stop)

    async def asyncTearDown(self):
        await asyncio.gather(*ns._tasks, return_exceptions=True)
        await ns.close_client()

    async def settle(self):
        while ns._tasks:
            await asyncio.gather(*list(ns._tasks), return_exceptions=True)

    async def with_session(self) -> None:
        """Give the binding a live, ready kagent Session."""
        self.ledger.binding["kagent_session_id"] = CONTEXT_ID
        self.fake.sessions[CONTEXT_ID] = (RuntimeState.READY, RuntimeOperation.NONE)
        self.fake.session_agents[CONTEXT_ID] = ns.agent_ref(
            self.ledger.binding["kind"], self.ledger.binding["role"]
        ).encode()
        self.fake.workspaces[CONTEXT_ID] = WORKSPACE.encode()


class SuspendRacingATurnStartTests(WorkspaceTestCase):
    """K5: SuspendSession must not land inside a turn start."""

    async def test_a_message_arriving_during_suspend_waits_then_resumes_before_sending(
        self,
    ):
        await self.with_session()
        self.fake.hold_suspend = True
        suspend = asyncio.create_task(workspaces.suspend_if_quiet(SESSION))
        await self.fake.suspend_started.wait()

        # SuspendSession is in flight; the user sends a message.
        submit = asyncio.create_task(ns.submit_message(SESSION, "hello"))
        await asyncio.sleep(0.01)
        self.assertFalse(
            submit.done(), "the message must wait for the suspend to finish"
        )
        self.assertEqual(self.ledger.rows, {}, "nothing is recorded inside the suspend")

        self.fake.release_suspend.set()
        await suspend
        await submit
        await self.settle()

        self.assertEqual(
            self.fake.calls()[:3],
            ["SuspendSession", "ResumeSession", "SendStreamingMessage"],
        )
        self.assertEqual(self.fake.sessions[CONTEXT_ID][0], RuntimeState.READY)

    async def test_suspend_refuses_when_a_message_is_already_recorded(self):
        await self.with_session()
        await self.ledger.record_message(
            session_id=SESSION,
            conversation_id="conv-1",
            text="hi",
            state="recorded",
            source="user",
        )
        with self.assertRaises(workspaces.WorkspaceConflict):
            await workspaces.suspend_if_quiet(SESSION)
        self.assertEqual(self.fake.calls(), [])

    async def test_suspend_refuses_while_a_turn_is_sending_or_delivered_or_queued(self):
        await self.with_session()
        for state in ("sending", "delivered", "queued"):
            with self.subTest(state=state):
                self.ledger.rows.clear()
                await self.ledger.record_message(
                    session_id=SESSION,
                    conversation_id="conv-1",
                    text="hi",
                    state=state,
                    source="user",
                )
                with self.assertRaises(workspaces.WorkspaceConflict):
                    await workspaces.suspend_if_quiet(SESSION)
        self.assertEqual(self.fake.calls(), [])

    async def test_a_finished_turn_does_not_block_suspend(self):
        await self.with_session()
        await self.ledger.record_message(
            session_id=SESSION,
            conversation_id="conv-1",
            text="hi",
            state="completed",
            source="user",
        )
        session = await workspaces.suspend_if_quiet(SESSION)
        self.assertEqual(session.state, RuntimeState.SUSPENDED)

    async def test_a_delivery_claim_cannot_interleave_with_the_suspend(self):
        # The turn is recorded first and its delivery task is waiting for the lock when the
        # idle check runs: the check sees the open delivery and does not suspend.
        await self.with_session()
        async with ns._lock(SESSION):
            mid = await self.ledger.record_message(
                session_id=SESSION,
                conversation_id="conv-1",
                text="hi",
                state="recorded",
                source="user",
            )
            check = asyncio.create_task(workspaces._idle_suspend(SESSION, CONTEXT_ID))
            await asyncio.sleep(0.01)
        self.assertFalse(await check)
        self.assertNotIn("SuspendSession", self.fake.calls())
        self.assertEqual(self.ledger.rows[mid]["state"], "recorded")


class IdleOutTests(WorkspaceTestCase):
    async def asyncSetUp(self):
        await super().asyncSetUp()
        # The idle re-read is SQL (covered against Postgres); here the workspace is still idle.
        self.is_idle = AsyncMock(return_value=True)
        patcher = patch.object(workspaces, "_is_idle", self.is_idle)
        patcher.start()
        self.addCleanup(patcher.stop)

    async def test_activity_after_the_idle_check_picked_it_is_not_suspended(self):
        await self.with_session()
        self.is_idle.return_value = False  # a preview touched it after the SELECT
        self.assertFalse(await workspaces._idle_suspend(SESSION, CONTEXT_ID))
        self.assertNotIn("SuspendSession", self.fake.calls())
        self.is_idle.assert_awaited_once_with(SESSION)

    async def test_an_explicit_suspend_does_not_check_idleness(self):
        await self.with_session()
        self.is_idle.return_value = False
        await workspaces.suspend_if_quiet(SESSION)
        self.assertEqual(self.fake.calls().count("SuspendSession"), 1)
        self.is_idle.assert_not_awaited()

    async def test_a_quiet_ready_session_is_suspended(self):
        await self.with_session()
        self.assertTrue(await workspaces._idle_suspend(SESSION, CONTEXT_ID))
        self.assertEqual(self.fake.sessions[CONTEXT_ID][0], RuntimeState.SUSPENDED)

    async def test_an_active_turn_is_left_alone(self):
        await self.with_session()
        await self.ledger.record_message(
            session_id=SESSION,
            conversation_id="conv-1",
            text="hi",
            state="delivered",
            source="user",
        )
        self.assertFalse(await workspaces._idle_suspend(SESSION, CONTEXT_ID))
        self.assertNotIn("SuspendSession", self.fake.calls())

    async def test_an_already_suspended_session_is_not_suspended_again(self):
        await self.with_session()
        self.fake.sessions[CONTEXT_ID] = (RuntimeState.SUSPENDED, RuntimeOperation.NONE)
        self.assertTrue(await workspaces._idle_suspend(SESSION, CONTEXT_ID))
        self.assertNotIn("SuspendSession", self.fake.calls())

    async def test_a_session_kagent_is_changing_is_left_for_a_later_pass(self):
        await self.with_session()
        self.fake.sessions[CONTEXT_ID] = (RuntimeState.READY, RuntimeOperation.RESUME)
        self.assertFalse(await workspaces._idle_suspend(SESSION, CONTEXT_ID))
        self.assertNotIn("SuspendSession", self.fake.calls())

    async def test_a_session_kagent_has_deleted_is_not_suspended(self):
        self.ledger.binding["kagent_session_id"] = "gone"
        self.assertFalse(await workspaces._idle_suspend(SESSION, "gone"))
        self.assertNotIn("SuspendSession", self.fake.calls())

    async def test_suspend_idle_marks_suspended_workspaces_and_survives_a_failure(self):
        rows = [
            {"session_id": "a", "kagent_session_id": "ka", "user_id": "u"},
            {"session_id": "b", "kagent_session_id": "kb", "user_id": "u"},
            {"session_id": "c", "kagent_session_id": "kc", "user_id": "u"},
        ]
        conn = SimpleNamespace(fetch=AsyncMock(return_value=rows), execute=AsyncMock())

        class Connection:
            async def __aenter__(self):
                return conn

            async def __aexit__(self, *exc):
                return False

        async def idle(session_id, kagent_session_id):
            if session_id == "a":
                raise Unreachable("kagent down")
            return session_id == "b"  # "c" is busy

        lifecycle = AsyncMock()
        with (
            patch.object(workspaces.db, "connection", Connection),
            patch.object(workspaces, "_idle_suspend", idle),
            patch.object(workspaces, "_lifecycle", lifecycle),
            patch.object(workspaces, "publish", AsyncMock()) as publish,
        ):
            suspended = await workspaces.suspend_idle()
        self.assertEqual(suspended, ["b"])
        marked = [c.args[1] for c in conn.execute.await_args_list]
        self.assertEqual(
            marked, ["b"], "only a confirmed suspend restarts the debounce"
        )
        publish.assert_awaited_once()

    async def test_a_resume_restarts_the_debounce_but_a_ready_session_is_not_resumed(
        self,
    ):
        await self.with_session()
        touch = AsyncMock()
        row = {"session_id": SESSION, "kagent_session_id": CONTEXT_ID}
        with (
            patch.object(workspaces, "_owned_row", AsyncMock(return_value=row)),
            patch.object(workspaces, "_lifecycle", AsyncMock()),
            patch.object(workspaces, "touch", touch),
        ):
            await workspaces.resume(SESSION, "user-1")
            self.assertNotIn("ResumeSession", self.fake.calls())
            self.fake.sessions[CONTEXT_ID] = (
                RuntimeState.SUSPENDED,
                RuntimeOperation.NONE,
            )
            await workspaces.resume(SESSION, "user-1")
        self.assertEqual(self.fake.calls().count("ResumeSession"), 1)
        self.assertEqual(touch.await_count, 2)


class CreateAndReplacementTests(WorkspaceTestCase):
    async def test_create_sends_the_workspace(self):
        await workspaces._create_session(SESSION, "user-1", reject_removes_rows=True)
        self.assertEqual(self.fake.created_workspaces(), [WORKSPACE])
        self.assertEqual(self.ledger.binding["kagent_session_id"], CONTEXT_ID)

    async def test_create_is_not_repeated_once_the_session_exists(self):
        await workspaces._create_session(SESSION, "user-1", reject_removes_rows=True)
        await workspaces._create_session(SESSION, "user-1", reject_removes_rows=True)
        self.assertEqual(len(self.fake.created_workspaces()), 1)

    async def test_a_replacement_session_resends_the_same_workspace(self):
        await workspaces._create_session(SESSION, "user-1", reject_removes_rows=True)
        await ns.get_client().delete_session(CONTEXT_ID)
        self.fake.next_session_ids = ["replacement"]
        await ns._ensure_kagent_session(self.ledger.binding)
        self.assertEqual(self.ledger.binding["kagent_session_id"], "replacement")
        self.assertEqual(self.fake.created_workspaces(), [WORKSPACE, WORKSPACE])
        self.assertEqual(
            len({r for r in self.fake.created_request_ids}),
            2,
            "the replacement is created under a new request id",
        )

    async def test_kagent_rejects_a_changed_workspace_under_one_request_id(self):
        await workspaces._create_session(SESSION, "user-1", reject_removes_rows=True)
        changed = SessionWorkspace(repo=WORKSPACE.repo, ref="other", branch="x")
        with self.assertRaises(SessionError) as caught:
            await ns.get_client().create_session(
                ns.agent_ref("claude"),
                request_id=ns._request_id(self.ledger.binding),
                workspace=changed,
            )
        self.assertEqual(caught.exception.grpc_status, 6)

    async def test_a_rejected_workspace_removes_the_rows(self):
        delete_rows = AsyncMock()
        with (
            patch.object(
                ns.get_client(),
                "create_session",
                AsyncMock(
                    side_effect=SessionError("origin not allowed", grpc_status=3)
                ),
            ),
            patch.object(workspaces, "_delete_rows", delete_rows),
        ):
            with self.assertRaises(workspaces.WorkspaceRejected):
                await workspaces._create_session(
                    SESSION, "user-1", reject_removes_rows=True
                )
        delete_rows.assert_awaited_once_with(SESSION, evidence="kagent-rejected:3")

    async def test_an_unknown_create_outcome_keeps_the_rows_for_refresh(self):
        delete_rows = AsyncMock()
        with (
            patch.object(
                ns.get_client(),
                "create_session",
                AsyncMock(side_effect=Unreachable("down")),
            ),
            patch.object(workspaces, "_delete_rows", delete_rows),
        ):
            await workspaces._create_session(
                SESSION, "user-1", reject_removes_rows=True
            )
        delete_rows.assert_not_awaited()
        self.assertIsNone(self.ledger.binding["kagent_session_id"])


UNAVAILABLE = SessionError("environment snapshot is being prepared", grpc_status=14)


class CreateRetryTests(WorkspaceTestCase):
    """An unconfirmed create is retried by the reconcile pass with the same request id."""

    def failing_creates(self, *errors):
        """Make the next CreateSession calls fail with ``errors`` (in order), then reach kagent.

        Returns the request ids of every call, failed or not.
        """
        client = ns.get_client()
        create = client.create_session
        pending = list(errors)
        request_ids: list[str] = []

        async def create_session(agent, **kwargs):
            request_ids.append(kwargs["request_id"])
            if pending:
                raise pending.pop(0)
            return await create(agent, **kwargs)

        patcher = patch.object(client, "create_session", create_session)
        patcher.start()
        self.addCleanup(patcher.stop)
        return request_ids

    def make_due(self):
        self.ledger.create_state["create_retry_at"] = datetime.now(UTC)

    async def test_unavailable_then_success_runs_without_a_manual_refresh(self):
        request_ids = self.failing_creates(UNAVAILABLE)
        await workspaces._create_session(SESSION, "user-1", reject_removes_rows=True)
        state = self.ledger.create_state
        self.assertIsNone(self.ledger.binding["kagent_session_id"])
        self.assertEqual(state["create_attempts"], 1)
        self.assertIsNone(state["create_stopped"])
        self.assertGreater(state["create_retry_at"], datetime.now(UTC))
        observed, detail = workspaces._pending_create(state)
        self.assertEqual(observed, WorkspaceObservedState.RESUMING)
        self.assertIn("retrying automatically", detail)
        self.assertNotIn("refresh", detail.lower())

        self.assertEqual(await workspaces.retry_creates(), [], "not due yet")
        self.make_due()
        self.assertEqual(await workspaces.retry_creates(), [SESSION])

        self.assertEqual(self.ledger.binding["kagent_session_id"], CONTEXT_ID)
        self.assertEqual(len(request_ids), 2)
        self.assertEqual(len(set(request_ids)), 1, "one create identity")
        self.assertEqual(len(self.fake.created_request_ids), 1)
        self.assertEqual(self.fake.created_workspaces(), [WORKSPACE])
        self.assertEqual(state["create_attempts"], 0)
        self.assertIsNone(state["create_retry_at"])
        self.assertIsNone(state["create_error"])
        self.assertEqual(await workspaces.retry_creates(), [], "nothing left to retry")

    async def test_a_lost_reply_is_retried_and_recovers_the_same_session(self):
        client = ns.get_client()
        create = client.create_session
        calls = []

        async def lose_first_reply(agent, **kwargs):
            calls.append(kwargs["request_id"])
            session = await create(agent, **kwargs)
            if len(calls) == 1:
                raise ns.OutcomeUnknown("sanitized lost create reply")
            return session

        with patch.object(client, "create_session", lose_first_reply):
            await workspaces._create_session(
                SESSION, "user-1", reject_removes_rows=True
            )
            self.make_due()
            await workspaces.retry_creates()
        self.assertEqual(self.ledger.binding["kagent_session_id"], CONTEXT_ID)
        self.assertEqual(calls[0], calls[1])
        self.assertEqual(len(self.fake.created_request_ids), 1)

    async def test_a_permanent_error_stops_without_retrying(self):
        for status in (3, 5, 7, 16):
            with self.subTest(status=status):
                self.ledger.create_state.update(
                    create_attempts=0, create_retry_at=None, create_stopped=None
                )
                request_ids = self.failing_creates(
                    SessionError("no such agent or bad workspace", grpc_status=status)
                )
                try:
                    # The refresh path: a rejection keeps the rows.
                    await workspaces._create_session(
                        SESSION, "user-1", reject_removes_rows=False
                    )
                except workspaces.WorkspaceRejected:
                    self.assertIn(status, workspaces._REJECTED)
                state = self.ledger.create_state
                self.assertEqual(state["create_stopped"], "rejected")
                self.assertIsNone(state["create_retry_at"])
                self.make_due()
                self.assertEqual(await workspaces.retry_creates(), [])
                self.assertEqual(len(request_ids), 1, "no automatic retry")
                observed, detail = workspaces._pending_create(state)
                self.assertEqual(observed, WorkspaceObservedState.FAILED)
                self.assertIn(f"(gRPC {status})", detail)
                self.assertNotIn("no such agent or bad workspace", detail)
                self.assertIn("refused", detail)
        self.assertIsNone(self.ledger.binding["kagent_session_id"])

    async def test_a_create_hitting_a_deleted_request_is_permanent(self):
        self.failing_creates(
            SessionError("request id belongs to a deleted session", grpc_status=9)
        )
        await workspaces._create_session(SESSION, "user-1", reject_removes_rows=False)
        self.assertEqual(self.ledger.create_state["create_stopped"], "rejected")

    async def test_a_rejected_first_create_still_removes_the_rows(self):
        self.failing_creates(SessionError("origin not allowed", grpc_status=3))
        delete_rows = AsyncMock()
        with patch.object(workspaces, "_delete_rows", delete_rows):
            with self.assertRaises(workspaces.WorkspaceRejected):
                await workspaces._create_session(
                    SESSION, "user-1", reject_removes_rows=True
                )
        delete_rows.assert_awaited_once_with(SESSION, evidence="kagent-rejected:3")

    async def test_backoff_doubles_to_the_cap(self):
        state = dict(self.ledger.create_state)
        delays = []
        for _ in range(6):
            before = datetime.now(UTC)
            state = workspaces._create_retry_fields(
                state, "unavailable", permanent=False
            )
            delays.append(round((state["create_retry_at"] - before).total_seconds()))
        self.assertEqual(delays, [15, 30, 60, 120, 120, 120])
        self.assertEqual(state["create_attempts"], 6)

    async def test_retries_give_up_after_the_window_and_refresh_restarts_them(self):
        request_ids = self.failing_creates(UNAVAILABLE, UNAVAILABLE)
        await workspaces._create_session(SESSION, "user-1", reject_removes_rows=True)
        state = self.ledger.create_state
        state["create_first_failed_at"] = datetime.now(UTC) - timedelta(
            seconds=settings.workspace_create_retry_window_seconds
        )
        self.make_due()
        self.assertEqual(await workspaces.retry_creates(), [])
        self.assertEqual(state["create_stopped"], "gave_up")
        self.assertEqual(state["create_attempts"], 2)
        self.assertIsNone(state["create_retry_at"])
        observed, detail = workspaces._pending_create(state)
        self.assertEqual(observed, WorkspaceObservedState.FAILED)
        self.assertIn("after 2 attempts", detail)
        self.assertIn("Refresh to try again", detail)

        self.assertEqual(await workspaces.retry_creates(), [], "stopped")
        self.assertEqual(len(request_ids), 2)

        # A manual refresh still works, under the same identity, and clears the stop.
        row = {"session_id": SESSION, "kagent_session_id": None}
        with (
            patch.object(workspaces, "_owned_row", AsyncMock(return_value=row)),
            patch.object(workspaces, "_lifecycle", AsyncMock()),
        ):
            await workspaces.refresh(SESSION, "user-1")
        self.assertEqual(self.ledger.binding["kagent_session_id"], CONTEXT_ID)
        self.assertEqual(len(set(request_ids)), 1)
        self.assertIsNone(state["create_stopped"])
        self.assertEqual(state["create_attempts"], 0)

    async def test_a_failed_manual_refresh_after_a_stop_rearms_retries(self):
        self.ledger.create_state.update(
            create_attempts=7,
            create_first_failed_at=datetime.now(UTC) - timedelta(hours=1),
            create_stopped="gave_up",
            create_error="old",
        )
        self.failing_creates(UNAVAILABLE)
        await workspaces._create_session(
            SESSION, "user-1", reject_removes_rows=False, restart_retries=True
        )
        state = self.ledger.create_state
        self.assertIsNone(state["create_stopped"])
        self.assertEqual(state["create_attempts"], 1)
        self.assertIsNotNone(state["create_retry_at"])

    async def test_a_retry_racing_a_manual_refresh_creates_one_session(self):
        client = ns.get_client()
        create = client.create_session
        started, release = asyncio.Event(), asyncio.Event()
        request_ids = []

        async def held_create(agent, **kwargs):
            request_ids.append(kwargs["request_id"])
            started.set()
            await release.wait()
            return await create(agent, **kwargs)

        self.ledger.create_state.update(
            create_attempts=1,
            create_first_failed_at=datetime.now(UTC),
            create_error="kagent unavailable (gRPC 14)",
        )
        self.make_due()
        row = {"session_id": SESSION, "kagent_session_id": None}
        with (
            patch.object(client, "create_session", held_create),
            patch.object(workspaces, "_owned_row", AsyncMock(return_value=row)),
            patch.object(workspaces, "_lifecycle", AsyncMock()),
            patch.object(workspaces, "publish", AsyncMock()) as published,
        ):
            retry = asyncio.create_task(workspaces.retry_creates())
            await started.wait()
            refresh = asyncio.create_task(workspaces.refresh(SESSION, "user-1"))
            await asyncio.sleep(0.01)
            self.assertFalse(refresh.done(), "refresh waits for the in-flight retry")
            release.set()
            self.assertEqual(await retry, [SESSION])
            await refresh
        published.assert_awaited_once()
        self.assertEqual(
            len(request_ids), 1, "refresh found the Session the retry made"
        )
        self.assertEqual(len(self.fake.created_request_ids), 1)
        self.assertEqual(self.ledger.binding["kagent_session_id"], CONTEXT_ID)
        state = self.ledger.create_state
        self.assertEqual(state["create_attempts"], 0)
        self.assertIsNone(state["create_retry_at"])
        self.assertIsNone(state["create_stopped"])

    async def test_a_secret_outage_is_retried_with_backoff(self):
        with patch(
            "mainloop.runtime.agent_credentials.publish_for_binding",
            AsyncMock(side_effect=OSError("Kubernetes API unavailable")),
        ):
            self.make_due()
            with self.assertLogs(ns.logger, "ERROR"):
                self.assertEqual(await workspaces.retry_creates(), [])
        state = self.ledger.create_state
        self.assertEqual(state["create_attempts"], 1)
        self.assertIsNone(state["create_stopped"])
        self.assertIsNotNone(state["create_retry_at"])
        # The owner sees a classified reason, never the raw exception text.
        self.assertEqual(state["create_error"], "Mainloop error (OSError)")
        _, detail = workspaces._pending_create(state)
        self.assertNotIn("Kubernetes API unavailable", detail)

    async def test_the_detail_uses_the_classified_reason(self):
        self.failing_creates(
            SessionError("dial tcp 10.0.0.7:9000: secret-ish detail", grpc_status=14)
        )
        await workspaces._create_session(SESSION, "user-1", reject_removes_rows=True)
        state = self.ledger.create_state
        self.assertEqual(state["create_error"], "kagent unavailable (gRPC 14)")
        _, detail = workspaces._pending_create(state)
        self.assertNotIn("10.0.0.7", detail)

    async def test_a_replacement_after_a_delivery_confirm_keeps_retrying(self):
        # The first create fails and its retries end (stale state for this identity).
        self.failing_creates(UNAVAILABLE)
        await workspaces._create_session(SESSION, "user-1", reject_removes_rows=True)
        self.ledger.create_state.update(
            create_first_failed_at=datetime.now(UTC) - timedelta(days=2),
            create_stopped="gave_up",
            create_retry_at=None,
        )
        # A message delivery confirms the Session: the old identity's state is cleared.
        await ns._ensure_kagent_session(self.ledger.binding)
        self.assertEqual(self.ledger.binding["kagent_session_id"], CONTEXT_ID)
        state = self.ledger.create_state
        self.assertEqual(
            (state["create_attempts"], state["create_stopped"], state["create_error"]),
            (0, None, None),
        )

        # kagent deletes the Session; the replacement's create fails transiently.
        await ns.get_client().delete_session(CONTEXT_ID)
        self.fake.next_session_ids = ["replacement"]
        request_ids = self.failing_creates(UNAVAILABLE)
        with self.assertRaises(SessionError):
            await ns._ensure_kagent_session(self.ledger.binding)
        self.assertIsNone(self.ledger.binding["kagent_session_id"])
        self.assertIsNone(state["create_stopped"])
        self.assertIsNotNone(state["create_retry_at"], "the new identity is retried")

        self.make_due()
        self.assertEqual(await workspaces.retry_creates(), [SESSION])
        self.assertEqual(self.ledger.binding["kagent_session_id"], "replacement")
        self.assertEqual(len(set(request_ids)), 1, "the replacement's one identity")
        self.assertNotEqual(request_ids[0], ns.create_request_id(SESSION))
        self.assertEqual(state["create_attempts"], 0)

    async def test_a_failure_after_a_concurrent_confirm_is_not_recorded(self):
        await self.with_session()
        await workspaces._record_create_failure(SESSION, UNAVAILABLE, permanent=False)
        self.assertEqual(self.ledger.create_state["create_attempts"], 0)
        self.assertIsNone(self.ledger.create_state["create_retry_at"])

    def test_an_old_unrecorded_create_is_left_to_refresh(self):
        row = {
            "create_attempts": 0,
            "create_error": None,
            "create_stopped": None,
            "created_at": datetime.now(UTC) - timedelta(days=3),
        }
        observed, detail = workspaces._pending_create(row)
        self.assertEqual(observed, WorkspaceObservedState.UNKNOWN)
        self.assertIn("refresh to retry", detail)
        row["created_at"] = datetime.now(UTC)
        observed, detail = workspaces._pending_create(row)
        self.assertEqual(observed, WorkspaceObservedState.RESUMING)
        self.assertIn("retrying automatically", detail)


class MainThreadTests(WorkspaceTestCase):
    async def test_the_main_thread_is_never_suspended(self):
        await self.with_session()
        self.ledger.binding["role"] = "main"
        with self.assertRaises(workspaces.WorkspaceConflict):
            await workspaces.suspend_if_quiet(SESSION)
        with self.assertRaises(workspaces.WorkspaceConflict):
            await workspaces.suspend_if_quiet(SESSION, only_if_idle=True)
        self.assertNotIn("SuspendSession", self.fake.calls())


class DeleteTests(WorkspaceTestCase):
    async def test_workspace_delete_removes_rows_only_after_kagent_confirms(self):
        await self.with_session()
        delete_rows = AsyncMock()
        with (
            patch.object(workspaces, "_owned_row", AsyncMock()),
            patch.object(workspaces, "_delete_rows", delete_rows),
        ):
            await workspaces.delete(SESSION, "user-1")
        self.assertEqual(self.fake.sessions[CONTEXT_ID][0], RuntimeState.DELETED)
        delete_rows.assert_awaited_once_with(
            SESSION, evidence=f"kagent-deleted:{CONTEXT_ID}"
        )

    async def test_workspace_delete_with_kagent_unreachable_keeps_rows(self):
        await self.with_session()
        delete_rows = AsyncMock()
        with (
            patch.object(workspaces, "_owned_row", AsyncMock()),
            patch.object(workspaces, "_delete_rows", delete_rows),
            patch.object(
                ns.get_client(),
                "delete_session",
                AsyncMock(side_effect=Unreachable("x")),
            ),
        ):
            with self.assertRaises(workspaces.WorkspaceUnconfirmed):
                await workspaces.delete(SESSION, "user-1")
        delete_rows.assert_not_awaited()

    async def test_delete_of_an_unknown_create_resolves_it_and_deletes_the_session(
        self,
    ):
        # The create's reply was lost: Mainloop has no Session id, kagent has the Session.
        self.fake.sessions[CONTEXT_ID] = (RuntimeState.READY, RuntimeOperation.NONE)
        self.fake.session_agents[CONTEXT_ID] = ns.agent_ref(
            self.ledger.binding["kind"], self.ledger.binding["role"]
        ).encode()
        self.fake.workspaces[CONTEXT_ID] = WORKSPACE.encode()
        self.fake.created_request_ids[ns._request_id(self.ledger.binding)] = CONTEXT_ID
        self.assertIsNone(self.ledger.binding["kagent_session_id"])
        delete_rows = AsyncMock()
        with (
            patch.object(workspaces, "_owned_row", AsyncMock()),
            patch.object(workspaces, "_delete_rows", delete_rows),
        ):
            await workspaces.delete(SESSION, "user-1")
        self.assertEqual(self.fake.sessions[CONTEXT_ID][0], RuntimeState.DELETED)
        self.assertEqual(self.fake.calls(), ["CreateSession", "DeleteSession"])
        delete_rows.assert_awaited_once_with(
            SESSION, evidence=f"kagent-deleted:{CONTEXT_ID}"
        )

    async def test_delete_keeps_the_rows_while_the_create_is_still_unknown(self):
        delete_rows = AsyncMock()
        with (
            patch.object(workspaces, "_owned_row", AsyncMock()),
            patch.object(workspaces, "_delete_rows", delete_rows),
            patch.object(
                ns.get_client(),
                "create_session",
                AsyncMock(side_effect=Unreachable("x")),
            ),
        ):
            with self.assertRaises(workspaces.WorkspaceUnconfirmed):
                await workspaces.delete(SESSION, "user-1")
        delete_rows.assert_not_awaited()

    async def test_delete_keeps_unknown_create_rows_when_reconciliation_is_rejected(
        self,
    ):
        delete_rows = AsyncMock()
        with (
            patch.object(workspaces, "_owned_row", AsyncMock()),
            patch.object(workspaces, "_delete_rows", delete_rows),
            patch.object(
                ns.get_client(),
                "create_session",
                AsyncMock(side_effect=SessionError("no", grpc_status=3)),
            ),
        ):
            with self.assertRaises(workspaces.WorkspaceUnconfirmed):
                await workspaces.delete(SESSION, "user-1")
        delete_rows.assert_not_awaited()

    async def test_workspace_delete_refuses_while_a_turn_is_open(self):
        await self.with_session()
        await self.ledger.record_message(
            session_id=SESSION,
            conversation_id="conv-1",
            text="hi",
            state="sending",
            source="user",
        )
        with patch.object(workspaces, "_owned_row", AsyncMock()):
            with self.assertRaises(workspaces.WorkspaceConflict):
                await workspaces.delete(SESSION, "user-1")
        self.assertNotIn("DeleteSession", self.fake.calls())

    async def test_archive_deletes_the_kagent_session(self):
        await self.with_session()
        self.ledger.archived = True
        self.assertTrue(await ns.delete_kagent_session(SESSION))
        self.assertEqual(self.fake.sessions[CONTEXT_ID][0], RuntimeState.DELETED)
        self.assertTrue(self.ledger.kagent_deleted)

    async def test_archive_of_a_session_kagent_no_longer_has_counts_as_deleted(self):
        self.ledger.binding["kagent_session_id"] = "gone"
        self.assertTrue(await ns.delete_kagent_session(SESSION))
        self.assertTrue(self.ledger.kagent_deleted)

    async def test_archive_with_nothing_created_has_nothing_to_delete(self):
        self.assertTrue(await ns.delete_kagent_session(SESSION))
        self.assertEqual(self.fake.calls(), [])

    async def test_archive_waits_for_an_open_turn_and_the_reconcile_pass_deletes_after(
        self,
    ):
        await self.with_session()
        self.ledger.archived = True
        # A message recorded just before the archive: its turn must not be cut off.
        mid = await self.ledger.record_message(
            session_id=SESSION,
            conversation_id="conv-1",
            text="hi",
            state="recorded",
            source="user",
        )
        self.assertFalse(await ns.delete_kagent_session(SESSION))
        await ns.reconcile_archived_deletes()
        self.assertNotIn("DeleteSession", self.fake.calls())
        self.assertFalse(self.ledger.kagent_deleted)
        # Queued behind an open turn also counts.
        await self.ledger.set_delivery(mid, "queued")
        self.assertFalse(await ns.delete_kagent_session(SESSION))
        # Once the turn has settled the retry deletes the Session.
        await self.ledger.set_delivery(mid, "completed")
        await ns.reconcile_archived_deletes()
        self.assertEqual(self.fake.sessions[CONTEXT_ID][0], RuntimeState.DELETED)
        self.assertTrue(self.ledger.kagent_deleted)

    async def test_a_user_message_to_an_archived_session_is_refused(self):
        await self.with_session()
        self.session.archived_at = object()
        with self.assertRaisesRegex(ValueError, "archived"):
            await ns.submit_message(SESSION, "hello")
        self.assertEqual(self.ledger.rows, {}, "nothing is recorded")
        self.assertEqual(self.fake.calls(), [])

    async def test_a_message_that_loses_the_race_with_the_archive_is_refused(self):
        await self.with_session()
        reads = []

        async def get_session(session_id):
            # The archive lands between the first read and the one under the lock.
            reads.append(session_id)
            self.session.archived_at = object() if len(reads) > 1 else None
            return self.session

        with patch.object(ns.db, "get_session", get_session):
            with self.assertRaisesRegex(ValueError, "archived"):
                await ns.submit_message(SESSION, "hello")
        self.assertEqual(self.ledger.rows, {})

    async def test_an_unconfirmed_archive_delete_is_retried_by_the_reconcile_pass(self):
        await self.with_session()
        self.ledger.archived = True
        with patch.object(
            ns.get_client(), "delete_session", AsyncMock(side_effect=Unreachable("x"))
        ):
            self.assertFalse(await ns.delete_kagent_session(SESSION))
        self.assertFalse(self.ledger.kagent_deleted)
        await ns.reconcile_archived_deletes()
        self.assertTrue(self.ledger.kagent_deleted)
        self.assertEqual(self.fake.sessions[CONTEXT_ID][0], RuntimeState.DELETED)


class LifecycleMappingTests(unittest.TestCase):
    def test_preparation_state_mapping_and_disabled_behavior(self):
        session = KagentSession(
            id="s",
            state=RuntimeState.READY,
            operation=RuntimeOperation.NONE,
            context_id="s",
        )
        for git_enabled, push_enabled in (
            (True, True),
            (False, False),
            (True, False),
            (False, True),
        ):
            with (
                patch.object(workspaces.settings, "git_transport_enabled", git_enabled),
                patch.object(workspaces.settings, "push_gate_enabled", push_enabled),
            ):
                for state in ("requested", "failed"):
                    observed, detail = workspaces.state_of(session, prepare_state=state)
                    if git_enabled and push_enabled:
                        self.assertEqual(
                            observed,
                            (
                                WorkspaceObservedState.RESUMING
                                if state == "requested"
                                else WorkspaceObservedState.FAILED
                            ),
                        )
                        self.assertEqual(
                            detail,
                            (
                                "Preparing workspace"
                                if state == "requested"
                                else "Workspace preparation failed; replace the session"
                            ),
                        )
                    else:
                        self.assertEqual(
                            (observed, detail), (WorkspaceObservedState.RUNNING, None)
                        )

    def state(self, state, operation=RuntimeOperation.NONE, **kw):
        return workspaces.state_of(
            KagentSession(
                id="s", state=state, operation=operation, context_id="s", **kw
            )
        )

    def test_states(self):
        State, Op, Observed = RuntimeState, RuntimeOperation, WorkspaceObservedState
        cases = [
            (State.READY, Op.NONE, Observed.RUNNING),
            (State.READY, Op.SUSPEND, Observed.SUSPENDING),
            (State.READY, Op.RESUME, Observed.RESUMING),
            (State.SUSPENDED, Op.NONE, Observed.SUSPENDED),
            (State.SUSPENDED, Op.RESUME, Observed.RESUMING),
            (State.CREATING, Op.CREATE, Observed.RESUMING),
            (State.FAILED, Op.NONE, Observed.FAILED),
            (State.DELETING, Op.DELETE, Observed.UNKNOWN),
            (State.DELETED, Op.NONE, Observed.UNKNOWN),
        ]
        for state, op, expected in cases:
            with self.subTest(state=state.name, op=op.name):
                self.assertEqual(self.state(state, op)[0], expected)

    def test_a_failure_carries_kagents_reason(self):
        observed, detail = self.state(
            RuntimeState.FAILED, failure_message="clone failed"
        )
        self.assertEqual(observed, WorkspaceObservedState.FAILED)
        self.assertEqual(detail, "clone failed")


class ObserveTests(WorkspaceTestCase):
    async def test_no_kagent_session_yet_is_unknown_and_retrying(self):
        observed, detail = await workspaces._observe(None)
        self.assertEqual(observed, WorkspaceObservedState.UNKNOWN)
        self.assertIn("retrying automatically", detail)

    async def test_a_session_kagent_no_longer_has_is_unknown(self):
        observed, _ = await workspaces._observe("gone")
        self.assertEqual(observed, WorkspaceObservedState.UNKNOWN)

    async def test_unreachable_kagent_is_unknown_not_an_error(self):
        with patch.object(
            ns.get_client(), "get_session", AsyncMock(side_effect=Unreachable("down"))
        ):
            observed, detail = await workspaces._observe(CONTEXT_ID)
        self.assertEqual(observed, WorkspaceObservedState.UNKNOWN)
        self.assertIn("unreachable", detail)

    async def test_a_suspended_session_is_observed_as_suspended(self):
        await self.with_session()
        self.fake.sessions[CONTEXT_ID] = (RuntimeState.SUSPENDED, RuntimeOperation.NONE)
        observed, _ = await workspaces._observe(CONTEXT_ID)
        self.assertEqual(observed, WorkspaceObservedState.SUSPENDED)


if __name__ == "__main__":
    unittest.main()


class WakeForPreviewTests(WorkspaceTestCase):
    """A preview of a suspended workspace resumes it through kagent first, once."""

    async def asyncSetUp(self):
        await super().asyncSetUp()
        await self.with_session()
        self.row = {"session_id": SESSION, "kagent_session_id": CONTEXT_ID}
        self.touch = AsyncMock()
        self.publish = AsyncMock()
        for patcher in (
            patch.object(workspaces, "_owned_row", AsyncMock(return_value=self.row)),
            patch.object(workspaces, "_lifecycle", AsyncMock(return_value="lifecycle")),
            patch.object(workspaces, "touch", self.touch),
            patch.object(workspaces, "publish", self.publish),
        ):
            patcher.start()
            self.addCleanup(patcher.stop)

    def suspend_session(self):
        self.fake.sessions[CONTEXT_ID] = (
            RuntimeState.SUSPENDED,
            RuntimeOperation.NONE,
        )

    async def test_a_suspended_workspace_is_resumed_and_published(self):
        self.suspend_session()
        self.assertTrue(await workspaces.wake_for_preview(SESSION, "user-1"))
        self.assertEqual(self.fake.calls().count("ResumeSession"), 1)
        self.touch.assert_awaited_once_with(SESSION)
        self.publish.assert_awaited_once_with("user-1", "lifecycle")

    async def test_a_running_workspace_is_left_alone(self):
        self.assertFalse(await workspaces.wake_for_preview(SESSION, "user-1"))
        self.assertNotIn("ResumeSession", self.fake.calls())
        self.touch.assert_not_awaited()
        self.publish.assert_not_awaited()

    async def test_concurrent_previews_resume_once(self):
        self.suspend_session()
        results = await asyncio.gather(
            *(workspaces.wake_for_preview(SESSION, "user-1") for _ in range(8))
        )
        self.assertEqual(results, [True] * 8)
        self.assertEqual(self.fake.calls().count("ResumeSession"), 1)
        self.assertEqual(workspaces._waking, {})

    async def test_a_preview_after_the_resume_checks_again(self):
        self.suspend_session()
        self.assertTrue(await workspaces.wake_for_preview(SESSION, "user-1"))
        self.suspend_session()  # idle-out suspended it again
        self.assertTrue(await workspaces.wake_for_preview(SESSION, "user-1"))
        self.assertEqual(self.fake.calls().count("ResumeSession"), 2)

    async def test_a_cancelled_preview_does_not_cancel_the_shared_resume(self):
        self.suspend_session()
        first = asyncio.create_task(workspaces.wake_for_preview(SESSION, "user-1"))
        second = asyncio.create_task(workspaces.wake_for_preview(SESSION, "user-1"))
        await asyncio.sleep(0)
        first.cancel()
        self.assertTrue(await second)
        self.assertEqual(self.fake.calls().count("ResumeSession"), 1)

    async def test_a_failed_resume_raises_and_the_next_preview_retries(self):
        self.suspend_session()
        with patch.object(
            KagentClient, "resume_session", AsyncMock(side_effect=Unreachable("down"))
        ):
            with self.assertRaises(workspaces.WorkspaceUnconfirmed):
                await asyncio.gather(
                    workspaces.wake_for_preview(SESSION, "user-1"),
                    workspaces.wake_for_preview(SESSION, "user-1"),
                )
        self.assertEqual(workspaces._waking, {})
        self.touch.assert_not_awaited()
        self.assertTrue(await workspaces.wake_for_preview(SESSION, "user-1"))
        self.assertEqual(self.fake.calls().count("ResumeSession"), 1)

    async def test_a_refused_resume_is_a_conflict(self):
        self.suspend_session()
        with patch.object(
            KagentClient,
            "resume_session",
            AsyncMock(side_effect=SessionError("no", grpc_status=9)),
        ):
            with self.assertRaises(workspaces.WorkspaceConflict):
                await workspaces.wake_for_preview(SESSION, "user-1")
