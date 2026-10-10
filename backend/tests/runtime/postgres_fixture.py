"""Row cleanup for owned test databases, without rewriting relation files."""

import re


async def clear_rows(conn, tables=None, *, restart_sequences=False):
    """Match TRUNCATE CASCADE's table closure in a disposable fixture only.

    Audit tables reject DELETE in normal operation. Replication mode is local
    to this cleanup transaction, and restores FK/security triggers on both
    success and failure before a test can use this connection again.
    """
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
    async with conn.transaction():
        await conn.execute("SET LOCAL session_replication_role=replica")
        # Identifiers come from PostgreSQL's catalogs and quote_ident, never SQL
        # interpolated from the caller's table names.
        if statements:
            await conn.execute(";".join(statements))
