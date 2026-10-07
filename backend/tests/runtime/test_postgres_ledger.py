"""The slice's raw SQL against a real PostgreSQL (asyncpg).

Opt-in: skipped unless ``MAINLOOP_TEST_DATABASE_URL`` is set. The URL needs a role that can
``CREATE DATABASE``; the module creates a scratch database from it, applies the schema, and drops
the database afterwards, so nothing in the target server's existing databases is touched.

    MAINLOOP_TEST_DATABASE_URL=postgresql://user:pass@host:5432/postgres \
        uv run python -m unittest tests.runtime.test_postgres_ledger

The kagent gateway is faked; only the ledger, binding, delegation, workspace and reconcile SQL
runs for real. Kubernetes credential deletion is faked.
"""

from __future__ import annotations

import asyncio
import json
import os
import unittest
import uuid
from datetime import UTC, datetime
from unittest.mock import AsyncMock, patch
from urllib.parse import urlsplit, urlunsplit

import asyncpg
import httpx
from mainloop import api
from mainloop.config import settings
from mainloop.db import db
from mainloop.db.postgres import MIGRATION_SQL, SCHEMA_SQL
from mainloop.runtime import native_sessions as ns
from mainloop.runtime import preview_proxy, workspaces
from mainloop.runtime.delegation import (
    INBOX,
    PgStore,
    ensure_main_session,
    render_for_binding,
)
from mainloop.runtime.kagent_client import (
    KagentClient,
    KagentError,
    OutcomeUnknown,
    RuntimeOperation,
    RuntimeState,
    SessionError,
    SessionWorkspace,
    Unreachable,
    decode_fields,
)
from mainloop.services.github_pr import RepoMetadata
from mainloop.services.github_repo import GithubRepo, parse_github_repo
from tests.runtime.kagent_fake import CONTEXT_ID, FakeKagent

from models import (
    SessionStatus,
    WorkspaceAgentKind,
    WorkspaceDev,
    WorkspaceManifest,
    WorkspacePort,
)

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


async def _column_count(url: str, table: str, column: str) -> int:
    conn = await asyncpg.connect(url)
    try:
        return await conn.fetchval(
            "SELECT count(*) FROM information_schema.columns WHERE table_name=$1 AND column_name=$2",
            table,
            column,
        )
    finally:
        await conn.close()


