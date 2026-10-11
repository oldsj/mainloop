"""Main-provider selection on scratch PostgreSQL; runtime and credentials are faked."""

import asyncio
import json
import unittest
import uuid
from dataclasses import replace
from unittest.mock import AsyncMock, Mock, patch

import httpx
from mainloop import api
from mainloop.config import Settings, settings
from mainloop.db import db
from mainloop.identity import current_user
from mainloop.mcp_app import invoke
from mainloop.providers import registry
from mainloop.runtime import agent_credentials, main_provider
from mainloop.runtime import native_sessions as ns
from mainloop.runtime.agent_identity import token_for
from mainloop.runtime.delegation import PgStore, ensure_main_session, render_for_binding
from mainloop.runtime.kagent_client import (
    KagentSession,
    OutcomeUnknown,
    RuntimeOperation,
    RuntimeState,
    Task,
)
from tests.runtime import test_postgres_task_reports as task_fixtures
from tests.runtime.postgres_fixture import wait_for_lock
from tests.runtime.test_postgres_ledger import KagentFakeCase, PostgresTestCase


class Runtime:
    def __init__(self):
        self.sessions = {}
        self.creates = []
        self.suspends = []
        self.tasks = []
        self.lose_create_reply = False
        self.readiness_failure = False
        self.created = asyncio.Event()
        self.release = asyncio.Event()
        self.release.set()

    async def get_session(self, sid):
        return self.sessions[sid]

    async def list_tasks(self, agent, context_id):
        return self.tasks

    async def suspend_session(self, sid):
        self.suspends.append(sid)
        self.sessions[sid] = replace(self.sessions[sid], state=RuntimeState.SUSPENDED)
        return self.sessions[sid]

    async def create_session(self, agent, *, request_id, **kwargs):
        self.creates.append((agent, request_id, kwargs))
        sid = str(uuid.uuid5(uuid.NAMESPACE_URL, request_id))
        if sid not in self.sessions:
            self.sessions[sid] = KagentSession(
                sid, RuntimeState.READY, RuntimeOperation.NONE, sid, agent=agent
            )
        self.created.set()
        await self.release.wait()
        if self.lose_create_reply:
            self.lose_create_reply = False
            raise OutcomeUnknown("lost create reply")
        return self.sessions[sid]

    async def ensure_ready(self, runtime, **kwargs):
        if self.readiness_failure:
            raise OutcomeUnknown("readiness unavailable")
        return runtime


class SettingsTests(unittest.TestCase):
    def test_defaults_and_invalid_provider(self):
        default = Settings(_env_file=None)
        self.assertEqual(default.main_provider, "claude")
        with self.assertRaises(ValueError):
            Settings(_env_file=None, main_provider="automatic")
        self.assertNotEqual(
            registry().resolve("claude", "main").agents["main"],
            registry().resolve("codex", "main").agents["main"],
        )


