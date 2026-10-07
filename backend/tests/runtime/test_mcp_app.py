"""MCP protocol and identity checks with sanitized fakes, no agents or database."""

import unittest
from pathlib import Path
from unittest.mock import Mock, patch

from fastapi import HTTPException
from fastapi.testclient import TestClient
from mainloop.mcp_app import TOOLS, create_app, invoke
from mainloop.runtime.agent_credentials import MCP_ORIGIN, CredentialStore
from mainloop.runtime.agent_identity import hash_token
from mainloop.runtime.agent_tools import AgentService
from mainloop.runtime.kagent_client import (
    SessionCredential,
    _field_bytes,
    decode_fields,
)
from mainloop.runtime.policy import tools_for
from tests.runtime.test_context_model import KINDS, FakeStore


class MCPTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.store = FakeStore()
        self.service = AgentService(self.store, KINDS)
        self.ctx = await self.service.authenticate("tok-main")

    async def call(self, tool, **args):
        result = await invoke(self.service, self.ctx, tool, args)
        self.assertFalse(result.isError, result.content)
        return result.structuredContent

    async def test_delegate_report_policy_and_idempotency(self):
        child = (await self.call("delegate", kind="codex", brief="do it"))["session_id"]
        ctx = await self.service.authenticate(f"tok-{child}")
        self.assertEqual(
            tools_for(ctx.actor),
            {"whoami", "note", "decide", "report", "open_pull_request"},
        )
        for name in TOOLS.keys() - tools_for(ctx.actor):
            r = await invoke(self.service, ctx, name, {})
            self.assertTrue(r.isError)
            self.assertIn("[role]", r.content[0].text)
        for _ in range(2):
            r = await invoke(self.service, ctx, "report", {"summary": "done"})
            self.assertFalse(r.isError)
        self.assertEqual(self.store.reports, ["done"])

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

    async def test_status_read_cancel_and_clear_stay_in_own_tree(self):
        child = (await self.call("delegate", kind="claude", brief="do it"))[
            "session_id"
        ]
        self.assertIn(child[:8], (await self.call("status"))["text"])
        self.assertIn("truncated", (await self.call("read", session=child))["text"])
        r = await invoke(self.service, self.ctx, "cancel", {"session": "other-tree"})
        self.assertTrue(r.isError)
        await self.call("cancel", session=child)
        r = await self.call("clear", session=child)
        self.assertEqual(r["cleared"], [child])
        self.assertEqual(self.store.native_turns_sent_to_children, 0)

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
                TOOLS.keys() - {"report"},
            )
            r = rpc("tools/call", {"name": "whoami", "arguments": {}})
            self.assertEqual(r.json()["result"]["structuredContent"]["role"], "main")
            self.assertEqual(client.get("/health", headers=headers).status_code, 404)
            self.assertEqual(
                client.get("/agent-api/topics", headers=headers).status_code, 404
            )
            store.tokens.clear()
            self.assertEqual(rpc("tools/list").status_code, 401)


class CredentialTests(unittest.IsolatedAsyncioTestCase):
    async def test_publish_before_create_and_key_scoped_cleanup(self):
        api = Mock()
        store = CredentialStore(api)
        with patch(
            "mainloop.runtime.agent_credentials.credential_value",
            return_value="Bearer fixture",
        ):
            ref = await store.publish("binding-id")
        self.assertEqual(ref.origin, MCP_ORIGIN)
        self.assertEqual(ref.secret_key, "binding-id")
        body = api.patch_namespaced_secret.call_args.args[2]
        self.assertEqual(set(body["data"]), {"binding-id"})
        await store.remove("binding-id")
        self.assertEqual(
            api.patch_namespaced_secret.call_args.args[2],
            {"data": {"binding-id": None}},
        )

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
        conn.fetchval = AsyncMock(return_value="binding-id")
        conn.execute = AsyncMock()
        conn.fetch = AsyncMock(return_value=[{"session_id": "binding-id"}])

        @asynccontextmanager
        async def connection():
            yield conn

        with patch("mainloop.db.db.connection", connection), patch.object(
            module.credentials,
            "remove",
            AsyncMock(side_effect=RuntimeError("unavailable")),
        ):
            await module.revoke("binding-id")
        sql = conn.fetchval.call_args.args[0]
        self.assertIn("token_hash=NULL", sql)
        self.assertIn("credential_cleanup_pending=TRUE", sql)
        conn.execute.assert_not_called()
        with patch("mainloop.db.db.connection", connection), patch.object(
            module.credentials, "remove", AsyncMock()
        ) as remove:
            await module.reconcile_cleanup()
        remove.assert_awaited_once_with("binding-id")
        self.assertIn(
            "credential_cleanup_pending=FALSE", conn.execute.call_args.args[0]
        )
