"""MCP protocol and identity checks with sanitized fakes, no agents or database."""

import unittest
from pathlib import Path
from unittest.mock import AsyncMock, Mock, patch

from fastapi import HTTPException
from fastapi.testclient import TestClient
from mainloop.mcp_app import TOOLS, create_app, invoke
from mainloop.runtime.agent_credentials import MCP_ORIGIN, CredentialStore
from mainloop.runtime.agent_identity import hash_token
from mainloop.runtime.agent_tools import AgentService, Ctx
from mainloop.runtime.kagent_client import (
    SessionCredential,
    _field_bytes,
    decode_fields,
)
from mainloop.runtime.policy import Actor, surface_tools, tools_for
from tests.runtime.test_context_model import KINDS, FakeStore


class MCPTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        patcher = patch(
            "mainloop.tasks.lifecycle.authenticate_session",
            AsyncMock(return_value=None),
        )
        patcher.start()
        self.addCleanup(patcher.stop)
        self.store = FakeStore()
        self.service = AgentService(self.store, KINDS)
        self.ctx = await self.service.authenticate("tok-main")

    async def call(self, tool, **args):
        result = await invoke(self.service, self.ctx, tool, args)
        self.assertFalse(result.isError, result.content)
        return result.structuredContent

    async def test_legacy_delegation_payload_and_tools_are_removed(self):
        result = await invoke(
            self.service, self.ctx, "delegate", {"kind": "codex", "brief": "do it"}
        )
        self.assertTrue(result.isError)
        for name in ("status", "read", "cancel", "clear"):
            self.assertNotIn(name, TOOLS)
            result = await invoke(self.service, self.ctx, name, {})
            self.assertTrue(result.isError)

    async def test_validation_and_trim(self):
        for name, args in (
            ("note", {"text": " "}),
            ("read", {"session": "x", "since": -1}),
            ("delegate", {"kind": "unknown", "brief": "x"}),
            ("report", {"summary": "x" * 4001}),
            ("pending_done", {"id": "%"}),
        ):
            r = await invoke(self.service, self.ctx, name, args)
            self.assertTrue(r.isError)
        await self.call("topic_open", name=" billing ", status=" open ")
        await self.call("note", text=" note ", topic=" billing ")
        r = await self.call("pending_add", text="do it", topic="billing")
        await self.call("pending_done", id=r["id"][:8])
        self.assertIn("[0 pending]", (await self.call("topics"))["text"])

    async def test_workspace_grant_policy_and_server_resolved_identity(self):
        binding = {
            "session_id": "workspace-1",
            "role": "agent",
            "kind": "codex",
            "user_id": "u",
            "mcp_grant_kind": "workspace",
            "session_project_id": "project-1",
            "session_repo": "https://github.com/owner/repo",
            "session_branch": "feature/workspace",
            "workspace_repo": "https://github.com/owner/repo",
            "workspace_branch": "feature/workspace",
            "full_name": "owner/repo",
            "owner": "owner",
            "name": "repo",
            "html_url": "https://github.com/owner/repo",
            "kagent_session_id": "runtime-1",
            "token_hash": "hash-only",
        }
        actor = Actor("agent", 0, "workspace")
        ctx = Ctx(binding, actor)
        self.assertEqual(tools_for(actor), frozenset({"whoami", "open_pull_request"}))
        identity = await invoke(self.service, ctx, "whoami", {})
        self.assertFalse(identity.isError)
        self.assertEqual(
            identity.structuredContent,
            {
                "text": "agent codex session=workspac depth=0 grant=active scope=available",
                "session_id": "workspace-1",
                "role": "agent",
                "depth": 0,
                "mcp_grant_kind": "workspace",
                "grant_status": "active",
                "scope_status": "available",
                "project_id": "project-1",
                "workspace_id": "workspace-1",
                "repository": "owner/repo",
                "branch": "feature/workspace",
            },
        )
        self.assertNotIn("token", str(identity.structuredContent).lower())
        for name in ("report", "delegate", "note", "decide", "topic_open"):
            denied = await invoke(self.service, ctx, name, {})
            self.assertTrue(denied.isError, name)

        with patch(
            "mainloop.services.github_creation.open_pull_request",
            new=AsyncMock(return_value={"text": "scoped"}),
        ) as run:
            result = await invoke(
                self.service,
                ctx,
                "open_pull_request",
                {
                    "project_id": "project-1",
                    "branch": "feature/workspace",
                    "expected_sha": "a" * 40,
                    "title": "title",
                    "body": "body",
                    "request_id": "request-1",
                },
            )
        self.assertFalse(result.isError)
        run.assert_awaited_once()

    async def test_task_reads_use_the_store_without_native_turns(self):
        self.store.task_call = AsyncMock(return_value={"text": "stored task"})
        for name in ("task_get", "task_history"):
            await self.call(name, task_id="task-1")
        await self.call("task_list")
        self.assertEqual(self.store.task_call.await_count, 3)
        self.assertEqual(self.store.native_turns_sent_to_children, 0)

    async def test_delegated_standing_supplies_identity_then_task_discovery(self):
        from mainloop.runtime.standing import StandingInputs, render_standing

        for role in ("supervisor", "child"):
            text = render_standing(StandingInputs(role=role))
            self.assertIn("`whoami`", text)
            self.assertIn("`task_get`", text)
            self.assertIn("returned task_id", text)
            self.assertLess(text.index("`whoami`"), text.index("`task_get`"))
        self.assertEqual(self.store.native_turns_sent_to_children, 0)

    def test_continuation_discovery_keeps_existing_narrow_tool_schemas(self):
        for tool in ("task_get", "task_history"):
            schema = TOOLS[tool][0].model_json_schema()
            self.assertEqual(set(schema["properties"]), {"task_id"})
            self.assertFalse(schema["additionalProperties"])
        self.assertEqual(TOOLS["whoami"][0].model_json_schema()["properties"], {})
        for role, depth in (("supervisor", 1), ("child", 2)):
            actor = Actor(role, depth, "workspace")
            self.assertTrue(
                {"whoami", "task_get", "task_history", "task_list"}.issubset(
                    surface_tools(actor, "ordinary")
                )
            )

    async def test_revoked_and_terminal_auth(self):
        for field, value in (
            ("status", "completed"),
            ("status", "failed"),
            ("status", "cancelled"),
            ("archived_at", "now"),
        ):
            self.store.bindings["main-1"][field] = value
            with self.assertRaises(HTTPException) as cm:
                await self.service.authenticate("tok-main")
            self.assertEqual(cm.exception.status_code, 401)
            self.store.bindings["main-1"].pop(field)
        self.store.tokens.pop(hash_token("tok-main"))
        with self.assertRaises(HTTPException):
            await self.service.authenticate("tok-main")