async def _expect_column_missing(url: str) -> None:
    """Check that the scratch database is in the old shape before it is migrated."""
    if await _column_count(url, "native_bindings", "kagent_deleted_at"):
        raise AssertionError("kagent_deleted_at should be absent from the old schema")


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
                "queue_held",
            },
            "native_deliveries": {
                "message_id",
                "session_id",
                "state",
                "task_id",
                "evidence_ref",
                "detail",
                "source",
                "partial_text",
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


class OneOpenTurnAcrossProcessesTests(PostgresTestCase):
    """The REST and MCP containers both write the ledger: the database must hold the rule."""

    async def submit(self, sid, cid, source, text="x"):
        return await ns.ledger.record_submission(
            session_id=sid, conversation_id=cid, text=text, source=source
        )

    async def states(self, sid):
        rows = await self.pool.fetch(
            "SELECT state FROM native_deliveries WHERE session_id=$1 ORDER BY created_at",
            sid,
        )
        return sorted(r["state"] for r in rows)

    async def test_concurrent_submissions_open_exactly_one_turn(self):
        sid, cid = await self.bound_session()
        results = await asyncio.gather(
            *(self.submit(sid, cid, "report", f"r{i}") for i in range(6))
        )
        self.assertEqual(sorted(state for _, state in results).count("recorded"), 1)
        self.assertEqual(await self.states(sid), ["queued"] * 5 + ["recorded"])

    async def test_concurrent_user_messages_one_wins_the_rest_are_refused(self):
        sid, cid = await self.bound_session()
        results = await asyncio.gather(
            *(self.submit(sid, cid, "user", f"u{i}") for i in range(6)),
            return_exceptions=True,
        )
        refused = [r for r in results if isinstance(r, ValueError)]
        self.assertEqual(len(refused), 5)
        self.assertEqual(await self.states(sid), ["recorded"])
        # Refused messages leave no message row behind either.
        count = await self.pool.fetchval(
            "SELECT count(*) FROM messages WHERE conversation_id=$1", cid
        )
        self.assertEqual(count, 1)

    async def test_rest_user_and_mcp_report_share_the_same_admission_lock(self):
        sid, cid = await self.bound_session()
        user, report = await asyncio.gather(
            self.submit(sid, cid, "user", "owner message"),
            self.submit(sid, cid, "report", "child report"),
            return_exceptions=True,
        )
        self.assertIsInstance(report, tuple)
        if isinstance(user, ValueError):
            self.assertEqual(report[1], "recorded")
        else:
            self.assertIsInstance(user, tuple)
            self.assertEqual((user[1], report[1]), ("recorded", "queued"))
        self.assertEqual(await ns.ledger.open_count(sid), 1)

    async def test_a_submission_waits_for_the_session_lock_held_by_another_connection(
        self,
    ):
        sid, cid = await self.bound_session()
        async with self.pool.acquire() as other, other.transaction():
            await ns.Ledger._lock_deliveries(other, sid)
            pending = asyncio.ensure_future(self.submit(sid, cid, "user"))
            await asyncio.sleep(0.3)
            self.assertFalse(pending.done(), "the submission did not wait for the lock")
            # Another process opens a turn while holding the lock; the submission sees it.
            await other.execute(
                "INSERT INTO messages (id, conversation_id, role, content) VALUES ('m-other',$1,'user','a')",
                cid,
            )
            await other.execute(
                "INSERT INTO native_deliveries (message_id, session_id, state, source) VALUES ('m-other',$1,'recorded','user')",
                sid,
            )
        with self.assertRaisesRegex(ValueError, "still in flight"):
            await pending
        self.assertEqual(await self.states(sid), ["recorded"])

    async def test_a_submission_in_another_session_is_not_blocked(self):
        a, acid = await self.bound_session()
        b, bcid = await self.bound_session()
        async with self.pool.acquire() as other, other.transaction():
            await ns.Ledger._lock_deliveries(other, a)
            _, state = await asyncio.wait_for(self.submit(b, bcid, "user"), 2)
        self.assertEqual(state, "recorded")

    async def test_promotion_and_submission_cannot_both_open_a_turn(self):
        sid, cid = await self.bound_session()
        queued = await self.delivery(sid, cid, "queued", source="report")
        for _ in range(5):
            await self.pool.execute(
                "UPDATE native_deliveries SET state='queued' WHERE message_id=$1",
                queued,
            )
            await self.pool.execute(
                "DELETE FROM native_deliveries WHERE session_id=$1 AND message_id<>$2",
                sid,
                queued,
            )
            await asyncio.gather(
                ns.ledger.promote_queued(sid), self.submit(sid, cid, "report")
            )
            open_turns = await self.pool.fetchval(
                "SELECT count(*) FROM native_deliveries WHERE session_id=$1 AND state = ANY($2)",
                sid,
                list(ns.OPEN_STATES),
            )
            self.assertEqual(open_turns, 1)

    async def test_late_receipt_cannot_reopen_uncertain_work_alongside_a_new_turn(self):
        sid, cid = await self.bound_session()
        old = await self.delivery(sid, cid, "uncertain")
        await self.submit(sid, cid, "user")
        self.assertFalse(
            await ns.ledger.transition(
                old, "delivered", from_states=ns._RESOLVABLE, task_id="late-task"
            )
        )
        self.assertEqual(await self.state_of(old), "uncertain")
        self.assertEqual(await ns.ledger.open_count(sid), 1)
        # Its terminal outcome can still be recorded without replaying it.
        self.assertTrue(
            await ns.ledger.transition(
                old, "completed", from_states=ns._RESOLVABLE, task_id="late-task"
            )
        )


class LedgerTests(PostgresTestCase):
    async def test_record_submission_writes_message_and_delivery(self):
        sid, cid = await self.bound_session()
        message_id, state = await ns.ledger.record_submission(
            session_id=sid,
            conversation_id=cid,
            text="hello",
            source="user",
        )
        self.assertEqual(state, "recorded")
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

    async def test_transition_coalesces_and_gates(self):
        sid, cid = await self.bound_session()
        mid = await self.delivery(sid, cid, "recorded")
        await ns.ledger.transition(
            mid, "sending", from_states=("recorded",), detail="d"
        )
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

    async def settle(self, mid: str, cid: str, **kw) -> bool:
        return await ns.ledger.settle_cancelled(
            mid,
            from_states=kw.pop("from_states", ns.OPEN_STATES),
            conversation_id=cid,
            note_id=ns._stop_note_id(mid),
            note=ns.TURN_STOPPED_NOTE,
            **kw,
        )

    async def notes(self, cid: str) -> list[str]:
        return [
            r["content"]
            for r in await self.pool.fetch(
                "SELECT content FROM messages WHERE conversation_id=$1 AND role='assistant'",
                cid,
            )
        ]

    async def test_settle_cancelled_writes_the_state_and_the_note_together_once(self):
        sid, cid = await self.bound_session()
        mid = await self.delivery(sid, cid, "delivered")
        before = await self.pool.fetchval(
            "SELECT updated_at FROM conversations WHERE id=$1", cid
        )
        self.assertTrue(await self.settle(mid, cid, task_id="t1", detail="stopped"))
        row = await self.pool.fetchrow(
            "SELECT * FROM native_deliveries WHERE message_id=$1", mid
        )
        self.assertEqual(
            (row["state"], row["task_id"], row["evidence_ref"], row["detail"]),
            ("cancelled", "t1", "a2a:task/t1", "stopped"),
        )
        self.assertEqual(await self.notes(cid), [ns.TURN_STOPPED_NOTE])
        self.assertGreater(
            await self.pool.fetchval(
                "SELECT updated_at FROM conversations WHERE id=$1", cid
            ),
            before,
        )
        # Terminal: a replay moves nothing and writes nothing, and nothing is open.
        self.assertFalse(await self.settle(mid, cid, task_id="t1"))
        self.assertEqual(await self.notes(cid), [ns.TURN_STOPPED_NOTE])
        self.assertEqual(await ns.ledger.open_count(sid), 0)
        self.assertNotIn(sid, await ns.ledger.sessions_with_open_work())

    async def test_settle_cancelled_leaves_a_delivery_outside_from_states_alone(self):
        sid, cid = await self.bound_session()
        for state in ("completed", "failed", "uncertain", "queued"):
            mid = await self.delivery(sid, cid, state)
            self.assertFalse(await self.settle(mid, cid))
            self.assertEqual(await self.state_of(mid), state)
        self.assertEqual(await self.notes(cid), [])

        self.assertFalse(
            await self.pool.fetchval(
                "SELECT queue_held FROM native_bindings WHERE session_id=$1", sid
            )
        )

    async def test_settle_cancelled_rolls_back_the_state_when_the_note_fails(self):
        sid, cid = await self.bound_session()
        mid = await self.delivery(sid, cid, "delivered")
        with self.assertRaises(asyncpg.ForeignKeyViolationError):
            await self.settle(mid, "no-such-conversation", task_id="t1")
        row = await self.pool.fetchrow(
            "SELECT state, task_id, detail FROM native_deliveries WHERE message_id=$1",
            mid,
        )
        self.assertEqual(
            (row["state"], row["task_id"], row["detail"]), ("delivered", None, None)
        )
        self.assertEqual(await self.notes(cid), [])
        self.assertFalse(
            await self.pool.fetchval(
                "SELECT queue_held FROM native_bindings WHERE session_id=$1", sid
            )
        )

    async def test_two_concurrent_settles_produce_one_state_change_and_one_note(self):
        sid, cid = await self.bound_session()
        mid = await self.delivery(sid, cid, "delivered")
        results = await asyncio.gather(*(self.settle(mid, cid) for _ in range(4)))
        self.assertEqual(results.count(True), 1)
        self.assertEqual(await self.state_of(mid), "cancelled")
        self.assertEqual(await self.notes(cid), [ns.TURN_STOPPED_NOTE])

    async def test_cancellation_holds_queue_atomically_against_another_writer(self):
        sid, cid = await self.bound_session()
        mid = await self.delivery(sid, cid, "delivered")
        queued = await self.delivery(sid, cid, "queued", source="report")
        stopped, promoted, report = await asyncio.gather(
            self.settle(mid, cid),
            ns.ledger.promote_queued(sid),
            ns.ledger.record_submission(
                session_id=sid, conversation_id=cid, text="late report", source="report"
            ),
        )
        self.assertTrue(stopped)
        self.assertIsNone(promoted)
        self.assertEqual(report[1], "queued")
        self.assertEqual(await self.state_of(queued), "queued")
        self.assertEqual(await ns.ledger.active_count(sid), 0)
        self.assertNotIn(sid, await ns.ledger.sessions_with_open_work())
        # A new Ledger models another process after a restart; the hold is durable.
        restarted = ns.Ledger()
        self.assertIsNone(await restarted.promote_queued(sid))
        user, state = await restarted.record_submission(
            session_id=sid, conversation_id=cid, text="continue", source="user"
        )
        self.assertEqual(state, "recorded")
        self.assertIsNone(await restarted.promote_queued(sid))
        await restarted.transition(user, "completed", from_states=("recorded",))
        self.assertIsNotNone(await restarted.promote_queued(sid))

    async def test_cancel_keeps_durable_partial_when_the_response_has_no_artifacts(
        self,
    ):
        sid, cid = await self.bound_session()
        mid = await self.delivery(sid, cid, "delivered")
        await ns.ledger.remember_partial(mid, "An unfinished answer")
        await self.settle(mid, cid)
        self.assertEqual(
            await self.notes(cid), [ns.stopped_message("An unfinished answer")]
        )
        await ns.ledger.remember_partial(mid, "late stream")
        self.assertEqual(
            await self.notes(cid), [ns.stopped_message("An unfinished answer")]
        )

    async def test_cancel_racing_completion_ends_in_exactly_one_terminal_state(self):
        for _ in range(10):
            sid, cid = await self.bound_session()
            mid = await self.delivery(sid, cid, "delivered")
            cancelled, completed = await asyncio.gather(
                self.settle(mid, cid),
                ns.ledger.transition(
                    mid, "completed", from_states=ns._RESOLVABLE, task_id="t1"
                ),
            )
            self.assertNotEqual(cancelled, completed)  # exactly one won
            state = await self.state_of(mid)
            self.assertEqual(state, "cancelled" if cancelled else "completed")
            self.assertEqual(
                await self.notes(cid), [ns.TURN_STOPPED_NOTE] if cancelled else []
            )

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


async def _create_workspace(
    case: PostgresTestCase,
    *,
    idle_minutes: int = 30,
    kagent_session_id: str | None = "ctx-1",
    ports: tuple[WorkspacePort, ...] = (WorkspacePort(name="web", number=5173),),
) -> str:
    """Seed a branch workspace as ``workspaces.create`` stores it, without calling kagent."""
    project_id = f"proj-{uuid.uuid4().hex[:8]}"
    repo_url = f"https://github.com/example/{project_id}"
    await case.pool.execute(
        """INSERT INTO projects (id,user_id,owner,name,full_name,html_url)
           VALUES ($1,$2,'example',$3,$4,$5)""",
        project_id,
        case.user,
        project_id,
        f"example/{project_id}",
        repo_url,
    )
    workspace_id = str(uuid.uuid4())
    branch = f"b-{uuid.uuid4().hex[:6]}"
    manifest = WorkspaceManifest(
        repo_url=repo_url,
        ref="main",
        branch=branch,
        agent_kind=WorkspaceAgentKind.CLAUDE,
        dev=WorkspaceDev(ports=ports, idle_timeout_minutes=idle_minutes),
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
                manifest.repo_url,
                project_id,
                branch,
                manifest.ref,
            )
            await conn.execute(
                """INSERT INTO workspaces
                   (session_id,repo,ref,branch,depth,ports,idle_timeout_minutes,created_at)
                   VALUES ($1,$2,$3,$4,0,$5::jsonb,$6,$7)""",
                workspace_id,
                manifest.repo_url,
                manifest.ref,
                branch,
                json.dumps([p.model_dump(mode="json") for p in ports]),
                idle_minutes,
                now,
            )
            await ns.create_binding(
                workspace_id,
                "claude",
                mcp_grant_kind="workspace",
                conn=conn,
            )
    if kagent_session_id:
        await ns.ledger.update_binding(
            workspace_id, kagent_session_id=kagent_session_id
        )
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


class KagentFakeCase(PostgresTestCase):
    """A scratch database plus ``ns``'s kagent client pointed at a fake gateway."""

    async def asyncSetUp(self):
        await super().asyncSetUp()
        from mainloop.runtime.agent_credentials import credentials

        self.credential_publish = AsyncMock(
            side_effect=lambda _binding_id, reference: reference
        )
        self.credential_publish_patch = patch.object(
            credentials, "publish", new=self.credential_publish
        )
        self.credential_publish_patch.start()
        self.addCleanup(self.credential_publish_patch.stop)
        self.fake = FakeKagent()
        http = httpx.AsyncClient(
            transport=self.fake.transport(), base_url="http://kagent.test"
        )
        self._saved_client = ns._client
        ns._client = KagentClient("http://kagent.test", user_id="mainloop", client=http)
        ns._locks.clear()

    async def asyncTearDown(self):
        await ns.close_client()
        ns._client = self._saved_client
        await super().asyncTearDown()

    async def cid(self, workspace_id: str) -> str:
        return await self.pool.fetchval(
            "SELECT conversation_id FROM sessions WHERE id=$1", workspace_id
        )


class WorkspaceTests(KagentFakeCase):
    async def _workspace_with_lost_create_reply(self, branch: str) -> str:
        project_id = f"proj-{uuid.uuid4().hex[:8]}"
        repo_url = f"https://github.com/example/{project_id}"
        await self.pool.execute(
            """INSERT INTO projects (id,user_id,owner,name,full_name,html_url)
               VALUES ($1,$2,'example',$3,$4,$5)""",
            project_id,
            self.user,
            project_id,
            f"example/{project_id}",
            repo_url,
        )
        client = ns.get_client()
        create = client.create_session
        calls = 0

        async def lose_reply(agent, **kwargs):
            nonlocal calls
            session = await create(agent, **kwargs)
            calls += 1
            if calls == 1:
                raise OutcomeUnknown("sanitized lost create reply")
            return session

        with patch.object(client, "create_session", lose_reply):
            lifecycle = await workspaces.create(
                self.user,
                project_id,
                WorkspaceManifest(repo_url=repo_url, branch=branch),
            )
        self.assertEqual(lifecycle.observed_state.value, "unknown")
        return lifecycle.workspace_id

    async def test_create_stores_the_rows_and_sends_the_workspace_to_kagent(self):
        from mainloop.runtime.agent_identity import token_for

        project_id = f"proj-{uuid.uuid4().hex[:8]}"
        repo_url = f"https://github.com/example/{project_id}"
        await self.pool.execute(
            """INSERT INTO projects (id,user_id,owner,name,full_name,html_url)
               VALUES ($1,$2,'example',$3,$4,$5)""",
            project_id,
            self.user,
            project_id,
            f"example/{project_id}",
            repo_url,
        )
        manifest = WorkspaceManifest(
            repo_url=repo_url,
            ref="main",
            branch="feature/x",
            depth=1,
            dev=WorkspaceDev(ports=(WorkspacePort(name="web", number=5173),)),
        )
        lifecycle = await workspaces.create(self.user, project_id, manifest)
        wid = lifecycle.workspace_id
        self.assertEqual(
            self.fake.created_workspaces(),
            [
                SessionWorkspace(
                    repo=repo_url,
                    ref="main",
                    branch="feature/x",
                    depth=1,
                )
            ],
        )
        binding = await ns.get_binding(wid)
        self.assertEqual(binding["role"], "agent")
        self.assertEqual(binding["mcp_grant_kind"], "workspace")
        self.assertTrue(binding["token_hash"])
        reference = (
            json.loads(binding["credential_ref"])
            if isinstance(binding["credential_ref"], str)
            else binding["credential_ref"]
        )
        self.assertEqual(reference["secret_key"], wid)
        self.assertEqual(binding["kagent_session_id"], CONTEXT_ID)
        create = decode_fields(self.fake.session_calls("CreateSession")[0])
        self.assertEqual(len(create[7]), 1)
        credential = decode_fields(create[7][0])
        self.assertEqual(
            credential[1], [b"http://mainloop-mcp.mainloop.svc.cluster.local"]
        )
        self.assertEqual(credential[2], [b"Authorization"])
        self.assertEqual(
            decode_fields(credential[3][0]),
            {1: [b"mainloop-agent-tokens"], 2: [wid.encode()]},
        )
        self.assertNotIn(
            token_for(wid).encode(), self.fake.session_calls("CreateSession")[0]
        )
        self.assertEqual(lifecycle.manifest, manifest)
        self.assertEqual(lifecycle.observed_state.value, "running")
        # The stored copy is what a replacement Session resends.
        self.assertEqual(
            await ns.ledger.get_workspace(wid),
            SessionWorkspace(
                repo=repo_url,
                ref="main",
                branch="feature/x",
                depth=1,
            ),
        )
        self.assertEqual(
            [
                w["session_id"]
                for w in await self.pool.fetch(
                    "SELECT session_id FROM workspaces WHERE session_id=$1", wid
                )
            ],
            [wid],
        )

    async def test_claude_and_codex_workspaces_get_distinct_frozen_references(self):
        from models import WorkspaceAgentKind

        project_id = f"proj-{uuid.uuid4().hex[:8]}"
        repo_url = f"https://github.com/example/{project_id}"
        await self.pool.execute(
            """INSERT INTO projects (id,user_id,owner,name,full_name,html_url)
               VALUES ($1,$2,'example',$3,$4,$5)""",
            project_id,
            self.user,
            project_id,
            f"example/{project_id}",
            repo_url,
        )
        self.fake.next_session_ids = ["claude-runtime", "codex-runtime"]
        session_ids = []
        for index, kind in enumerate(
            (WorkspaceAgentKind.CLAUDE, WorkspaceAgentKind.CODEX)
        ):
            lifecycle = await workspaces.create(
                self.user,
                project_id,
                WorkspaceManifest(
                    repo_url=repo_url,
                    branch=f"feature/{index}",
                    agent_kind=kind,
                ),
            )
            session_ids.append(lifecycle.workspace_id)
        requests = [
            decode_fields(raw) for raw in self.fake.session_calls("CreateSession")
        ]
        self.assertEqual(len(requests), 2)
        self.assertEqual([len(request[7]) for request in requests], [1, 1])
        references = [decode_fields(request[7][0]) for request in requests]
        keys = [decode_fields(reference[3][0])[2][0] for reference in references]
        self.assertEqual(keys, [value.encode() for value in session_ids])
        self.assertEqual(len(set(keys)), 2)

    async def test_workspace_grant_rejects_non_owner_or_mismatched_repository(self):
        from models import WorkspaceAgentKind

        project_id = f"proj-{uuid.uuid4().hex[:8]}"
        repo_url = f"https://github.com/example/{project_id}"
        await self.pool.execute(
            """INSERT INTO projects (id,user_id,owner,name,full_name,html_url)
               VALUES ($1,$2,'example',$3,$4,$5)""",
            project_id,
            self.user,
            project_id,
            f"example/{project_id}",
            repo_url,
        )
        with self.assertRaises(workspaces.WorkspaceConflict):
            await workspaces.create(
                self.user,
                project_id,
                WorkspaceManifest(
                    repo_url="https://github.com/example/other",
                    branch="feature/wrong-repo",
                    agent_kind=WorkspaceAgentKind.CODEX,
                ),
            )
        self.assertEqual(self.fake.session_calls("CreateSession"), [])

    async def test_publish_failure_keeps_a_pending_enrollment_without_a_create_or_brief(
        self,
    ):
        from mainloop.runtime.agent_credentials import credentials

        project_id = f"proj-{uuid.uuid4().hex[:8]}"
        repo_url = f"https://github.com/example/{project_id}"
        await self.pool.execute(
            """INSERT INTO projects (id,user_id,owner,name,full_name,html_url)
               VALUES ($1,$2,'example',$3,$4,$5)""",
            project_id,
            self.user,
            project_id,
            f"example/{project_id}",
            repo_url,
        )
        with patch.object(
            credentials,
            "publish",
            AsyncMock(side_effect=RuntimeError("secret publish failed")),
        ):
            with self.assertRaisesRegex(RuntimeError, "secret publish failed"):
                await workspaces.create(
                    self.user,
                    project_id,
                    WorkspaceManifest(repo_url=repo_url, branch="feature/pending"),
                )
        binding = await self.pool.fetchrow(
            """SELECT b.* FROM native_bindings b JOIN sessions s ON s.id=b.session_id
               WHERE s.user_id=$1 AND s.project_id=$2""",
            self.user,
            project_id,
        )
        self.assertEqual(binding["mcp_grant_kind"], "workspace")
        self.assertTrue(binding["token_hash"])
        self.assertIsNone(binding["kagent_session_id"])
        self.assertEqual(self.fake.session_calls("CreateSession"), [])
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_deliveries WHERE session_id=$1",
                binding["session_id"],
            ),
            0,
        )

    async def test_lost_create_reply_retries_the_same_request_and_reference(self):
        project_id = f"proj-{uuid.uuid4().hex[:8]}"
        repo_url = f"https://github.com/example/{project_id}"
        await self.pool.execute(
            """INSERT INTO projects (id,user_id,owner,name,full_name,html_url)
               VALUES ($1,$2,'example',$3,$4,$5)""",
            project_id,
            self.user,
            project_id,
            f"example/{project_id}",
            repo_url,
        )
        client = ns.get_client()
        create = client.create_session
        calls = []

        async def lose_first_reply(agent, **kwargs):
            calls.append(
                (kwargs["request_id"], kwargs["workspace"], kwargs["credentials"])
            )
            session = await create(agent, **kwargs)
            if len(calls) == 1:
                raise OutcomeUnknown("sanitized lost create reply")
            return session

        with patch.object(client, "create_session", lose_first_reply):
            lifecycle = await workspaces.create(
                self.user,
                project_id,
                WorkspaceManifest(repo_url=repo_url, branch="feature/reconcile"),
            )
            self.assertEqual(lifecycle.observed_state.value, "unknown")
            await workspaces.refresh(lifecycle.workspace_id, self.user)
        self.assertEqual(len(calls), 2)
        self.assertEqual(calls[0], calls[1])
        self.assertEqual(len(self.fake.created_request_ids), 1)
        creates = self.fake.session_calls("CreateSession")
        self.assertEqual(len(creates), 2)
        fields = [decode_fields(raw) for raw in creates]
        self.assertEqual(fields[0][3], fields[1][3])
        self.assertEqual(fields[0][6], fields[1][6])
        self.assertEqual(fields[0][7], fields[1][7])

    async def test_cancelled_unknown_create_remains_deletable_without_republishing(
        self,
    ):
        wid = await self._workspace_with_lost_create_reply("feature/cancel-delete")
        self.assertEqual(len(self.fake.created_request_ids), 1)

        self.assertEqual(await ns.cancel(wid), "not_running")
        cancelled = await ns.get_binding(wid)
        self.assertIsNone(cancelled["token_hash"])
        self.assertEqual(cancelled["mcp_grant_kind"], "workspace")

        await workspaces.delete(wid, self.user)

        self.assertIsNone(await ns.get_binding(wid))
        self.assertEqual(len(self.fake.created_request_ids), 1)
        creates = self.fake.session_calls("CreateSession")
        self.assertEqual(len(creates), 2)
        first, second = (decode_fields(raw) for raw in creates)
        self.assertEqual(first[3], second[3])
        self.assertEqual(first[6], second[6])
        self.assertEqual(first[7], second[7])
        self.assertEqual(len(self.fake.session_calls("DeleteSession")), 1)
        self.assertEqual(self.fake.rpc_calls("SendStreamingMessage"), [])
        self.credential_publish.assert_awaited_once()

    async def test_cancelled_unknown_create_refresh_uses_frozen_reference(self):
        wid = await self._workspace_with_lost_create_reply("feature/cancel-refresh")
        self.assertEqual(await ns.cancel(wid), "not_running")

        refreshed = await workspaces.refresh(wid, self.user)

        self.assertEqual(refreshed.observed_state.value, "running")
        binding = await ns.get_binding(wid)
        self.assertEqual(binding["kagent_session_id"], CONTEXT_ID)
        self.assertIsNone(binding["token_hash"])
        self.assertEqual(len(self.fake.created_request_ids), 1)
        creates = self.fake.session_calls("CreateSession")
        self.assertEqual(len(creates), 2)
        first, second = (decode_fields(raw) for raw in creates)
        self.assertEqual(first[3], second[3])
        self.assertEqual(first[6], second[6])
        self.assertEqual(first[7], second[7])
        self.assertEqual(self.fake.rpc_calls("SendStreamingMessage"), [])
        self.credential_publish.assert_awaited_once()

        await workspaces.delete(wid, self.user)
        self.assertEqual(len(self.fake.session_calls("DeleteSession")), 1)
        self.assertIsNone(await ns.get_binding(wid))

    async def test_suspend_and_resume_keep_the_enrolled_identity(self):
        wid = await self._create_workspace_for_test()
        before = await ns.get_binding(wid)
        await workspaces.suspend(wid, self.user)
        await workspaces.resume(wid, self.user)
        after = await ns.get_binding(wid)
        self.assertEqual(after["mcp_grant_kind"], "workspace")
        self.assertEqual(after["token_hash"], before["token_hash"])

    async def _create_workspace_for_test(self):
        project_id = f"proj-{uuid.uuid4().hex[:8]}"
        repo_url = f"https://github.com/example/{project_id}"
        await self.pool.execute(
            """INSERT INTO projects (id,user_id,owner,name,full_name,html_url)
               VALUES ($1,$2,'example',$3,$4,$5)""",
            project_id,
            self.user,
            project_id,
            f"example/{project_id}",
            repo_url,
        )
        return (
            await workspaces.create(
                self.user,
                project_id,
                WorkspaceManifest(repo_url=repo_url, branch="feature/suspend"),
            )
        ).workspace_id

    async def test_a_create_kagent_rejects_leaves_nothing_behind(self):
        project_id = f"proj-{uuid.uuid4().hex[:8]}"
        repo_url = f"https://github.com/example/{project_id}"
        await self.pool.execute(
            """INSERT INTO projects (id,user_id,owner,name,full_name,html_url)
               VALUES ($1,$2,'example',$3,$4,$5)""",
            project_id,
            self.user,
            project_id,
            f"example/{project_id}",
            repo_url,
        )
        manifest = WorkspaceManifest(repo_url=repo_url, branch="feature/x")
        with patch.object(
            ns.get_client(),
            "create_session",
            AsyncMock(side_effect=SessionError("origin not allowed", grpc_status=3)),
        ):
            with self.assertRaises(workspaces.WorkspaceRejected):
                await workspaces.create(self.user, project_id, manifest)
        for query in (
            "SELECT count(*) FROM workspaces w JOIN sessions s ON s.id=w.session_id WHERE s.user_id=$1",
            "SELECT count(*) FROM sessions WHERE user_id=$1 AND project_id IS NOT NULL",
        ):
            self.assertEqual(await self.pool.fetchval(query, self.user), 0, query)

    async def test_a_create_with_an_unknown_outcome_keeps_the_rows_and_refresh_retries(
        self,
    ):
        project_id = f"proj-{uuid.uuid4().hex[:8]}"
        repo_url = f"https://github.com/example/{project_id}"
        await self.pool.execute(
            """INSERT INTO projects (id,user_id,owner,name,full_name,html_url)
               VALUES ($1,$2,'example',$3,$4,$5)""",
            project_id,
            self.user,
            project_id,
            f"example/{project_id}",
            repo_url,
        )
        manifest = WorkspaceManifest(repo_url=repo_url, branch="feature/x")
        with patch.object(
            ns.get_client(),
            "create_session",
            AsyncMock(side_effect=Unreachable("down")),
        ):
            lifecycle = await workspaces.create(self.user, project_id, manifest)
        self.assertEqual(lifecycle.observed_state.value, "unknown")
        wid = lifecycle.workspace_id
        self.assertIsNone((await ns.get_binding(wid))["kagent_session_id"])
        refreshed = await workspaces.refresh(wid, self.user)
        self.assertEqual(refreshed.observed_state.value, "running")
        self.assertEqual((await ns.get_binding(wid))["kagent_session_id"], CONTEXT_ID)

    async def test_the_lifecycle_is_owner_scoped_and_reports_last_activity(self):
        wid = await _create_workspace(self)
        self.fake.sessions["ctx-1"] = (RuntimeState.SUSPENDED, RuntimeOperation.NONE)
        lifecycle = await workspaces.get(wid, self.user)
        self.assertEqual(lifecycle.observed_state.value, "suspended")
        self.assertEqual(
            [w.workspace_id for w in await workspaces.list_for(self.user)], [wid]
        )
        with self.assertRaises(workspaces.WorkspaceNotFound):
            await workspaces.get(wid, "someone-else")
        self.assertEqual(await workspaces.list_for("someone-else"), [])
        # A delivery newer than the workspace counts as activity.
        before = lifecycle.last_activity_at
        await self.delivery(wid, await self.cid(wid), "completed")
        after = (await workspaces.get(wid, self.user)).last_activity_at
        self.assertGreaterEqual(after, before)

    async def test_delete_removes_native_rows_and_the_session_after_kagent_confirms(
        self,
    ):
        wid = await _create_workspace(self)
        self.fake.sessions["ctx-1"] = (RuntimeState.READY, RuntimeOperation.NONE)
        cid = await self.cid(wid)
        mid = await self.delivery(wid, cid, "completed")
        await workspaces.delete(wid, self.user)
        self.assertEqual(self.fake.sessions["ctx-1"][0], RuntimeState.DELETED)
        for query in (
            "SELECT count(*) FROM native_deliveries WHERE session_id=$1",
            "SELECT count(*) FROM native_bindings WHERE session_id=$1",
            "SELECT count(*) FROM workspaces WHERE session_id=$1",
            "SELECT count(*) FROM sessions WHERE id=$1",
        ):
            self.assertEqual(await self.pool.fetchval(query, wid), 0, query)
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

    async def test_delete_keeps_secret_cleanup_durable_after_binding_rows_are_removed(
        self,
    ):
        from fastapi import HTTPException
        from mainloop.runtime.agent_credentials import credentials, reconcile_cleanup
        from mainloop.runtime.agent_identity import token_for
        from mainloop.runtime.agent_tools import AgentService
        from mainloop.runtime.delegation import PgStore

        wid = await self._create_workspace_for_test()
        with patch.object(
            credentials,
            "remove",
            AsyncMock(side_effect=RuntimeError("sanitized Secret outage")),
        ):
            await workspaces.delete(wid, self.user)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_bindings WHERE session_id=$1", wid
            ),
            0,
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM agent_credential_cleanup WHERE session_id=$1",
                wid,
            ),
            1,
        )
        with self.assertRaises(HTTPException):
            await AgentService(PgStore()).authenticate(token_for(wid))
        with patch.object(credentials, "remove", AsyncMock()) as remove:
            await reconcile_cleanup()
        remove.assert_awaited_once()
        self.assertEqual(remove.await_args.args[0], wid)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM agent_credential_cleanup WHERE session_id=$1",
                wid,
            ),
            0,
        )

    async def test_delete_is_refused_while_a_delivery_is_open(self):
        wid = await _create_workspace(self)
        await self.delivery(wid, await self.cid(wid), "sending")
        with self.assertRaises(workspaces.WorkspaceConflict):
            await workspaces.delete(wid, self.user)
        self.assertIsNotNone(await ns.get_binding(wid))
        self.assertEqual(self.fake.session_calls("DeleteSession"), [])

    async def test_delete_keeps_the_rows_when_kagent_does_not_confirm(self):
        wid = await _create_workspace(self)
        with patch.object(
            ns.get_client(), "delete_session", AsyncMock(side_effect=Unreachable("x"))
        ):
            with self.assertRaises(workspaces.WorkspaceUnconfirmed):
                await workspaces.delete(wid, self.user)
        self.assertIsNotNone(await ns.get_binding(wid))

    async def test_archive_deletes_the_kagent_session_and_a_failed_delete_is_retried(
        self,
    ):
        wid = await _create_workspace(self)
        self.fake.sessions["ctx-1"] = (RuntimeState.READY, RuntimeOperation.NONE)
        # Only finished sessions can be archived.
        await self.pool.execute(
            "UPDATE sessions SET status='completed' WHERE id=$1", wid
        )
        with patch.object(
            ns.get_client(), "delete_session", AsyncMock(side_effect=Unreachable("x"))
        ):
            await db.archive_sessions(self.user, session_ids=[wid])
        self.assertIsNone(
            (
                await self.pool.fetchrow(
                    "SELECT kagent_deleted_at FROM native_bindings WHERE session_id=$1",
                    wid,
                )
            )["kagent_deleted_at"]
        )
        self.assertEqual(
            [r["session_id"] for r in await ns.ledger.undeleted_archived()], [wid]
        )
        await ns.reconcile_archived_deletes()
        self.assertEqual(self.fake.sessions["ctx-1"][0], RuntimeState.DELETED)
        self.assertEqual(await ns.ledger.undeleted_archived(), [])

    async def test_preview_target_is_owner_scoped_and_uses_the_stored_ports(self):
        wid = await _create_workspace(self)
        target = await preview_proxy._resolve_target(wid, self.user)
        self.assertEqual((target.actor, target.ports), ("session-ctx-1", {5173: "web"}))
        self.assertIsNone(await preview_proxy._resolve_target(wid, "someone-else"))
        self.assertIsNone(await preview_proxy._resolve_target("missing", self.user))

    async def test_preview_has_no_target_before_kagent_created_the_session(self):
        wid = await _create_workspace(self, kagent_session_id=None)
        self.assertIsNone(await preview_proxy._resolve_target(wid, self.user))

    async def test_preview_traffic_restarts_the_idle_clock(self):
        wid = await _create_workspace(self)
        await self.pool.execute(
            "UPDATE workspaces SET created_at=NOW() - INTERVAL '1 hour' WHERE session_id=$1",
            wid,
        )
        old = (await workspaces.get(wid, self.user)).last_activity_at
        await workspaces.touch(wid)
        self.assertGreater((await workspaces.get(wid, self.user)).last_activity_at, old)

    async def test_idle_selection(self):
        idle = await _create_workspace(self, idle_minutes=5, kagent_session_id="ctx-i")
        recent = await _create_workspace(
            self, idle_minutes=5, kagent_session_id="ctx-r"
        )
        young = await _create_workspace(
            self, idle_minutes=120, kagent_session_id="ctx-y"
        )
        nothing = await _create_workspace(self, idle_minutes=5, kagent_session_id=None)
        for wid in (idle, recent, young, nothing):
            await self.pool.execute(
                "UPDATE workspaces SET created_at=NOW() - INTERVAL '1 hour' WHERE session_id=$1",
                wid,
            )
        await self.pool.execute(
            "UPDATE workspaces SET last_active_at=NOW() WHERE session_id=$1", recent
        )
        asked: list[str] = []

        async def idle_suspend(session_id, kagent_session_id):
            asked.append(session_id)
            return True

        with (
            patch.object(workspaces, "_idle_suspend", idle_suspend),
            patch.object(workspaces, "publish", AsyncMock()),
        ):
            suspended = await workspaces.suspend_idle()
            self.assertEqual(set(suspended) & {idle, recent, young, nothing}, {idle})
            # Suspended and quiet since: not asked again.
            asked.clear()
            await workspaces.suspend_idle()
            self.assertNotIn(idle, asked)
            # A resume (activity newer than the idle suspend) makes it a candidate again.
            await self.pool.execute(
                "UPDATE workspaces SET last_active_at=NOW() - INTERVAL '10 minutes',"
                " idle_suspended_at=NOW() - INTERVAL '20 minutes' WHERE session_id=$1",
                idle,
            )
            await workspaces.suspend_idle()
            self.assertIn(idle, asked)

    async def test_the_main_thread_never_idles_out(self):
        main = await ensure_main_session(self.user)
        await ns.ledger.update_binding(main["session_id"], kagent_session_id="ctx-main")
        # No workspace row: not a candidate. Even given one, the role keeps it out.
        wid = await _create_workspace(self, idle_minutes=5, kagent_session_id="ctx-m2")
        await self.pool.execute(
            "UPDATE workspaces SET created_at=NOW() - INTERVAL '1 hour' WHERE session_id=$1",
            wid,
        )
        await self.pool.execute(
            "UPDATE native_bindings SET role='main' WHERE session_id=$1", wid
        )
        asked: list[str] = []

        async def idle_suspend(session_id, kagent_session_id):
            asked.append(session_id)
            return True

        with (
            patch.object(workspaces, "_idle_suspend", idle_suspend),
            patch.object(workspaces, "publish", AsyncMock()),
        ):
            await workspaces.suspend_idle()
        self.assertNotIn(main["session_id"], asked)
        self.assertNotIn(wid, asked)
        with patch.object(ns, "get_client") as client:
            with self.assertRaises(workspaces.WorkspaceConflict):
                await workspaces.suspend_if_quiet(wid)
            client.assert_not_called()

    async def test_idle_check_rereads_activity_under_the_lock(self):
        wid = await _create_workspace(self, idle_minutes=5, kagent_session_id="ctx-r1")
        await self.pool.execute(
            "UPDATE workspaces SET created_at=NOW() - INTERVAL '1 hour' WHERE session_id=$1",
            wid,
        )
        self.fake.sessions["ctx-r1"] = (RuntimeState.READY, RuntimeOperation.NONE)
        self.assertTrue(await workspaces._is_idle(wid))
        real = ns._live_session

        async def preview_lands_after_the_select(kagent_session_id):
            if kagent_session_id == "ctx-r1":
                await workspaces.touch(wid)
            return await real(kagent_session_id)

        def suspends() -> list[bytes]:
            return [
                c for c in self.fake.session_calls("SuspendSession") if b"ctx-r1" in c
            ]

        with (
            patch.object(ns, "_live_session", preview_lands_after_the_select),
            patch.object(workspaces, "publish", AsyncMock()),
        ):
            suspended = await workspaces.suspend_idle()
        self.assertNotIn(wid, suspended)
        self.assertEqual(suspends(), [])
        self.assertFalse(await workspaces._is_idle(wid))
        self.assertIsNone(
            await self.pool.fetchval(
                "SELECT idle_suspended_at FROM workspaces WHERE session_id=$1", wid
            ),
            "a refused idle suspend must not restart the debounce",
        )
        # Quiet again: the next pass suspends it.
        await self.pool.execute(
            "UPDATE workspaces SET last_active_at=NOW() - INTERVAL '10 minutes' WHERE session_id=$1",
            wid,
        )
        with patch.object(workspaces, "publish", AsyncMock()):
            self.assertIn(wid, await workspaces.suspend_idle())
        self.assertEqual(len(suspends()), 1)

    async def test_a_recent_delivery_counts_as_activity_for_idle_out(self):
        wid = await _create_workspace(self, idle_minutes=5)
        await self.pool.execute(
            "UPDATE workspaces SET created_at=NOW() - INTERVAL '1 hour' WHERE session_id=$1",
            wid,
        )
        await self.delivery(wid, await self.cid(wid), "completed")
        asked: list[str] = []

        async def idle_suspend(session_id, kagent_session_id):
            asked.append(session_id)
            return False

        with patch.object(workspaces, "_idle_suspend", idle_suspend):
            await workspaces.suspend_idle()
        self.assertNotIn(wid, asked)


