"""The slice's raw SQL against a real PostgreSQL (asyncpg).

Opt-in: skipped unless ``MAINLOOP_TEST_DATABASE_URL`` is set. The URL needs a role that can
``CREATE DATABASE``; the module creates a scratch database from it, applies the schema, and drops
the database afterwards, so nothing in the target server's existing databases is touched.

    MAINLOOP_TEST_DATABASE_URL=postgresql://user:pass@host:5432/postgres \
        uv run python -m unittest tests.runtime.test_postgres_ledger

The kagent gateway and the Substrate provisioner are faked; only the ledger, binding, delegation,
workspace and reconcile SQL runs for real. Kubernetes credential deletion is faked.
"""

from __future__ import annotations

import asyncio
import json
import os
import unittest
import uuid
from datetime import UTC, datetime, timedelta
from unittest.mock import AsyncMock, patch
from urllib.parse import urlsplit, urlunsplit

import asyncpg
from fastapi import HTTPException
from mainloop.config import settings
from mainloop.db import db
from mainloop.db.postgres import MIGRATION_SQL, SCHEMA_SQL
from mainloop.runtime import native_sessions as ns
from mainloop.runtime import workspace_adapter, workspace_api
from mainloop.runtime.actor_provisioner import (
    FakeActorProvisioner,
    set_actor_provisioner,
)
from mainloop.runtime.delegation import (
    INBOX,
    PgStore,
    ensure_main_session,
    render_for_binding,
)

from models import SessionStatus, WorkspaceAgentKind, WorkspaceDev, WorkspaceManifest

TEST_URL = os.environ.get("MAINLOOP_TEST_DATABASE_URL")


def _with_database(url: str, database: str) -> str:
    parts = urlsplit(url)
    return urlunsplit(parts._replace(path=f"/{database}"))


async def _admin(url: str, *statements: str) -> None:
    conn = await asyncpg.connect(url)
    try:
        for statement in statements:
            await conn.execute(statement)
    finally:
        await conn.close()


async def _init_schema(url: str) -> None:
    conn = await asyncpg.connect(url)
    try:
        await conn.execute(SCHEMA_SQL)
        await conn.execute(MIGRATION_SQL)
    finally:
        await conn.close()


@unittest.skipUnless(TEST_URL, "set MAINLOOP_TEST_DATABASE_URL to run Postgres tests")
class PostgresTestCase(unittest.IsolatedAsyncioTestCase):
    """A scratch database per test class and a pool wired into the global ``db`` per test."""

    @classmethod
    def setUpClass(cls):
        if not TEST_URL:
            raise unittest.SkipTest("MAINLOOP_TEST_DATABASE_URL is not set")
        cls.database = f"mainloop_test_{uuid.uuid4().hex[:12]}"
        cls.url = _with_database(TEST_URL, cls.database)
        asyncio.run(_admin(TEST_URL, f'CREATE DATABASE "{cls.database}"'))
        asyncio.run(_init_schema(cls.url))

    @classmethod
    def tearDownClass(cls):
        asyncio.run(
            _admin(TEST_URL, f'DROP DATABASE IF EXISTS "{cls.database}" WITH (FORCE)')
        )

    async def asyncSetUp(self):
        self.pool = await asyncpg.create_pool(self.url, min_size=1, max_size=6)
        self._saved_pool = db._pool
        db._pool = self.pool
        self.user = f"user-{uuid.uuid4().hex[:8]}"
        patcher = patch.object(settings, "agent_token_key", "integration-test-key")
        patcher.start()
        self.addCleanup(patcher.stop)
        credentials = patch(
            "mainloop.runtime.agent_credentials.credentials.remove", new=AsyncMock()
        )
        credentials.start()
        self.addCleanup(credentials.stop)

    async def asyncTearDown(self):
        db._pool = self._saved_pool
        await self.pool.close()

    # -- seeding ------------------------------------------------------------------------------

    async def thread(self) -> str:
        thread_id = f"mt-{self.user}"
        await self.pool.execute(
            "INSERT INTO main_threads (id, user_id) VALUES ($1,$2) ON CONFLICT DO NOTHING",
            thread_id,
            self.user,
        )
        return thread_id

    async def session(
        self, status: str = "waiting_on_user", *, user: str | None = None
    ) -> tuple[str, str]:
        """Insert a conversation and a session; return ``(session_id, conversation_id)``."""
        user = user or self.user
        thread_id = await self.thread()
        sid, cid = str(uuid.uuid4()), str(uuid.uuid4())
        await self.pool.execute(
            "INSERT INTO conversations (id, user_id, title) VALUES ($1,$2,'t')",
            cid,
            user,
        )
        await self.pool.execute(
            """INSERT INTO sessions (id,user_id,main_thread_id,title,description,prompt,
                                     conversation_id,status)
               VALUES ($1,$2,$3,'title','d','p',$4,$5)""",
            sid,
            user,
            thread_id,
            cid,
            status,
        )
        return sid, cid

    async def bound_session(
        self, kind: str = "claude", status: str = "waiting_on_user", **binding
    ) -> tuple[str, str]:
        sid, cid = await self.session(status)
        await ns.create_binding(sid, kind, **binding)
        return sid, cid

    async def delivery(
        self, sid: str, cid: str, state: str, *, source: str = "user", text: str = "hi"
    ) -> str:
        message_id = str(uuid.uuid4())
        await self.pool.execute(
            "INSERT INTO messages (id, conversation_id, role, content) VALUES ($1,$2,'user',$3)",
            message_id,
            cid,
            text,
        )
        await self.pool.execute(
            "INSERT INTO native_deliveries (message_id, session_id, state, source) VALUES ($1,$2,$3,$4)",
            message_id,
            sid,
            state,
            source,
        )
        return message_id

    async def state_of(self, message_id: str) -> str:
        return await self.pool.fetchval(
            "SELECT state FROM native_deliveries WHERE message_id=$1", message_id
        )