class HTTPTests(unittest.TestCase):
    def test_stateless_protocol_auth_discovery_and_origin(self):
        store = FakeStore()
        with TestClient(create_app(AgentService(store, KINDS))) as client:
            headers = {
                "Authorization": "Bearer tok-main",
                "Accept": "application/json, text/event-stream",
            }

            def rpc(method, params=None, headers=headers):
                return client.post(
                    "/mcp",
                    headers=headers,
                    json={
                        "jsonrpc": "2.0",
                        "id": 1,
                        "method": method,
                        "params": params or {},
                    },
                )

            for auth in ("", "Bearer nope", "Bearer kagent-credential-injected"):
                r = rpc("tools/list", headers={**headers, "Authorization": auth})
                self.assertEqual(r.status_code, 401)
                self.assertEqual(r.content, b"")
            r = rpc(
                "initialize",
                {
                    "protocolVersion": "2025-06-18",
                    "capabilities": {},
                    "clientInfo": {"name": "fixture", "version": "1"},
                },
            )
            self.assertEqual(r.status_code, 200)
            self.assertNotIn("mcp-session-id", r.headers)
            r = rpc("tools/list")
            self.assertEqual(
                {t["name"] for t in r.json()["result"]["tools"]},
                surface_tools(Actor("main", 0)),
            )
            r = rpc("tools/call", {"name": "whoami", "arguments": {}})
            self.assertEqual(r.json()["result"]["structuredContent"]["role"], "main")
            self.assertEqual(client.get("/health", headers=headers).status_code, 404)
            self.assertEqual(
                client.get("/agent-api/topics", headers=headers).status_code, 404
            )
            store.tokens.clear()
            self.assertEqual(rpc("tools/list").status_code, 401)


class ManagedLifespanTests(unittest.TestCase):
    def test_managed_startup_installs_task_ports_without_reconciliation_loops(self):
        from mainloop.db import db
        from mainloop.tasks.projection import Projection
        from mainloop.tasks.provisioning import Provisioning
        from mainloop.tasks.service import ports

        with (
            patch.object(ports, "provisioning", None),
            patch.object(ports, "projection", None),
            patch.object(ports, "_projection_cursor", ""),
            patch("mainloop.runtime.agent_identity.require_token_key") as key,
            patch.object(db, "connect", AsyncMock()) as connect,
            patch.object(db, "disconnect", AsyncMock()) as disconnect,
            patch(
                "mainloop.runtime.native_sessions.close_client", AsyncMock()
            ) as close,
            patch(
                "mainloop.runtime.native_sessions.reconcile_loop", AsyncMock()
            ) as native,
            patch(
                "mainloop.tasks.service.reconciliation_dispatcher", AsyncMock()
            ) as tasks,
        ):
            with TestClient(create_app()):
                key.assert_called_once_with()
                connect.assert_awaited_once_with()
                self.assertIsInstance(ports.provisioning, Provisioning)
                self.assertIsInstance(ports.projection, Projection)
            close.assert_awaited_once_with()
            disconnect.assert_awaited_once_with()
            native.assert_not_called()
            tasks.assert_not_called()

    def test_injected_service_does_not_install_ports_or_manage_database(self):
        from mainloop.db import db
        from mainloop.tasks.service import ports

        with (
            patch.object(ports, "provisioning", None),
            patch.object(ports, "projection", None),
            patch("mainloop.tasks.provisioning.install") as install,
            patch.object(db, "connect", AsyncMock()) as connect,
            patch.object(db, "disconnect", AsyncMock()) as disconnect,
        ):
            with TestClient(create_app(AgentService(FakeStore(), KINDS))):
                self.assertIsNone(ports.provisioning)
                self.assertIsNone(ports.projection)
            install.assert_not_called()
            connect.assert_not_called()
            disconnect.assert_not_called()