class DatabaseFromBeforeTheKagentWorkspacesTests(KagentFakeCase):
    """A database created before ``native_bindings.kagent_deleted_at`` existed.

    ``CREATE TABLE IF NOT EXISTS`` leaves such a table alone, so the column has to come from an
    ``ALTER`` in the schema. The paths that read it are exercised: workspace reads, reconcile and
    idle-out, and archive.
    """

    @classmethod
    def setUpClass(cls):
        super().setUpClass()
        asyncio.run(
            _admin(cls.url, "ALTER TABLE native_bindings DROP COLUMN kagent_deleted_at")
        )
        asyncio.run(_expect_column_missing(cls.url))
        asyncio.run(_init_schema(cls.url))  # what Database.connect runs at startup

    def test_the_column_is_added_and_the_schema_can_be_applied_again(self):
        asyncio.run(_init_schema(self.url))
        self.assertEqual(
            asyncio.run(
                _column_count(self.url, "native_bindings", "kagent_deleted_at")
            ),
            1,
        )

    async def test_workspace_reads_work(self):
        wid = await _create_workspace(self)
        self.assertEqual((await workspaces.get(wid, self.user)).workspace_id, wid)
        self.assertEqual(
            [w.workspace_id for w in await workspaces.list_for(self.user)], [wid]
        )
        self.assertIsNotNone(await preview_proxy._resolve_target(wid, self.user))

    async def test_reconcile_and_idle_out_work(self):
        wid = await _create_workspace(self, idle_minutes=5, kagent_session_id="ctx-old")
        await self.pool.execute(
            "UPDATE workspaces SET created_at=NOW() - INTERVAL '1 hour' WHERE session_id=$1",
            wid,
        )
        self.fake.sessions["ctx-old"] = (RuntimeState.READY, RuntimeOperation.NONE)
        await ns.reconcile_archived_deletes()
        with patch.object(workspaces, "publish", AsyncMock()):
            self.assertIn(wid, await workspaces.suspend_idle())
        self.assertEqual(self.fake.sessions["ctx-old"][0], RuntimeState.SUSPENDED)

    async def test_archive_deletes_the_kagent_session(self):
        wid = await _create_workspace(self, kagent_session_id="ctx-arch")
        self.fake.sessions["ctx-arch"] = (RuntimeState.READY, RuntimeOperation.NONE)
        await self.pool.execute(
            "UPDATE sessions SET status='completed' WHERE id=$1", wid
        )
        await db.archive_sessions(self.user, session_ids=[wid])
        self.assertEqual(self.fake.sessions["ctx-arch"][0], RuntimeState.DELETED)
        self.assertIsNotNone(
            await self.pool.fetchval(
                "SELECT kagent_deleted_at FROM native_bindings WHERE session_id=$1", wid
            )
        )