class SchemaTests(PostgresTestCase):
    async def test_native_tables_have_the_kagent_shape(self):
        for table, expected in {
            "native_bindings": {
                "session_id",
                "kind",
                "kagent_session_id",
                "kagent_request_id",
                "model",
                "role",
                "parent_session_id",
                "topic_id",
                "token_hash",
                "standing_hash",
                "turns",
                "reported_at",
            },
            "native_deliveries": {
                "message_id",
                "session_id",
                "state",
                "task_id",
                "evidence_ref",
                "detail",
                "source",
            },
        }.items():
            columns = {
                r["column_name"]
                for r in await self.pool.fetch(
                    "SELECT column_name FROM information_schema.columns WHERE table_name=$1",
                    table,
                )
            }
            self.assertTrue(expected <= columns, (table, expected - columns))
            self.assertFalse(
                columns
                & {
                    "agent_name",
                    "native_session_id",
                    "approval_policy",
                    "generation",
                    "journal_cursor",
                    "lineage_seq",
                    "turns_in_lineage",
                    "cursor_before",
                },
                table,
            )
        for table in ("native_events", "native_lineage"):
            self.assertIsNone(await self.pool.fetchval("SELECT to_regclass($1)", table))

    async def test_init_is_idempotent(self):
        await _init_schema(self.url)
        await _init_schema(self.url)

    async def test_upgrade_from_the_substrate_shape_keeps_rows(self):
        """The schema main shipped before kagent: its columns and tables are dropped, rows stay."""
        sid, cid = await self.session("waiting_on_user")
        message_id = await self.delivery(sid, cid, "completed")
        legacy = """
            ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS agent_name TEXT NOT NULL DEFAULT 'a';
            ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS native_session_id TEXT;
            ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS approval_policy TEXT NOT NULL DEFAULT 'never';
            ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS generation INTEGER NOT NULL DEFAULT 1;
            ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS journal_cursor INTEGER NOT NULL DEFAULT 0;
            ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS journal_ref TEXT;
            ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS lineage_seq INTEGER NOT NULL DEFAULT 1;
            ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS context_tokens INTEGER;
            ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS baseline_tokens INTEGER;
            ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS turns_in_lineage INTEGER NOT NULL DEFAULT 0;
            ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS continuations INTEGER NOT NULL DEFAULT 0;
            ALTER TABLE native_deliveries ADD COLUMN IF NOT EXISTS cursor_before INTEGER;
            ALTER TABLE native_deliveries ADD COLUMN IF NOT EXISTS generation INTEGER NOT NULL DEFAULT 1;
            CREATE TABLE IF NOT EXISTS native_lineage (
                session_id TEXT NOT NULL REFERENCES sessions(id), seq INTEGER NOT NULL,
                native_session_id TEXT NOT NULL, started_reason TEXT NOT NULL,
                PRIMARY KEY (session_id, seq));
            CREATE TABLE IF NOT EXISTS native_events (
                id TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES sessions(id),
                kind TEXT NOT NULL, evidence_ref TEXT NOT NULL);
        """
        await self.pool.execute(legacy)
        await self.pool.execute(
            "INSERT INTO native_bindings (session_id, kind, role, turns_in_lineage, lineage_seq)"
            " VALUES ($1,'claude','main',7,2)",
            sid,
        )
        await self.pool.execute(
            "INSERT INTO native_lineage VALUES ($1,1,'n','start')", sid
        )
        await self.pool.execute(
            "INSERT INTO native_events VALUES ('e',$1,'continuation','r')", sid
        )
        await _init_schema(self.url)
        await _init_schema(self.url)
        binding = await ns.get_binding(sid)
        self.assertEqual((binding["kind"], binding["role"]), ("claude", "main"))
        self.assertIsNone(binding["kagent_session_id"])
        self.assertEqual(binding["turns"], 0)
        self.assertEqual(await self.state_of(message_id), "completed")
        self.assertNotIn("lineage_seq", binding)
        self.assertIsNone(
            await self.pool.fetchval("SELECT to_regclass('native_events')")
        )

    async def test_upgrade_settles_open_substrate_deliveries_once(self):
        """A delivery open at the cutover has no kagent task and its binding has no kagent
        Session, so nothing could ever resolve it and it would block the session for good.
        """
        sid, cid = await self.session("active")
        await self.pool.execute(
            "ALTER TABLE native_deliveries ADD COLUMN IF NOT EXISTS cursor_before INTEGER"
        )
        await self.pool.execute(
            "INSERT INTO native_bindings (session_id, kind, role) VALUES ($1,'claude','main')",
            sid,
        )
        ids = {
            state: await self.delivery(sid, cid, state)
            for state in ("recorded", "sending", "delivered", "queued", "completed")
        }
        self.assertEqual(await ns.ledger.open_count(sid), 3)

        await _init_schema(self.url)

        self.assertEqual(await ns.ledger.open_count(sid), 0)
        self.assertEqual(
            {state: await self.state_of(mid) for state, mid in ids.items()},
            {
                "recorded": "uncertain",
                "sending": "uncertain",
                "delivered": "uncertain",
                "queued": "queued",
                "completed": "completed",
            },
        )
        row = await self.pool.fetchrow(
            "SELECT detail FROM native_deliveries WHERE message_id=$1", ids["sending"]
        )
        self.assertIn("not replayed", row["detail"])

        # Only the cutover settles them: work recorded afterwards is left alone on a restart.
        fresh = await self.delivery(sid, cid, "sending")
        await _init_schema(self.url)
        self.assertEqual(await self.state_of(fresh), "sending")


class BindingTests(PostgresTestCase):
    async def test_create_lookup_and_update(self):
        sid, _ = await self.session()
        topic = await PgStore().topic(self.user, "alpha", create=True)
        binding = await ns.create_binding(
            sid, "codex", role="child", parent_session_id="p", topic_id=topic["id"]
        )
        self.assertEqual(binding["kind"], "codex")
        self.assertEqual(binding["role"], "child")
        self.assertEqual(binding["turns"], 0)
        self.assertIsNone(binding["kagent_session_id"])
        self.assertIsNone(binding["reported_at"])
        self.assertTrue(binding["token_hash"])

        await ns.ledger.update_binding(
            sid, kagent_session_id="ctx-1", standing_hash="h", model="gpt"
        )
        await ns.ledger.bump_turns(sid)
        await ns.ledger.bump_turns(sid)
        after = await ns.get_binding(sid)
        self.assertEqual(after["kagent_session_id"], "ctx-1")
        self.assertEqual(after["standing_hash"], "h")
        self.assertEqual((after["model"], after["turns"]), ("gpt", 2))
        self.assertGreater(after["updated_at"], binding["updated_at"])

        await ns.ledger.update_binding(sid)  # no fields: nothing to do
        self.assertIsNone(await ns.get_binding("missing"))

    async def test_create_binding_on_a_caller_connection_joins_its_transaction(self):
        sid, _ = await self.session()
        async with db.connection() as conn:
            with self.assertRaises(RuntimeError):
                async with conn.transaction():
                    await ns.create_binding(sid, "claude", conn=conn)
                    raise RuntimeError("roll back")
        self.assertIsNone(await ns.get_binding(sid))

    async def test_replace_kagent_session_is_compare_and_set_and_settles_open_turns(
        self,
    ):
        sid, cid = await self.bound_session(role="main")
        await ns.ledger.update_binding(
            sid, kagent_session_id="old-session", standing_hash="h"
        )
        ids = {
            s: await self.delivery(sid, cid, s)
            for s in ("recorded", "sending", "delivered", "queued", "completed")
        }
        self.assertFalse(
            await ns.ledger.replace_kagent_session(sid, "someone-else", "req-x")
        )
        self.assertTrue(
            await ns.ledger.replace_kagent_session(sid, "old-session", "req-1")
        )
        binding = await ns.get_binding(sid)
        self.assertEqual(
            (
                binding["kagent_session_id"],
                binding["kagent_request_id"],
                binding["standing_hash"],
            ),
            (None, "req-1", None),
        )
        states = {s: await self.state_of(m) for s, m in ids.items()}
        self.assertEqual(
            states,
            {
                "recorded": "recorded",  # never sent: still goes to the new Session
                "sending": "uncertain",
                "delivered": "uncertain",
                "queued": "queued",
                "completed": "completed",
            },
        )
        # A second replacement from the same stale view loses.
        self.assertFalse(
            await ns.ledger.replace_kagent_session(sid, "old-session", "req-2")
        )
        self.assertEqual((await ns.get_binding(sid))["kagent_request_id"], "req-1")

    async def test_agent_role_has_no_token_and_roles_are_unique_per_session(self):
        sid, _ = await self.session()
        binding = await ns.create_binding(sid, "claude")
        self.assertIsNone(binding["token_hash"])
        with self.assertRaises(asyncpg.UniqueViolationError):
            await ns.create_binding(sid, "claude")

    async def test_token_hash_lookup_and_joined_get(self):
        sid, _ = await self.session()
        binding = await ns.create_binding(sid, "claude", role="main")
        store = PgStore()
        found = await store.binding_by_token_hash(binding["token_hash"])
        self.assertEqual((found["session_id"], found["user_id"]), (sid, self.user))
        self.assertEqual((await store.get_binding(sid))["user_id"], self.user)
        self.assertIsNone(await store.binding_by_token_hash("nope"))

    async def test_the_unique_token_index_rejects_a_second_holder(self):
        a, _ = await self.session()
        b, _ = await self.session()
        await ns.create_binding(a, "claude", role="child")
        await self.pool.execute(
            "INSERT INTO native_bindings (session_id, kind, token_hash) VALUES ($1,'claude','dup')",
            b,
        )
        await self.pool.execute("DELETE FROM native_bindings WHERE session_id=$1", b)
        token = (await ns.get_binding(a))["token_hash"]
        with self.assertRaises(asyncpg.UniqueViolationError):
            await self.pool.execute(
                "INSERT INTO native_bindings (session_id, kind, token_hash) VALUES ($1,'claude',$2)",
                b,
                token,
            )


