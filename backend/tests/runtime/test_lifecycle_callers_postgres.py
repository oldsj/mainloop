"""Actual final resume, preview and native-send boundaries against owned PostgreSQL."""

import asyncio
import json
import unittest
from contextlib import asynccontextmanager
from dataclasses import replace
from unittest.mock import AsyncMock, patch

import asyncpg
from mainloop.config import settings
from mainloop.db import db
from mainloop.runtime import native_sessions as ns
from mainloop.runtime import preview_proxy, workspaces
from mainloop.runtime.kagent_client import RuntimeState, StreamEvent
from mainloop.tasks import lifecycle
from tests.runtime import test_git_credentials_postgres as fixtures


class NestedAcquisition(RuntimeError):
    """Deterministic failure before waiting for our own single pooled connection."""


class LifecycleCallerTests(fixtures.GitCredentialsCase):
    async def single_pool(self):
        await self.pool.close()
        self.pool = await asyncpg.create_pool(self.url, min_size=1, max_size=1)
        db._pool = self.pool
        self.native.pool = self.pool

    @asynccontextmanager
    async def traced_connection(self):
        task = asyncio.current_task()
        if task in self.held:
            conn = self.held[task]
            locks = await conn.fetch(
                "SELECT classid,objid,granted FROM pg_locks WHERE pid=pg_backend_pid() AND locktype='advisory'"
            )
            self.trace.append(
                {
                    "nested": True,
                    "pid": await conn.fetchval("SELECT pg_backend_pid()"),
                    "locks": [dict(r) for r in locks],
                }
            )
            print("CALLER_ACQUISITION " + json.dumps(self.trace[-1]))
            raise NestedAcquisition("pool reacquisition beneath held advisory locks")
        async with self.pool.acquire() as conn:
            self.held[task] = conn
            self.trace.append(
                {"nested": False, "pid": await conn.fetchval("SELECT pg_backend_pid()")}
            )
            try:
                yield conn
            finally:
                locks = await conn.fetchval(
                    "SELECT count(*) FROM pg_locks WHERE pid=pg_backend_pid() AND locktype='advisory'"
                )
                self.assertEqual(locks, 0)
                self.held.pop(task)

    async def caller(self, sid, which):
        if which == "resume":
            return await workspaces._resume_if_suspended(
                {"session_id": sid, "kagent_session_id": self.runtime}
            )
        with patch.object(preview_proxy, "current_user", return_value=self.user):
            async with preview_proxy._router_admission(
                preview_proxy.PreviewHost(5173, sid)
            ) as target:
                self.assertEqual(
                    target.actor, preview_proxy.session_actor(self.runtime)
                )
                return target

    async def exercise_caller(self, which, enabled):
        with patch.object(settings, "git_transport_enabled", enabled), patch.object(
            settings, "push_gate_enabled", enabled
        ):
            sid = await self.create("feature/" + which)
        binding = await ns.get_binding(sid)
        self.runtime = binding["kagent_session_id"]
        await self.pool.execute(
            "UPDATE workspaces SET ports=$2::jsonb WHERE session_id=$1",
            sid,
            '[{"number":5173,"name":"web"}]',
        )
        self.native.sessions[self.runtime] = replace(
            self.native.sessions[self.runtime], state=RuntimeState.SUSPENDED
        )
        await self.single_pool()
        self.held, self.trace = {}, []
        get, resume = self.native.get_session, self.native.resume_session

        async def committed_get(runtime):
            self.assertFalse(self.held[asyncio.current_task()].is_in_transaction())
            return await get(runtime)

        async def committed_resume(runtime):
            self.assertFalse(self.held[asyncio.current_task()].is_in_transaction())
            return await resume(runtime)

        with patch.object(settings, "git_transport_enabled", enabled), patch.object(
            settings, "push_gate_enabled", enabled
        ), patch.object(db, "connection", self.traced_connection), patch.object(
            self.native, "get_session", committed_get
        ), patch.object(
            self.native, "resume_session", committed_resume
        ):
            result = await asyncio.wait_for(self.caller(sid, which), 5)
        if which == "resume":
            self.assertTrue(result[1])
            self.assertEqual(result[0].state, RuntimeState.READY)
        self.assertEqual(len(self.trace), 1)
        self.assertFalse(self.held)

    async def test_actual_resume_single_connection_flags_off(self):
        await self.exercise_caller("resume", False)

    async def test_actual_resume_single_connection_flags_on(self):
        await self.exercise_caller("resume", True)

    async def test_actual_preview_single_connection_flags_off(self):
        await self.exercise_caller("preview", False)

    async def test_actual_preview_single_connection_flags_on(self):
        await self.exercise_caller("preview", True)

    async def test_ready_then_committed_owner_cancel_before_final_send(self):
        sid = await self.create()
        binding = await ns.get_binding(sid)
        cid = await self.pool.fetchval(
            "SELECT conversation_id FROM sessions WHERE id=$1", sid
        )
        mid = await self.delivery(sid, cid, "recorded")
        entered, release = asyncio.Event(), asyncio.Event()
        emitted, denials = [], []
        original = ns._guarded_send

        async def send(*args, **kwargs):
            emitted.append("native turn bytes")
            yield StreamEvent()

        async def paused(*args, **kwargs):
            entered.set()
            await release.wait()
            try:
                async for event in original(*args, **kwargs):
                    yield event
            except lifecycle.LifecycleDenied as exc:
                denials.append(exc.code)
                raise

        with patch.object(self.native, "send_message", send, create=True), patch.object(
            self.native,
            "find_task_for_message",
            AsyncMock(return_value=None),
            create=True,
        ), patch.object(ns, "_guarded_send", paused), patch.object(
            ns, "_with_standing", AsyncMock(return_value=("fixture prompt", None))
        ), patch.object(
            ns, "_after", AsyncMock()
        ):
            delivery = asyncio.create_task(ns._deliver(sid, mid, "fixture prompt"))
            try:
                await asyncio.wait_for(entered.wait(), 5)
                self.assertEqual(await self.state_of(mid), "sending")
                self.assertEqual(
                    await self.pool.fetchval(
                        "SELECT warmup_state FROM git_enrollments WHERE binding_id=$1",
                        sid,
                    ),
                    "complete",
                )
                result = await ns.cancel(sid)
                self.assertEqual(result, "unknown")
                row = await self.pool.fetchrow(
                    "SELECT s.status,b.token_hash,e.revoked_at FROM sessions s JOIN native_bindings b ON b.session_id=s.id JOIN git_enrollments e ON e.binding_id=s.id WHERE s.id=$1",
                    sid,
                )
                self.assertEqual(row["status"], "cancelled")
                self.assertIsNone(row["token_hash"])
                self.assertIsNotNone(row["revoked_at"])
                self.assertEqual(await self.state_of(mid), "failed")
                self.assertEqual(
                    self.native.sessions[binding["kagent_session_id"]].state,
                    RuntimeState.READY,
                )
            finally:
                release.set()
                await asyncio.wait_for(delivery, 5)
        print(
            "CANCEL_FINAL_SEND "
            + json.dumps(
                {
                    "emitted": emitted,
                    "delivery_state": await self.state_of(mid),
                    "readiness_complete": True,
                    "committed_revoked": True,
                }
            )
        )
        self.assertFalse(emitted)
        self.assertEqual(denials, ["session_terminal"])

    async def exercise_waiter(self, which):
        sid = await self.create("feature/holder")
        waiter_sid = await self.create("feature/waiter")
        self.runtime = (await ns.get_binding(sid))["kagent_session_id"]
        await self.pool.execute(
            "UPDATE workspaces SET ports=$2::jsonb WHERE session_id=$1",
            sid,
            '[{"number":5173,"name":"web"}]',
        )
        await self.single_pool()
        self.held, self.trace = {}, []
        entered, release, admitted = asyncio.Event(), asyncio.Event(), asyncio.Event()
        peer = await asyncpg.connect(self.url)
        observer = await asyncpg.connect(self.url)
        peer_pid = await peer.fetchval("SELECT pg_backend_pid()")
        original_get = self.native.get_session
        original_resolve = preview_proxy._resolve_target

        async def paused_get(runtime):
            self.assertFalse(self.held[asyncio.current_task()].is_in_transaction())
            entered.set()
            await release.wait()
            return await original_get(runtime)

        async def paused_resolve(*args, **kwargs):
            target = await original_resolve(*args, **kwargs)
            self.assertFalse(kwargs["conn"].is_in_transaction())
            entered.set()
            await release.wait()
            return target

        async def waiter():
            async with lifecycle.guard(waiter_sid, "preview", conn=peer):
                admitted.set()

        async def blocked_policy():
            while True:
                row = await observer.fetchrow(
                    "SELECT w.classid,w.objid FROM pg_locks w JOIN pg_locks h ON h.locktype=w.locktype AND h.classid=w.classid AND h.objid=w.objid WHERE w.pid=$1 AND w.locktype='advisory' AND NOT w.granted AND h.granted AND h.pid=$2",
                    peer_pid,
                    self.trace[0]["pid"],
                )
                if row:
                    return dict(row)
                await asyncio.sleep(0)

        holder = waiting = None
        try:
            with patch.object(db, "connection", self.traced_connection), patch.object(
                self.native,
                "get_session",
                paused_get if which == "resume" else original_get,
            ), patch.object(
                preview_proxy,
                "_resolve_target",
                paused_resolve if which == "preview" else original_resolve,
            ):
                holder = asyncio.create_task(self.caller(sid, which))
                await asyncio.wait_for(entered.wait(), 5)
                waiting = asyncio.create_task(waiter())
                lock = await asyncio.wait_for(blocked_policy(), 5)
                policy_hash = await observer.fetchval(
                    "SELECT hashtextextended($1,0)", "push-policy:" + self.pid
                )
                self.assertEqual(
                    lock,
                    {
                        "classid": (policy_hash >> 32) & 0xFFFFFFFF,
                        "objid": policy_hash & 0xFFFFFFFF,
                    },
                )
                self.assertFalse(admitted.is_set())
                holder_locks = await observer.fetchval(
                    "SELECT count(*) FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND granted",
                    self.trace[0]["pid"],
                )
                self.assertEqual(holder_locks, 4)
                print(
                    "SAME_PROJECT_WAITER "
                    + json.dumps(
                        {
                            "caller": which,
                            "holder_pid": self.trace[0]["pid"],
                            "waiter_pid": peer_pid,
                            "blocked_lock": lock,
                            "holder_locks": holder_locks,
                        }
                    )
                )
                release.set()
                await asyncio.wait_for(asyncio.gather(holder, waiting), 5)
                self.assertTrue(admitted.is_set())
                self.assertEqual(len(self.trace), 1)
                self.assertEqual(
                    await peer.fetchval(
                        "SELECT count(*) FROM pg_locks WHERE pid=pg_backend_pid() AND locktype='advisory'"
                    ),
                    0,
                )
        finally:
            release.set()
            for task in (holder, waiting):
                if task and not task.done():
                    task.cancel()
            await asyncio.gather(
                *(t for t in (holder, waiting) if t), return_exceptions=True
            )
            await peer.close()
            await observer.close()

    async def test_resume_same_project_waiter_releases_locks(self):
        await self.exercise_waiter("resume")

    async def test_preview_same_project_waiter_releases_locks(self):
        await self.exercise_waiter("preview")

    async def final_send(self, binding, *, denied=None):
        emitted = []

        async def send(*args, **kwargs):
            emitted.append("native turn bytes")
            yield "fixture receipt"

        with patch.object(self.native, "send_message", send, create=True):
            events = ns._guarded_send(
                binding,
                await ns.binding_agent_ref(binding),
                context_id=binding["kagent_session_id"],
            )
            try:
                if denied:
                    with self.assertRaisesRegex(
                        (ValueError, lifecycle.LifecycleDenied), denied
                    ):
                        await anext(events)
                    self.assertFalse(emitted)
                else:
                    self.assertEqual(await anext(events), "fixture receipt")
                    self.assertEqual(emitted, ["native turn bytes"])
            finally:
                await events.aclose()

    async def test_revoked_original_plan_denies_even_when_flags_are_disabled(self):
        from mainloop.push_gate import credentials

        sid = await self.create()
        binding = await ns.get_binding(sid)
        async with self.pool.acquire() as conn, credentials.locked(
            conn, sid
        ), conn.transaction():
            await credentials.revoke_deferred(conn, sid)
        # Keep the owner/MCP principal live to exercise the enrollment tombstone itself.
        self.assertIsNotNone((await ns.get_binding(sid))["token_hash"])
        for enabled in (True, False):
            with self.subTest(enabled=enabled), patch.object(
                settings, "git_transport_enabled", enabled
            ), patch.object(settings, "push_gate_enabled", enabled):
                await self.final_send(binding, denied="git_enrollment_revoked")

    async def test_missing_original_plan_cannot_select_another_create_issuance(self):
        sid = await self.create()
        binding = await ns.get_binding(sid)
        await self.pool.execute(
            "UPDATE native_bindings SET kagent_request_id=$2 WHERE session_id=$1",
            sid,
            "00000000-0000-0000-0000-000000000001",
        )
        current = await ns.get_binding(sid)
        with patch.object(settings, "git_transport_enabled", False):
            await self.final_send(current, denied="git_plan_missing")
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT create_request_id FROM git_enrollments WHERE binding_id=$1", sid
            ),
            ns._request_id(binding),
        )

    async def test_enabled_workspace_without_required_plan_denies_before_get(self):
        with patch.object(settings, "git_transport_enabled", False), patch.object(
            settings, "push_gate_enabled", False
        ):
            sid = await self.create()
        binding = await ns.get_binding(sid)
        gets = len(self.native.gets)
        await self.final_send(binding, denied="git_plan_missing")
        self.assertEqual(len(self.native.gets), gets)

    async def test_current_enrolled_owner_first_send_waits_for_preparation(self):
        sid = await self.create()
        await self.final_send(await ns.get_binding(sid), denied="git_prepare_pending")

    async def test_live_ordinary_no_plan_send_remains_usable(self):
        with patch.object(settings, "git_transport_enabled", False), patch.object(
            settings, "push_gate_enabled", False
        ):
            sid = await self.create()
            await self.final_send(await ns.get_binding(sid))
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM git_enrollments WHERE binding_id=$1", sid
            ),
            0,
        )

    async def test_ordinary_nonworkspace_send_with_git_enabled(self):
        sid, _ = await self.bound_session()
        await ns.ledger.update_binding(sid, kagent_session_id="ordinary-runtime")
        await self.final_send(await ns.get_binding(sid))
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM git_enrollments WHERE binding_id=$1", sid
            ),
            0,
        )

    async def test_preview_wrong_owner_and_undeclared_port_release_locks(self):
        sid = await self.create()
        await self.single_pool()
        self.held, self.trace = {}, []
        for user in (self.user, "another-owner"):
            with self.subTest(user=user), patch.object(
                preview_proxy, "current_user", return_value=user
            ), patch.object(db, "connection", self.traced_connection):
                with self.assertRaisesRegex(
                    lifecycle.LifecycleDenied, "preview_target_unavailable"
                ):
                    async with preview_proxy._router_admission(
                        preview_proxy.PreviewHost(1, sid)
                    ):
                        self.fail("invalid preview admitted")
        self.assertFalse(self.held)
        self.assertEqual(len(self.trace), 2)

    async def test_final_send_rechecks_changed_runtime_binding(self):
        sid = await self.create()
        binding = await ns.get_binding(sid)
        await self.pool.execute(
            "UPDATE native_bindings SET kagent_session_id=$2 WHERE session_id=$1",
            sid,
            "new-runtime",
        )
        await self.final_send(binding, denied="binding_changed")

    async def test_final_send_rechecks_ordinary_terminal_and_revoked_authority(self):
        with patch.object(settings, "git_transport_enabled", False), patch.object(
            settings, "push_gate_enabled", False
        ):
            sid = await self.create()
            binding = await ns.get_binding(sid)
            for sql, code in (
                (
                    "UPDATE sessions SET status='cancelled' WHERE id=$1",
                    "session_terminal",
                ),
                (
                    "UPDATE native_bindings SET token_hash=NULL WHERE session_id=$1",
                    "binding_revoked",
                ),
            ):
                async with self.pool.acquire() as conn:
                    tx = conn.transaction()
                    await tx.start()
                    try:
                        await conn.execute(sql, sid)
                        # The final guard uses a peer connection, so commit actual denial first.
                        await tx.commit()
                        await self.final_send(binding, denied=code)
                    finally:
                        await conn.execute(
                            "UPDATE sessions SET status='waiting_on_user' WHERE id=$1",
                            sid,
                        )