class StopTurnTests(PostgresTestCase):
    """``stop_turn`` over the real ledger SQL and a fake kagent."""

    async def asyncSetUp(self):
        await super().asyncSetUp()
        self.fake = FakeKagent()
        http = httpx.AsyncClient(
            transport=self.fake.transport(), base_url="http://kagent.test"
        )
        self._saved_client = ns._client
        ns._client = KagentClient("http://kagent.test", user_id="mainloop", client=http)
        ns._locks.clear()
        ns._streaming.clear()

    async def asyncTearDown(self):
        await asyncio.gather(*ns._tasks, return_exceptions=True)
        await ns.close_client()
        ns._client = self._saved_client
        await super().asyncTearDown()

    async def send(self, sid: str, text: str = "hello") -> str:
        mid = await ns.submit_message(sid, text)
        while ns._tasks:
            await asyncio.gather(*list(ns._tasks), return_exceptions=True)
        return mid

    async def rows(self, cid: str) -> list[tuple[str, str]]:
        return [
            (r["role"], r["content"])
            for r in await self.pool.fetch(
                "SELECT role, content FROM messages WHERE conversation_id=$1 ORDER BY created_at",
                cid,
            )
        ]

    async def test_stop_keeps_the_session_and_the_next_message_is_a_fresh_turn(self):
        sid, cid = await self.bound_session()
        self.fake.send_script = ["cut"]
        first = await self.send(sid)
        self.assertEqual(await self.state_of(first), "delivered")
        info = await ns.identity(sid)
        self.assertTrue(info.turn_in_flight)

        self.assertEqual(await ns.stop_turn(sid), "stopped")

        self.assertEqual(await self.state_of(first), "cancelled")
        self.assertEqual(
            await self.pool.fetchval("SELECT status FROM sessions WHERE id=$1", sid),
            "waiting_on_user",
        )
        self.assertFalse((await ns.identity(sid)).turn_in_flight)
        self.assertIn(("assistant", ns.stopped_message("ok")), await self.rows(cid))
        self.assertEqual(await ns.stop_turn(sid), "no_open_turn")
        binding = await ns.get_binding(sid)
        self.assertEqual(binding["kagent_session_id"], CONTEXT_ID)

        second = await self.send(sid, "again")
        self.assertEqual(await self.state_of(second), "completed")
        sent = self.fake.rpc_calls("SendStreamingMessage")[-1]["params"]["message"]
        self.assertEqual(sent["contextId"], CONTEXT_ID)
        self.assertEqual(len(self.fake.session_calls("CreateSession")), 1)
        self.assertEqual(self.fake.session_calls("DeleteSession"), [])

    async def test_a_kagent_error_changes_nothing_in_the_database(self):
        sid, cid = await self.bound_session()
        self.fake.send_script = ["cut"]
        first = await self.send(sid)
        snapshot = (
            await self.pool.fetchrow(
                "SELECT * FROM native_deliveries WHERE message_id=$1", first
            ),
            await self.rows(cid),
        )
        self.fake.cancel_task_fails = True
        with self.assertRaises(KagentError):
            await ns.stop_turn(sid)
        self.assertEqual(
            (
                await self.pool.fetchrow(
                    "SELECT * FROM native_deliveries WHERE message_id=$1", first
                ),
                await self.rows(cid),
            ),
            snapshot,
        )
        self.assertEqual(await self.state_of(first), "delivered")

    async def test_stop_that_loses_to_completion_records_completed_with_the_reply(self):
        sid, cid = await self.bound_session()
        self.fake.send_script = ["cut"]
        first = await self.send(sid)
        self.fake.tasks["task-fixture-1"]["status"] = {"state": "TASK_STATE_COMPLETED"}
        self.assertEqual(await ns.stop_turn(sid), "finished")
        self.assertEqual(await self.state_of(first), "completed")
        self.assertNotIn(("assistant", ns.TURN_STOPPED_NOTE), await self.rows(cid))