class LedgerTests(PostgresTestCase):
    async def test_record_message_writes_message_and_delivery(self):
        sid, cid = await self.bound_session()
        message_id = await ns.ledger.record_message(
            session_id=sid,
            conversation_id=cid,
            text="hello",
            state="recorded",
            source="user",
        )
        row = await self.pool.fetchrow(
            "SELECT * FROM native_deliveries WHERE message_id=$1", message_id
        )
        self.assertEqual(
            (row["state"], row["source"], row["session_id"]), ("recorded", "user", sid)
        )
        self.assertIsNone(row["task_id"])
        message = await self.pool.fetchrow(
            "SELECT role, content, conversation_id FROM messages WHERE id=$1",
            message_id,
        )
        self.assertEqual(
            (message["role"], message["content"], message["conversation_id"]),
            ("user", "hello", cid),
        )

    async def test_record_message_is_blocked_by_the_workspace_suspend_fence(self):
        workspace_id = await self.workspace()
        cid = await self.pool.fetchval(
            "SELECT conversation_id FROM sessions WHERE id=$1", workspace_id
        )

        async def send() -> str:
            return await ns.ledger.record_message(
                session_id=workspace_id,
                conversation_id=cid,
                text="x",
                state="recorded",
                source="user",
            )

        before = await self.pool.fetchval(
            "SELECT last_activity_at FROM workspace_lifecycles WHERE workspace_id=$1",
            workspace_id,
        )
        await self.pool.execute(
            "UPDATE workspace_lifecycles SET last_activity_at=NOW() - INTERVAL '1 hour' WHERE workspace_id=$1",
            workspace_id,
        )
        await send()
        touched = await self.pool.fetchval(
            "SELECT last_activity_at FROM workspace_lifecycles WHERE workspace_id=$1",
            workspace_id,
        )
        self.assertGreater(touched, before - timedelta(minutes=1))

        for desired, observed in (
            ("suspended", "running"),
            ("running", "suspending"),
            ("running", "suspended"),
        ):
            await self.pool.execute(
                "UPDATE workspace_lifecycles SET desired_state=$2, observed_state=$3 WHERE workspace_id=$1",
                workspace_id,
                desired,
                observed,
            )
            with self.assertRaisesRegex(ValueError, "suspending or suspended"):
                await send()

        await self.pool.execute(
            "UPDATE workspace_lifecycles SET desired_state='running', observed_state='running' WHERE workspace_id=$1",
            workspace_id,
        )
        await self.pool.execute(
            "UPDATE workspace_bindings SET desired_state='deleting' WHERE workspace_id=$1",
            workspace_id,
        )
        with self.assertRaisesRegex(ValueError, "being deleted"):
            await send()
        # The refused sends left no half-written rows.
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_deliveries WHERE session_id=$1",
                workspace_id,
            ),
            1,
        )

    async def test_suspend_fence_serialises_with_a_concurrent_suspension(self):
        """The ``FOR UPDATE`` on the binding row makes a send wait for a suspension in flight."""
        workspace_id = await self.workspace()
        cid = await self.pool.fetchval(
            "SELECT conversation_id FROM sessions WHERE id=$1", workspace_id
        )
        holder = await self.pool.acquire()
        try:
            tx = holder.transaction()
            await tx.start()
            await holder.fetchrow(
                "SELECT workspace_id FROM workspace_bindings WHERE workspace_id=$1 FOR UPDATE",
                workspace_id,
            )
            send = asyncio.ensure_future(
                ns.ledger.record_message(
                    session_id=workspace_id,
                    conversation_id=cid,
                    text="x",
                    state="recorded",
                    source="user",
                )
            )
            await asyncio.sleep(0.3)
            self.assertFalse(send.done(), "the send must wait for the binding lock")
            await holder.execute(
                "UPDATE workspace_lifecycles SET desired_state='suspended' WHERE workspace_id=$1",
                workspace_id,
            )
            await tx.commit()
        finally:
            await self.pool.release(holder)
        with self.assertRaisesRegex(ValueError, "suspending or suspended"):
            await send

    async def test_open_count_and_resolvable_deliveries(self):
        sid, cid = await self.bound_session()
        states = ["recorded", "sending", "delivered", "queued", "uncertain"]
        states += ["completed", "failed"]
        ids = {s: await self.delivery(sid, cid, s, text=f"text-{s}") for s in states}
        self.assertEqual(await ns.ledger.open_count(sid), 3)
        resolvable = await ns.ledger.resolvable_deliveries(sid)
        self.assertEqual(
            [r["message_id"] for r in resolvable],
            [ids["sending"], ids["delivered"], ids["uncertain"]],
        )
        self.assertEqual(resolvable[0]["content"], "text-sending")
        listed = await ns.ledger.deliveries(sid)
        self.assertEqual([r["message_id"] for r in listed], [ids[s] for s in states])

    async def test_delivery_state_and_recorded_deliveries(self):
        sid, cid = await self.bound_session()
        first = await self.delivery(sid, cid, "recorded", text="one")
        await self.delivery(sid, cid, "sending")
        second = await self.delivery(sid, cid, "recorded", text="two")
        self.assertEqual(await ns.ledger.delivery_state(first), "recorded")
        self.assertIsNone(await ns.ledger.delivery_state("missing"))
        self.assertEqual(
            await ns.ledger.recorded_deliveries(sid),
            [(first, "one"), (second, "two")],
        )

    async def test_set_delivery_and_transition_coalesce_and_gate(self):
        sid, cid = await self.bound_session()
        mid = await self.delivery(sid, cid, "recorded")
        await ns.ledger.set_delivery(mid, "sending", detail="d")
        row = await self.pool.fetchrow(
            "SELECT * FROM native_deliveries WHERE message_id=$1", mid
        )
        self.assertEqual(
            (row["state"], row["detail"], row["task_id"]), ("sending", "d", None)
        )

        moved = await ns.ledger.transition(
            mid,
            "delivered",
            from_states=("sending",),
            task_id="t1",
            evidence_ref="a2a:task/t1",
        )
        self.assertTrue(moved)
        # Not in from_states any more: refused, and nothing is overwritten.
        moved = await ns.ledger.transition(
            mid, "uncertain", from_states=("sending",), task_id="t2", detail="late"
        )
        self.assertFalse(moved)
        # COALESCE keeps what is already recorded when the new value is NULL, except that a
        # delivery that got through drops its stale detail.
        self.assertTrue(
            await ns.ledger.transition(mid, "completed", from_states=("delivered",))
        )
        row = await self.pool.fetchrow(
            "SELECT * FROM native_deliveries WHERE message_id=$1", mid
        )
        self.assertEqual(
            (row["state"], row["task_id"], row["evidence_ref"], row["detail"]),
            ("completed", "t1", "a2a:task/t1", None),
        )
        self.assertFalse(
            await ns.ledger.transition("missing", "failed", from_states=("sending",))
        )

    async def test_an_uncertain_delivery_that_resolves_loses_its_uncertainty_detail(
        self,
    ):
        sid, cid = await self.bound_session()
        mid = await self.delivery(sid, cid, "sending")
        await ns.ledger.transition(
            mid, "uncertain", from_states=("sending",), detail="outcome unknown"
        )
        self.assertTrue(
            await ns.ledger.transition(
                mid, "delivered", from_states=("uncertain",), task_id="t1"
            )
        )
        detail = await self.pool.fetchval(
            "SELECT detail FROM native_deliveries WHERE message_id=$1", mid
        )
        self.assertIsNone(detail)

    async def test_transition_has_a_single_winner_under_concurrency(self):
        sid, cid = await self.bound_session()
        mid = await self.delivery(sid, cid, "delivered")
        results = await asyncio.gather(
            *(
                ns.ledger.transition(
                    mid, "completed", from_states=("delivered",), task_id=f"t{i}"
                )
                for i in range(5)
            )
        )
        self.assertEqual(results.count(True), 1)

    async def test_promote_queued_takes_the_oldest_only_when_idle(self):
        sid, cid = await self.bound_session()
        first = await self.delivery(sid, cid, "queued", text="one")
        second = await self.delivery(sid, cid, "queued", text="two")
        busy = await self.delivery(sid, cid, "sending")
        self.assertIsNone(await ns.ledger.promote_queued(sid))
        self.assertEqual(await self.state_of(first), "queued")

        await self.pool.execute(
            "UPDATE native_deliveries SET state='completed' WHERE message_id=$1", busy
        )
        self.assertEqual(await ns.ledger.promote_queued(sid), (first, "one"))
        self.assertEqual(await self.state_of(first), "recorded")
        # The promoted delivery is open now, so the next one waits.
        self.assertIsNone(await ns.ledger.promote_queued(sid))
        self.assertEqual(await self.state_of(second), "queued")
        await self.pool.execute(
            "UPDATE native_deliveries SET state='completed' WHERE message_id=$1", first
        )
        self.assertEqual(await ns.ledger.promote_queued(sid), (second, "two"))
        self.assertIsNone(await ns.ledger.promote_queued(sid))

    async def test_promote_queued_is_atomic_under_concurrency(self):
        sid, cid = await self.bound_session()
        for n in range(3):
            await self.delivery(sid, cid, "queued", text=str(n))
        results = await asyncio.gather(
            *(ns.ledger.promote_queued(sid) for _ in range(6))
        )
        self.assertEqual(len([r for r in results if r is not None]), 1)
        self.assertEqual(await ns.ledger.open_count(sid), 1)

    async def test_fail_open_returns_the_prior_state_and_closes_only_open_work(self):
        sid, cid = await self.bound_session()
        other, other_cid = await self.bound_session()
        ids = {}
        for state in (
            "recorded",
            "sending",
            "delivered",
            "queued",
            "uncertain",
            "completed",
            "failed",
        ):
            ids[state] = await self.delivery(sid, cid, state)
        await self.pool.execute(
            "UPDATE native_deliveries SET task_id='task-d' WHERE message_id=$1",
            ids["delivered"],
        )
        untouched = await self.delivery(other, other_cid, "sending")

        opened = await ns.ledger.fail_open(sid, "cancelled by user")

        prior = {r["message_id"]: (r["state"], r["task_id"]) for r in opened}
        self.assertEqual(
            prior,
            {
                ids["recorded"]: ("recorded", None),
                ids["sending"]: ("sending", None),
                ids["delivered"]: ("delivered", "task-d"),
                ids["queued"]: ("queued", None),
                ids["uncertain"]: ("uncertain", None),
            },
        )
        rows = {
            r["message_id"]: r
            for r in await self.pool.fetch(
                "SELECT * FROM native_deliveries WHERE session_id=$1", sid
            )
        }
        for state in ("recorded", "sending", "delivered", "queued", "uncertain"):
            self.assertEqual(rows[ids[state]]["state"], "failed")
            self.assertEqual(rows[ids[state]]["detail"], "cancelled by user")
        self.assertEqual(rows[ids["completed"]]["state"], "completed")
        self.assertIsNone(rows[ids["completed"]]["detail"])
        self.assertEqual(await self.state_of(untouched), "sending")
        # Nothing is open any more, and a second cancel closes nothing.
        self.assertEqual(await ns.ledger.fail_open(sid, "again"), [])
        self.assertEqual(await ns.ledger.open_count(sid), 0)

    async def test_mirror_reply_is_idempotent_on_the_message_id(self):
        sid, cid = await self.bound_session()
        reply_id = str(uuid.uuid4())
        self.assertTrue(await ns.ledger.mirror_reply(cid, reply_id, "first"))
        self.assertFalse(await ns.ledger.mirror_reply(cid, reply_id, "second"))
        row = await self.pool.fetchrow(
            "SELECT role, content FROM messages WHERE id=$1", reply_id
        )
        self.assertEqual((row["role"], row["content"]), ("assistant", "first"))

    async def test_sessions_with_open_work(self):
        sid_open, cid_open = await self.bound_session()
        sid_queued, cid_queued = await self.bound_session()
        sid_recent, cid_recent = await self.bound_session()
        sid_stale, cid_stale = await self.bound_session()
        sid_done, cid_done = await self.bound_session()
        await self.delivery(sid_open, cid_open, "sending")
        await self.delivery(sid_queued, cid_queued, "queued")
        await self.delivery(sid_recent, cid_recent, "uncertain")
        stale = await self.delivery(sid_stale, cid_stale, "uncertain")
        await self.delivery(sid_done, cid_done, "completed")
        await self.pool.execute(
            "UPDATE native_deliveries SET updated_at=NOW() - INTERVAL '2 hours' WHERE message_id=$1",
            stale,
        )
        work = set(await ns.ledger.sessions_with_open_work())
        self.assertTrue({sid_open, sid_queued, sid_recent} <= work)
        self.assertFalse({sid_stale, sid_done} & work)

    async def test_topic_name_and_identity(self):
        sid, cid = await self.session()
        topic = await PgStore().topic(self.user, "release", create=True)
        await ns.create_binding(
            sid, "claude", role="child", parent_session_id="p", topic_id=topic["id"]
        )
        await self.delivery(sid, cid, "uncertain")
        self.assertEqual(await ns.ledger.topic_name(topic["id"]), "release")
        self.assertIsNone(await ns.ledger.topic_name("missing"))
        info = await ns.identity(sid)
        self.assertEqual(
            (info.kind, info.role, info.topic), ("claude", "child", "release")
        )
        self.assertEqual(info.deliveries[0].state, "uncertain")
        self.assertTrue(info.note.startswith("delivery unknown"))
        self.assertIsNone(await ns.identity("missing"))

    async def workspace(self, *, idle_minutes: int = 30) -> str:
        return await _create_workspace(self, idle_minutes=idle_minutes)


