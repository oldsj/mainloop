"""native_sessions over kagent: ledger rules with an in-memory ledger and a fake kagent gateway.

Fixture-backed. No database, network or live kagent.
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
from mainloop.runtime.kagent_client import (
    A2AError,
    KagentClient,
    KagentError,
    OutcomeUnknown,
    RuntimeOperation,
    RuntimeState,
    SessionCredential,
    SessionError,
    SessionWorkspace,
    Unreachable,
    assistant_message_id,
)
from mainloop.sse import notify_session_message
from tests.runtime.kagent_fake import CONTEXT_ID, TASK_ID, FakeKagent

from models import SessionStatus

SESSION = "session-1"


class MemoryLedger:
    """The `Ledger` interface over dicts (the SQL itself is exercised against Postgres in CI)."""

    def __init__(self):
        self.binding = {
            "session_id": SESSION,
            "kind": "claude",
            "role": "agent",
            "parent_session_id": None,
            "topic_id": None,
            "kagent_session_id": None,
            "kagent_request_id": None,
            "model": None,
            "turns": 0,
            "standing_hash": None,
            "reported_at": None,
        }
        self.queue_held = False
        self.rows: dict[str, dict] = {}
        self.replies: dict[str, str] = {}
        # Conversation notes written with a ``cancelled`` settle, by note id.
        self.notes: dict[str, str] = {}
        self.sequence = 0
        # The Session ``workspace`` of a branch workspace (None for any other session).
        self.workspace: SessionWorkspace | None = None
        self.archived = False
        self.kagent_deleted = False

    async def get_binding(self, session_id, *, conn=None):
        return self.binding if session_id == SESSION else None

    async def update_binding(self, session_id, **fields):
        self.binding.update(fields)

    async def remember_child_start_failure(self, session_id, reason):
        if self.binding["role"] != "child" or self.binding["turns"]:
            return False
        if any(
            r["state"] in ("sending", "delivered", "completed", "uncertain")
            or r.get("task_id")
            for r in self.rows.values()
        ):
            return False
        self.binding.setdefault("child_start_failure", reason)
        return True

    async def replace_kagent_session(
        self, session_id, old_kagent_session_id, request_id
    ):
        if self.binding.get("child_start_failure"):
            return False
        if self.binding["kagent_session_id"] != old_kagent_session_id:
            return False
        self.binding.update(
            kagent_session_id=None, kagent_request_id=request_id, standing_hash=None
        )
        for r in self.rows.values():
            if r["state"] in ("sending", "delivered"):
                r.update(
                    state="uncertain",
                    detail="the kagent Session was deleted; not replaying",
                )
        return True

    async def bump_turns(self, session_id):
        self.binding["turns"] += 1

    async def record_message(self, *, session_id, conversation_id, text, state, source):
        self.sequence += 1
        mid = f"msg-{self.sequence}"
        self.rows[mid] = {
            "message_id": mid,
            "session_id": session_id,
            "state": state,
            "source": source,
            "task_id": None,
            "evidence_ref": None,
            "detail": None,
            "content": text,
            "updated_at": datetime.now(UTC),
        }
        return mid

    async def record_submission(self, *, session_id, conversation_id, text, source):
        busy = await self.open_count(session_id)
        if busy and source in ("user", "brief"):
            raise ValueError(
                "A previous message is still in flight; wait for its reply before sending another."
            )
        if source == "user":
            self.queue_held = False
        state = "queued" if busy or self.queue_held else "recorded"
        mid = await self.record_message(
            session_id=session_id,
            conversation_id=conversation_id,
            text=text,
            state=state,
            source=source,
        )
        return mid, state

    async def delivery_state(self, message_id):
        row = self.rows.get(message_id)
        return row["state"] if row else None

    async def recorded_deliveries(self, session_id):
        return [
            (r["message_id"], r["content"])
            for r in self.rows.values()
            if r["state"] == "recorded"
        ]

    async def open_count(self, session_id):
        return sum(r["state"] in ns.OPEN_STATES for r in self.rows.values())

    async def active_count(self, session_id):
        queued = () if self.queue_held else ("queued",)
        return sum(r["state"] in (*ns.OPEN_STATES, *queued) for r in self.rows.values())

    async def remember_partial(self, message_id, text):
        if text and self.rows[message_id]["state"] in ns._RESOLVABLE:
            self.rows[message_id]["partial_text"] = text

    async def get_workspace(self, session_id, *, conn=None):
        return self.workspace

    async def undeleted_archived(self):
        if (
            self.archived
            and not self.kagent_deleted
            and self.binding["kagent_session_id"]
        ):
            return [{"session_id": SESSION}]
        return []

    async def mark_kagent_deleted(self, session_id):
        self.kagent_deleted = True

    async def set_delivery(
        self, message_id, state, *, task_id=None, evidence_ref=None, detail=None
    ):
        row = self.rows[message_id]
        row.update(state=state, updated_at=datetime.now(UTC))
        for key, value in (("task_id", task_id), ("evidence_ref", evidence_ref)):
            if value is not None:
                row[key] = value
        # A delivery that got through no longer carries a stale failure or uncertainty detail.
        if detail is not None or state in ("delivered", "completed"):
            row["detail"] = detail

    async def transition(self, message_id, state, *, from_states, **kw):
        if state == "sending" and self.binding.get("child_start_failure"):
            return False
        if self.rows[message_id]["state"] not in from_states:
            return False
        await self.set_delivery(message_id, state, **kw)
        return True

    async def settle_cancelled(
        self,
        message_id,
        *,
        from_states,
        conversation_id,
        note_id,
        note,
        task_id=None,
        detail=None,
        partial=None,
    ):
        if self.rows[message_id]["state"] not in from_states:
            return False
        await self.set_delivery(
            message_id,
            "cancelled",
            task_id=task_id,
            evidence_ref=f"a2a:task/{task_id}" if task_id else None,
            detail=detail,
        )
        self.queue_held = True
        partial = partial or self.rows[message_id].get("partial_text")
        self.notes.setdefault(note_id, ns.stopped_message(partial) if partial else note)
        return True

    async def deliveries(self, session_id):
        return list(self.rows.values())

    async def resolvable_deliveries(self, session_id):
        return [dict(r) for r in self.rows.values() if r["state"] in ns._RESOLVABLE]

    async def promote_queued(self, session_id):
        if await self.open_count(session_id) or self.queue_held:
            return None
        for r in self.rows.values():
            if r["state"] == "queued":
                r["state"] = "recorded"
                return r["message_id"], r["content"]
        return None

    async def fail_open(self, session_id, detail):
        opened = []
        for r in self.rows.values():
            if r["state"] in (
                "recorded",
                "sending",
                "delivered",
                "queued",
                "uncertain",
            ):
                opened.append(
                    {
                        "message_id": r["message_id"],
                        "task_id": r["task_id"],
                        "state": r["state"],
                    }
                )
                r.update(state="failed", detail=detail)
        return opened

    async def mirror_reply(self, conversation_id, message_id, text):
        if message_id in self.replies:
            return False
        self.replies[message_id] = text
        return True

    async def sessions_with_open_work(self):
        return [SESSION]

    async def topic_name(self, topic_id):
        return None


class NativeSessionTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.fake = FakeKagent()
        self.ledger = MemoryLedger()
        self.ledger.binding["token_hash"] = str(12345)
        self.session = SimpleNamespace(
            id=SESSION,
            user_id="user-1",
            conversation_id="conv-1",
            status=SessionStatus.ACTIVE,
            archived_at=None,
        )
        self.mirrored: list[str] = []

        self.now = 0.0

        async def sleep(seconds):
            self.now += seconds

        http = httpx.AsyncClient(
            transport=self.fake.transport(), base_url="http://kagent.test"
        )
        ns._client = KagentClient(
            "http://kagent.test",
            user_id="mainloop",
            client=http,
            sleep=sleep,
            clock=lambda: self.now,
        )
        ns._streaming.clear()

        async def notify_message(user_id, session_id, message_id, role):
            self.mirrored.append(message_id)

        self.updated: list[SessionStatus] = []

        async def update_session(session_id, **fields):
            if "status" in fields:
                self.session.status = fields["status"]
                self.updated.append(fields["status"])

        for patcher in (
            patch(
                "mainloop.runtime.agent_credentials.credentials.publish",
                AsyncMock(
                    return_value=SessionCredential(
                        "http://mainloop-mcp.mainloop.svc.cluster.local",
                        "Authorization",
                        "mainloop-agent-tokens",
                        SESSION,
                    )
                ),
            ),
            patch.object(ns, "ledger", self.ledger),
            patch.object(ns.db, "get_session", AsyncMock(return_value=self.session)),
            patch.object(ns.db, "update_session", update_session),
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
            # Gathering already-completed tasks can finish without yielding.
            # Let _spawn's completion callbacks retire them before polling again.
            await asyncio.sleep(0)

    async def send(self, text="hello", **kw) -> str:
        mid = await ns.submit_message(SESSION, text, **kw)
        await self.settle()
        return mid

    async def test_credentials_precede_create_and_survive_replacement(self):
        from mainloop.runtime.agent_credentials import credentials
        from mainloop.runtime.kagent_client import decode_fields

        self.ledger.binding["role"] = "main"
        calls = []

        async def publish(binding_id):
            calls.append(binding_id)
            self.assertEqual(
                len([r for r in self.fake.requests if r[1].endswith("CreateSession")]),
                len(calls) - 1,
            )
            return SessionCredential(
                "http://mainloop-mcp.mainloop.svc.cluster.local",
                "Authorization",
                "mainloop-agent-tokens",
                binding_id,
            )

        with patch.object(credentials, "publish", publish):
            await ns._ensure_kagent_session(self.ledger.binding)
            await ns.get_client().delete_session(CONTEXT_ID)
            self.fake.next_session_ids = ["replacement-session"]
            await ns._ensure_kagent_session(self.ledger.binding)
        created = [r for r in self.fake.requests if r[1].endswith("CreateSession")]
        self.assertEqual(len(created), 2)
        first, second = (decode_fields(r[2]) for r in created)
        self.assertEqual(first[7], second[7])
        self.assertEqual(
            decode_fields(first[5][0])[2], [ns.settings.kagent_main_agent.encode()]
        )
        self.assertEqual(calls, [SESSION, SESSION])

    async def test_credential_failure_never_calls_create(self):
        from mainloop.runtime.agent_credentials import credentials

        self.ledger.binding["role"] = "child"
        with patch.object(
            credentials,
            "publish",
            AsyncMock(side_effect=RuntimeError("publication failed")),
        ):
            with self.assertRaisesRegex(RuntimeError, "publication failed"):
                await ns._ensure_kagent_session(self.ledger.binding)
        self.assertEqual(self.fake.requests, [])

    async def test_rejected_initial_child_start_is_terminal_after_publication(self):
        from mainloop.runtime.agent_credentials import credentials

        self.ledger.binding["role"] = "child"
        with patch.object(
            ns.get_client(),
            "create_session",
            AsyncMock(side_effect=SessionError("invalid revision", grpc_status=3)),
        ), patch.object(credentials, "publish", AsyncMock()) as publish:
            mid = await self.send(source="brief")
        publish.assert_awaited_once_with(SESSION)
        self.assertEqual(self.ledger.rows[mid]["state"], "failed")
        self.assertEqual(self.session.status, SessionStatus.FAILED)
        self.assertEqual(self.updated, [SessionStatus.FAILED])
        self.assertEqual(self.fake.rpc_calls("SendStreamingMessage"), [])

    async def test_readiness_timeout_reconciles_and_deletes_before_terminal_failure(
        self,
    ):
        self.ledger.binding["role"] = "child"
        with patch.object(
            ns.get_client(),
            "ensure_ready",
            AsyncMock(side_effect=SessionError("readiness timeout")),
        ):
            mid = await self.send(source="brief")
        methods = [r[1].rsplit("/", 1)[1] for r in self.fake.requests]
        self.assertEqual(methods, ["CreateSession", "GetSession", "DeleteSession"])
        self.assertEqual(self.session.status, SessionStatus.FAILED)
        self.assertEqual(self.updated, [SessionStatus.FAILED])
        self.assertEqual(self.ledger.rows[mid]["state"], "failed")
        self.assertEqual(self.fake.rpc_calls("SendStreamingMessage"), [])

    async def test_post_readiness_context_failure_disposes_before_terminal_state(self):
        self.ledger.binding["role"] = "child"
        update = ns.db.update_session

        async def after_disposal(session_id, **fields):
            if fields.get("status") == SessionStatus.FAILED:
                self.assertEqual(
                    self.fake.sessions[CONTEXT_ID],
                    (RuntimeState.DELETED, RuntimeOperation.NONE),
                )
            await update(session_id, **fields)

        with patch(
            "mainloop.runtime.delegation.render_for_binding",
            AsyncMock(side_effect=RuntimeError("standing context DB read failed")),
        ), patch.object(ns.db, "update_session", after_disposal):
            mid = await self.send(source="brief")
        self.assertEqual(self.updated, [SessionStatus.FAILED])
        self.assertEqual(self.ledger.rows[mid]["state"], "failed")
        self.assertEqual(len(self.fake.session_calls("CreateSession")), 1)
        self.assertEqual(len(self.fake.session_calls("DeleteSession")), 1)
        self.assertEqual(self.fake.rpc_calls("SendStreamingMessage"), [])

    async def test_context_failure_with_pending_lost_disposal_remains_retryable(self):
        self.ledger.binding["role"] = "child"
        client = ns.get_client()
        delete = client.delete_session
        deleted = []

        async def lost(session_id):
            deleted.append(session_id)
            self.fake.sessions[session_id] = (
                RuntimeState.DELETING,
                RuntimeOperation.DELETE,
            )
            raise OutcomeUnknown("delete response lost")

        with patch(
            "mainloop.runtime.delegation.render_for_binding",
            AsyncMock(side_effect=RuntimeError("standing context DB read failed")),
        ), patch.object(client, "delete_session", lost):
            mid = await self.send(source="brief")
        self.assertEqual(self.updated, [])
        self.assertEqual(self.ledger.rows[mid]["state"], "recorded")
        self.assertTrue(self.ledger.binding["child_start_failure"])
        self.assertTrue(self.ledger.binding["token_hash"])
        self.assertEqual(deleted, [CONTEXT_ID])

        async def finish(session_id):
            deleted.append(session_id)
            return await delete(session_id)

        with patch.object(client, "delete_session", finish):
            await ns.sync(SESSION)
            await self.settle()
        self.assertEqual(deleted, [CONTEXT_ID, CONTEXT_ID])
        self.assertEqual(self.updated, [SessionStatus.FAILED])
        self.assertEqual(self.ledger.rows[mid]["state"], "failed")
        self.assertEqual(len(self.fake.session_calls("CreateSession")), 1)
        self.assertEqual(self.fake.rpc_calls("SendStreamingMessage"), [])

    async def test_failed_binding_write_after_creation_keeps_actor_for_disposal(self):
        self.ledger.binding["role"] = "child"

        # Real reads return separate dictionaries, so the local admitted ID must survive
        # independently of a failed persistence operation.
        async def read(session_id, **kwargs):
            return dict(self.ledger.binding)

        update = self.ledger.update_binding
        failed = False

        async def write(session_id, **fields):
            nonlocal failed
            if fields.get("kagent_session_id") and not failed:
                failed = True
                raise RuntimeError("binding write failed after creation")
            await update(session_id, **fields)

        with patch.object(self.ledger, "get_binding", read), patch.object(
            self.ledger, "update_binding", write
        ):
            mid = await self.send(source="brief")
        self.assertEqual(self.ledger.binding["kagent_session_id"], CONTEXT_ID)
        self.assertEqual(len(self.fake.session_calls("CreateSession")), 1)
        self.assertEqual(len(self.fake.session_calls("DeleteSession")), 1)
        self.assertEqual(self.ledger.rows[mid]["state"], "failed")
        self.assertEqual(self.updated, [SessionStatus.FAILED])
        self.assertEqual(self.fake.rpc_calls("SendStreamingMessage"), [])

    async def test_context_failure_pending_delete_response_defers_terminal_state(self):
        self.ledger.binding["role"] = "child"
        client = ns.get_client()

        async def pending(session_id):
            self.fake.sessions[session_id] = (
                RuntimeState.DELETING,
                RuntimeOperation.DELETE,
            )
            return await client.get_session(session_id)

        with patch(
            "mainloop.runtime.delegation.render_for_binding",
            AsyncMock(side_effect=RuntimeError("standing context DB read failed")),
        ), patch.object(client, "delete_session", pending):
            mid = await self.send(source="brief")
        self.assertEqual(self.updated, [])
        self.assertEqual(self.ledger.rows[mid]["state"], "recorded")
        await ns.sync(SESSION)
        await self.settle()
        self.assertEqual(self.updated, [SessionStatus.FAILED])
        self.assertEqual(self.ledger.rows[mid]["state"], "failed")
        self.assertEqual(len(self.fake.session_calls("CreateSession")), 1)
        self.assertEqual(self.fake.rpc_calls("SendStreamingMessage"), [])

    async def test_non_protocol_readiness_error_also_disposes_actor(self):
        self.ledger.binding["role"] = "child"
        with patch.object(
            ns.get_client(),
            "ensure_ready",
            AsyncMock(side_effect=RuntimeError("readiness projection failed")),
        ):
            await self.send(source="brief")
        self.assertEqual(len(self.fake.session_calls("DeleteSession")), 1)
        self.assertEqual(self.updated, [SessionStatus.FAILED])
        self.assertEqual(self.fake.rpc_calls("SendStreamingMessage"), [])

    async def test_main_context_error_leaves_actor_and_main_recoverable(self):
        self.ledger.binding["role"] = "main"
        with patch(
            "mainloop.runtime.delegation.render_for_binding",
            AsyncMock(side_effect=RuntimeError("standing context DB read failed")),
        ):
            mid = await self.send()
        self.assertEqual(self.session.status, SessionStatus.WAITING_ON_USER)
        self.assertEqual(self.ledger.rows[mid]["state"], "failed")
        self.assertFalse(self.ledger.binding.get("child_start_failure"))
        self.assertEqual(len(self.fake.session_calls("DeleteSession")), 0)

    async def test_context_error_cannot_dispose_brief_claimed_by_another_process(self):
        self.ledger.binding["role"] = "child"

        async def competing_claim(binding):
            mid = next(iter(self.ledger.rows))
            self.assertTrue(
                await self.ledger.transition(mid, "sending", from_states=("recorded",))
            )
            raise RuntimeError("context read failed after competing claim")

        with patch("mainloop.runtime.delegation.render_for_binding", competing_claim):
            mid = await self.send(source="brief")
        self.assertEqual(self.ledger.rows[mid]["state"], "sending")
        self.assertEqual(self.updated, [])
        self.assertFalse(self.ledger.binding.get("child_start_failure"))
        self.assertEqual(len(self.fake.session_calls("DeleteSession")), 0)

    async def test_unknown_readiness_disposal_defers_failure_and_never_replaces(self):
        self.ledger.binding["role"] = "child"
        client = ns.get_client()
        with patch.object(
            client,
            "ensure_ready",
            AsyncMock(side_effect=SessionError("readiness timeout")),
        ), patch.object(
            client,
            "get_session",
            AsyncMock(side_effect=Unreachable("lookup unavailable")),
        ):
            mid = await self.send(source="brief")
        self.assertEqual(self.ledger.rows[mid]["state"], "recorded")
        self.assertNotEqual(self.session.status, SessionStatus.FAILED)
        self.assertTrue(self.ledger.binding["child_start_failure"])
        await ns.sync(SESSION)
        await self.settle()
        self.assertEqual(self.session.status, SessionStatus.FAILED)
        self.assertEqual(len(self.fake.session_calls("CreateSession")), 1)
        self.assertEqual(len(self.fake.session_calls("DeleteSession")), 1)
        self.assertEqual(self.fake.rpc_calls("SendStreamingMessage"), [])

    async def test_lost_create_response_reconciles_same_request_then_cleans_up(self):
        self.ledger.binding["role"] = "child"
        client = ns.get_client()
        create = client.create_session
        calls = []

        async def lost(*args, **kwargs):
            calls.append(kwargs["request_id"])
            await create(*args, **kwargs)
            raise OutcomeUnknown("create response lost")

        with patch.object(client, "create_session", lost):
            mid = await self.send(source="brief")
        self.assertEqual(self.ledger.rows[mid]["state"], "recorded")
        self.assertNotEqual(self.session.status, SessionStatus.FAILED)
        await ns.sync(SESSION)
        await self.settle()
        self.assertEqual(self.session.status, SessionStatus.FAILED)
        self.assertEqual(len(self.fake.created_request_ids), 1)
        self.assertEqual(len(self.fake.session_calls("CreateSession")), 2)
        self.assertEqual(self.fake.rpc_calls("SendStreamingMessage"), [])
        self.assertEqual(calls, [ns.create_request_id(SESSION)])

    async def test_main_start_rejection_does_not_make_main_terminal(self):
        self.ledger.binding["role"] = "main"
        with patch.object(
            ns.get_client(),
            "create_session",
            AsyncMock(side_effect=SessionError("invalid revision", grpc_status=3)),
        ):
            mid = await self.send()
        self.assertEqual(self.ledger.rows[mid]["state"], "failed")
        self.assertEqual(self.session.status, SessionStatus.WAITING_ON_USER)

    async def test_reserved_aborted_create_progresses_only_on_same_request_retry(self):
        from tests.runtime.kagent_fake import grpc_response

        self.ledger.binding.update(role="child", kagent_request_id="persisted-create")
        client = ns.get_client()
        create = client.create_session
        calls = []

        async def contended(*args, **kwargs):
            calls.append(kwargs["request_id"])
            session = await create(*args, **kwargs)
            if len(calls) == 1:
                self.fake.sessions[session.id] = (
                    RuntimeState.CREATING,
                    RuntimeOperation.CREATE,
                )
                with patch.object(
                    client._client,
                    "post",
                    AsyncMock(return_value=grpc_response(None, status=10)),
                ):
                    return await create(*args, **kwargs)
            self.fake.sessions[session.id] = (RuntimeState.READY, RuntimeOperation.NONE)
            return await client.get_session(session.id)

        with patch.object(client, "create_session", contended):
            mid = await self.send(source="brief")
            self.assertEqual(self.ledger.rows[mid]["state"], "recorded")
            self.assertEqual(self.updated, [])
            await ns.sync(SESSION)
            await self.settle()
        self.assertEqual(calls, ["persisted-create", "persisted-create"])
        self.assertEqual(len(self.fake.created_request_ids), 1)
        self.assertEqual(self.updated, [SessionStatus.FAILED])
        self.assertEqual(self.fake.rpc_calls("SendStreamingMessage"), [])

    async def test_known_pending_create_and_lost_delete_require_operation_retries(self):
        self.ledger.binding.update(
            role="child", child_start_failure="timeout", kagent_request_id="original"
        )
        client = ns.get_client()
        actor = await client.create_session(
            ns.agent_ref("claude", "child"), request_id="original"
        )
        self.ledger.binding["kagent_session_id"] = actor.id
        self.fake.sessions[actor.id] = (RuntimeState.CREATING, RuntimeOperation.CREATE)
        create = client.create_session
        delete = client.delete_session
        creates, deletes = [], []

        async def retry_create(*args, **kwargs):
            creates.append(kwargs["request_id"])
            self.fake.sessions[actor.id] = (RuntimeState.READY, RuntimeOperation.NONE)
            return await create(*args, **kwargs)

        async def retry_delete(session_id):
            deletes.append(session_id)
            if len(deletes) == 1:
                self.fake.sessions[actor.id] = (
                    RuntimeState.DELETED,
                    RuntimeOperation.DELETE,
                )
                raise OutcomeUnknown("delete response lost after admission")
            return await delete(session_id)

        with patch.object(client, "create_session", retry_create), patch.object(
            client, "delete_session", retry_delete
        ):
            with self.assertRaises(ns.ChildStartPending):
                await ns._settle_child_start_failure(self.ledger.binding)
            self.assertEqual(self.updated, [])
            with self.assertRaises(SessionError):
                await ns._settle_child_start_failure(self.ledger.binding)
        self.assertEqual(creates, ["original"])
        self.assertEqual(deletes, [actor.id, actor.id])
        self.assertEqual(self.updated, [SessionStatus.FAILED])
        self.assertEqual(self.fake.rpc_calls("SendStreamingMessage"), [])

    async def test_claimed_brief_blocks_disposal_and_pending_pass_cannot_reset_it(self):
        self.ledger.binding["role"] = "child"
        mid = await self.ledger.record_message(
            session_id=SESSION,
            conversation_id="c",
            text="brief",
            state="sending",
            source="brief",
        )
        self.assertFalse(
            await ns._remember_child_start_failure(
                dict(self.ledger.binding), "contending create"
            )
        )
        self.assertFalse(
            await self.ledger.transition(mid, "recorded", from_states=("recorded",))
        )
        self.assertEqual(self.ledger.rows[mid]["state"], "sending")
        self.assertEqual(self.updated, [])

    async def test_main_unknown_start_is_nonterminal(self):
        self.ledger.binding["role"] = "main"
        with patch.object(
            ns.get_client(),
            "create_session",
            AsyncMock(side_effect=OutcomeUnknown("Aborted after reservation")),
        ):
            await self.send()
        self.assertEqual(self.session.status, SessionStatus.WAITING_ON_USER)
        self.assertFalse(self.ledger.binding.get("child_start_failure"))

    async def test_concurrent_create_loser_cannot_abandon_reserved_actor(self):
        self.ledger.binding.update(role="child", kagent_request_id="contended")
        client = ns.get_client()
        create = client.create_session
        reserved, release = asyncio.Event(), asyncio.Event()
        calls = []

        async def contend(*args, **kwargs):
            calls.append(kwargs["request_id"])
            if len(calls) == 1:
                actor = await create(*args, **kwargs)
                reserved.set()
                await release.wait()
                return actor
            await reserved.wait()
            raise OutcomeUnknown("Aborted after reservation")

        with patch.object(client, "create_session", contend):
            winner = asyncio.create_task(
                ns._ensure_kagent_session(dict(self.ledger.binding))
            )
            await reserved.wait()
            with self.assertRaises(ns.ChildStartPending):
                await ns._ensure_kagent_session(dict(self.ledger.binding))
            self.assertEqual(self.updated, [])
            release.set()
            with self.assertRaises(SessionError):
                await winner
        self.assertEqual(calls, ["contended", "contended"])
        self.assertEqual(len(self.fake.created_request_ids), 1)
        self.assertEqual(len(self.fake.session_calls("DeleteSession")), 1)
        self.assertEqual(self.fake.rpc_calls("SendStreamingMessage"), [])

    async def test_unrelated_pending_operation_does_not_revoke_identity(self):
        self.ledger.binding.update(
            role="child", kagent_session_id=CONTEXT_ID, child_start_failure="timeout"
        )
        self.fake.sessions[CONTEXT_ID] = (RuntimeState.READY, RuntimeOperation.SUSPEND)
        with self.assertRaises(ns.ChildStartPending):
            await ns._settle_child_start_failure(self.ledger.binding)
        self.assertEqual(self.updated, [])
        self.assertEqual(len(self.fake.session_calls("CreateSession")), 0)
        self.assertEqual(len(self.fake.session_calls("DeleteSession")), 0)

    # ---- happy path -----------------------------------------------------------------------

    async def test_turn_completes_and_mirrors_reply_once(self):
        mid = await self.send()
        row = self.ledger.rows[mid]
        self.assertEqual(row["state"], "completed")
        self.assertEqual(row["task_id"], TASK_ID)
        self.assertEqual(row["evidence_ref"], f"a2a:task/{TASK_ID}")
        reply_id = assistant_message_id(SESSION, TASK_ID)
        self.assertEqual(self.ledger.replies, {reply_id: "ok"})
        self.assertEqual(self.mirrored, [reply_id])
        self.assertEqual(self.ledger.binding["turns"], 1)
        self.assertEqual(self.ledger.binding["kagent_session_id"], CONTEXT_ID)
        self.assertEqual(self.session.status, SessionStatus.WAITING_ON_USER)
        sent = self.fake.rpc_calls("SendStreamingMessage")
        self.assertEqual(sent[0]["params"]["message"]["messageId"], mid)
        self.assertEqual(sent[0]["params"]["message"]["contextId"], CONTEXT_ID)

    async def test_a_turn_sends_no_reply_text_over_sse(self):
        # Only the mirrored reply's id goes out (session:message); the text is read from the
        # conversation. No per-event progress stream is published.
        events = []

        async def publish_to_user(user_id, event):
            events.append(event)

        with patch.object(ns, "notify_session_message", notify_session_message), patch(
            "mainloop.sse.event_bus.publish_to_user", publish_to_user
        ):
            await self.send()
        self.assertEqual([e.event.value for e in events], ["session:message"])
        self.assertNotIn("ok", str(events[0].data.values()))

    async def test_session_is_created_once_with_a_stable_request_id(self):
        await self.send()
        await self.send()
        creates = self.fake.session_calls("CreateSession")
        self.assertEqual(len(creates), 1)
        self.assertEqual(len(self.fake.session_calls("GetSession")), 1)

    async def test_suspended_session_is_resumed_before_the_turn(self):
        await self.send()
        await ns.get_client().suspend_session(CONTEXT_ID)
        await self.send()
        self.assertEqual(len(self.fake.session_calls("ResumeSession")), 1)
        self.assertEqual(len(self.fake.accepted_message_ids), 2)

    async def test_agent_kind_selects_the_kagent_agent(self):
        self.ledger.binding["kind"] = "codex"
        self.ledger.binding["role"] = "child"
        await self.send()
        path = next(p for _, p, b in self.fake.requests if isinstance(b, dict))
        self.assertTrue(path.endswith("/codex-subscription-https"))

    async def test_a_workspace_session_runs_on_the_workspace_agent(self):
        self.ledger.binding["kind"] = "codex"
        await self.send()
        path = next(p for _, p, b in self.fake.requests if isinstance(b, dict))
        self.assertTrue(path.endswith("/codex-workspace"))

    async def test_standing_context_prefixes_only_the_first_turn(self):
        self.ledger.binding["role"] = "main"
        with patch(
            "mainloop.runtime.delegation.render_for_binding",
            AsyncMock(return_value="STANDING"),
        ):
            await self.send("one")
            await self.send("two")
        texts = [
            c["params"]["message"]["parts"][0]["text"]
            for c in self.fake.rpc_calls("SendStreamingMessage")
        ]
        self.assertTrue(texts[0].startswith("STANDING"))
        self.assertTrue(texts[0].endswith("one"))
        self.assertEqual(texts[1], "two")

    async def test_standing_context_is_resent_if_the_first_send_was_never_accepted(
        self,
    ):
        self.ledger.binding["role"] = "main"
        self.fake.send_script = ["unreachable"]
        with patch(
            "mainloop.runtime.delegation.render_for_binding",
            AsyncMock(return_value="STANDING"),
        ):
            await self.send("one")
            self.assertIsNone(self.ledger.binding["standing_hash"])
            await self.send("two")
        self.assertIsNotNone(self.ledger.binding["standing_hash"])

    # ---- retry and failure ---------------------------------------------------------------

    async def test_not_accepted_is_retried_transparently(self):
        self.fake.send_script = ["not-accepted", "ok"]
        mid = await self.send()
        self.assertEqual(self.ledger.rows[mid]["state"], "completed")
        ids = {
            c["params"]["message"]["messageId"]
            for c in self.fake.rpc_calls("SendStreamingMessage")
        }
        self.assertEqual(ids, {mid})
        self.assertEqual(len(self.fake.accepted_message_ids), 1)

    async def test_persistent_not_accepted_is_a_definite_failure(self):
        self.fake.send_script = ["not-accepted"] * 100
        mid = await self.send()
        row = self.ledger.rows[mid]
        self.assertEqual(row["state"], "failed")
        self.assertIn("not sent", row["detail"])
        self.assertIn("SendNotAccepted", row["detail"])
        self.assertEqual(self.ledger.replies, {})
        # Retried with the same message for at most the 30s budget, never requeued.
        self.assertLessEqual(self.now, 30.0)
        self.assertEqual(set(self.sent_message_ids()), {mid})
        await ns.sync(SESSION)
        await self.settle()
        self.assertEqual(self.ledger.rows[mid]["state"], "failed")

    async def test_unreachable_gateway_fails_without_sending(self):
        self.fake.send_script = ["unreachable"]
        mid = await self.send()
        self.assertEqual(self.ledger.rows[mid]["state"], "failed")
        self.assertIn("Unreachable", self.ledger.rows[mid]["detail"])
        self.assertEqual(self.fake.accepted_message_ids, [])

    async def test_session_error_fails_before_anything_is_sent(self):
        failed = "00000000-0000-4000-8000-0000000000ff"
        self.fake.sessions[failed] = (RuntimeState.FAILED, RuntimeOperation.NONE)
        self.ledger.binding["kagent_session_id"] = failed
        mid = await self.send()
        self.assertEqual(self.ledger.rows[mid]["state"], "failed")
        self.assertIn("not sent: SessionError", self.ledger.rows[mid]["detail"])
        self.assertEqual(self.fake.rpc_calls("SendStreamingMessage"), [])
        # A failed Session is reported, not silently replaced.
        self.assertEqual(self.ledger.binding["kagent_session_id"], failed)

    async def test_cut_stream_is_resolved_by_observing_the_task_not_resending(self):
        self.fake.send_script = ["cut"]
        mid = await self.send()
        row = self.ledger.rows[mid]
        # The task is visible (still working in the fake), so the delivery is confirmed delivered.
        self.assertEqual(row["state"], "delivered")
        self.assertEqual(row["task_id"], TASK_ID)
        self.assertEqual(len(self.fake.rpc_calls("SendStreamingMessage")), 1)

    async def test_lost_request_with_no_trace_is_uncertain_and_never_replayed(self):
        self.fake.send_script = ["drop"]
        mid = await self.send()
        row = self.ledger.rows[mid]
        self.assertEqual(row["state"], "uncertain")
        self.assertEqual(len(self.fake.rpc_calls("SendStreamingMessage")), 1)
        await ns.sync(SESSION)
        await self.settle()
        self.assertEqual(self.ledger.rows[mid]["state"], "uncertain")
        self.assertEqual(len(self.fake.rpc_calls("SendStreamingMessage")), 1)

    async def test_uncertain_delivery_is_resolved_when_the_task_turns_up(self):
        self.fake.send_script = ["drop"]
        mid = await self.send()
        # The send actually landed: kagent has a task holding the message id.
        self.fake._record_task(
            {"messageId": mid, "contextId": CONTEXT_ID, "parts": [{"text": "hello"}]},
            completed=True,
        )
        await ns.sync(SESSION)
        await self.settle()
        self.assertEqual(self.ledger.rows[mid]["state"], "completed")
        self.assertEqual(list(self.ledger.replies.values()), ["ok"])
        self.assertEqual(len(self.fake.rpc_calls("SendStreamingMessage")), 1)

    async def test_sync_is_idempotent_for_mirroring(self):
        self.fake.send_script = ["cut"]
        mid = await self.send()
        self.assertEqual(self.ledger.rows[mid]["state"], "delivered")
        self.fake.tasks[TASK_ID]["status"] = {"state": "TASK_STATE_COMPLETED"}
        await ns.sync(SESSION)
        await ns.sync(SESSION)
        await self.settle()
        self.assertEqual(len(self.ledger.replies), 1)
        self.assertEqual(self.ledger.binding["turns"], 1)

    async def test_sending_without_a_task_stays_open_until_the_grace_expires(self):
        mid = await self.ledger.record_message(
            session_id=SESSION,
            conversation_id="conv-1",
            text="x",
            state="sending",
            source="user",
        )
        self.ledger.binding["kagent_session_id"] = CONTEXT_ID
        await ns.sync(SESSION)
        self.assertEqual(self.ledger.rows[mid]["state"], "sending")
        self.ledger.rows[mid]["updated_at"] = datetime.now(UTC) - timedelta(minutes=5)
        await ns.sync(SESSION)
        self.assertEqual(self.ledger.rows[mid]["state"], "uncertain")

    async def test_running_task_is_followed_with_a_snapshot_after_restart(self):
        self.fake.send_script = ["cut"]
        mid = await self.send()
        # sync (new process) sees a delivered, non-terminal task and re-attaches.
        self.fake.subscribe_events = __import__(
            "tests.runtime.kagent_fake", fromlist=["stream_chunks"]
        ).stream_chunks(mid)[-1:]
        self.fake.tasks[TASK_ID]["status"] = {"state": "TASK_STATE_WORKING"}
        await ns.sync(SESSION)
        await self.settle()
        self.assertEqual(self.ledger.rows[mid]["state"], "completed")
        self.assertEqual(list(self.ledger.replies.values()), ["ok"])
        self.assertEqual(len(self.fake.rpc_calls("SubscribeToTask")), 1)

    async def test_failed_task_closes_the_delivery_without_a_reply(self):
        mid = await self.send()
        row = self.ledger.rows[mid]
        row["state"] = "delivered"
        self.ledger.replies.clear()
        self.fake.tasks[TASK_ID]["status"] = {
            "state": "TASK_STATE_FAILED",
            "message": {"messageId": "x", "parts": [{"text": "boom"}]},
        }
        self.fake.tasks[TASK_ID]["artifacts"] = []
        await ns.sync(SESSION)
        await self.settle()
        self.assertEqual(row["state"], "failed")
        self.assertIn("boom", row["detail"])
        self.assertEqual(self.ledger.replies, {})

    # ---- queueing ------------------------------------------------------------------------

    async def test_user_message_is_refused_while_a_turn_is_open(self):
        await self.ledger.record_message(
            session_id=SESSION,
            conversation_id="c",
            text="x",
            state="delivered",
            source="user",
        )
        with self.assertRaisesRegex(ValueError, "still in flight"):
            await ns.submit_message(SESSION, "again")

    async def test_report_is_queued_then_sent_when_idle(self):
        first = await ns.submit_message(SESSION, "first")
        queued = await ns.submit_message(SESSION, "report", source="report")
        self.assertEqual(self.ledger.rows[queued]["state"], "queued")
        await self.settle()
        self.assertEqual(self.ledger.rows[first]["state"], "completed")
        self.assertEqual(self.ledger.rows[queued]["state"], "completed")
        self.assertEqual(
            [
                c["params"]["message"]["parts"][0]["text"]
                for c in self.fake.rpc_calls("SendStreamingMessage")
            ],
            ["first", "report"],
        )

    async def test_ended_session_refuses_user_messages(self):
        self.session.status = SessionStatus.CANCELLED
        with self.assertRaisesRegex(ValueError, "start a new one"):
            await ns.submit_message(SESSION, "hi")

    # ---- cancel and identity -------------------------------------------------------------

    async def test_cancel_cancels_the_task_and_is_sticky(self):
        self.fake.send_script = ["cut"]
        mid = await self.send()
        outcome = await ns.cancel(SESSION)
        self.assertEqual(outcome, "stopped")
        self.assertEqual(self.session.status, SessionStatus.CANCELLED)
        self.assertEqual(self.ledger.rows[mid]["state"], "failed")
        self.assertEqual(len(self.fake.rpc_calls("CancelTask")), 1)
        await ns.sync(SESSION)
        self.assertEqual(self.session.status, SessionStatus.CANCELLED)

    async def test_cancel_with_nothing_sent_calls_nothing(self):
        await self.ledger.record_message(
            session_id=SESSION,
            conversation_id="c",
            text="x",
            state="queued",
            source="report",
        )
        self.ledger.binding["kagent_session_id"] = CONTEXT_ID
        self.assertEqual(await ns.cancel(SESSION), "not_running")
        self.assertEqual(self.fake.rpc_calls("CancelTask"), [])
        self.assertEqual(self.fake.rpc_calls("ListTasks"), [])

    async def test_main_thread_cannot_be_cancelled(self):
        self.ledger.binding["role"] = "main"
        with self.assertRaises(ValueError):
            await ns.cancel(SESSION)

    # ---- stop the open turn, keep the session -------------------------------------------------

    def notes_for(self, mid: str) -> list[str]:
        return [
            note
            for note_id, note in self.ledger.notes.items()
            if note_id == ns._stop_note_id(mid)
        ]

    async def test_stop_turn_cancels_the_task_and_the_next_message_starts_fresh(self):
        self.fake.send_script = ["cut"]
        mid = await self.send()
        self.assertEqual(self.ledger.rows[mid]["state"], "delivered")
        self.assertEqual(await ns.stop_turn(SESSION), "stopped")
        row = self.ledger.rows[mid]
        self.assertEqual(row["state"], "cancelled")
        self.assertEqual(row["task_id"], TASK_ID)
        self.assertEqual(self.notes_for(mid), [ns.stopped_message("ok")])
        self.assertEqual(len(self.fake.rpc_calls("CancelTask")), 1)
        # The session and its kagent Session are untouched: idle, not cancelled or deleted.
        self.assertEqual(self.session.status, SessionStatus.WAITING_ON_USER)
        self.assertEqual(self.fake.session_calls("DeleteSession"), [])
        # A late observation cannot revive or re-settle it.
        await ns.sync(SESSION)
        self.assertEqual(self.ledger.rows[mid]["state"], "cancelled")
        # The next message is a new task on the same Session.
        next_mid = await self.send("again")
        self.assertEqual(self.ledger.rows[next_mid]["state"], "completed")
        sent = self.fake.rpc_calls("SendStreamingMessage")[-1]["params"]["message"]
        self.assertEqual(sent["contextId"], CONTEXT_ID)
        self.assertNotIn("taskId", sent)
        self.assertEqual(len(self.fake.session_calls("CreateSession")), 1)

    async def test_stop_turn_clears_a_parked_task(self):
        for parked in ("TASK_STATE_INPUT_REQUIRED", "TASK_STATE_AUTH_REQUIRED"):
            with self.subTest(parked):
                self.fake.send_script = ["cut"]
                mid = await self.send()
                self.fake.tasks[TASK_ID]["status"] = {"state": parked}
                self.assertEqual(await ns.stop_turn(SESSION), "stopped")
                self.assertEqual(self.ledger.rows[mid]["state"], "cancelled")
                self.assertEqual(await ns.ledger.open_count(SESSION), 0)

    async def test_stop_turn_without_an_open_turn_is_a_noop(self):
        self.ledger.binding["kagent_session_id"] = CONTEXT_ID
        self.assertEqual(await ns.stop_turn(SESSION), "no_open_turn")
        mid = await self.send()  # completes
        self.assertEqual(await ns.stop_turn(SESSION), "no_open_turn")
        self.assertEqual(self.ledger.rows[mid]["state"], "completed")
        self.assertEqual(self.fake.rpc_calls("CancelTask"), [])
        self.assertEqual(self.ledger.notes, {})

    async def test_stop_turn_twice_is_idempotent(self):
        self.fake.send_script = ["cut"]
        mid = await self.send()
        self.assertEqual(await ns.stop_turn(SESSION), "stopped")
        self.assertEqual(await ns.stop_turn(SESSION), "no_open_turn")
        self.assertEqual(len(self.fake.rpc_calls("CancelTask")), 1)
        self.assertEqual(self.notes_for(mid), [ns.stopped_message("ok")])

    async def test_stop_turn_that_loses_the_race_to_completion_records_the_completion(
        self,
    ):
        # The turn finished at kagent while Mainloop still showed it open: kagent returns a
        # finished task unchanged, so the delivery completes with its reply and is not cancelled.
        self.fake.send_script = ["cut"]
        mid = await self.send()
        self.fake.tasks[TASK_ID]["status"] = {"state": "TASK_STATE_COMPLETED"}
        self.assertEqual(await ns.stop_turn(SESSION), "finished")
        self.assertEqual(self.ledger.rows[mid]["state"], "completed")
        self.assertEqual(self.ledger.notes, {})
        self.assertEqual(self.ledger.binding["turns"], 1)
        self.assertEqual(await ns.stop_turn(SESSION), "no_open_turn")

    async def test_stop_turn_when_the_stream_settles_the_cancellation_first(self):
        # The stream sees the task cancelled and settles it between kagent's answer and ours:
        # one terminal state, one note, and the stop still reports it stopped.
        self.fake.send_script = ["cut"]
        mid = await self.send()
        client = ns.get_client()
        real_cancel = client.cancel_task

        async def cancel_then_stream_settles(agent, task_id):
            task = await real_cancel(agent, task_id)
            proj = ns.TaskProjection()
            proj.replace(task)
            await ns._finalize(SESSION, mid, proj)
            return task

        with patch.object(client, "cancel_task", cancel_then_stream_settles):
            self.assertEqual(await ns.stop_turn(SESSION), "stopped")
        self.assertEqual(self.ledger.rows[mid]["state"], "cancelled")
        self.assertEqual(self.notes_for(mid), [ns.stopped_message("ok")])
        self.assertEqual(len(self.ledger.notes), 1)

    async def test_a_cancelled_task_seen_by_sync_settles_as_cancelled(self):
        self.fake.send_script = ["cut"]
        mid = await self.send()
        self.fake.tasks[TASK_ID]["status"] = {"state": "TASK_STATE_CANCELED"}
        await ns.sync(SESSION)
        self.assertEqual(self.ledger.rows[mid]["state"], "cancelled")
        self.assertEqual(self.notes_for(mid), [ns.stopped_message("ok")])

    async def test_stop_turn_kagent_error_leaves_the_ledger_untouched(self):
        self.fake.send_script = ["cut"]
        mid = await self.send()
        before = dict(self.ledger.rows[mid])
        self.fake.cancel_task_fails = True
        with self.assertRaises(KagentError):
            await ns.stop_turn(SESSION)
        self.assertEqual(self.ledger.rows[mid], before)
        self.assertEqual(self.ledger.notes, {})
        self.assertEqual(self.session.status, SessionStatus.ACTIVE)
        # Retry once kagent answers.
        self.fake.cancel_task_fails = False
        self.assertEqual(await ns.stop_turn(SESSION), "stopped")
        self.assertEqual(self.ledger.rows[mid]["state"], "cancelled")

    async def test_stop_turn_task_that_keeps_running_is_unconfirmed(self):
        self.fake.send_script = ["cut"]
        mid = await self.send()
        before = dict(self.ledger.rows[mid])
        self.fake.cancel_task_ignored = True
        with self.assertRaises(ns.StopUnconfirmed):
            await ns.stop_turn(SESSION)
        self.assertEqual(self.ledger.rows[mid], before)
        self.assertEqual(self.ledger.notes, {})

    async def test_stop_turn_of_a_send_with_no_visible_task_is_unconfirmed(self):
        mid = await self.ledger.record_message(
            session_id=SESSION,
            conversation_id="conv-1",
            text="x",
            state="sending",
            source="user",
        )
        self.ledger.binding["kagent_session_id"] = CONTEXT_ID
        with self.assertRaises(ns.StopUnconfirmed):
            await ns.stop_turn(SESSION)
        self.assertEqual(self.ledger.rows[mid]["state"], "sending")
        self.assertEqual(self.fake.rpc_calls("CancelTask"), [])

    async def test_stop_of_a_recorded_delivery_claimed_meanwhile_stops_the_in_flight_turn(
        self,
    ):
        # The stop reads the delivery as ``recorded``; before it settles, another writer claims
        # it to ``sending`` and kagent takes the message. The stop must re-read and cancel that
        # task, not report ``finished`` and leave the turn running.
        self.fake.send_script = ["cut"]
        mid = await self.send()
        self.ledger.rows[mid].update(state="recorded", task_id=None)
        real_settle = self.ledger.settle_cancelled
        claimed = []

        async def claimed_first(message_id, **kw):
            if not claimed:
                claimed.append(message_id)
                self.ledger.rows[message_id]["state"] = "sending"
            return await real_settle(message_id, **kw)

        with patch.object(self.ledger, "settle_cancelled", claimed_first):
            self.assertEqual(await ns.stop_turn(SESSION), "stopped")
        self.assertEqual(claimed, [mid])
        self.assertEqual(self.ledger.rows[mid]["state"], "cancelled")
        self.assertEqual(self.ledger.rows[mid]["task_id"], TASK_ID)
        self.assertEqual(len(self.fake.rpc_calls("CancelTask")), 1)
        self.assertEqual(len(self.notes_for(mid)), 1)

    async def test_stop_turn_of_a_message_kagent_never_saw_calls_nothing(self):
        mid = await self.ledger.record_message(
            session_id=SESSION,
            conversation_id="conv-1",
            text="x",
            state="recorded",
            source="user",
        )
        self.assertEqual(await ns.stop_turn(SESSION), "stopped")
        self.assertEqual(self.ledger.rows[mid]["state"], "cancelled")
        self.assertEqual(self.fake.rpc_calls("CancelTask"), [])
        self.assertEqual(self.notes_for(mid), [ns.TURN_STOPPED_NOTE])

    async def test_a_report_queued_behind_a_stopped_turn_waits_for_the_owner(self):
        self.fake.send_script = ["cut"]
        first = await self.send()
        queued = await self.send("report", source="report")
        self.assertEqual(self.ledger.rows[queued]["state"], "queued")
        await ns.stop_turn(SESSION)
        await self.settle()
        self.assertEqual(self.ledger.rows[first]["state"], "cancelled")
        self.assertEqual(self.ledger.rows[queued]["state"], "queued")
        self.assertEqual(len(self.fake.rpc_calls("SendStreamingMessage")), 1)
        # A reconcile pass does not start it either.
        await ns.sync(SESSION)
        await self.settle()
        self.assertEqual(self.ledger.rows[queued]["state"], "queued")
        # A report that arrives after the stop is queued too, behind the held one.
        later = await self.send("another", source="report")
        self.assertEqual(self.ledger.rows[later]["state"], "queued")
        # The owner's next message goes first; the held reports then follow, one at a time.
        mine = await self.send("carry on")
        self.assertEqual(self.ledger.rows[mine]["state"], "completed")
        self.assertEqual(self.ledger.rows[queued]["state"], "completed")
        self.assertEqual(self.ledger.rows[later]["state"], "completed")
        sent = [
            c["params"]["message"]["parts"][0]["text"]
            for c in self.fake.rpc_calls("SendStreamingMessage")
        ]
        self.assertEqual(
            [t for t in sent if t in ("carry on", "report", "another")],
            ["carry on", "report", "another"],
        )

    async def test_a_stop_that_finds_the_turn_finished_does_not_hold_the_queue(self):
        self.ledger.binding["kagent_session_id"] = CONTEXT_ID
        mid = await self.send()
        self.assertEqual(self.ledger.rows[mid]["state"], "completed")
        self.assertEqual(await ns.stop_turn(SESSION), "no_open_turn")
        report = await self.send("report", source="report")
        self.assertEqual(self.ledger.rows[report]["state"], "completed")

    async def test_a_stopped_turn_keeps_the_partial_reply_marked_as_stopped(self):
        self.fake.send_script = ["cut"]
        mid = await self.send()
        await ns.stop_turn(SESSION)
        (note,) = self.notes_for(mid)
        self.assertTrue(note.endswith(ns.TURN_STOPPED_NOTE))
        self.assertTrue(note.startswith("ok"))
        self.assertEqual(self.ledger.replies, {})

    async def test_a_stop_before_any_text_writes_only_the_note(self):
        self.assertEqual(ns.stopped_message(None), ns.TURN_STOPPED_NOTE)
        self.assertEqual(ns.stopped_message("  \n"), ns.TURN_STOPPED_NOTE)
        self.assertEqual(
            ns.stopped_message(" half an answer "),
            "half an answer\n\n" + ns.TURN_STOPPED_NOTE,
        )

    async def test_stop_keeps_streamed_text_when_cancel_and_get_omit_artifacts(self):
        self.fake.send_script = ["cut"]
        self.fake.cut_after = 3  # include the artifact before the stream disconnects
        mid = await self.send()
        self.assertTrue(self.ledger.rows[mid].get("partial_text"))
        partial = self.ledger.rows[mid]["partial_text"]
        self.fake.tasks[TASK_ID].pop("artifacts", None)
        self.assertEqual(await ns.stop_turn(SESSION), "stopped")
        self.assertEqual(self.notes_for(mid), [ns.stopped_message(partial)])

    async def test_stream_cancellation_holds_the_queue_before_stop_returns(self):
        self.fake.send_script = ["cut"]
        mid = await self.send()
        queued = await self.send("report", source="report")
        client = ns.get_client()
        real_cancel = client.cancel_task

        async def observe_in_other_process(agent, task_id):
            task = await real_cancel(agent, task_id)
            projection = ns.TaskProjection()
            projection.replace(task)
            await ns._finalize(SESSION, mid, projection)
            self.assertIsNone(await self.ledger.promote_queued(SESSION))
            return task

        with patch.object(client, "cancel_task", observe_in_other_process):
            self.assertEqual(await ns.stop_turn(SESSION), "stopped")
        self.assertEqual(self.ledger.rows[queued]["state"], "queued")

    async def test_stop_does_not_cancel_a_new_owner_message_from_another_process(self):
        self.fake.send_script = ["cut"]
        first = await self.send()
        client = ns.get_client()
        real_cancel = client.cancel_task
        next_message = None

        async def observe_then_accept_next_message(agent, task_id):
            nonlocal next_message
            task = await real_cancel(agent, task_id)
            projection = ns.TaskProjection()
            projection.replace(task)
            await ns._finalize(SESSION, first, projection)
            next_message, _ = await self.ledger.record_submission(
                session_id=SESSION,
                conversation_id="conv-1",
                text="continue",
                source="user",
            )
            return task

        with patch.object(client, "cancel_task", observe_then_accept_next_message):
            self.assertEqual(await ns.stop_turn(SESSION), "stopped")
        self.assertEqual(self.ledger.rows[first]["state"], "cancelled")
        self.assertEqual(self.ledger.rows[next_message]["state"], "recorded")
        self.assertFalse(self.ledger.queue_held)

    # ---- uncertain delivery: look the task up by messageId, never resend --------------------

    def sent_message_ids(self) -> list[str]:
        return [
            c["params"]["message"]["messageId"]
            for c in self.fake.rpc_calls("SendStreamingMessage")
        ]

    def methods(self) -> list[str]:
        return [
            body["method"]
            for _, path, body in self.fake.requests
            if path.startswith("/agents/") and isinstance(body, dict)
        ]

    async def test_lost_response_is_resolved_by_listing_tasks_for_the_message_id(self):
        # kagent accepted and finished the task, but the response never reached Mainloop.
        self.fake.send_script = ["lost-response"]
        mid = await self.send()
        # The very same pass that saw the loss looked the task up by message id (ListTasks over the
        # session's context), found it, and closed the delivery from it: no second send.
        # ListTasks has no artifacts, so the found task is re-read for its reply text.
        self.assertEqual(
            self.methods(), ["SendStreamingMessage", "ListTasks", "GetTask"]
        )
        self.assertEqual(
            self.fake.rpc_calls("ListTasks")[0]["params"]["contextId"], CONTEXT_ID
        )
        row = self.ledger.rows[mid]
        self.assertEqual(row["state"], "completed")
        self.assertEqual(row["task_id"], TASK_ID)
        self.assertEqual(list(self.ledger.replies.values()), ["ok"])
        self.assertEqual(self.sent_message_ids(), [mid])

    async def test_uncertain_delivery_is_looked_up_on_every_observation_and_never_resent(
        self,
    ):
        self.fake.send_script = ["drop"]
        mid = await self.send()
        self.assertEqual(self.ledger.rows[mid]["state"], "uncertain")
        # Several syncs and reconcile-loop passes while nothing shows the message: each one
        # observes (ListTasks) and none sends.
        for _ in range(3):
            await ns.sync(SESSION)
            await self.settle()
        self.assertEqual(self.ledger.rows[mid]["state"], "uncertain")
        self.assertEqual(self.sent_message_ids(), [mid])
        self.assertEqual(self.methods().count("ListTasks"), 4)  # 1 resolve + 3 syncs
        self.assertEqual(self.fake.accepted_message_ids, [])
        # The send then turns out to have landed: the next observation finds it by message id.
        self.fake._record_task(
            {"messageId": mid, "contextId": CONTEXT_ID, "parts": [{"text": "hello"}]},
            completed=True,
        )
        await ns.sync(SESSION)
        await self.settle()
        self.assertEqual(self.ledger.rows[mid]["state"], "completed")
        self.assertEqual(self.sent_message_ids(), [mid])

    async def test_a_lookup_that_fails_leaves_the_delivery_uncertain_not_resent(self):
        self.fake.send_script = ["lost-response"]
        self.fake.list_tasks_fails = True
        mid = await self.send()
        row = self.ledger.rows[mid]
        self.assertEqual(row["state"], "uncertain")
        self.assertIn("not replaying", row["detail"])
        self.assertEqual(self.sent_message_ids(), [mid])
        self.fake.list_tasks_fails = False
        await ns.sync(SESSION)
        await self.settle()
        self.assertEqual(self.ledger.rows[mid]["state"], "completed")
        self.assertEqual(self.sent_message_ids(), [mid])

    async def test_a_delivery_open_after_a_restart_is_looked_up_not_resent(self):
        # The process died after persisting 'sending' and before any outcome. The fake already
        # holds a task for the message (the send had landed).
        mid = await self.ledger.record_message(
            session_id=SESSION,
            conversation_id="conv-1",
            text="hello",
            state="sending",
            source="user",
        )
        self.ledger.binding["kagent_session_id"] = CONTEXT_ID
        self.fake._record_task(
            {"messageId": mid, "contextId": CONTEXT_ID, "parts": [{"text": "hello"}]},
            completed=True,
        )
        await ns.sync(SESSION)
        await self.settle()
        self.assertEqual(self.ledger.rows[mid]["state"], "completed")
        self.assertEqual(self.methods(), ["ListTasks", "GetTask"])
        self.assertEqual(list(self.ledger.replies.values()), ["ok"])

    async def test_the_user_resending_is_a_new_message_not_a_replay(self):
        self.fake.send_script = ["drop"]
        first = await self.send("hello")
        self.assertEqual(self.ledger.rows[first]["state"], "uncertain")
        # An uncertain delivery does not block; the owner's explicit send is a new logical message.
        second = await self.send("hello again")
        self.assertNotEqual(first, second)
        self.assertEqual(self.sent_message_ids(), [first, second])
        self.assertEqual(self.ledger.rows[first]["state"], "uncertain")

    # ---- review repairs: claims, restarts, deleted Sessions -----------------------------

    async def test_a_cancel_before_the_delivery_runs_means_nothing_is_sent(self):
        self.ledger.binding["kagent_session_id"] = CONTEXT_ID
        await ns.get_client().create_session(
            ns.agent_ref("claude"), request_id=ns.create_request_id(SESSION)
        )
        mid = await ns.submit_message(SESSION, "hello")  # spawned, not yet run
        self.assertEqual(await ns.cancel(SESSION), "not_running")
        await self.settle()
        self.assertEqual(self.ledger.rows[mid]["state"], "failed")
        self.assertEqual(self.ledger.rows[mid]["detail"], "cancelled by user")
        self.assertEqual(self.sent_message_ids(), [])

    async def test_a_recorded_delivery_left_by_a_restart_is_sent_once_by_sync(self):
        # The process died after recording and before claiming the send: kagent never saw it.
        mid = await self.ledger.record_message(
            session_id=SESSION,
            conversation_id="conv-1",
            text="hello",
            state="recorded",
            source="user",
        )
        await ns.sync(SESSION)
        await ns.sync(SESSION)  # a second pass while the first delivery is running
        await self.settle()
        self.assertEqual(self.ledger.rows[mid]["state"], "completed")
        self.assertEqual(self.sent_message_ids(), [mid])
        await ns.sync(SESSION)
        await self.settle()
        self.assertEqual(self.sent_message_ids(), [mid])

    async def test_two_delivery_passes_for_one_message_send_it_once(self):
        mid = await self.ledger.record_message(
            session_id=SESSION,
            conversation_id="conv-1",
            text="hello",
            state="recorded",
            source="user",
        )
        await asyncio.gather(
            ns._deliver(SESSION, mid, "hello"), ns._deliver(SESSION, mid, "hello")
        )
        await self.settle()
        self.assertEqual(self.sent_message_ids(), [mid])
        self.assertEqual(self.ledger.rows[mid]["state"], "completed")

    async def test_a_deleted_kagent_session_is_replaced_with_standing_context(self):
        self.ledger.binding["role"] = "main"
        replacement = "00000000-0000-4000-8000-0000000000aa"
        self.fake.next_session_ids = [CONTEXT_ID, replacement]
        with patch(
            "mainloop.runtime.delegation.render_for_binding",
            AsyncMock(return_value="STANDING"),
        ):
            await self.send("one")
            await ns.get_client().delete_session(CONTEXT_ID)  # e.g. the idle TTL
            second = await self.send("two")
        self.assertEqual(self.ledger.rows[second]["state"], "completed")
        self.assertEqual(self.ledger.binding["kagent_session_id"], replacement)
        new_request_id = self.ledger.binding["kagent_request_id"]
        self.assertIsNotNone(new_request_id)
        self.assertNotEqual(new_request_id, ns.create_request_id(SESSION))
        sends = self.fake.rpc_calls("SendStreamingMessage")
        self.assertEqual(sends[1]["params"]["message"]["contextId"], replacement)
        self.assertTrue(
            sends[1]["params"]["message"]["parts"][0]["text"].startswith("STANDING")
        )

    async def test_a_create_refused_for_a_deleted_session_mints_a_new_request_id(self):
        # The stable request id was used for a Session that kagent later deleted, and the
        # binding never stored that Session's id.
        await ns.get_client().create_session(
            ns.agent_ref("claude"), request_id=ns.create_request_id(SESSION)
        )
        await ns.get_client().delete_session(CONTEXT_ID)
        replacement = "00000000-0000-4000-8000-0000000000bb"
        self.fake.next_session_ids = [replacement]
        mid = await self.send()
        self.assertEqual(self.ledger.rows[mid]["state"], "completed")
        self.assertEqual(self.ledger.binding["kagent_session_id"], replacement)
        creates = self.fake.session_calls("CreateSession")
        self.assertEqual(
            len(creates), 3
        )  # the original, the refused retry, the new one

    async def test_an_open_turn_on_a_deleted_session_does_not_block_for_good(self):
        self.fake.send_script = ["cut"]
        mid = await self.send()
        self.assertEqual(self.ledger.rows[mid]["state"], "delivered")
        await ns.get_client().delete_session(CONTEXT_ID)
        self.fake.tasks.clear()
        self.fake.sessions.clear()  # purged: GetTask and GetSession both fail
        await ns.sync(SESSION)
        await self.settle()
        self.assertEqual(self.ledger.rows[mid]["state"], "uncertain")
        self.assertEqual(self.sent_message_ids(), [mid])
        # The user can send again; that goes to a new Session.
        replacement = "00000000-0000-4000-8000-0000000000cc"
        self.fake.next_session_ids = [replacement]
        second = await self.send("again")
        self.assertEqual(self.ledger.rows[second]["state"], "completed")
        self.assertEqual(self.sent_message_ids(), [mid, second])

    async def test_a_sending_delivery_whose_lookup_keeps_failing_expires_to_uncertain(
        self,
    ):
        mid = await self.ledger.record_message(
            session_id=SESSION,
            conversation_id="conv-1",
            text="x",
            state="sending",
            source="user",
        )
        self.ledger.binding["kagent_session_id"] = CONTEXT_ID
        await ns.get_client().create_session(
            ns.agent_ref("claude"), request_id=ns.create_request_id(SESSION)
        )
        self.fake.list_tasks_fails = True
        await ns.sync(SESSION)
        self.assertEqual(self.ledger.rows[mid]["state"], "sending")
        self.ledger.rows[mid]["updated_at"] = datetime.now(UTC) - timedelta(minutes=5)
        await ns.sync(SESSION)
        self.assertEqual(self.ledger.rows[mid]["state"], "uncertain")
        self.assertEqual(self.sent_message_ids(), [])

    async def test_cancel_of_a_send_with_no_visible_task_is_unknown(self):
        mid = await self.ledger.record_message(
            session_id=SESSION,
            conversation_id="conv-1",
            text="x",
            state="sending",
            source="user",
        )
        self.ledger.binding["kagent_session_id"] = CONTEXT_ID
        self.assertEqual(await ns.cancel(SESSION), "unknown")
        self.assertEqual(self.ledger.rows[mid]["state"], "failed")

    async def test_identity_reports_session_state_and_uncertainty(self):
        self.fake.send_script = ["drop"]
        await self.send()
        info = await ns.identity(SESSION)
        self.assertEqual(info.agent_name, "claude-workspace")
        self.assertEqual(info.kagent_session_id, CONTEXT_ID)
        self.assertEqual(info.session_state, "ready")
        self.assertIn("delivery unknown", info.note)
        self.assertEqual(info.deliveries[0].state, "uncertain")

    async def test_identity_says_when_the_kagent_session_is_gone_not_that_kagent_is_down(
        self,
    ):
        await self.send()
        self.fake.sessions.clear()  # deleted on the kagent side
        info = await ns.identity(SESSION)
        self.assertIsNone(info.session_state)
        self.assertIn("kagent session unavailable", info.note)
        self.assertNotIn("unreachable", info.note)

    async def test_identity_of_an_unbound_session_is_none(self):
        self.assertIsNone(await ns.identity("other"))


class DeliveryDetailTests(unittest.TestCase):
    """What the owner is shown as the reason for a failed delivery."""

    def test_the_text_is_one_bounded_line(self):
        self.assertEqual(ns.safe_detail("a\n\n  b\tc "), "a b c")
        long = ns.safe_detail("word " * 500)
        self.assertEqual(len(long), ns.DETAIL_MAX_CHARS)
        self.assertTrue(long.endswith("…"))
        self.assertIsNone(ns.safe_detail(None))
        self.assertIsNone(ns.safe_detail("  \n "))

    def test_credentials_are_redacted(self):
        for secret in (
            "Authorization: Bearer abc.DEF_123-x",
            "token=hunter2",
            "password: hunter2",
            "api_key = hunter2",
            "https://user:hunter2@example.test/path",
            "sk-ant-api03-abcdefghijkl",
            "ghp_abcdefghijklmnop",
            "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.sig",
            "A" * 48,
        ):
            with self.subTest(secret=secret):
                out = ns.safe_detail(f"failed: {secret} at the end")
                self.assertIn("[redacted]", out)
                self.assertTrue(out.startswith("failed: "))
                self.assertTrue(out.endswith(" at the end") or "[redacted]" in out)
                for leak in ("hunter2", "abc.DEF", "abcdefghijkl", "sig", "AAAA"):
                    self.assertNotIn(leak, out)

    def test_ordinary_error_text_is_left_alone(self):
        text = "not sent: SessionError: SessionService CreateSession failed (grpc 9): Agent does not have a ready prepared revision"
        self.assertEqual(ns.safe_detail(text), text)

    def test_the_error_class_and_a2a_reason_are_kept(self):
        self.assertEqual(
            ns.describe_error(
                A2AError(-32602, "invalid params", reason="INVALID_PARAMS")
            ),
            "A2AError (INVALID_PARAMS): invalid params",
        )
        self.assertEqual(ns.describe_error(A2AError(-32603, "boom")), "A2AError: boom")
        self.assertEqual(
            ns.describe_error(Unreachable("no route")), "Unreachable: no route"
        )


class NextAgentTests(unittest.TestCase):
    def test_workspaces_and_owner_sessions_use_the_workspace_agents_children_the_defaults(
        self,
    ):
        with (
            patch.object(settings, "kagent_main_agent", "main-a"),
            patch.object(settings, "kagent_claude_agent", "child-claude"),
            patch.object(settings, "kagent_codex_agent", "child-codex"),
            patch.object(settings, "kagent_workspace_claude_agent", "ws-claude"),
            patch.object(settings, "kagent_workspace_codex_agent", "ws-codex"),
        ):
            self.assertEqual(
                ns.agent_name("claude"), "ws-claude"
            )  # default role: agent
            self.assertEqual(ns.agent_name("codex", "agent"), "ws-codex")
            self.assertEqual(ns.agent_name("claude", "child"), "child-claude")
            self.assertEqual(ns.agent_name("codex", "child"), "child-codex")
            self.assertEqual(ns.agent_name("claude", "main"), "main-a")
            self.assertEqual(ns.agent_ref("codex", "agent").name, "ws-codex")

    def test_unknown_kind_has_no_agent(self):
        with self.assertRaises(ValueError):
            ns.agent_name("gemini")

    def test_create_request_id_is_stable(self):
        self.assertEqual(ns.create_request_id("a"), ns.create_request_id("a"))
        self.assertNotEqual(ns.create_request_id("a"), ns.create_request_id("b"))


if __name__ == "__main__":
    unittest.main()
