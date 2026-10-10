"""Row cleanup for owned test databases, without rewriting relation files."""

import re
from dataclasses import dataclass


async def _row_statements(conn, tables, restart_sequences):
    if conn.is_in_transaction():
        raise ValueError("row cleanup must own its transaction")
    database = await conn.fetchval("SELECT current_database()")
    if not re.fullmatch(r"mainloop_test_[a-z0-9_]+_[a-f0-9]{12}", database):
        raise ValueError("row cleanup requires an owned test database")
    if tables is not None and not tables:
        raise ValueError("specify at least one table")
    if tables is not None and any(
        not re.fullmatch(r"[a-z_][a-z0-9_]*", table) for table in tables
    ):
        raise ValueError("invalid fixture table name")
    rows = await conn.fetch(
        """WITH RECURSIVE targets(oid) AS (
             SELECT c.oid FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
             WHERE n.nspname='public' AND c.relkind='r'
               AND ($1::text[] IS NULL OR c.relname=ANY($1::text[]))
             UNION
             SELECT f.conrelid FROM pg_constraint f JOIN targets t ON f.confrelid=t.oid
             WHERE f.contype='f'
           )
           SELECT quote_ident(n.nspname)||'.'||quote_ident(c.relname) AS name
           FROM targets t JOIN pg_class c ON c.oid=t.oid
           JOIN pg_namespace n ON n.oid=c.relnamespace ORDER BY name""",
        tables,
    )
    if tables is not None and not {"public." + table for table in tables}.issubset(
        {row["name"] for row in rows}
    ):
        raise ValueError("unknown fixture table")
    statements = [
        f"DELETE FROM {row['name']}"  # nosec B608 - identifiers from PostgreSQL quote_ident
        for row in rows
    ]
    if restart_sequences:
        sequences = await conn.fetch(
            """SELECT quote_ident(n.nspname)||'.'||quote_ident(c.relname) AS name,
                      s.seqstart
               FROM pg_sequence s JOIN pg_class c ON c.oid=s.seqrelid
               JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public'"""
        )
        statements.extend(
            f"ALTER SEQUENCE {row['name']} RESTART WITH {row['seqstart']}"
            for row in sequences
        )
    return database, statements


async def clear_rows(conn, tables=None, *, restart_sequences=False):
    """Match TRUNCATE CASCADE's table closure in a disposable fixture only.

    Audit tables reject DELETE in normal operation. Replication mode is local
    to this cleanup transaction, and restores FK/security triggers on both
    success and failure before a test can use this connection again.
    """
    _, statements = await _row_statements(conn, tables, restart_sequences)
    async with conn.transaction():
        await conn.execute("SET LOCAL session_replication_role=replica")
        # Identifiers come from PostgreSQL's catalogs and quote_ident, never SQL
        # interpolated from the caller's table names.
        if statements:
            await conn.execute(";".join(statements))


@dataclass(frozen=True)
class RowReset:
    statement: str

    async def clear(self, conn):
        if conn.is_in_transaction():
            raise ValueError("row cleanup must own its transaction")
        await conn.execute(self.statement)


async def row_reset_plan(conn):
    """Compile once for a class explicitly qualified to keep its schema stable.

    The DO statement owns an implicit transaction, so errors roll back both
    deletion and SET LOCAL. Check the exact database on every use in the same
    round trip; a cached plan must never clean a foreign database.
    """
    database, statements = await _row_statements(conn, None, True)
    return RowReset(
        "DO $fixture_reset$ BEGIN "
        f"IF current_database() <> '{database}' THEN "
        "RAISE EXCEPTION 'row cleanup requires its owned test database'; END IF; "
        "SET LOCAL session_replication_role=replica; "
        + ";".join(statements)
        + "; END $fixture_reset$"
    )  # nosec B608 - validated fixture database and catalog-quoted identifiers


async def wait_for_lock(conn, *, pid=None, query=None, timeout=3, locktype=None):
    """Observe actual PostgreSQL blocking without a client round trip per poll.

    Lock tests need server evidence, beyond a coroutine's readiness signal.
    Keep the bounded observation on the server and refresh its stats snapshot.
    Pass locktype, such as 'advisory', to match only that kind of waited-on lock.
    """
    await conn.execute(
        """CREATE OR REPLACE FUNCTION pg_temp.fixture_wait_for_lock(
             target_pid integer, query_pattern text, seconds double precision,
             wanted_locktype text
           ) RETURNS boolean LANGUAGE plpgsql AS $$
           DECLARE expires timestamptz := clock_timestamp() + seconds * interval '1 second';
           BEGIN
             LOOP
               PERFORM pg_stat_clear_snapshot();
               IF EXISTS(SELECT 1 FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid
                         WHERE a.datname=current_database() AND NOT l.granted
                           AND (target_pid IS NULL OR l.pid=target_pid)
                           AND (query_pattern IS NULL OR a.query LIKE query_pattern)
                           AND (wanted_locktype IS NULL OR l.locktype=wanted_locktype)) THEN
                 RETURN true;
               END IF;
               IF clock_timestamp() >= expires THEN RETURN false; END IF;
               PERFORM pg_sleep(0.01);
             END LOOP;
           END $$"""
    )
    return await conn.fetchval(
        "SELECT pg_temp.fixture_wait_for_lock($1,$2,$3,$4)",
        pid,
        query,
        timeout,
        locktype,
    )
