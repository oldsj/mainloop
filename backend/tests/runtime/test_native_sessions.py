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
from mainloop.runtime import native_sessions as ns
from mainloop.runtime.kagent_client import (
    KagentClient,
    OutcomeUnknown,
    RuntimeOperation,
    RuntimeState,
    SessionCredential,
    SessionError,
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
        self.rows: dict[str, dict] = {}
        self.replies: dict[str, str] = {}
        self.sequence = 0

    async def get_binding(self, session_id, *, conn=None):
        return self.binding if session_id == SESSION else None

    async def update_binding(self, session_id, **fields):
        self.binding.update(fields)

    async def remember_child_start_failure(self, session_id, reason):
        if self.binding["role"] != "child" or self.binding["turns"]:
            return False
        if any(
            r["state"] in ("sending", "delivered", "completed", "uncertain")
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

    async def deliveries(self, session_id):
        return list(self.rows.values())

    async def resolvable_deliveries(self, session_id):
        return [dict(r) for r in self.rows.values() if r["state"] in ns._RESOLVABLE]

    async def promote_queued(self, session_id):
        if await self.open_count(session_id):
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
            patch.object(
                ns.workspace_adapter, "get_workspace", AsyncMock(return_value=None)
            ),
        ):
            patcher.start()
            self.addCleanup(patcher.stop)

    async def asyncTearDown(self):
        await asyncio.gather(*ns._tasks, return_exceptions=True)
        await ns.close_client()

    async def settle(self):
        while ns._tasks:
            await asyncio.gather(*list(ns._tasks), return_exceptions=True)

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
        await self.send()
        path = next(p for _, p, b in self.fake.requests if isinstance(b, dict))
        self.assertTrue(path.endswith("/codex-subscription-https"))

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
        self.assertEqual(self.fake.accepted_message_ids, [])

    async def test_session_error_fails_before_anything_is_sent(self):
        failed = "00000000-0000-4000-8000-0000000000ff"
        self.fake.sessions[failed] = (RuntimeState.FAILED, RuntimeOperation.NONE)
        self.ledger.binding["kagent_session_id"] = failed
        mid = await self.send()
        self.assertEqual(self.ledger.rows[mid]["state"], "failed")
        self.assertIn("not sent", self.ledger.rows[mid]["detail"])
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
        self.assertEqual(info.agent_name, "claude-subscription")
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


class NextAgentTests(unittest.TestCase):
    def test_unknown_kind_has_no_agent(self):
        with self.assertRaises(ValueError):
            ns.agent_name("gemini")

    def test_create_request_id_is_stable(self):
        self.assertEqual(ns.create_request_id("a"), ns.create_request_id("a"))
        self.assertNotEqual(ns.create_request_id("a"), ns.create_request_id("b"))


if __name__ == "__main__":
    unittest.main()