class CredentialTests(unittest.IsolatedAsyncioTestCase):
    async def test_per_binding_secret_and_cleanup(self):
        import base64

        api = Mock()
        store = CredentialStore(api)
        with patch(
            "mainloop.runtime.agent_credentials.credential_value",
            return_value="Bearer fixture",
        ):
            ref = await store.publish("binding-id")
            other = await store.publish("other-binding")
        self.assertEqual(ref.origin, MCP_ORIGIN)
        self.assertEqual(ref.secret_name, "mainloop-mcp-binding-id")
        self.assertNotEqual(ref.secret_name, other.secret_name)
        self.assertEqual(ref.secret_key, "authorization")
        body = api.create_namespaced_secret.call_args_list[0].args[1]
        self.assertEqual(
            body["metadata"]["labels"],
            {"mainloop.dev/actor-egress": "true", "mainloop.dev/purpose": "mcp"},
        )
        self.assertEqual(
            body["data"],
            {"authorization": base64.b64encode(b"Bearer fixture").decode()},
        )
        await store.remove("binding-id")
        self.assertEqual(
            api.delete_namespaced_secret.call_args.args[0], ref.secret_name
        )

    async def test_uncertain_create_is_reconciled_without_overwriting(self):
        import base64
        from types import SimpleNamespace

        from kubernetes.client.exceptions import ApiException

        api = Mock()
        api.create_namespaced_secret.side_effect = ApiException(status=409)
        existing = SimpleNamespace(
            data={"authorization": base64.b64encode(b"Bearer fixture").decode()},
            type="Opaque",
            metadata=SimpleNamespace(
                labels={
                    "mainloop.dev/actor-egress": "true",
                    "mainloop.dev/purpose": "mcp",
                }
            ),
        )
        api.read_namespaced_secret.return_value = existing
        store = CredentialStore(api)
        with patch(
            "mainloop.runtime.agent_credentials.credential_value",
            return_value="Bearer fixture",
        ):
            await store.publish("binding-id")
            existing.data["another-binding"] = "conflict"
            with self.assertRaisesRegex(RuntimeError, "conflicts"):
                await store.publish("binding-id")
        api.patch_namespaced_secret.assert_not_called()
        api.replace_namespaced_secret.assert_not_called()

    async def test_errors_are_sanitized_and_delete_is_idempotent(self):
        from kubernetes.client.exceptions import ApiException

        api = Mock()
        store = CredentialStore(api)
        api.create_namespaced_secret.side_effect = ApiException(
            status=500, reason="secret bytes"
        )
        with patch(
            "mainloop.runtime.agent_credentials.credential_value",
            return_value="Bearer fixture",
        ):
            with self.assertRaisesRegex(RuntimeError, "could not publish") as error:
                await store.publish("binding-id")
        self.assertNotIn("secret bytes", str(error.exception))
        api.delete_namespaced_secret.side_effect = ApiException(status=404)
        await store.remove("binding-id")
        api.delete_namespaced_secret.side_effect = ApiException(status=500)
        with self.assertRaisesRegex(RuntimeError, "could not delete"):
            await store.remove("binding-id")

    async def test_shared_or_other_binding_reference_is_rejected(self):
        from mainloop.runtime.agent_credentials import credential_reference

        api = Mock()
        store = CredentialStore(api)
        with self.assertRaisesRegex(RuntimeError, "does not match"):
            await store.remove("binding-id", credential_reference("other"))
        with self.assertRaisesRegex(RuntimeError, "does not match"):
            await store.publish(
                "binding-id",
                SessionCredential(
                    MCP_ORIGIN, "Authorization", "mainloop-agent-tokens", "binding-id"
                ),
            )
        api.assert_not_called()

    async def test_complete_header_contract(self):
        from mainloop.runtime.agent_credentials import credential_value

        with patch(
            "mainloop.runtime.agent_credentials.token_for", return_value="ml_fixture"
        ):
            self.assertEqual(credential_value("binding-id"), "Bearer ml_fixture")

    def test_credential_protobuf_fields(self):
        ref = SessionCredential(
            MCP_ORIGIN, "Authorization", "mainloop-agent-tokens", "binding-id"
        )
        fixture = bytes.fromhex(
            (
                Path(__file__).parent / "fixtures/kagent/session-credential.hex"
            ).read_text()
        )
        self.assertEqual(_field_bytes(7, ref.encode()), fixture)
        fields = decode_fields(decode_fields(fixture)[7][0])
        self.assertEqual(fields[1], [MCP_ORIGIN.encode()])
        self.assertEqual(fields[2], [b"Authorization"])
        self.assertEqual(
            decode_fields(fields[3][0]),
            {1: [b"mainloop-agent-tokens"], 2: [b"binding-id"]},
        )