async def _create_workspace(case: PostgresTestCase, *, idle_minutes: int = 30) -> str:
    """Seed an existing Substrate-era branch workspace (``POST /workspaces`` now refuses new ones).

    Writes the rows the old create route wrote, registers its actor with a fake provisioner and
    records the observation, so the routes that still serve existing workspaces can be tested.
    """
    project_id = f"proj-{uuid.uuid4().hex[:8]}"
    await case.pool.execute(
        """INSERT INTO projects (id,user_id,owner,name,full_name,html_url)
           VALUES ($1,$2,'o','n',$3,'https://github.com/example/repo')""",
        project_id,
        case.user,
        f"o/{project_id}",
    )
    provisioner = FakeActorProvisioner()
    set_actor_provisioner(provisioner)
    case.addCleanup(set_actor_provisioner, None)
    workspace_id = str(uuid.uuid4())
    branch = f"b-{uuid.uuid4().hex[:6]}"
    actor_name = f"ml-{workspace_id[:16]}"
    atespace = settings.substrate_atespace
    secret = settings.shim_token_secret_name(atespace, actor_name)
    template = settings.substrate_actor_template
    manifest = WorkspaceManifest(
        repo_url="https://github.com/example/repo",
        branch=branch,
        agent_kinds=(WorkspaceAgentKind.CLAUDE,),
        resource_class="default",
        dev=WorkspaceDev(image="example/dev:1", idle_timeout_minutes=idle_minutes),
    )
    thread_id = await case.thread()
    conversation_id = str(uuid.uuid4())
    now = datetime.now(UTC)
    async with case.pool.acquire() as conn:
        async with conn.transaction():
            await conn.execute(
                "INSERT INTO conversations (id,user_id,title) VALUES ($1,$2,$3)",
                conversation_id,
                case.user,
                f"{project_id} · {branch}",
            )
            await conn.execute(
                """INSERT INTO sessions
                   (id,user_id,main_thread_id,title,description,prompt,conversation_id,
                    status,created_at,repo_url,project_id,branch_name,base_branch)
                   VALUES ($1,$2,$3,$4,$5,$6,$7,'active',$8,$9,$10,$11,$12)""",
                workspace_id,
                case.user,
                thread_id,
                f"{project_id} · {branch}",
                "Branch development workspace",
                "Development workspace",
                conversation_id,
                now,
                "https://github.com/example/repo",
                project_id,
                branch,
                branch,
            )
            await conn.execute(
                """INSERT INTO workspace_bindings
                   (workspace_id,atespace,actor_name,actor_template,
                    shim_token_secret_name,observed_state,desired_state,created_at,updated_at)
                   VALUES ($1,$2,$3,$4,$5,'unknown','active',$6,$6)""",
                workspace_id,
                atespace,
                actor_name,
                template,
                secret,
                now,
            )
            await conn.execute(
                """INSERT INTO workspace_lifecycles
                   (workspace_id,desired_state,observed_state,manifest,conditions,
                    last_activity_at,updated_at)
                   VALUES ($1,'running','unknown',$2::jsonb,'[]'::jsonb,$3,$3)""",
                workspace_id,
                json.dumps(manifest.model_dump(mode="json")),
                now,
            )
            await ns.create_binding(workspace_id, "claude", conn=conn)
    provisioned = await provisioner.create(
        atespace=atespace,
        actor_name=actor_name,
        template=template,
        shim_token_secret_name=secret,
    )
    await workspace_adapter._record_observation(workspace_id, actor=provisioned.actor)
    return workspace_id