class DeliveryFailureReasonTests(PostgresTestCase):
    """The reason a delivery failed is stored with it and reaches the owner through the API."""

    async def asyncSetUp(self):
        await super().asyncSetUp()
        self.fake = FakeKagent()
        http = httpx.AsyncClient(
            transport=self.fake.transport(), base_url="http://kagent.test"
        )
        self._saved_client = ns._client
        ns._client = KagentClient("http://kagent.test", user_id="mainloop", client=http)
        ns._locks.clear()
        ns._streaming.clear()

    async def asyncTearDown(self):
        await asyncio.gather(*ns._tasks, return_exceptions=True)
        await ns.close_client()
        ns._client = self._saved_client
        await super().asyncTearDown()

    async def send(self, sid: str, text: str = "hello") -> str:
        mid = await ns.submit_message(sid, text)
        while ns._tasks:
            await asyncio.gather(*list(ns._tasks), return_exceptions=True)
        return mid

    async def stored(self, mid: str):
        return await self.pool.fetchrow(
            "SELECT state, detail FROM native_deliveries WHERE message_id=$1", mid
        )

    async def test_a_message_never_sent_keeps_the_kagent_error_class_and_message(self):
        sid, _ = await self.bound_session()
        failed = "00000000-0000-4000-8000-0000000000ff"
        self.fake.sessions[failed] = (RuntimeState.FAILED, RuntimeOperation.NONE)
        await ns.ledger.update_binding(sid, kagent_session_id=failed)
        mid = await self.send(sid)
        row = await self.stored(mid)
        self.assertEqual(row["state"], "failed")
        self.assertTrue(row["detail"].startswith("not sent: SessionError: "))
        self.assertEqual(self.fake.rpc_calls("SendStreamingMessage"), [])
        # The same text reaches the UI, next to the message it belongs to.
        info = await ns.identity(sid)
        (delivery,) = info.deliveries
        self.assertEqual(
            (delivery.message_id, delivery.state, delivery.detail),
            (mid, "failed", row["detail"]),
        )
        self.assertFalse(info.turn_in_flight)

    async def test_a_kagent_rejection_stores_the_a2a_error_class_and_reason(self):
        sid, _ = await self.bound_session()
        self.fake.send_script = ["other-error"]
        mid = await self.send(sid)
        row = await self.stored(mid)
        self.assertEqual(row["state"], "failed")
        self.assertEqual(
            row["detail"], "send rejected: A2AError (INVALID_PARAMS): invalid params"
        )

    async def test_a_failed_task_stores_its_reason_without_credentials(self):
        sid, _ = await self.bound_session()
        self.fake.send_script = ["cut"]
        mid = await self.send(sid)
        self.fake.tasks["task-fixture-1"]["status"] = {
            "state": "TASK_STATE_FAILED",
            "message": {
                "messageId": "x",
                "parts": [
                    {
                        "text": "claude exited with an error: exit status 1\n"
                        "Authorization: Bearer abc.def-123 token=hunter2 "
                        "https://user:pw@example.test/x"
                    }
                ],
            },
        }
        self.fake.tasks["task-fixture-1"]["artifacts"] = []
        await ns.sync(sid)
        row = await self.stored(mid)
        self.assertEqual(row["state"], "failed")
        self.assertIn("claude exited with an error: exit status 1", row["detail"])
        for secret in ("abc.def-123", "hunter2", "user:pw"):
            self.assertNotIn(secret, row["detail"])
        self.assertNotIn("\n", row["detail"])

    async def test_a_failed_task_with_no_reason_says_so(self):
        sid, _ = await self.bound_session()
        self.fake.send_script = ["cut"]
        mid = await self.send(sid)
        self.fake.tasks["task-fixture-1"]["status"] = {"state": "TASK_STATE_FAILED"}
        self.fake.tasks["task-fixture-1"]["artifacts"] = []
        await ns.sync(sid)
        self.assertEqual(
            (await self.stored(mid))["detail"], "task failed: kagent gave no reason"
        )

    async def test_the_ledger_bounds_every_reason_it_stores(self):
        sid, cid = await self.bound_session()
        long = "x " * 1000
        sending = await self.delivery(sid, cid, "sending")
        await ns.ledger.transition(
            sending, "failed", from_states=("sending",), detail=long
        )
        queued = await self.delivery(sid, cid, "queued")
        await ns.ledger.fail_open(sid, long)
        for mid in (sending, queued):
            detail = (await self.stored(mid))["detail"]
            self.assertLessEqual(len(detail), ns.DETAIL_MAX_CHARS)
            self.assertTrue(detail.endswith("…"))

    async def test_rows_written_before_sanitising_are_cleaned_on_read(self):
        sid, cid = await self.bound_session()
        mid = await self.delivery(sid, cid, "failed")
        await self.pool.execute(
            "UPDATE native_deliveries SET detail=$2 WHERE message_id=$1",
            mid,
            "not sent: boom\npassword: hunter2 " + "y " * 600,
        )
        (delivery,) = (await ns.identity(sid)).deliveries
        self.assertNotIn("hunter2", delivery.detail)
        self.assertNotIn("\n", delivery.detail)
        self.assertLessEqual(len(delivery.detail), ns.DETAIL_MAX_CHARS)

    async def test_credential_formats_on_all_writes_and_legacy_reads(self):
        credentials = (
            ('{"password": "synthetic secret value"}', "synthetic secret value"),
            ("{'token': 'synthetic-token'}", "synthetic-token"),
            ("Authorization: Basic dXNlcjpwYXNz", "dXNlcjpwYXNz"),
            ("Authorization: Digest synthetic-auth", "synthetic-auth"),
            ("SERVICE_TOKEN=synthetic-token", "synthetic-token"),
            ('OPENAI_API_KEY="synthetic key value"', "synthetic key value"),
            ("CLIENT_SECRET=synthetic-secret", "synthetic-secret"),
            ("PASSWORD=synthetic-password", "synthetic-password"),
            ("Bearer synthetic-bearer", "synthetic-bearer"),
            ("sk-proj-abcdefghijk", "sk-proj-abcdefghijk"),
        )
        for credential, secret in credentials:
            for path in ("transition", "fail_open", "settle_cancelled", "legacy"):
                with self.subTest(credential=credential, path=path):
                    sid, cid = await self.bound_session()
                    mid = await self.delivery(sid, cid, "sending")
                    detail = "task failed: " + credential
                    if path == "transition":
                        await ns.ledger.transition(
                            mid, "failed", from_states=("sending",), detail=detail
                        )
                    elif path == "fail_open":
                        await ns.ledger.fail_open(sid, detail)
                    elif path == "settle_cancelled":
                        await ns.ledger.settle_cancelled(
                            mid,
                            from_states=("sending",),
                            conversation_id=cid,
                            note_id=f"note-{mid}",
                            note=ns.TURN_STOPPED_NOTE,
                            detail=detail,
                        )
                    else:
                        await self.pool.execute(
                            "UPDATE native_deliveries SET state='failed', detail=$2 WHERE message_id=$1",
                            mid,
                            detail,
                        )
                    if path != "legacy":
                        self.assertNotIn(secret, (await self.stored(mid))["detail"])
                    (delivery,) = (await ns.identity(sid)).deliveries
                    self.assertNotIn(secret, delivery.detail)
                    self.assertIn("[redacted]", delivery.detail)

    async def test_the_main_thread_endpoint_carries_the_failure_for_the_header(self):
        from mainloop import api

        main = await ensure_main_session(self.user)
        sid = main["session_id"]
        self.fake.sessions[CONTEXT_ID] = (RuntimeState.READY, RuntimeOperation.NONE)
        await ns.ledger.update_binding(sid, kagent_session_id=CONTEXT_ID)
        self.fake.send_script = ["unreachable"]
        mid = await self.send(sid)
        info = await api.get_main_thread_info(user_id=self.user)
        native = info.native
        self.assertFalse(native.turn_in_flight)
        (delivery,) = native.deliveries
        self.assertEqual((delivery.message_id, delivery.state), (mid, "failed"))
        self.assertTrue(delivery.detail.startswith("not sent: "), delivery.detail)
        self.assertIn("Unreachable", delivery.detail)
        # The failed message is still in the conversation, so the UI can pin the reason to it.
        cid = (await db.get_session(sid)).conversation_id
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM messages WHERE id=$1 AND conversation_id=$2",
                mid,
                cid,
            ),
            1,
        )

    async def test_a_retry_is_a_new_message_and_never_resends_the_failed_id(self):
        sid, _ = await self.bound_session()
        self.fake.send_script = ["unreachable", "ok"]
        first = await self.send(sid)
        self.assertEqual((await self.stored(first))["state"], "failed")
        second = await self.send(sid)
        self.assertNotEqual(first, second)
        self.assertEqual((await self.stored(second))["state"], "completed")
        # The failed one stays failed and is not requeued.
        await ns.sync(sid)
        self.assertEqual((await self.stored(first))["state"], "failed")
        # kagent accepted only the retry; the failed id never reached it a second time.
        self.assertEqual(self.fake.accepted_message_ids, [second])


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
            patch("mainloop.runtime.hitl_observer.observe_hitl_once", new=AsyncMock()),
            patch(
                "mainloop.runtime.hitl_continuation.reconcile_hitl_responses",
                new=AsyncMock(),
            ),
            patch.object(
                workspaces, "suspend_idle", new=AsyncMock(return_value=[])
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
    async def test_project_detail_returns_only_the_projects_visible_owner_sessions(
        self,
    ):
        project = await db.get_or_create_project(self.user, GithubRepo("owner", "repo"))
        other_project = await db.get_or_create_project(
            self.user, GithubRepo("owner", "other")
        )
        older, _ = await self.bound_session("claude", "active")
        newer, _ = await self.bound_session("codex")
        archived, _ = await self.session("completed")
        unrelated, _ = await self.session()
        await self.session()  # No project association.
        foreign, _ = await self.session(user=f"other-{self.user}")
        main = await ensure_main_session(self.user)
        await self.pool.execute(
            "UPDATE sessions SET project_id=$1 WHERE id=ANY($2::text[])",
            project.id,
            [older, newer, archived, foreign, main["session_id"]],
        )
        await db.update_session(unrelated, project_id=other_project.id)
        await self.pool.execute(
            "UPDATE sessions SET created_at=NOW() - INTERVAL '1 day' WHERE id=$1",
            older,
        )
        await db.archive_sessions(self.user, [archived])

        with (
            patch.object(settings, "owner_id", self.user),
            patch.object(settings, "api_hosts", "test"),
            patch.object(api, "list_open_prs", AsyncMock(return_value=[])) as prs,
            patch.object(
                api, "list_recent_commits", AsyncMock(return_value=[])
            ) as commits,
        ):
            async with httpx.AsyncClient(
                transport=httpx.ASGITransport(app=api.app, raise_app_exceptions=False),
                base_url="http://test",
            ) as client:
                response = await client.get(f"/projects/{project.id}/detail")
        self.assertEqual(response.status_code, 200, response.text)
        body = response.json()
        self.assertEqual(body["project"]["id"], project.id)
        self.assertEqual(body["open_prs"], [])
        self.assertEqual(body["recent_commits"], [])
        self.assertEqual([s["id"] for s in body["sessions"]], [newer, older])
        self.assertEqual(
            [s["agent_kind"] for s in body["sessions"]], ["codex", "claude"]
        )
        prs.assert_awaited_once_with(project.html_url, limit=10)
        commits.assert_awaited_once_with(project.html_url, branch=None, limit=10)

        filtered = await db.list_sessions(
            self.user, status=SessionStatus.ACTIVE, limit=1, project_id=project.id
        )
        self.assertEqual([s.id for s in filtered], [older])
        limited = await db.list_sessions(self.user, limit=1, project_id=project.id)
        self.assertEqual([s.id for s in limited], [newer])
        including_archived = await db.list_sessions(
            self.user, include_archived=True, project_id=project.id
        )
        self.assertEqual({s.id for s in including_archived}, {older, newer, archived})

    async def test_session_summaries_include_standalone_agents_and_runtime_kind(self):
        main = await ensure_main_session(self.user)
        standalone, _ = await self.session()
        child, _ = await self.session()
        legacy, _ = await self.session()
        archived, _ = await self.session("completed")
        await ns.create_binding(standalone, "codex")
        await ns.create_binding(
            child, "claude", role="child", parent_session_id=main["session_id"]
        )
        await db.archive_sessions(self.user, [archived])

        sessions = {s.id: s for s in await db.list_sessions(self.user)}
        self.assertEqual(set(sessions), {standalone, child, legacy})
        self.assertEqual(sessions[standalone].agent_kind, "codex")
        self.assertEqual(sessions[child].agent_kind, "claude")
        self.assertIsNone(sessions[legacy].agent_kind)
        self.assertEqual((await db.get_session(standalone)).agent_kind, "codex")

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
            response = await api.get_conversation(cid, user_id=self.user)
        sync.assert_awaited_once_with(main["session_id"])
        self.assertEqual(response.conversation.id, cid)

        sessions = {
            s.id: s for s in await api.list_sessions(user_id=self.user, status=None)
        }
        self.assertEqual(sessions[child_id].parent_session_id, main["session_id"])
        self.assertEqual(sessions[child_id].topic, "alpha")


class ProjectFromRepoTests(KagentFakeCase):
    """Find-or-create of a project for a GitHub repository, and the API path that uses it."""

    REPO = GithubRepo("oldsj", "mainloop")

    async def rows(self, user: str | None = None):
        return await self.pool.fetch(
            "SELECT * FROM projects WHERE user_id=$1", user or self.user
        )

    async def test_the_same_repository_is_one_project(self):
        first = await db.get_or_create_project(self.user, self.REPO)
        second = await db.get_or_create_project(self.user, self.REPO)
        self.assertEqual(first.id, second.id)
        self.assertEqual(first.full_name, "oldsj/mainloop")
        self.assertEqual(first.html_url, "https://github.com/oldsj/mainloop")
        self.assertEqual((first.owner, first.name), ("oldsj", "mainloop"))
        self.assertEqual(first.default_branch, "")
        self.assertEqual(len(await self.rows()), 1)
        self.assertGreaterEqual(second.last_used_at, first.last_used_at)

    async def test_concurrent_callers_get_the_same_project(self):
        projects = await asyncio.gather(
            *(db.get_or_create_project(self.user, self.REPO) for _ in range(12))
        )
        self.assertEqual({p.id for p in projects}, {projects[0].id})
        self.assertEqual(len(await self.rows()), 1)
        stored = (await self.rows())[0]
        self.assertEqual(stored["id"], projects[0].id)

    async def test_first_creation_under_real_contention_never_raises(self):
        """Every connection is open up front, so concurrent first inserts really overlap.

        Only the ``lower(full_name)`` index may be unique on a project: ``ON CONFLICT`` absorbs
        conflicts on its own arbiter index only, so a second unique index would raise here.
        """
        pool = await asyncpg.create_pool(self.url, min_size=8, max_size=8)
        saved, db._pool = db._pool, pool
        try:
            for round_ in range(50):
                name = f"Repo{round_}"
                repos = [
                    GithubRepo("Oldsj", name if i % 2 else name.lower())
                    for i in range(8)
                ]
                results = await asyncio.gather(
                    *(db.get_or_create_project(self.user, repo) for repo in repos),
                    return_exceptions=True,
                )
                errors = [r for r in results if isinstance(r, BaseException)]
                self.assertEqual(errors, [], f"round {round_}")
                self.assertEqual({r.id for r in results}, {results[0].id})
            rows = await self.rows()
            self.assertEqual(len(rows), 50)
            self.assertEqual(len({row["full_name"].lower() for row in rows}), 50)
        finally:
            db._pool = saved
            await pool.close()

    async def test_projects_are_per_user_and_per_repository(self):
        other_user = f"user-{uuid.uuid4().hex[:8]}"
        mine = await db.get_or_create_project(self.user, self.REPO)
        theirs = await db.get_or_create_project(other_user, self.REPO)
        another = await db.get_or_create_project(self.user, GithubRepo("oldsj", "x"))
        self.assertEqual(len({mine.id, theirs.id, another.id}), 3)
        self.assertEqual(len(await self.rows()), 2)
        self.assertEqual(len(await self.rows(other_user)), 1)

    async def test_an_existing_project_keeps_its_stored_url_and_metadata(self):
        project_id = f"proj-{uuid.uuid4().hex[:8]}"
        await self.pool.execute(
            """INSERT INTO projects (id,user_id,owner,name,full_name,html_url,default_branch,
                                     last_used_at)
               VALUES ($1,$2,'oldsj','mainloop','oldsj/mainloop',
                       'https://github.com/oldsj/mainloop.git','trunk',
                       NOW() - INTERVAL '1 day')""",
            project_id,
            self.user,
        )
        project = await db.get_or_create_project(self.user, self.REPO)
        self.assertEqual(project.id, project_id)
        self.assertEqual(project.default_branch, "trunk")
        self.assertEqual(project.html_url, "https://github.com/oldsj/mainloop.git")
        self.assertGreater(
            project.last_used_at.timestamp(), datetime.now(UTC).timestamp() - 60
        )

    async def test_a_refresh_records_the_default_branch(self):
        project = await db.get_or_create_project(self.user, self.REPO)
        await db.update_project_metadata(project.id, default_branch="trunk")
        self.assertEqual((await db.get_project(project.id)).default_branch, "trunk")
        await db.update_project_metadata(project.id, default_branch="")
        self.assertEqual((await db.get_project(project.id)).default_branch, "trunk")

    async def test_nullable_project_counts_are_read_as_zero(self):
        project = await db.get_or_create_project(self.user, self.REPO)
        await self.pool.execute(
            "UPDATE projects SET open_pr_count=NULL, open_issue_count=NULL WHERE id=$1",
            project.id,
        )
        stored = await db.get_project(project.id)
        self.assertEqual((stored.open_pr_count, stored.open_issue_count), (0, 0))

    async def test_repository_names_differing_only_in_case_are_one_project(self):
        first = await db.get_or_create_project(self.user, GithubRepo("Foo", "Bar"))
        second = await db.get_or_create_project(self.user, GithubRepo("foo", "bar"))
        third = await db.get_or_create_project(
            self.user, parse_github_repo("FOO/BAR.GIT")
        )
        self.assertEqual({first.id, second.id, third.id}, {first.id})
        self.assertEqual(len(await self.rows()), 1)
        self.assertEqual(second.full_name, "Foo/Bar")  # the case it was created with
        self.assertEqual(second.html_url, "https://github.com/Foo/Bar")
        projects = await asyncio.gather(
            *(
                db.get_or_create_project(self.user, GithubRepo("x", name))
                for name in ("Y", "y", "Y", "y")
            )
        )
        self.assertEqual({p.id for p in projects}, {projects[0].id})
        self.assertEqual(len(await self.rows()), 2)

    async def client(self) -> httpx.AsyncClient:
        for patcher in (
            patch.object(settings, "owner_id", self.user),
            patch.object(settings, "api_hosts", "test"),
            patch.object(workspaces, "publish", AsyncMock()),
        ):
            patcher.start()
            self.addCleanup(patcher.stop)
        client = httpx.AsyncClient(
            transport=httpx.ASGITransport(app=api.app), base_url="http://test"
        )
        self.addAsyncCleanup(client.aclose)
        return client

    async def test_post_workspaces_with_a_repo_creates_the_project_and_the_workspace(
        self,
    ):
        client = await self.client()
        self.fake.next_session_ids = ["ctx-a", "ctx-b"]
        for text, branch in (
            ("oldsj/mainloop", "feature/a"),
            ("https://github.com/oldsj/mainloop.git", "feature/b"),
        ):
            response = await client.post(
                "/workspaces", json={"repo": text, "branch": branch}
            )
            self.assertEqual(response.status_code, 201, response.text)
            manifest = response.json()["manifest"]
            self.assertEqual(manifest["repo_url"], "https://github.com/oldsj/mainloop")
            self.assertEqual((manifest["ref"], manifest["branch"]), ("", branch))
        projects = await self.rows()
        self.assertEqual(len(projects), 1)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM sessions WHERE user_id=$1 AND project_id=$2",
                self.user,
                projects[0]["id"],
            ),
            2,
        )
        self.assertEqual(
            self.fake.created_workspaces(),
            [
                SessionWorkspace(
                    repo="https://github.com/oldsj/mainloop", ref="", branch=branch
                )
                for branch in ("feature/a", "feature/b")
            ],
        )

    async def test_empty_and_legacy_null_base_refs_are_readable_through_sessions_api(
        self,
    ):
        client = await self.client()
        created = await client.post(
            "/workspaces", json={"repo": "https://github.com/oldsj/mainloop"}
        )
        self.assertEqual(created.status_code, 201, created.text)
        wid = created.json()["workspace_id"]
        self.assertEqual(created.json()["manifest"]["ref"], "")
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT base_branch FROM sessions WHERE id=$1", wid
            ),
            "",
        )
        for legacy_null in (False, True):
            with self.subTest(legacy_null=legacy_null):
                if legacy_null:
                    await self.pool.execute(
                        "UPDATE sessions SET base_branch=NULL WHERE id=$1", wid
                    )
                listed = await client.get("/sessions")
                self.assertEqual(listed.status_code, 200, listed.text)
                session = next(s for s in listed.json() if s["id"] == wid)
                self.assertEqual(session["base_branch"], "")
                detail = await client.get(f"/sessions/{wid}")
                self.assertEqual(detail.status_code, 200, detail.text)
                self.assertEqual(detail.json()["base_branch"], "")

    async def test_post_workspaces_with_a_bad_branch_or_ref_stores_no_project(self):
        client = await self.client()
        for body in (
            {"repo": "oldsj/mainloop", "branch": "bad branch"},
            {"repo": "oldsj/mainloop", "branch": "a..b"},
            {"repo": "oldsj/mainloop", "branch": "x", "ref": "bad ref"},
        ):
            with self.subTest(body=body):
                response = await client.post("/workspaces", json=body)
                self.assertEqual(response.status_code, 422, response.text)
        self.assertEqual(await self.rows(), [])
        self.assertEqual(self.fake.created_workspaces(), [])

    async def test_post_workspaces_without_a_branch_generates_one(self):
        client = await self.client()
        response = await client.post(
            "/workspaces", json={"repo": "Oldsj/Mainloop", "ref": "v1.2"}
        )
        self.assertEqual(response.status_code, 201, response.text)
        manifest = response.json()["manifest"]
        self.assertEqual(manifest["ref"], "v1.2")
        self.assertRegex(manifest["branch"], r"^mainloop/[0-9a-f]{8}$")
        (project,) = await self.rows()
        self.assertEqual(project["full_name"], "Oldsj/Mainloop")

    async def test_the_refresh_endpoint_records_the_default_branch(self):
        client = await self.client()
        response = await client.post(
            "/workspaces", json={"repo": "oldsj/mainloop", "branch": "x"}
        )
        self.assertEqual(response.status_code, 201, response.text)
        (project,) = await self.rows()
        self.assertEqual(project["default_branch"], "")
        metadata = RepoMetadata(
            owner="oldsj",
            name="mainloop",
            full_name="oldsj/mainloop",
            description="d",
            default_branch="trunk",
            avatar_url="https://avatars.example/u",
            html_url="https://github.com/oldsj/mainloop",
            open_issues_count=3,
        )
        with patch.object(api, "get_repo_metadata", AsyncMock(return_value=metadata)):
            response = await client.post(f"/projects/{project['id']}/refresh")
        self.assertEqual(response.status_code, 200, response.text)
        stored = await db.get_project(project["id"])
        self.assertEqual(
            (stored.default_branch, stored.description, stored.open_issue_count),
            ("trunk", "d", 3),
        )
        # From now on a workspace without a ref starts from the recorded default.
        response = await client.post("/workspaces", json={"repo": "oldsj/mainloop"})
        self.assertEqual(response.json()["manifest"]["ref"], "trunk")

    async def test_post_workspaces_with_an_invalid_repo_stores_nothing(self):
        client = await self.client()
        response = await client.post(
            "/workspaces",
            json={"repo": "https://github.com/oldsj/mainloop/tree/main", "branch": "x"},
        )
        self.assertEqual(response.status_code, 422)
        self.assertEqual(await self.rows(), [])
        self.assertEqual(self.fake.created_workspaces(), [])