class RevocationTests(unittest.IsolatedAsyncioTestCase):
    async def test_failed_secret_cleanup_keeps_durable_retry_and_auth_revoked(self):
        from contextlib import asynccontextmanager
        from unittest.mock import AsyncMock

        from mainloop.runtime import agent_credentials as module

        conn = Mock()
        in_transaction = False
        conn.is_in_transaction = Mock(side_effect=lambda: in_transaction)
        reference = {
            "origin": MCP_ORIGIN,
            "header": "Authorization",
            "secret_name": "mainloop-mcp-binding-id",
            "secret_key": "authorization",
        }
        conn.fetchrow = AsyncMock(
            side_effect=[
                {
                    "mcp_grant_kind": "workspace",
                    "credential_ref": reference,
                    "token_hash": "fixture-hash",
                    "credential_cleanup_pending": False,
                },
                {"credential_ref": reference},
                {"credential_ref": reference},
            ]
        )
        conn.execute = AsyncMock()
        conn.fetchval = AsyncMock(return_value=None)
        conn.fetch = AsyncMock(return_value=[{"session_id": "binding-id"}])

        @asynccontextmanager
        async def transaction():
            nonlocal in_transaction
            previous = in_transaction
            in_transaction = True
            try:
                yield
            finally:
                in_transaction = previous

        conn.transaction = transaction

        @asynccontextmanager
        async def connection():
            yield conn

        @asynccontextmanager
        async def lock(_conn, _binding_id):
            self.assertIs(_conn, conn)
            self.assertEqual(_binding_id, "binding-id")
            yield

        async def unavailable(*args):
            self.assertFalse(conn.is_in_transaction())
            raise RuntimeError("unavailable")

        with (
            patch("mainloop.db.db.connection", connection),
            patch.object(module, "_binding_lock", lock),
            patch(
                "mainloop.push_gate.credentials.cleanup_all", AsyncMock()
            ) as git_cleanup,
            patch.object(
                module.credentials, "remove", AsyncMock(side_effect=unavailable)
            ) as remove,
        ):
            await module.revoke("binding-id")
        git_cleanup.assert_awaited_once_with()
        self.assertFalse(conn.is_in_transaction())
        lock_keys = [
            call.args[1]
            for call in conn.mock_calls
            if call[0] in ("execute", "fetchval")
            and "pg_advisory_lock(" in call.args[0]
        ]
        self.assertEqual(
            lock_keys,
            [
                "mainloop:task-authority:binding-id",
                "push-grant:binding-id",
                "mainloop:task-authority:binding-id",
                "mainloop:task-runtime:binding-id",
            ],
        )
        unlock_keys = [
            call.args[1]
            for call in conn.mock_calls
            if call[0] in ("execute", "fetchval")
            and "pg_advisory_unlock(" in call.args[0]
        ]
        self.assertEqual(unlock_keys, list(reversed(lock_keys)))
        remove.assert_awaited_once_with(
            "binding-id", module.reference_from_data(reference)
        )
        sql = "\n".join(call.args[0] for call in conn.execute.await_args_list)
        self.assertIn("token_hash=NULL", sql)
        self.assertIn("credential_cleanup_pending=TRUE", sql)
        self.assertIn("agent_credential_cleanup", sql)

        async def removed(*args):
            self.assertFalse(conn.is_in_transaction())

        with (
            patch("mainloop.db.db.connection", connection),
            patch.object(module, "_binding_lock", lock),
            patch.object(
                module.credentials, "remove", AsyncMock(side_effect=removed)
            ) as remove,
        ):
            await module.reconcile_cleanup()
        remove.assert_awaited_once_with(
            "binding-id", module.reference_from_data(reference)
        )
        sql = "\n".join(call.args[0] for call in conn.execute.await_args_list)
        self.assertIn("credential_cleanup_pending=FALSE", sql)