class DelegationTests(PostgresTestCase):
    def setUp(self):
        spawn = patch.object(ns, "_spawn", side_effect=lambda coro: coro.close())
        self.spawned = spawn.start()
        self.addCleanup(spawn.stop)

    async def test_ensure_main_session_creates_once_and_reuses_the_conversation(self):
        existing = await db.create_conversation(self.user, title="history")
        await self.pool.execute(
            "INSERT INTO messages (id, conversation_id, role, content) VALUES ($1,$2,'user','old')",
            str(uuid.uuid4()),
            existing.id,
        )
        first = await ensure_main_session(self.user)
        again = await ensure_main_session(self.user)
        self.assertEqual(first["session_id"], again["session_id"])
        self.assertEqual((first["role"], first["kind"]), ("main", "claude"))
        session = await db.get_session(first["session_id"])
        self.assertEqual(session.conversation_id, existing.id)
        self.assertEqual(session.status, SessionStatus.WAITING_ON_USER)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_bindings b JOIN sessions s ON s.id=b.session_id"
                " WHERE s.user_id=$1 AND b.role='main'",
                self.user,
            ),
            1,
        )

    async def test_ensure_main_session_leaves_no_orphan_when_the_binding_fails(self):
        with (
            patch.object(settings, "agent_token_key", ""),
            patch.object(settings, "db_password", ""),
            self.assertRaises(RuntimeError),
        ):
            await ensure_main_session(self.user)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM sessions WHERE user_id=$1 AND title='Main thread'",
                self.user,
            ),
            0,
        )
        created = await ensure_main_session(self.user)
        self.assertEqual(created["role"], "main")

    async def test_concurrent_first_requests_share_one_main_session(self):
        bindings = await asyncio.gather(
            *(ensure_main_session(self.user) for _ in range(5))
        )
        self.assertEqual(len({b["session_id"] for b in bindings}), 1)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM sessions WHERE user_id=$1 AND title='Main thread'",
                self.user,
            ),
            1,
        )

    async def test_spawn_child_leaves_no_orphan_when_the_binding_fails(self):
        store = PgStore()
        parent = await ensure_main_session(self.user)
        topic = await store.topic(self.user, "alpha", create=True)
        parent_row = await store.get_binding(parent["session_id"])
        before = await self.pool.fetchval(
            "SELECT count(*) FROM sessions WHERE user_id=$1", self.user
        )
        with (
            patch.object(settings, "agent_token_key", ""),
            patch.object(settings, "db_password", ""),
            self.assertRaises(RuntimeError),
        ):
            await store.spawn_child(parent_row, topic, "codex", "Fix it", "do it")
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM sessions WHERE user_id=$1", self.user
            ),
            before,
        )

    async def test_render_for_binding_main_uses_topics_records_and_recent_messages(
        self,
    ):
        main = await ensure_main_session(self.user)
        sid = main["session_id"]
        cid = (await db.get_session(sid)).conversation_id
        store = PgStore()
        topic = await store.topic(self.user, "alpha", create=True)
        await store.set_topic_status(topic["id"], "in progress")
        await store.add_record(topic["id"], "note", "a note", None)
        await store.add_record(topic["id"], "decision", "a decision", None)
        await store.add_record(topic["id"], "pending", "ship it", None)
        for n in range(3):
            await self.pool.execute(
                "INSERT INTO messages (id, conversation_id, role, content) VALUES ($1,$2,'user',$3)",
                str(uuid.uuid4()),
                cid,
                f"visible-{n}",
            )
        # A message still being delivered is about to be the next prompt: not carried over.
        await self.delivery(sid, cid, "sending", text="in-flight")
        await self.delivery(sid, cid, "queued", text="queued-one")
        await self.delivery(sid, cid, "completed", text="done-one")

        text = await render_for_binding(await ns.get_binding(sid))

        self.assertIn("alpha", text)
        self.assertIn("a decision", text)
        self.assertIn("[alpha] ship it", text)
        self.assertIn("visible-2", text)
        self.assertIn("done-one", text)
        self.assertNotIn("in-flight", text)
        self.assertNotIn("queued-one", text)

    async def test_render_for_binding_main_without_topics_and_for_children(self):
        main = await ensure_main_session(self.user)
        self.assertTrue(
            await render_for_binding(await ns.get_binding(main["session_id"]))
        )
        sid, _ = await self.session()
        child = await ns.create_binding(sid, "codex", role="child")
        self.assertTrue(await render_for_binding(child))

    async def test_topic_records_and_pending_close(self):
        store = PgStore()
        topic = await store.topic(self.user, "alpha", create=True)
        again = await store.topic(self.user, "alpha", create=True)
        self.assertEqual(topic["id"], again["id"])  # ON CONFLICT (user_id, name)
        self.assertIsNone(await store.topic(self.user, "nope", create=False))
        rid = await store.add_record(topic["id"], "pending", "todo", None)
        self.assertFalse(await store.close_pending(self.user, "no-such-prefix"))
        self.assertTrue(await store.close_pending(self.user, rid[:8]))
        self.assertFalse(await store.close_pending(self.user, rid[:8]))
        index = await store.topic_index(self.user)
        self.assertEqual([(t.name, t.pending) for t in index], [("alpha", 0)])

    async def test_count_live_children(self):
        store = PgStore()
        parent, _ = await self.bound_session(role="main")
        live, _ = await self.session("active")
        done, _ = await self.session("completed")
        reported, _ = await self.session("active")
        failed_brief, failed_cid = await self.session("active")
        for sid in (live, done, reported, failed_brief):
            await ns.create_binding(
                sid, "claude", role="child", parent_session_id=parent
            )
        await ns.ledger.update_binding(reported, reported_at=datetime.now(UTC))
        await self.delivery(failed_brief, failed_cid, "failed", source="brief")
        self.assertEqual(await store.count_live_children(parent), 1)
        self.assertGreaterEqual(await store.count_live_children(None), 1)

    async def test_children_state(self):
        store = PgStore()
        parent, _ = await self.bound_session(role="main")
        topic = await store.topic(self.user, "alpha", create=True)

        async def child(status: str, delivery: str | None, **extra):
            sid, cid = await self.session(status)
            await ns.create_binding(
                sid,
                "claude",
                role="child",
                parent_session_id=parent,
                topic_id=extra.pop("topic_id", None),
            )
            if delivery:
                await self.delivery(sid, cid, delivery)
            if extra.get("reported"):
                await ns.ledger.update_binding(sid, reported_at=datetime.now(UTC))
            if extra.get("reply"):
                await self.pool.execute(
                    "INSERT INTO messages (id, conversation_id, role, content) VALUES ($1,$2,'assistant',$3)",
                    str(uuid.uuid4()),
                    cid,
                    extra["reply"],
                )
            if extra.get("archived"):
                await self.pool.execute(
                    "UPDATE sessions SET archived_at=NOW() WHERE id=$1", sid
                )
            return sid

        working = await child(
            "active", "delivered", reply="  spaced   reply\n", topic_id=topic["id"]
        )
        cancelled = await child("cancelled", "failed")
        reported = await child("completed", "completed", reported=True)
        unknown = await child("active", "uncertain")
        failed = await child("active", "failed")
        idle = await child("waiting_on_user", "completed")
        await child("active", "sending", archived=True)

        states = {c["session_id"]: c for c in await store.children_state(parent)}
        self.assertEqual(len(states), 6)
        self.assertEqual(
            {sid: c["state"] for sid, c in states.items()},
            {
                working: "working",
                cancelled: "cancelled",
                reported: "reported",
                unknown: "delivery-unknown",
                failed: "failed-to-start",
                idle: "idle",
            },
        )
        self.assertEqual(states[working]["topic"], "alpha")
        self.assertEqual(states[working]["last_reply"], "spaced reply")
        self.assertEqual(states[idle]["topic"], INBOX)
        self.assertEqual(states[idle]["turns"], 0)
        self.assertRegex(states[idle]["last_activity"], r"^\d\d:\d\d:\d\dZ$")
        self.assertEqual(await store.children_state("no-parent"), [])

    async def test_messages_and_archive(self):
        store = PgStore()
        sid, cid = await self.session()
        for n in range(4):
            await self.pool.execute(
                "INSERT INTO messages (id, conversation_id, role, content, created_at)"
                " VALUES ($1,$2,'user',$3, NOW() + $4 * INTERVAL '1 second')",
                str(uuid.uuid4()),
                cid,
                f"m{n}",
                n,
            )
        rows = await store.messages(sid, 1, 2)
        self.assertEqual([r["content"] for r in rows], ["m1", "m2"])

    async def test_deliver_report_claims_once_records_evidence_and_queues_for_the_parent(
        self,
    ):
        store = PgStore()
        parent, parent_cid = await self.bound_session(role="main")
        topic = await store.topic(self.user, "alpha", create=True)
        child_id, child_cid = await self.session("active")
        child = await ns.create_binding(
            child_id,
            "claude",
            role="child",
            parent_session_id=parent,
            topic_id=topic["id"],
        )
        mid = await self.delivery(child_id, child_cid, "completed", source="brief")
        await self.pool.execute(
            "UPDATE native_deliveries SET evidence_ref='a2a:task/t9' WHERE message_id=$1",
            mid,
        )

        message_id = await store.deliver_report(child, topic, "all done", False)

        self.assertTrue(message_id)
        record = await self.pool.fetchrow(
            "SELECT kind, text, session_id, evidence_ref FROM topic_records WHERE topic_id=$1 AND kind='report'",
            topic["id"],
        )
        self.assertEqual(tuple(record), ("report", "all done", child_id, "a2a:task/t9"))
        self.assertIsNotNone((await ns.get_binding(child_id))["reported_at"])
        session = await db.get_session(child_id)
        self.assertEqual(session.status, SessionStatus.COMPLETED)
        self.assertEqual(session.summary, "all done")
        self.assertEqual(
            await self.pool.fetchrow(
                "SELECT state, source, session_id FROM native_deliveries WHERE message_id=$1",
                message_id,
            ),
            await self.pool.fetchrow(
                "SELECT 'recorded'::text, 'report'::text, $1::text", parent
            ),
        )
        self.assertIn(
            "[report from child",
            await self.pool.fetchval(
                "SELECT content FROM messages WHERE id=$1", message_id
            ),
        )
        # A second report is not claimed: no second record, no second message.
        self.assertEqual(await store.deliver_report(child, topic, "again", False), "")
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM topic_records WHERE topic_id=$1", topic["id"]
            ),
            1,
        )

    async def test_deliver_report_queues_behind_an_open_parent_turn_and_keeps_cancelled(
        self,
    ):
        store = PgStore()
        parent, parent_cid = await self.bound_session(role="main")
        await self.delivery(parent, parent_cid, "sending")
        child_id, _ = await self.session("cancelled")
        child = await ns.create_binding(
            child_id, "claude", role="child", parent_session_id=parent
        )
        message_id = await store.deliver_report(child, None, "late", True)
        self.assertEqual(await self.state_of(message_id), "queued")
        self.assertEqual(
            (await db.get_session(child_id)).status, SessionStatus.CANCELLED
        )
        self.assertIn(
            "fallback",
            await self.pool.fetchval(
                "SELECT content FROM messages WHERE id=$1", message_id
            ),
        )

    async def test_auto_report_uses_the_topic_and_skips_reported_children(self):
        from mainloop.runtime import delegation

        parent, _ = await self.bound_session(role="main")
        topic = await PgStore().topic(self.user, "alpha", create=True)
        child_id, _ = await self.session("active")
        await ns.create_binding(
            child_id,
            "claude",
            role="child",
            parent_session_id=parent,
            topic_id=topic["id"],
        )
        await delegation.auto_report(child_id, "x" * 5000)
        await delegation.auto_report(child_id, "second")
        texts = [
            r["text"]
            for r in await self.pool.fetch(
                "SELECT text FROM topic_records WHERE topic_id=$1", topic["id"]
            )
        ]
        self.assertEqual([len(t) for t in texts], [4000])

    async def test_spawn_child_creates_session_binding_and_brief(self):
        store = PgStore()
        parent = await ensure_main_session(self.user)
        topic = await store.topic(self.user, "alpha", create=True)
        parent_row = await store.get_binding(parent["session_id"])
        child_id = await store.spawn_child(
            parent_row, topic, "codex", "Fix it", "do the thing"
        )
        binding = await ns.get_binding(child_id)
        self.assertEqual(
            (
                binding["role"],
                binding["kind"],
                binding["parent_session_id"],
                binding["topic_id"],
            ),
            ("child", "codex", parent["session_id"], topic["id"]),
        )
        deliveries = await ns.ledger.deliveries(child_id)
        self.assertEqual(
            [(d["state"], d["source"]) for d in deliveries], [("recorded", "brief")]
        )
        self.assertEqual(self.spawned.call_count, 1)

    async def test_submit_message_queues_reports_and_refuses_user_messages_while_busy(
        self,
    ):
        sid, _ = await self.bound_session()
        first = await ns.submit_message(sid, "one")
        self.assertEqual(await self.state_of(first), "recorded")
        with self.assertRaisesRegex(ValueError, "still in flight"):
            await ns.submit_message(sid, "two")
        queued = await ns.submit_message(sid, "report", source="report")
        self.assertEqual(await self.state_of(queued), "queued")
        await self.pool.execute(
            "UPDATE native_deliveries SET state='completed' WHERE message_id=$1", first
        )
        await ns._promote_queued(sid)
        self.assertEqual(await self.state_of(queued), "recorded")