class ProjectsFromBeforeTheCaseInsensitiveIndexTests(PostgresTestCase):
    """A database that has projects but not ``idx_projects_user_lower_full_name`` yet.

    ``get_or_create_project``'s ``ON CONFLICT (user_id, lower(full_name))`` needs the index, so
    the migration has to build it over the rows that are already there.
    """

    @classmethod
    def setUpClass(cls):
        super().setUpClass()
        asyncio.run(
            _admin(
                cls.url,
                "DROP INDEX idx_projects_user_lower_full_name",
                # What an older database has: the exact-case table constraint.
                """ALTER TABLE projects ADD CONSTRAINT projects_user_id_full_name_key
                   UNIQUE (user_id, full_name)""",
                """INSERT INTO projects (id, user_id, owner, name, full_name, html_url,
                                        default_branch)
                   VALUES ('proj-old', 'user-old', 'Foo', 'Bar', 'Foo/Bar',
                           'https://github.com/Foo/Bar', 'trunk'),
                          ('proj-other', 'user-old', 'foo', 'baz', 'foo/baz',
                           'https://github.com/foo/baz', 'main')""",
            )
        )

    async def test_the_migration_indexes_the_existing_rows_and_can_run_again(self):
        repo = GithubRepo("foo", "bar")
        with self.assertRaises(asyncpg.InvalidColumnReferenceError):
            await db.get_or_create_project("user-old", repo)
        await _init_schema(self.url)
        await _init_schema(self.url)
        projects = await asyncio.gather(
            *(db.get_or_create_project("user-old", repo) for _ in range(8))
        )
        self.assertEqual({p.id for p in projects}, {"proj-old"})
        self.assertEqual(projects[0].default_branch, "trunk")
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM projects WHERE user_id='user-old'"
            ),
            2,
        )
        with self.assertRaises(asyncpg.UniqueViolationError):
            await self.pool.execute(
                """INSERT INTO projects (id,user_id,owner,name,full_name,html_url)
                   VALUES ('proj-dup','user-old','FOO','BAR','FOO/BAR','u')"""
            )
        self.assertEqual(
            await self.pool.fetchval(
                """SELECT count(*) FROM pg_indexes
                   WHERE indexname IN ('idx_projects_user_full_name',
                                       'projects_user_id_full_name_key')"""
            ),
            0,
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM pg_constraint WHERE conname='projects_user_id_full_name_key'"
            ),
            0,
        )


if __name__ == "__main__":
    unittest.main()