class DelegatedCallerTests(fixtures.GitTaskCredentialTests):
    async def test_current_delegated_send_and_preview_with_one_connection(self):
        _, _, parent = await self.task()
        _, _, child = await self.task(parent=parent)
        sid = child.session_id
        binding = await ns.get_binding(sid)
        await self.pool.execute(
            "UPDATE workspaces SET ports=$2::jsonb WHERE session_id=$1",
            sid,
            '[{"number":5173,"name":"web"}]',
        )
        await self.pool.close()
        self.pool = await asyncpg.create_pool(self.url, min_size=1, max_size=1)
        db._pool = self.pool
        self.native.pool = self.pool
        emitted = []

        async def send(*args, **kwargs):
            emitted.append("native turn bytes")
            yield "fixture receipt"

        with patch.object(self.native, "send_message", send, create=True):
            events = ns._guarded_send(binding, await ns.binding_agent_ref(binding))
            try:
                self.assertEqual(
                    await asyncio.wait_for(anext(events), 5), "fixture receipt"
                )
            finally:
                await events.aclose()
        self.assertEqual(emitted, ["native turn bytes"])
        with patch.object(preview_proxy, "current_user", return_value=self.user):
            async with preview_proxy._router_admission(
                preview_proxy.PreviewHost(5173, sid)
            ) as target:
                self.assertEqual(target.workspace_id, sid)
        await self.pool.execute(
            "UPDATE task_attempts SET state='draining' WHERE id=$1", parent.id
        )
        emitted.clear()
        with patch.object(self.native, "send_message", send, create=True):
            events = ns._guarded_send(binding, await ns.binding_agent_ref(binding))
            try:
                with self.assertRaises(lifecycle.LifecycleDenied):
                    await anext(events)
            finally:
                await events.aclose()
        self.assertFalse(emitted)
        with self.assertRaises(lifecycle.LifecycleDenied):
            await workspaces._resume_if_suspended(binding)
        with patch.object(preview_proxy, "current_user", return_value=self.user):
            with self.assertRaises(lifecycle.LifecycleDenied):
                async with preview_proxy._router_admission(
                    preview_proxy.PreviewHost(5173, sid)
                ):
                    self.fail("dead parent preview admitted")


def load_tests(loader, tests, pattern):
    """Reuse the task setup without rerunning its original module's test methods.

    The original credential/task regressions run from their unchanged module.
    Only tests defined by these two new classes belong to this caller suite.
    """
    return unittest.TestSuite(
        cls(name)
        for cls in (LifecycleCallerTests, DelegatedCallerTests)
        for name in sorted(cls.__dict__)
        if name.startswith("test_")
    )