class WorkspaceTests(PostgresTestCase):
    async def test_create_is_refused_and_writes_nothing(self):
        with self.assertRaises(HTTPException) as raised:
            await workspace_api.create_workspace()
        self.assertEqual(raised.exception.status_code, 409)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM workspace_bindings b JOIN sessions s"
                " ON s.id=b.workspace_id WHERE s.user_id=$1",
                self.user,
            ),
            0,
        )

    async def test_a_seeded_workspace_has_its_native_binding_and_lifecycle(self):
        workspace_id = await _create_workspace(self)
        binding = await ns.get_binding(workspace_id)
        self.assertEqual((binding["kind"], binding["role"]), ("claude", "agent"))
        lifecycle = await workspace_adapter.ensure_workspace_lifecycle(workspace_id)
        self.assertEqual(lifecycle.workspace_id, workspace_id)

    async def test_delete_removes_native_rows_and_the_session(self):
        workspace_id = await _create_workspace(self)
        cid = await self.pool.fetchval(
            "SELECT conversation_id FROM sessions WHERE id=$1", workspace_id
        )
        mid = await self.delivery(workspace_id, cid, "completed")
        await self.pool.execute(
            "UPDATE native_bindings SET kagent_session_id='ctx', turns=3 WHERE session_id=$1",
            workspace_id,
        )
        with patch.object(workspace_api, "_publish", new=AsyncMock()):
            response = await workspace_api.delete_workspace(
                workspace_id, user_id=self.user
            )
        self.assertEqual(response.status_code, 204)
        for table, query in (
            (
                "native_deliveries",
                "SELECT count(*) FROM native_deliveries WHERE session_id=$1",
            ),
            (
                "native_bindings",
                "SELECT count(*) FROM native_bindings WHERE session_id=$1",
            ),
            (
                "workspace_lifecycles",
                "SELECT count(*) FROM workspace_lifecycles WHERE workspace_id=$1",
            ),
            (
                "workspace_bindings",
                "SELECT count(*) FROM workspace_bindings WHERE workspace_id=$1",
            ),
            ("sessions", "SELECT count(*) FROM sessions WHERE id=$1"),
        ):
            self.assertEqual(await self.pool.fetchval(query, workspace_id), 0, table)
        self.assertEqual(
            await self.pool.fetchval("SELECT count(*) FROM messages WHERE id=$1", mid),
            0,
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM conversations WHERE id=$1", cid
            ),
            0,
        )

    async def test_delete_is_refused_while_a_delivery_is_open(self):
        workspace_id = await _create_workspace(self)
        cid = await self.pool.fetchval(
            "SELECT conversation_id FROM sessions WHERE id=$1", workspace_id
        )
        await self.delivery(workspace_id, cid, "sending")
        with self.assertRaises(HTTPException) as caught:
            await workspace_api.delete_workspace(workspace_id, user_id=self.user)
        self.assertEqual(caught.exception.status_code, 409)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT desired_state FROM workspace_bindings WHERE workspace_id=$1",
                workspace_id,
            ),
            "active",
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_bindings WHERE session_id=$1", workspace_id
            ),
            1,
        )

    async def test_delete_rolls_back_to_the_previous_state_when_the_actor_delete_fails(
        self,
    ):
        workspace_id = await _create_workspace(self)
        provisioner = FakeActorProvisioner()
        provisioner.delete = AsyncMock(side_effect=RuntimeError("boom"))  # type: ignore[method-assign]
        set_actor_provisioner(provisioner)
        with self.assertRaises(HTTPException) as caught:
            await workspace_api.delete_workspace(workspace_id, user_id=self.user)
        self.assertEqual(caught.exception.status_code, 502)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT desired_state FROM workspace_bindings WHERE workspace_id=$1",
                workspace_id,
            ),
            "active",
        )
        self.assertIsNotNone(await ns.get_binding(workspace_id))

    async def test_adapter_delivery_states_and_idle_selection(self):
        workspace_id = await _create_workspace(self, idle_minutes=5)
        cid = await self.pool.fetchval(
            "SELECT conversation_id FROM sessions WHERE id=$1", workspace_id
        )
        self.assertEqual(await workspace_adapter._delivery_states(workspace_id), set())
        await self.delivery(workspace_id, cid, "uncertain")
        await self.delivery(workspace_id, cid, "completed")
        self.assertEqual(
            await workspace_adapter._delivery_states(workspace_id), {"uncertain"}
        )
        async with db.connection() as conn:
            self.assertEqual(
                await workspace_adapter._delivery_states(workspace_id, conn=conn),
                {"uncertain"},
            )

        await self.pool.execute(
            "UPDATE workspace_lifecycles SET desired_state='running', observed_state='running',"
            " last_activity_at=NOW() - INTERVAL '1 hour' WHERE workspace_id=$1",
            workspace_id,
        )
        await self.pool.execute(
            "UPDATE native_deliveries SET created_at=NOW() - INTERVAL '1 hour' WHERE session_id=$1",
            workspace_id,
        )
        with patch.object(
            workspace_adapter,
            "suspend_workspace_if_idle",
            new=AsyncMock(return_value=None),
        ) as suspend:
            await workspace_adapter.suspend_idle_workspaces()
            self.assertIn(workspace_id, [c.args[0] for c in suspend.await_args_list])
            # A recent delivery counts as activity (the LATERAL over native_deliveries).
            await self.pool.execute(
                "UPDATE native_deliveries SET created_at=NOW() WHERE session_id=$1",
                workspace_id,
            )
            suspend.reset_mock()
            await workspace_adapter.suspend_idle_workspaces()
            self.assertNotIn(workspace_id, [c.args[0] for c in suspend.await_args_list])


