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

    async def test_binding_secret_publication_and_durable_delete_retry(self):
        import base64
        from types import SimpleNamespace
        from unittest.mock import Mock

        from kubernetes.client.exceptions import ApiException

        sid, _ = await self.bound_session(role="child")
        binding = await self.reload_binding(sid)
        api = Mock()
        objects = {}

        def create(namespace, body, **kwargs):
            name = body["metadata"]["name"]
            if name in objects:
                raise ApiException(status=409)
            objects[name] = body

        def read(name, namespace, **kwargs):
            body = objects[name]
            return SimpleNamespace(
                data=body["data"],
                type=body["type"],
                metadata=SimpleNamespace(labels=body["metadata"]["labels"]),
            )

        def delete(name, namespace, **kwargs):
            if name not in objects:
                raise ApiException(status=404)
            del objects[name]

        api.create_namespaced_secret.side_effect = create
        api.read_namespaced_secret.side_effect = read
        api.delete_namespaced_secret.side_effect = ApiException(status=500)
        with patch.object(credentials, "credentials", credentials.CredentialStore(api)):
            reference = await credentials.publish_for_binding(binding)
            # A new pool/process observes the persisted reference and reconciles create.
            await self.pool.expire_connections()
            self.assertEqual(await credentials.publish_for_binding(binding), reference)
            body = objects[reference.secret_name]
            self.assertEqual(body["metadata"]["labels"], credentials.SECRET_LABELS)
            self.assertEqual(
                body["data"],
                {
                    "authorization": base64.b64encode(
                        ("Bearer " + token_for(sid)).encode()
                    ).decode()
                },
            )
            await credentials.revoke(sid)
            self.assertIsNone((await self.reload_binding(sid))["token_hash"])
            with self.assertRaises(HTTPException):
                await self.auth(sid)
            self.assertIn(reference.secret_name, objects)
            api.delete_namespaced_secret.side_effect = delete
            await self.pool.expire_connections()
            await credentials.reconcile_cleanup()
            self.assertFalse(objects)
            self.assertFalse(
                (await self.reload_binding(sid))["credential_cleanup_pending"]
            )
            await credentials.credentials.remove(sid, reference)

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
            agent=await native_sessions.binding_agent_ref(binding),
            id="reserved-actor",
            context_id="reserved-actor",
            state=RuntimeState.CREATING,
            operation=RuntimeOperation.CREATE,
        )
        creates, deletes = [], []

        async def create(agent, *, request_id, credentials, workspace=None):
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
            credentials.MCP_ORIGIN,
            "Authorization",
            f"mainloop-mcp-{sid}",
            "authorization",
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

    async def test_a_delivery_with_a_task_is_a_claim_whatever_state_it_ended_in(self):
        for state, task_id, expected in (
            ("cancelled", "task-1", False),  # a first turn the owner stopped
            ("failed", "task-2", False),
            ("cancelled", None, True),  # cancelled before anything was sent
            ("failed", None, True),
        ):
            with self.subTest(state=state, task_id=task_id):
                sid, cid = await self.bound_session(role="child", status="active")
                mid = await self.delivery(sid, cid, state, source="brief")
                await self.pool.execute(
                    "UPDATE native_deliveries SET task_id=$2 WHERE message_id=$1",
                    mid,
                    task_id,
                )
                self.assertEqual(
                    await native_sessions.ledger.remember_child_start_failure(
                        sid, "kagent blip"
                    ),
                    expected,
                )
                binding = await self.reload_binding(sid)
                self.assertEqual(bool(binding["child_start_failure"]), expected)

    async def test_context_failure_intent_reloads_before_disposal_and_revocation(self):
        sid, cid = await self.bound_session(role="child", status="active")
        mid = await self.delivery(sid, cid, "recorded", source="brief")
        actor = KagentSession(
            agent=native_sessions.agent_ref("claude", "child"),
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
            remove.assert_awaited_once()
            self.assertEqual(remove.await_args.args[0], sid)
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

    async def test_terminal_transitions_revoke_hash_and_remove_secret(self):
        for status in (
            SessionStatus.COMPLETED,
            SessionStatus.FAILED,
            SessionStatus.CANCELLED,
        ):
            sid, _ = await self.bound_session(role="main")
            self.assertEqual((await self.auth(sid)).binding["session_id"], sid)
            with patch.object(credentials.credentials, "remove", AsyncMock()) as remove:
                await db.update_session(sid, status=status)
            remove.assert_awaited_once()
            self.assertEqual(remove.await_args.args[0], sid)
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
        remove.assert_awaited_once()
        self.assertEqual(remove.await_args.args[0], child)
        self.assertIsNone((await native_sessions.get_binding(child))["token_hash"])
        with self.assertRaises(HTTPException):
            await self.auth(child)
        self.assertEqual((await self.auth(parent)).actor.role, "main")

    async def test_workspace_publish_and_revoke_serialize_across_database_connections(
        self,
    ):
        sid, _ = await self.bound_session(role="agent", mcp_grant_kind="workspace")
        binding = await self.reload_binding(sid)
        publish_started = asyncio.Event()
        allow_publish = asyncio.Event()

        async def publish(binding_id, reference):
            self.assertEqual(binding_id, sid)
            publish_started.set()
            await allow_publish.wait()
            return reference

        with patch.object(
            credentials.credentials, "publish", AsyncMock(side_effect=publish)
        ) as publish_call, patch.object(
            credentials.credentials, "remove", AsyncMock()
        ) as remove:
            publishing = asyncio.create_task(credentials.publish_for_binding(binding))
            await publish_started.wait()
            revoking = asyncio.create_task(credentials.revoke(sid))
            await asyncio.sleep(0)
            self.assertFalse(revoking.done())
            allow_publish.set()
            reference = await publishing
            await revoking

        self.assertEqual(reference.secret_key, "authorization")
        self.assertEqual(reference.secret_name, f"mainloop-mcp-{sid}")
        publish_call.assert_awaited_once()
        remove.assert_awaited_once()
        self.assertEqual(remove.await_args.args[0], sid)
        current = await self.reload_binding(sid)
        self.assertIsNone(current["token_hash"])
        self.assertFalse(current["credential_cleanup_pending"])
        with self.assertRaises(HTTPException):
            await self.auth(sid)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM agent_credential_cleanup WHERE session_id=$1",
                sid,
            ),
            0,
        )
        with self.assertRaisesRegex(RuntimeError, "revoked"):
            await credentials.publish_for_binding(binding)
        self.assertEqual(publish_call.await_count, 1)

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
        self.assertEqual(remove.await_args.args[0], sid)
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
