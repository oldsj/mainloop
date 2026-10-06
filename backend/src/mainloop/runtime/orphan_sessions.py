"""Operator sweep: kagent Sessions that Mainloop has no ``native_bindings`` row for.

A create whose reply was lost can leave a Session that Mainloop never recorded, and a deleted
workspace row can outlive its Session if kagent was unreachable. Mainloop cannot find those by
itself, so this lists every Session kagent holds for ``KAGENT_USER_ID`` that no binding names.
It only lists unless ``--delete`` is given.

Run it with the backend's environment (database and kagent settings). The production image has
no ``uv``; the command is on PATH::

    mainloop-sweep-kagent-sessions            # list
    mainloop-sweep-kagent-sessions --delete   # list, then delete

In local development use ``uv run mainloop-sweep-kagent-sessions`` from ``backend/``.

Run it when Mainloop is quiet. A Session whose create has just returned but whose binding is not
yet written looks orphaned for a moment, so each Session is checked against the database again
immediately before it is deleted. A binding with no Session id yet (a create whose outcome is
unknown, or one not yet attempted) may own a Session that is listed here. Deletion is refused
until those creates are resolved through the normal session/workspace lifecycle.
"""

from __future__ import annotations

import argparse
import asyncio
import logging
import sys

from mainloop.db import db
from mainloop.runtime import native_sessions as ns
from mainloop.runtime.kagent_client import KagentError, KagentSession, RuntimeState

logger = logging.getLogger(__name__)

_GONE = (RuntimeState.DELETED,)


async def _bound_ids() -> tuple[set[str], int]:
    """Return every kagent Session id a binding names, and the count of bindings with none."""
    async with db.connection() as conn:
        rows = await conn.fetch(
            "SELECT kagent_session_id FROM native_bindings WHERE kagent_session_id IS NOT NULL"
        )
        unresolved = await conn.fetchval(
            "SELECT count(*) FROM native_bindings WHERE kagent_session_id IS NULL"
        )
    return {r["kagent_session_id"] for r in rows}, unresolved


async def find_orphans() -> tuple[list[KagentSession], int]:
    """Return the Sessions with no binding, and the number of bindings with no Session id yet.

    The bindings are read after the listing, so a Session created during the listing is not
    reported just because the listing was older than its binding.
    """
    sessions = await ns.get_client().list_sessions()
    bound, unresolved = await _bound_ids()
    orphans = [s for s in sessions if s.state not in _GONE and s.id not in bound]
    return orphans, unresolved


async def delete_orphan(session: KagentSession) -> bool:
    """Delete one Session unless a binding has claimed it since the listing."""
    bound, unresolved = await _bound_ids()
    if unresolved:
        raise KagentError(
            "unresolved creates exist; resolve them before deleting orphans"
        )
    if session.id in bound:
        return False
    await ns.get_client().delete_session(session.id)
    return True


def describe(session: KagentSession) -> str:
    """Return one line naming the Session, its state and workspace repository, if any."""
    repo = session.workspace.repo if session.workspace else "-"
    return f"{session.id}  {session.state.name.lower():<10}  {repo}  {session.name or ''}".rstrip()


async def sweep(delete: bool, out=print) -> int:
    """List the orphaned Sessions, delete them when asked, and return the exit status."""
    orphans, unresolved = await find_orphans()
    if unresolved:
        out(
            f"warning: {unresolved} binding(s) have no kagent Session id yet; a create whose "
            "outcome is unknown may own a listed Session. Refresh or delete those first."
        )
    if not orphans:
        out("No orphaned kagent Sessions.")
        return 0
    out(f"{len(orphans)} kagent Session(s) with no native_bindings row:")
    for session in orphans:
        out("  " + describe(session))
    if not delete:
        out("Nothing deleted. Run again with --delete to delete them.")
        return 0
    if unresolved:
        out("Nothing deleted: resolve unknown creates before using --delete.")
        return 1
    failed = 0
    for session in orphans:
        try:
            deleted = await delete_orphan(session)
        except KagentError as exc:
            failed += 1
            out(f"  not deleted {session.id}: {exc}")
            continue
        out(f"  {'deleted' if deleted else 'skipped (now bound)'} {session.id}")
    return 1 if failed else 0


async def _run(delete: bool) -> int:
    await db.connect()
    try:
        return await sweep(delete)
    finally:
        await ns.close_client()
        await db.disconnect()


def main(argv: list[str] | None = None) -> None:
    """Run the sweep from the command line."""
    parser = argparse.ArgumentParser(
        description="List (and with --delete, delete) kagent Sessions Mainloop has no binding for."
    )
    parser.add_argument(
        "--delete",
        action="store_true",
        help="delete the orphans; the default only lists",
    )
    args = parser.parse_args(argv)
    sys.exit(asyncio.run(_run(args.delete)))


if __name__ == "__main__":
    main()