class MainProviderTests(PostgresTestCase):
    reuse_database = True

    async def asyncSetUp(self):
        await super().asyncSetUp()
        self.runtime = Runtime()
        self.publish = AsyncMock(side_effect=lambda identity, reference: reference)
        self.spawn = Mock()
        for patcher in (
            patch.object(settings, "owner_id", self.user),
            patch.object(settings, "main_provider", "claude"),
            patch.object(ns, "get_client", lambda: self.runtime),
            patch.object(agent_credentials.credentials, "publish", self.publish),
            patch.object(ns, "_spawn_deliver", self.spawn),
        ):
            patcher.start()
            self.addCleanup(patcher.stop)
        self.binding = await ensure_main_session(self.user)
        self.sid = self.binding["session_id"]
        self.cid = await self.pool.fetchval(
            "SELECT conversation_id FROM sessions WHERE id=$1", self.sid
        )
        self.old_id = str(uuid.uuid4())
        self.runtime.sessions[self.old_id] = KagentSession(
            self.old_id,
            RuntimeState.READY,
            RuntimeOperation.NONE,
            self.old_id,
            agent=await ns.binding_agent_ref(self.binding),
        )
        await ns.ledger.update_binding(
            self.sid,
            kagent_session_id=self.old_id,
            turns=7,
            model="old-model",
            standing_hash="old-context",
        )
        self.binding = await ns.get_binding(self.sid)

    async def select(self, kind="codex"):
        return await main_provider.select(self.user, kind)

    async def test_switch_retains_product_identity_history_and_revokes_old_authority(
        self,
    ):
        old_hash = self.binding["token_hash"]
        await self.delivery(
            self.sid, self.cid, "completed", text="remember the project"
        )
        await self.delivery(
            self.sid, self.cid, "failed", text="do not replay this failed request"
        )
        child, _ = await self.bound_session(role="child", parent_session_id=self.sid)
        child_before = await ns.get_binding(child)
        messages = await self.pool.fetch("SELECT * FROM messages ORDER BY id")
        value = await self.select()
        self.assertEqual(
            (value.desired_provider, value.effective_provider, value.status),
            ("codex", "codex", "ready"),
        )
        self.assertEqual(value.session_id, self.sid)
        self.assertNotEqual(value.kagent_session_id, self.old_id)
        self.assertEqual(value.history[0].kagent_session_id, self.old_id)
        self.assertEqual(self.runtime.suspends, [self.old_id])
        self.assertEqual(
            await self.pool.fetch("SELECT * FROM messages ORDER BY id"), messages
        )
        self.assertEqual(await ns.get_binding(child), child_before)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT conversation_id FROM sessions WHERE id=$1", self.sid
            ),
            self.cid,
        )
        self.assertIsNone(await PgStore().binding_by_token_hash(old_hash))
        fresh = await ns.get_binding(self.sid)
        self.assertIsNotNone(await PgStore().binding_by_token_hash(fresh["token_hash"]))
        self.assertEqual(fresh["turns"], 0)
        self.assertIsNone(fresh["model"])
        self.assertIsNone(fresh["standing_hash"])
        snapshot = json.loads(
            await self.pool.fetchval("SELECT snapshot FROM main_native_history")
        )
        self.assertEqual((snapshot["turns"], snapshot["model"]), (7, "old-model"))
        standing = await render_for_binding(fresh)
        self.assertIn("remember the project", standing)
        self.assertNotIn("do not replay this failed request", standing)
        self.assertEqual(self.runtime.creates[0][0], ns.agent_ref("codex", "main"))
        self.publish.assert_awaited_once_with(
            fresh["credential_identity"],
            agent_credentials.reference_from_data(fresh["credential_ref"]),
        )
        self.spawn.assert_not_called()

    async def test_same_provider_is_idempotent_even_when_busy(self):
        await self.delivery(self.sid, self.cid, "sending")
        value = await self.select("claude")
        self.assertEqual(value.kagent_session_id, self.old_id)
        self.assertFalse(self.runtime.creates)
        self.assertEqual(await ns.get_binding(self.sid), self.binding)

    async def test_switching_back_creates_new_native_identity_and_retains_both_histories(
        self,
    ):
        codex = await self.select()
        claude = await self.select("claude")
        self.assertEqual(claude.effective_provider, "claude")
        self.assertEqual(
            {item.kagent_session_id for item in claude.history},
            {self.old_id, codex.kagent_session_id},
        )
        self.assertNotIn(
            claude.kagent_session_id, {self.old_id, codex.kagent_session_id}
        )
        self.assertEqual(await self.select("claude"), claude)
        self.assertEqual(len(self.runtime.creates), 2)

    async def test_every_unresolved_delivery_blocks_switch_without_mutation(self):
        for state in ("recorded", "queued", "sending", "delivered", "uncertain"):
            with self.subTest(state=state):
                mid = await self.delivery(self.sid, self.cid, state)
                with self.assertRaisesRegex(
                    main_provider.MainProviderError, "main_provider_busy"
                ):
                    await self.select()
                self.assertEqual(await ns.get_binding(self.sid), self.binding)
                await self.pool.execute(
                    "DELETE FROM native_deliveries WHERE message_id=$1", mid
                )
        self.assertFalse(self.runtime.suspends)
        self.assertFalse(self.runtime.creates)

    async def test_active_product_state_and_native_task_are_refused(self):
        await self.pool.execute(
            "UPDATE sessions SET status='active' WHERE id=$1", self.sid
        )
        with self.assertRaisesRegex(
            main_provider.MainProviderError, "main_provider_busy"
        ):
            await self.select()
        await self.pool.execute(
            "UPDATE sessions SET status='idle' WHERE id=$1", self.sid
        )
        self.runtime.tasks = [
            Task(
                id="native-task",
                context_id=self.old_id,
                status={"state": "input-required"},
            )
        ]
        with self.assertRaisesRegex(
            main_provider.MainProviderError, "main_native_turn_active"
        ):
            await self.select()
        self.assertFalse(self.runtime.suspends)
        self.assertEqual(await ns.get_binding(self.sid), self.binding)
        self.runtime.sessions[self.old_id] = replace(
            self.runtime.sessions[self.old_id], state=RuntimeState.SUSPENDED
        )
        with self.assertRaisesRegex(
            main_provider.MainProviderError, "main_native_turn_active"
        ):
            await self.select()
        self.assertEqual(await ns.get_binding(self.sid), self.binding)

    async def test_unsettled_native_and_unconfirmed_suspend_are_refused(self):
        self.runtime.sessions[self.old_id] = replace(
            self.runtime.sessions[self.old_id], operation=RuntimeOperation.RESUME
        )
        with self.assertRaisesRegex(
            main_provider.MainProviderError, "main_native_state_unsettled"
        ):
            await self.select()
        self.runtime.sessions[self.old_id] = replace(
            self.runtime.sessions[self.old_id], operation=RuntimeOperation.NONE
        )
        with patch.object(
            self.runtime,
            "suspend_session",
            AsyncMock(return_value=self.runtime.sessions[self.old_id]),
        ):
            with self.assertRaisesRegex(
                main_provider.MainProviderError, "main_native_suspend_unconfirmed"
            ):
                await self.select()
        self.assertEqual(await ns.get_binding(self.sid), self.binding)

    async def test_deleting_native_is_not_treated_as_confirmed_absence(self):
        self.runtime.sessions[self.old_id] = replace(
            self.runtime.sessions[self.old_id],
            state=RuntimeState.DELETING,
            operation=RuntimeOperation.DELETE,
        )
        with self.assertRaisesRegex(
            main_provider.MainProviderError, "main_native_state_unsettled"
        ):
            await self.select()
        self.assertEqual(await ns.get_binding(self.sid), self.binding)
        self.assertFalse(self.runtime.creates)

    async def test_unknown_suspend_reply_recovers_original_identity_before_switch(self):
        suspend = self.runtime.suspend_session

        async def lose_reply(sid):
            await suspend(sid)
            raise OutcomeUnknown("suspend reply lost")

        with patch.object(self.runtime, "suspend_session", lose_reply):
            with self.assertRaises(OutcomeUnknown):
                await self.select()
        self.assertEqual(await ns.get_binding(self.sid), self.binding)
        self.assertEqual((await self.select()).status, "ready")
        self.assertEqual(len(self.runtime.suspends), 1)

    async def test_lost_create_reply_is_recovered_after_connection_restart(self):
        self.runtime.lose_create_reply = True
        failed = await self.select()
        self.assertEqual(
            (failed.status, failed.effective_provider, failed.kagent_session_id),
            ("failed", None, None),
        )
        self.assertIn("lost create reply", failed.error)
        binding = await ns.get_binding(self.sid)
        await self.pool.expire_connections()
        ready = await self.select()
        self.assertEqual(ready.status, "ready")
        self.assertEqual(len(self.runtime.creates), 2)
        self.assertEqual(self.runtime.creates[0], self.runtime.creates[1])
        self.assertEqual(
            (await ns.get_binding(self.sid))["kagent_request_id"],
            binding["kagent_request_id"],
        )
        self.assertEqual(len(ready.history), 1)
        self.assertEqual(len(self.runtime.sessions), 2)

    async def test_local_write_failure_after_create_reuses_native_identity(self):
        original = ns.ledger.update_binding

        async def fail_write(sid, **kwargs):
            if kwargs.get("kagent_session_id"):
                raise RuntimeError("local write failed after native create")
            return await original(sid, **kwargs)

        with patch.object(ns.ledger, "update_binding", fail_write):
            failed = await self.select()
        self.assertEqual(failed.status, "failed")
        self.assertIsNone(failed.kagent_session_id)
        ready = await self.select()
        self.assertEqual(ready.status, "ready")
        self.assertEqual(self.runtime.creates[0], self.runtime.creates[1])
        self.assertEqual(len(self.runtime.sessions), 2)

    async def test_wrong_native_contract_is_never_activated(self):
        original = self.runtime.create_session

        async def wrong_contract(*args, **kwargs):
            return replace(
                await original(*args, **kwargs), agent=ns.agent_ref("claude", "main")
            )

        with patch.object(self.runtime, "create_session", wrong_contract):
            failed = await self.select()
        self.assertEqual(failed.status, "failed")
        self.assertIsNone(failed.effective_provider)
        self.assertIsNone(failed.kagent_session_id)
        self.assertEqual((await self.select()).status, "ready")

    async def test_readiness_failure_retains_target_and_retry_does_not_create_again(
        self,
    ):
        self.runtime.readiness_failure = True
        failed = await self.select()
        self.assertEqual(failed.status, "failed")
        self.assertIsNotNone(failed.kagent_session_id)
        self.assertIsNone(failed.effective_provider)
        self.runtime.readiness_failure = False
        recovered = await self.select()
        self.assertEqual(recovered.kagent_session_id, failed.kagent_session_id)
        self.assertEqual(recovered.status, "ready")
        self.assertEqual(len(self.runtime.creates), 1)

    async def test_crash_after_commit_holds_reports_and_chat_until_explicit_recovery(
        self,
    ):
        with patch.object(
            main_provider, "_prepare", AsyncMock(side_effect=asyncio.CancelledError)
        ):
            with self.assertRaises(asyncio.CancelledError):
                await self.select()
        self.assertEqual((await main_provider.info(self.sid)).status, "pending")
        with self.assertRaises(ValueError):
            await ns.submit_message(self.sid, "must not be recorded")
        with self.assertRaisesRegex(
            main_provider.MainProviderError, "main_provider_selection_pending"
        ):
            await self.select("claude")
        report = await ns.submit_message(self.sid, "child progress", source="report")
        self.assertEqual(await ns.ledger.delivery_state(report), "queued")
        self.assertIsNone(await ns.ledger.promote_queued(self.sid))
        recovered = await self.select()
        self.assertEqual(recovered.status, "ready")
        self.assertEqual((await ns.ledger.promote_queued(self.sid))[0], report)
        self.assertIsNone(await ns.ledger.promote_queued(self.sid))
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM messages WHERE content='child progress'"
            ),
            1,
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM messages WHERE content='must not be recorded'"
            ),
            0,
        )

    async def test_concurrent_switches_share_one_native_contract(self):
        self.runtime.release.clear()
        first = asyncio.create_task(self.select())
        await self.runtime.created.wait()
        second = asyncio.create_task(self.select())
        try:
            async with self.pool.acquire() as conn:
                self.assertTrue(
                    await wait_for_lock(
                        conn, query="%pg_advisory_lock%", locktype="advisory"
                    )
                )
        finally:
            self.runtime.release.set()
        a, b = await asyncio.gather(first, second)
        self.assertEqual(a, b)
        self.assertEqual(len(self.runtime.creates), 1)

    async def test_chat_waiting_on_switch_uses_new_provider_once(self):
        self.runtime.release.clear()
        switching = asyncio.create_task(self.select())
        await self.runtime.created.wait()
        chatting = asyncio.create_task(ns.submit_message(self.sid, "new instruction"))
        try:
            async with self.pool.acquire() as conn:
                self.assertTrue(
                    await wait_for_lock(
                        conn, query="%pg_advisory_lock%", locktype="advisory"
                    )
                )
        finally:
            self.runtime.release.set()
        switched, mid = await asyncio.gather(switching, chatting)
        self.assertEqual(switched.effective_provider, "codex")
        self.assertEqual(await ns.ledger.delivery_state(mid), "recorded")
        self.spawn.assert_called_once_with(self.sid, mid, "new instruction")

    async def test_chat_committed_before_switch_prevents_switch(self):
        await ns.submit_message(self.sid, "first instruction")
        with self.assertRaisesRegex(
            main_provider.MainProviderError, "main_provider_busy"
        ):
            await self.select()
        self.assertEqual(await ns.get_binding(self.sid), self.binding)

    async def test_routing_is_pinned_across_config_change_and_retry(self):
        self.runtime.lose_create_reply = True
        await self.select()
        with patch.object(settings, "kagent_main_codex_agent", "replacement-agent"):
            await self.select()
            self.assertEqual(
                (await ns.binding_agent_ref(await ns.get_binding(self.sid))).name,
                "mainloop-main-codex",
            )
        self.assertEqual(self.runtime.creates[0][0], self.runtime.creates[1][0])

    async def test_new_authority_can_be_revoked_and_its_secret_cleaned(self):
        await self.select()
        binding = await ns.get_binding(self.sid)
        remove = AsyncMock()
        with patch.object(agent_credentials.credentials, "remove", remove):
            await agent_credentials.revoke(self.sid)
        self.assertIsNone((await ns.get_binding(self.sid))["token_hash"])
        remove.assert_awaited_once_with(
            binding["credential_identity"],
            agent_credentials.reference_from_data(binding["credential_ref"]),
        )

    async def test_unauthorized_invalid_and_disabled_selection_do_not_mutate(self):
        with self.assertRaisesRegex(
            main_provider.MainProviderError, "main_provider_owner_required"
        ):
            await main_provider.select("different-owner", "codex")
        with self.assertRaises(ValueError):
            await self.select("invalid")
        disabled = (
            registry().resolve("codex", "main").model_copy(update={"enabled": False})
        )
        with patch.object(settings, "provider_profiles", [disabled]):
            with self.assertRaisesRegex(ValueError, "disabled"):
                await self.select()
        self.assertEqual(await ns.get_binding(self.sid), self.binding)
        self.assertFalse(self.runtime.creates)

    async def test_same_agent_for_both_providers_is_rejected_before_mutation(self):
        with patch.object(
            settings, "kagent_main_codex_agent", settings.kagent_main_agent
        ):
            with self.assertRaisesRegex(
                main_provider.MainProviderError, "main_provider_agent_conflict"
            ):
                await self.select()
        self.assertEqual(await ns.get_binding(self.sid), self.binding)

    async def test_api_validation_owner_guard_and_truthful_identity(self):
        with patch.object(settings, "api_hosts", "localhost"):
            async with httpx.AsyncClient(
                transport=httpx.ASGITransport(app=api.app), base_url="http://localhost"
            ) as client:
                self.assertEqual(
                    (
                        await client.put(
                            "/main-thread/provider", json={"provider": "bad"}
                        )
                    ).status_code,
                    422,
                )
                self.assertEqual(
                    (
                        await client.put(
                            "/main-thread/provider",
                            json={"provider": "codex"},
                            headers={"Origin": "https://untrusted.example"},
                        )
                    ).status_code,
                    403,
                )
                api.app.dependency_overrides[current_user] = lambda: "different-owner"
                try:
                    self.assertEqual(
                        (
                            await client.put(
                                "/main-thread/provider", json={"provider": "codex"}
                            )
                        ).status_code,
                        403,
                    )
                finally:
                    api.app.dependency_overrides.pop(current_user, None)
                self.runtime.lose_create_reply = True
                response = await client.put(
                    "/main-thread/provider", json={"provider": "codex"}
                )
                self.assertEqual(response.status_code, 200)
                self.assertEqual(response.json()["status"], "failed")
                self.assertIsNone(response.json()["effective_provider"])
                self.assertEqual(
                    (
                        await client.post("/chat", json={"message": "pending chat"})
                    ).status_code,
                    409,
                )
                response = await client.put(
                    "/main-thread/provider", json={"provider": "codex"}
                )
                self.assertEqual(response.json()["effective_provider"], "codex")
                identity = (await client.get("/main-thread")).json()
                self.assertEqual(identity["native"]["kind"], "codex")
                self.assertEqual(
                    identity["native"]["kagent_session_id"],
                    response.json()["kagent_session_id"],
                )

    async def test_retired_native_is_protected_from_orphan_sweep(self):
        from mainloop.runtime.orphan_sessions import _bound_ids

        await self.select()
        ids, unresolved = await _bound_ids()
        self.assertIn(self.old_id, ids)
        self.assertEqual(unresolved, 0)

    async def test_first_use_default_and_race_with_selection_have_one_main(self):
        another = self.user + "-first-use"
        with patch.object(settings, "owner_id", another), patch.object(
            settings, "main_provider", "codex"
        ):
            values = await asyncio.gather(
                ensure_main_session(another),
                ensure_main_session(another),
                main_provider.select(another, "codex"),
            )
        self.assertEqual(values[0]["session_id"], values[1]["session_id"])
        self.assertEqual(values[0]["session_id"], values[2].session_id)
        self.assertEqual(values[0]["kind"], "codex")
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_bindings WHERE main_owner_id=$1", another
            ),
            1,
        )

    async def test_first_use_preserves_main_history_and_excludes_workspace_conversations(
        self,
    ):
        another = self.user + "-conversation"
        conversation = await db.create_conversation(another)
        _, workspace_cid = await self.session(user=another)
        binding = await ensure_main_session(another)
        selected = await self.pool.fetchval(
            "SELECT conversation_id FROM sessions WHERE id=$1", binding["session_id"]
        )
        self.assertEqual(selected, conversation.id)
        self.assertNotEqual(selected, workspace_cid)


