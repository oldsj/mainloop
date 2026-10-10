"""Fixture reuse must preserve isolation and restore normal database authority."""

import asyncio
import uuid
from unittest.mock import patch

import asyncpg
from tests.runtime.postgres_fixture import clear_rows
from tests.runtime.test_postgres_ledger import TEST_URL, PostgresTestCase
from tests.runtime.test_push_transport_git import GitFixtureCase


class RowResetTests(PostgresTestCase):
    async def asyncSetUp(self):
        await super().asyncSetUp()
        prefix = "fixture_" + uuid.uuid4().hex[:8]
        self.parent, self.child, self.other = (
            prefix + "_parent",
            prefix + "_child",
            prefix + "_other",
        )
        await self.pool.execute(
            f"""CREATE TABLE {self.parent}(id integer PRIMARY KEY);
                CREATE TABLE {self.child}(id integer PRIMARY KEY,
                    parent_id integer REFERENCES {self.parent}(id));
                CREATE TABLE {self.other}(id integer PRIMARY KEY);
                CREATE FUNCTION {prefix}_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
                BEGIN RAISE EXCEPTION 'fixture immutable'; END $$;
                CREATE TRIGGER immutable BEFORE UPDATE OR DELETE ON {self.child}
                    FOR EACH ROW EXECUTE FUNCTION {prefix}_immutable();
                INSERT INTO {self.parent} VALUES(1);
                INSERT INTO {self.child} VALUES(1,1);
                INSERT INTO {self.other} VALUES(1);"""
        )

    async def row_count(self, conn, table):
        sql = f"SELECT count(*) FROM {table}"  # nosec B608 - generated fixture name
        return await conn.fetchval(sql)

    async def test_cleanup_preserves_relation_files_and_restores_security(self):
        async with self.pool.acquire() as conn:
            before = await conn.fetch(
                "SELECT relname,relfilenode FROM pg_class WHERE relname=ANY($1::text[]) ORDER BY relname",
                [self.parent, self.child],
            )
            await clear_rows(conn, [self.parent])
            for table in (self.parent, self.child):
                self.assertEqual(await self.row_count(conn, table), 0)
            self.assertEqual(await self.row_count(conn, self.other), 1)
            self.assertEqual(
                await conn.fetch(
                    "SELECT relname,relfilenode FROM pg_class WHERE relname=ANY($1::text[]) ORDER BY relname",
                    [self.parent, self.child],
                ),
                before,
            )
            self.assertEqual(
                await conn.fetchval("SHOW session_replication_role"), "origin"
            )
            await conn.execute(
                f"INSERT INTO {self.parent} VALUES(1); INSERT INTO {self.child} VALUES(1,1)"  # nosec B608 - generated fixture names
            )
            with self.assertRaisesRegex(asyncpg.RaiseError, "fixture immutable"):
                await conn.execute(
                    f"DELETE FROM {self.child}"  # nosec B608 - generated fixture name
                )
            with self.assertRaises(asyncpg.ForeignKeyViolationError):
                await conn.execute(
                    f"INSERT INTO {self.child} VALUES(2,99)"  # nosec B608 - generated fixture name
                )

    async def test_failed_cleanup_rolls_back_rows_and_replication_mode(self):
        execute = asyncpg.Connection.execute

        async def failed(conn, sql, *args, **kwargs):
            # This only matches our generated cleanup statement for injection.
            if sql.startswith("DELETE ") and sql.split(";", 1)[0].endswith(
                "." + self.child
            ):
                await execute(conn, sql.split(";", 1)[0])
                raise RuntimeError("injected cleanup failure")
            return await execute(conn, sql, *args, **kwargs)

        async with self.pool.acquire() as conn:
            with patch.object(asyncpg.Connection, "execute", failed):
                with self.assertRaisesRegex(RuntimeError, "injected cleanup failure"):
                    await clear_rows(conn, [self.parent])
            self.assertEqual(
                await conn.fetchval("SHOW session_replication_role"), "origin"
            )
            for table in (self.parent, self.child):
                self.assertEqual(await self.row_count(conn, table), 1)

    async def test_cleanup_refuses_foreign_databases_unknown_tables_and_nested_transactions(
        self,
    ):
        conn = await asyncpg.connect(TEST_URL)
        try:
            with self.assertRaisesRegex(ValueError, "owned test database"):
                await clear_rows(conn, [self.parent])
        finally:
            await conn.close()
        async with self.pool.acquire() as conn:
            with self.assertRaisesRegex(ValueError, "unknown fixture table"):
                await clear_rows(conn, ["fixture_missing"])
            async with conn.transaction():
                with self.assertRaisesRegex(ValueError, "own its transaction"):
                    await clear_rows(conn, [self.parent])
            self.assertEqual(
                await conn.fetchval("SHOW session_replication_role"), "origin"
            )

    async def test_sharing_resets_rows_and_sequences_and_requires_explicit_opt_in(self):
        class First(PostgresTestCase):
            reuse_database = True

        class Second(PostgresTestCase):
            reuse_database = True

        class Unqualified(First):
            pass

        await asyncio.to_thread(First.setUpClass)
        conn = await asyncpg.connect(First.url)
        try:
            await conn.execute(
                "INSERT INTO main_threads(id,user_id) VALUES('shared','shared')"
            )
            await conn.fetchval("SELECT nextval('project_merge_policy_audit_id_seq')")
            await asyncio.to_thread(Second.setUpClass)
            self.assertEqual(First.database, Second.database)
            self.assertEqual(
                await conn.fetchval("SELECT count(*) FROM main_threads"), 0
            )
            self.assertEqual(
                await conn.fetchval(
                    "SELECT nextval('project_merge_policy_audit_id_seq')"
                ),
                1,
            )
            await asyncio.to_thread(Unqualified.setUpClass)
            try:
                self.assertNotEqual(Unqualified.database, First.database)
            finally:
                await asyncio.to_thread(Unqualified.doClassCleanups)
        finally:
            await conn.close()


class GitSeedIsolationTests(GitFixtureCase):
    async def test_fixture_copies_share_no_writable_files(self):
        other = GitFixtureCase()
        await other.asyncSetUp()
        self.addCleanup(other.doCleanups)
        self.assertEqual(self.base, other.base)
        for path in ("file", ".git/config", ".git/HEAD"):
            first, second = self.client / path, other.client / path
            self.assertNotEqual(first.stat().st_ino, second.stat().st_ino)
            original = second.read_bytes()
            first.write_bytes(b"changed fixture\n")
            self.assertEqual(second.read_bytes(), original)