class ReconcileTests(PostgresTestCase):
    async def test_reconcile_loop_syncs_sessions_with_open_work_and_checks_idle(self):
        busy, busy_cid = await self.bound_session()
        quiet, quiet_cid = await self.bound_session()
        await self.delivery(busy, busy_cid, "sending")
        await self.delivery(quiet, quiet_cid, "completed")
        synced: list[str] = []

        async def sync(session_id: str) -> None:
            synced.append(session_id)

        class Stop(Exception):
            pass

        async def sleep(_: float) -> None:
            raise asyncio.CancelledError

        with (
            patch.object(ns, "sync", new=sync),
            patch.object(
                workspace_adapter,
                "suspend_idle_workspaces",
                new=AsyncMock(return_value=0),
            ) as idle,
            patch.object(ns.asyncio, "sleep", new=sleep),
        ):
            with self.assertRaises(asyncio.CancelledError):
                await ns.reconcile_loop()
        self.assertIn(busy, synced)
        self.assertNotIn(quiet, synced)
        idle.assert_awaited_once()

    async def test_sync_observes_unresolved_deliveries_through_a_real_ledger(self):
        """sync() reading ``resolvable_deliveries`` and settling them via ``transition``."""
        sid, cid = await self.bound_session()
        await ns.ledger.update_binding(sid, kagent_session_id="ctx-1")
        mid = await self.delivery(sid, cid, "sending")
        await self.pool.execute(
            "UPDATE native_deliveries SET updated_at=NOW() - INTERVAL '5 minutes' WHERE message_id=$1",
            mid,
        )

        class Gateway:
            async def find_task_for_message(self, *_):
                return None

        with patch.object(ns, "get_client", return_value=Gateway()):
            await ns.sync(sid)
        row = await self.pool.fetchrow(
            "SELECT state, detail FROM native_deliveries WHERE message_id=$1", mid
        )
        self.assertEqual(row["state"], "uncertain")
        self.assertIn("not replaying", row["detail"])
        self.assertEqual(
            (await db.get_session(sid)).status, SessionStatus.WAITING_ON_USER
        )