class MainProviderTaskTests(KagentFakeCase):
    reuse_database = True

    # Borrow the qualified fixture setup without inheriting its test methods.
    asyncSetUp = task_fixtures.TaskReportsPostgresTests.asyncSetUp
    asyncTearDown = task_fixtures.TaskReportsPostgresTests.asyncTearDown
    request = task_fixtures.TaskReportsPostgresTests.request
    create_task = task_fixtures.TaskReportsPostgresTests.create_task
    tree = task_fixtures.TaskReportsPostgresTests.tree
    report = task_fixtures.TaskReportsPostgresTests.report
    record = task_fixtures.TaskReportsPostgresTests.record

    async def test_code_tree_and_root_capacity_survive_switch_and_report_reaches_codex(
        self,
    ):
        from fastapi import HTTPException
        from mainloop.tasks import reports

        agent, old_main, parent_ctx, child_ctx, parent, child = await self.tree(
            mode="code"
        )
        sid = old_main.binding["session_id"]
        original_native = await ns._ensure_kagent_session(await ns.get_binding(sid))
        cid = await self.pool.fetchval(
            "SELECT conversation_id FROM sessions WHERE id=$1", sid
        )
        before = {}
        # Read every column, including snapshots, immutable source, generations,
        # parent and capacity fields, with fixed SQL for each stored entity.
        queries = {
            "tasks": "SELECT row_to_json(t)::text AS row FROM tasks t ORDER BY row",
            "task_attempts": "SELECT row_to_json(t)::text AS row FROM task_attempts t ORDER BY row",
            "task_operations": "SELECT row_to_json(t)::text AS row FROM task_operations t ORDER BY row",
            "workspace_writer_claims": "SELECT row_to_json(t)::text AS row FROM workspace_writer_claims t ORDER BY row",
            "workspaces": "SELECT row_to_json(t)::text AS row FROM workspaces t ORDER BY row",
        }
        for table, query in queries.items():
            before[table] = await self.pool.fetch(query)
        native_before = await self.pool.fetch(
            "SELECT * FROM native_bindings WHERE session_id<>$1 ORDER BY session_id",
            sid,
        )
        with patch.object(settings, "owner_id", self.user):
            switched = await main_provider.select(self.user, "codex")
        self.assertEqual(switched.status, "ready")
        self.assertEqual(switched.history[0].kagent_session_id, original_native.id)
        self.assertTrue(self.fake.session_calls("SuspendSession"))
        for table, rows in before.items():
            self.assertEqual(await self.pool.fetch(queries[table]), rows)
        self.assertEqual(
            await self.pool.fetch(
                "SELECT * FROM native_bindings WHERE session_id<>$1 ORDER BY session_id",
                sid,
            ),
            native_before,
        )
        with self.assertRaises(HTTPException):
            await agent.authenticate(token_for(sid))
        binding = await ns.get_binding(sid)
        main = await agent.authenticate(token_for(binding["credential_identity"]))
        read = await invoke(agent, main, "task_get", {"task_id": child.task_id})
        self.assertFalse(read.isError, read.content)
        with patch.object(settings, "task_max_children_per_parent", 1):
            rejected = await invoke(
                agent,
                main,
                "delegate",
                self.request(mode="coordination").model_dump(mode="json"),
            )
        self.assertTrue(rejected.isError)
        self.assertIn("parent_capacity", rejected.content[0].text)
        result = await self.record(agent, child_ctx, self.report(child))
        await reports.dispatch_pending(db)
        await reports.dispatch_pending(db)
        rows = await self.pool.fetch(
            "SELECT n.* FROM task_event_deliveries d JOIN native_deliveries n ON n.message_id=d.delivery_id WHERE event_id=$1",
            result["event_id"],
        )
        self.assertEqual({r["session_id"] for r in rows}, {sid, parent.binding_id})
        main_report = next(r for r in rows if r["session_id"] == sid)
        self.assertEqual(main_report["state"], "queued")
        mid, text = await ns.ledger.promote_queued(sid)
        self.assertEqual(mid, main_report["message_id"])
        await ns._deliver(sid, mid, text)
        self.assertEqual(await ns.ledger.delivery_state(mid), "completed")
        sends = self.fake.rpc_calls("SendStreamingMessage")
        self.assertEqual(len(sends), 1)
        self.assertEqual(
            sends[0]["params"]["message"]["contextId"], switched.kagent_session_id
        )
        self.assertTrue(
            any(
                path.endswith("/mainloop-main-codex")
                for _, path, body in self.fake.requests
                if isinstance(body, dict)
                and body.get("method") == "SendStreamingMessage"
            )
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT conversation_id FROM sessions WHERE id=$1", sid
            ),
            cid,
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM task_attempts WHERE capacity_held"
            ),
            2,
        )
