"""Credential lifecycle SQL against opt-in scratch Postgres; Kubernetes remains fake."""

import asyncio
from dataclasses import replace
from unittest.mock import AsyncMock, patch

import asyncpg
from fastapi import HTTPException
from mainloop.db import db
from mainloop.runtime import agent_credentials as credentials
from mainloop.runtime import native_sessions
from mainloop.runtime.agent_identity import token_for
from mainloop.runtime.agent_tools import AgentService
from mainloop.runtime.delegation import PgStore
from mainloop.runtime.kagent_client import (
    KagentSession,
    OutcomeUnknown,
    RuntimeOperation,
    RuntimeState,
    SessionCredential,
    SessionError,
)
from tests.runtime.test_postgres_ledger import PostgresTestCase, _init_schema

from models import SessionStatus


class CredentialPostgresTests(PostgresTestCase):
    async def reload_binding(self, sid):
        # Explicitly bypass the pool and all process-local binding dictionaries.
        conn = await asyncpg.connect(self.url)
        try:
            return dict(
                await conn.fetchrow(
                    "SELECT * FROM native_bindings WHERE session_id=$1", sid
                )
            )
        finally:
            await conn.close()

    async def test_startup_intent_reloads_and_retries_create_then_delete(self):
        sid, cid = await self.bound_session(role="child", status="active")
        mid = await self.delivery(sid, cid, "recorded", source="brief")
        await native_sessions.ledger.update_binding(
            sid,
            kagent_session_id="reserved-actor",
            kagent_request_id="persisted-request",
        )
        binding = await native_sessions.get_binding(sid)
        self.assertTrue(
            await native_sessions._remember_child_start_failure(binding, "timeout")
        )
        actor = KagentSession(
            id="reserved-actor",
            context_id="reserved-actor",
            state=RuntimeState.CREATING,
            operation=RuntimeOperation.CREATE,
        )
        creates, deletes = [], []

        async def create(agent, *, request_id, credentials):
            creates.append((request_id, credentials))
            nonlocal actor
            actor = replace(
                actor, state=RuntimeState.READY, operation=RuntimeOperation.NONE
            )
            return actor

        async def delete(session_id):
            nonlocal actor
            deletes.append(session_id)
            if len(deletes) == 1:
                actor = replace(
                    actor,
                    state=RuntimeState.DELETING,
                    operation=RuntimeOperation.DELETE,
                )
                raise OutcomeUnknown("response lost after admitted delete")
            actor = replace(
                actor, state=RuntimeState.DELETED, operation=RuntimeOperation.NONE
            )
            return actor

        client = AsyncMock()
        client.get_session.side_effect = lambda session_id: actor
        client.create_session.side_effect = create
        client.delete_session.side_effect = delete
        ref = SessionCredential(
            "http://fixture-mcp", "Authorization", "fixture-tokens", sid
        )
        with patch.object(
            native_sessions, "get_client", return_value=client
        ), patch.object(
            credentials.credentials, "publish", AsyncMock(return_value=ref)
        ), patch.object(
            credentials.credentials,
            "remove",
            AsyncMock(side_effect=RuntimeError("fake outage")),
        ):
            binding = await self.reload_binding(sid)
            self.assertEqual(binding["child_start_failure"], "timeout")
            with self.assertRaises(native_sessions.ChildStartPending):
                await native_sessions._settle_child_start_failure(binding)
            self.assertEqual((await db.get_session(sid)).status, SessionStatus.ACTIVE)
            pending = await self.reload_binding(sid)
            self.assertTrue(pending["token_hash"])
            self.assertFalse(pending["credential_cleanup_pending"])
            self.assertEqual(
                await native_sessions.ledger.delivery_state(mid), "recorded"
            )
            self.assertEqual(pending["child_start_failure"], "timeout")
            with self.assertRaises(SessionError):
                await native_sessions._settle_child_start_failure(pending)
        self.assertEqual(creates, [("persisted-request", (ref,))])
        self.assertEqual(deletes, ["reserved-actor", "reserved-actor"])
        terminal = await self.reload_binding(sid)
        self.assertIsNone(terminal["token_hash"])
        self.assertTrue(terminal["credential_cleanup_pending"])
        self.assertEqual(terminal["kagent_session_id"], "reserved-actor")
        self.assertEqual(terminal["kagent_request_id"], "persisted-request")
        self.assertEqual((await db.get_session(sid)).status, SessionStatus.FAILED)
        self.assertEqual(await native_sessions.ledger.delivery_state(mid), "failed")
        self.assertEqual(
            {call[0] for call in client.method_calls},
            {"get_session", "create_session", "delete_session"},
        )

    async def test_startup_intent_and_send_claim_contend_on_binding(self):
        sid, cid = await self.bound_session(role="child", status="active")
        mid = await self.delivery(sid, cid, "recorded", source="brief")
        intent, claimed = await asyncio.gather(
            native_sessions.ledger.remember_child_start_failure(
                sid, "contended startup"
            ),
            native_sessions.ledger.transition(
                mid, "sending", from_states=("recorded",)
            ),
        )
        self.assertNotEqual(intent, claimed)
        binding = await self.reload_binding(sid)
        self.assertEqual(bool(binding["child_start_failure"]), intent)
        self.assertEqual(
            await native_sessions.ledger.delivery_state(mid),
            "recorded" if intent else "sending",
        )
        self.assertTrue(binding["token_hash"])
        self.assertEqual((await db.get_session(sid)).status, SessionStatus.ACTIVE)

    async def test_context_failure_intent_reloads_before_disposal_and_revocation(self):
        sid, cid = await self.bound_session(role="child", status="active")
        mid = await self.delivery(sid, cid, "recorded", source="brief")
        actor = KagentSession(
            id="context-failure-actor",
            context_id="context-failure-actor",
            state=RuntimeState.READY,
            operation=RuntimeOperation.NONE,
        )
        client = AsyncMock()
        client.create_session.return_value = actor
        client.ensure_ready.return_value = actor
        client.get_session.side_effect = lambda session_id: actor
        deletes = []

        async def delete(session_id):
            nonlocal actor
            deletes.append(session_id)
            if len(deletes) == 1:
                actor = replace(
                    actor,
                    state=RuntimeState.DELETING,
                    operation=RuntimeOperation.DELETE,
                )
                raise OutcomeUnknown("delete response lost after context failure")
            actor = replace(
                actor, state=RuntimeState.DELETED, operation=RuntimeOperation.NONE
            )
            return actor

        client.delete_session.side_effect = delete
        ref = SessionCredential(
            "http://fixture-mcp", "Authorization", "fixture-tokens", sid
        )
        with patch.object(
            native_sessions, "get_client", return_value=client
        ), patch.object(
            credentials.credentials, "publish", AsyncMock(return_value=ref)
        ), patch(
            "mainloop.runtime.delegation.render_for_binding",
            AsyncMock(side_effect=RuntimeError("standing context DB read failed")),
        ) as render, patch.object(
            credentials.credentials,
            "remove",
            AsyncMock(side_effect=RuntimeError("fake cleanup outage")),
        ) as remove:
            await native_sessions._deliver(sid, mid, "brief")
            pending = await self.reload_binding(sid)
            self.assertEqual(pending["kagent_session_id"], actor.id)
            self.assertIn(
                "standing context DB read failed", pending["child_start_failure"]
            )
            self.assertTrue(pending["token_hash"])
            self.assertFalse(pending["credential_cleanup_pending"])
            self.assertEqual((await db.get_session(sid)).status, SessionStatus.ACTIVE)
            self.assertEqual(
                await native_sessions.ledger.delivery_state(mid), "recorded"
            )
            remove.assert_not_awaited()
            await self.pool.expire_connections()
            await native_sessions._deliver(sid, mid, "brief")
            render.assert_awaited_once()
            remove.assert_awaited_once_with(sid)
        terminal = await self.reload_binding(sid)
        self.assertIsNone(terminal["token_hash"])
        self.assertTrue(terminal["credential_cleanup_pending"])
        self.assertEqual(terminal["kagent_session_id"], "context-failure-actor")
        self.assertEqual(deletes, ["context-failure-actor", "context-failure-actor"])
        self.assertEqual(actor.state, RuntimeState.DELETED)
        self.assertEqual(actor.operation, RuntimeOperation.NONE)
        self.assertEqual((await db.get_session(sid)).status, SessionStatus.FAILED)
        self.assertEqual(await native_sessions.ledger.delivery_state(mid), "failed")
        client.create_session.assert_awaited_once()
        client.ensure_ready.assert_awaited_once()
        self.assertEqual(
            {call[0] for call in client.method_calls},
            {"get_session", "create_session", "ensure_ready", "delete_session"},
        )

    async def auth(self, sid):
        return await AgentService(PgStore()).authenticate(token_for(sid))

    async def test_terminal_transitions_revoke_hash_and_remove_key(self):
        for status in (
            SessionStatus.COMPLETED,
            SessionStatus.FAILED,
            SessionStatus.CANCELLED,
        ):
            sid, _ = await self.bound_session(role="child")
            self.assertEqual((await self.auth(sid)).binding["session_id"], sid)
            with patch.object(credentials.credentials, "remove", AsyncMock()) as remove:
                await db.update_session(sid, status=status)
            remove.assert_awaited_once_with(sid)
            row = await self.pool.fetchrow(
                "SELECT token_hash, credential_cleanup_pending FROM native_bindings WHERE session_id=$1",
                sid,
            )
            self.assertIsNone(row["token_hash"])
            self.assertFalse(row["credential_cleanup_pending"])
            with self.assertRaises(HTTPException) as denied:
                await self.auth(sid)
            self.assertEqual(denied.exception.status_code, 401)

    async def test_archive_revokes_even_a_stale_terminal_hash(self):
        parent, _ = await self.bound_session(role="main")
        child, _ = await self.bound_session(role="child", parent_session_id=parent)
        await self.pool.execute(
            "UPDATE sessions SET status='completed' WHERE id=$1", child
        )
        with patch.object(credentials.credentials, "remove", AsyncMock()) as remove:
            archived = await db.archive_sessions(self.user, parent_session_id=parent)
        self.assertEqual(archived, [child])
        remove.assert_awaited_once_with(child)
        self.assertIsNone((await native_sessions.get_binding(child))["token_hash"])
        with self.assertRaises(HTTPException):
            await self.auth(child)
        self.assertEqual((await self.auth(parent)).actor.role, "main")

    async def test_secret_outage_retains_retry_after_new_pool(self):
        sid, _ = await self.bound_session(role="child")
        with patch.object(
            credentials.credentials,
            "remove",
            AsyncMock(side_effect=RuntimeError("fake outage")),
        ):
            await db.update_session(sid, status=SessionStatus.COMPLETED)
        self.assertTrue(
            (await native_sessions.get_binding(sid))["credential_cleanup_pending"]
        )
        with self.assertRaises(HTTPException):
            await self.auth(sid)
        # Retry observes persisted state through fresh connections, rather than process memory.
        await self.pool.expire_connections()
        with patch.object(credentials.credentials, "remove", AsyncMock()) as remove:
            await credentials.reconcile_cleanup()
        self.assertIn(
            ((sid,), {}), [(c.args, c.kwargs) for c in remove.await_args_list]
        )
        self.assertFalse(
            (await native_sessions.get_binding(sid))["credential_cleanup_pending"]
        )
        with patch.object(credentials.credentials, "remove", AsyncMock()) as remove:
            await credentials.reconcile_cleanup()
        remove.assert_not_awaited()

    async def test_fresh_schema_and_replay_keep_active_identity(self):
        sid, _ = await self.bound_session(role="child")
        await _init_schema(self.url)
        await _init_schema(self.url)
        binding = await native_sessions.get_binding(sid)
        self.assertTrue(binding["token_hash"])
        self.assertFalse(binding["credential_cleanup_pending"])
        self.assertIsNone(binding["child_start_failure"])

    async def test_child_start_failure_uses_durable_identity_cleanup(self):
        sid, _ = await self.bound_session(role="child", status="active")
        binding = await native_sessions.get_binding(sid)
        with patch.object(
            credentials.credentials,
            "remove",
            AsyncMock(side_effect=RuntimeError("fake outage")),
        ):
            await native_sessions._fail_child_start(binding, "initial create rejected")
        self.assertEqual((await db.get_session(sid)).status, SessionStatus.FAILED)
        row = await native_sessions.get_binding(sid)
        self.assertIsNone(row["token_hash"])
        self.assertTrue(row["credential_cleanup_pending"])
        with self.assertRaises(HTTPException):
            await self.auth(sid)