class ApiQueryTests(PostgresTestCase):
    async def test_conversation_and_session_list_queries_read_the_binding(self):
        from mainloop import api

        main = await ensure_main_session(self.user)
        cid = (await db.get_session(main["session_id"])).conversation_id
        topic = await PgStore().topic(self.user, "alpha", create=True)
        child_id, _ = await self.session("active")
        await ns.create_binding(
            child_id,
            "claude",
            role="child",
            parent_session_id=main["session_id"],
            topic_id=topic["id"],
        )
        with patch.object(ns, "sync", new=AsyncMock()) as sync:
            response = await api.get_conversation(cid)
        sync.assert_awaited_once_with(main["session_id"])
        self.assertEqual(response.conversation.id, cid)

        sessions = {
            s.id: s for s in await api.list_sessions(user_id=self.user, status=None)
        }
        self.assertEqual(sessions[child_id].parent_session_id, main["session_id"])
        self.assertEqual(sessions[child_id].topic, "alpha")

    async def test_preview_target_resolves_the_agent_kind(self):
        from mainloop.runtime import preview_proxy

        workspace_id = await _create_workspace(self)
        target = await preview_proxy._resolve_target(workspace_id, self.user)
        self.assertIsNotNone(target)
        self.assertIsNone(
            await preview_proxy._resolve_target(workspace_id, "someone-else")
        )
        self.assertIsNone(await preview_proxy._resolve_target("missing", self.user))


if __name__ == "__main__":
    unittest.main()
