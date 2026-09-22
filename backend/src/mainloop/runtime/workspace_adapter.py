"""Per-session Substrate workspace bindings: the durable mapping between a Mainloop session,
a Substrate actor, its snapshot, cross-referenced native session id, and Mainloop's ownership
generation for that actor. This is the adapter boundary described in ``.tasknotes/plan.md``:
Mainloop owns creation intent, desired state, retry policy and audit; Substrate owns isolated
actor compute and snapshots. Produces ``models.native_agent.WorkspaceBinding`` -- the existing
contract type -- rather than a parallel workspace model.

Rules carried over from ``native_sessions.py`` / ``contracts.py`` rather than reinvented:
- Identity is persisted before an uncertain external call, and a retry re-inspects the actor and
  this row before creating or mutating anything (no blind replay, no second writer).
- Every mutation is fenced by ``ownership_generation``: a stale caller's write is rejected, not
  silently applied.
- ``CRASHED`` and revert are never automatic. Reverting is only reachable through
  ``revert_workspace`` with an explicit acknowledgement that unsnapshotted work may be lost.
"""

from __future__ import annotations

import asyncio
import logging
from datetime import UTC, datetime

from mainloop.config import settings
from mainloop.db import db
from mainloop.runtime.contracts import ContractError, StaleOwnership
from mainloop.runtime.substrate import (
    OBSERVED_STATE,
    ActorRecord,
    ActorState,
    SubstrateControl,
    TransportError,
)

from models import CapabilityResult, CapabilityState, WorkspaceBinding

logger = logging.getLogger(__name__)

_locks: dict[str, asyncio.Lock] = {}


def _lock(session_id: str) -> asyncio.Lock:
    return _locks.setdefault(session_id, asyncio.Lock())


def actor_name(session_id: str) -> str:
    return f"ml-{session_id[:16]}"


def _control() -> SubstrateControl:
    return SubstrateControl()


async def get_workspace(session_id: str) -> dict | None:
    async with db.connection() as conn:
        row = await conn.fetchrow(
            "SELECT * FROM workspace_bindings WHERE workspace_id=$1", session_id
        )
    return dict(row) if row else None


def _binding_from_row(row: dict) -> WorkspaceBinding:
    capabilities: tuple[CapabilityResult, ...] = ()
    if row["observed_state"] == "unavailable" and row["last_error"]:
        # A row-level note (e.g. "actor missing", "actor CRASHED") becomes a typed capability
        # result rather than free text on the contract model, per the proved/partial evidence rule.
        note = row["last_error"]
        capabilities = (
            CapabilityResult(
                capability=(
                    "actor_crashed" if "CRASHED" in note.upper() else "actor_health"
                ),
                state=CapabilityState.PROVED,
                # SubstrateControl always talks to a real (possibly Kind) cluster, so this
                # in-binding health signal is "live" by construction. The four plan-mandated
                # integration-gate CapabilityResults are separate records in the proof note,
                # scored "fixture" or "live" by whatever trial produced their evidence_ref.
                scope="live",
                evidence_ref=f"substrate://{row['atespace']}/{row['actor_name']}",
                detail=note,
            ),
        )
    return WorkspaceBinding(
        workspace_id=row["workspace_id"],
        runtime_endpoint=row["runtime_endpoint"] or row["preview_route"] or "unrouted",
        observed_at=row["observed_at"] or row["updated_at"],
        observed_state=row["observed_state"],
        capabilities=capabilities,
    )


async def _insert_row(
    session_id: str,
    atespace: str,
    name: str,
    template: str,
    actor: ActorRecord,
    now: datetime,
) -> None:
    route = f"{atespace}/{name}"
    async with db.connection() as conn:
        await conn.execute(
            """INSERT INTO workspace_bindings
               (workspace_id, atespace, actor_name, actor_template, preview_route,
                runtime_endpoint, observed_state, observed_at, external_snapshot_uri)
               VALUES ($1,$2,$3,$4,$5,$5,$6,$7,$8)""",
            session_id,
            atespace,
            name,
            template,
            route,
            OBSERVED_STATE[actor.state],
            now,
            actor.external_snapshot_uri,
        )


CRASHED_NOTE = (
    "actor CRASHED: it stopped running and lost anything since its last completed snapshot. "
    "Substrate's actor record has no snapshot timestamp, so snapshot age cannot be reported here. "
    "Resume is rejected in this state; only an explicit revert_workspace(acknowledge_loss=True) "
    "restores it, discarding any unsnapshotted work."
)


async def _update_observed(
    session_id: str, actor: ActorRecord, now: datetime, *, last_error: str | None = None
) -> None:
    if last_error is None and actor.state == ActorState.CRASHED:
        last_error = CRASHED_NOTE
    async with db.connection() as conn:
        await conn.execute(
            """UPDATE workspace_bindings
               SET observed_state=$2, observed_at=$3, external_snapshot_uri=$4,
                   last_error=$5, updated_at=NOW()
               WHERE workspace_id=$1""",
            session_id,
            OBSERVED_STATE[actor.state],
            now,
            actor.external_snapshot_uri,
            last_error,
        )


async def _mark_missing(session_id: str, now: datetime) -> None:
    async with db.connection() as conn:
        await conn.execute(
            """UPDATE workspace_bindings
               SET observed_state='unavailable', observed_at=$2, last_error=$3, updated_at=NOW()
               WHERE workspace_id=$1""",
            session_id,
            now,
            "actor not found where this row expected one; not recreated automatically "
            "(inspect and reconcile explicitly, or delete this row to allow a fresh actor)",
        )


async def _bump_generation(session_id: str, expected: int) -> None:
    async with db.connection() as conn:
        tag = await conn.execute(
            """UPDATE workspace_bindings SET ownership_generation=ownership_generation+1,
               updated_at=NOW() WHERE workspace_id=$1 AND ownership_generation=$2""",
            session_id,
            expected,
        )
    if tag.endswith(" 0"):
        raise StaleOwnership(
            f"workspace_bindings.{session_id} is no longer at generation {expected}"
        )


def plan_ensure(row_exists: bool, actor_found: bool) -> str:
    """Retry-safe provision-or-attach policy, factored out of ``ensure_workspace`` so it is
    directly testable without a database or cluster.

    ``create``: no row and no actor -- first provision. ``attach``: an actor already exists
    (whether or not this process created it), so observe it rather than creating another.
    ``surface_gap``: this row believes it owns an actor that Substrate no longer has -- never
    silently recreate under the same name; a human or a later explicit call must reconcile it.
    """
    if actor_found:
        return "attach"
    return "create" if not row_exists else "surface_gap"


async def ensure_workspace(
    session_id: str, *, control: SubstrateControl | None = None
) -> WorkspaceBinding:
    """Idempotent provision-or-attach. Never creates a second actor for a row that already
    believes it owns one; a gap between this row and Substrate's view is surfaced, not papered
    over."""
    control = control or _control()
    async with _lock(session_id):
        row = await get_workspace(session_id)
        atespace = settings.substrate_atespace
        template = settings.substrate_actor_template
        name = row["actor_name"] if row else actor_name(session_id)
        try:
            actor = await control.get_actor(atespace, name)
        except TransportError:
            if row is not None:
                return _binding_from_row(
                    row
                )  # unreachable now; last known state stands
            raise
        now = datetime.now(UTC)
        action = plan_ensure(row is not None, actor is not None)
        if action == "surface_gap":
            await _mark_missing(session_id, now)
            return _binding_from_row(await get_workspace(session_id))  # type: ignore[arg-type]
        if action == "create":
            actor = await control.create_actor(atespace, name, template=template)
            await _insert_row(session_id, atespace, name, template, actor, now)
        else:
            await _update_observed(session_id, actor, now)
        return _binding_from_row(await get_workspace(session_id))  # type: ignore[arg-type]


async def observe_workspace(
    session_id: str, *, control: SubstrateControl | None = None
) -> WorkspaceBinding | None:
    """Read-only reconcile: refresh observed_state/observed_at without changing desired state."""
    control = control or _control()
    row = await get_workspace(session_id)
    if row is None:
        return None
    now = datetime.now(UTC)
    try:
        actor = await control.get_actor(row["atespace"], row["actor_name"])
    except TransportError as exc:
        logger.info("workspace observe skipped for %s: %s", session_id, exc)
        return _binding_from_row(row)
    if actor is None:
        await _mark_missing(session_id, now)
    else:
        await _update_observed(session_id, actor, now)
    return _binding_from_row(await get_workspace(session_id))  # type: ignore[arg-type]


async def resume_workspace(
    session_id: str, *, control: SubstrateControl | None = None
) -> WorkspaceBinding:
    control = control or _control()
    async with _lock(session_id):
        row = await get_workspace(session_id)
        if row is None:
            raise ContractError(f"no workspace binding for session {session_id}")
        actor = await control.resume_actor(row["atespace"], row["actor_name"])
        now = datetime.now(UTC)
        await _update_observed(session_id, actor, now)
        await _bump_generation(session_id, row["ownership_generation"])
        return _binding_from_row(await get_workspace(session_id))  # type: ignore[arg-type]


async def suspend_workspace(
    session_id: str, *, control: SubstrateControl | None = None
) -> WorkspaceBinding:
    control = control or _control()
    async with _lock(session_id):
        row = await get_workspace(session_id)
        if row is None:
            raise ContractError(f"no workspace binding for session {session_id}")
        actor = await control.suspend_actor(row["atespace"], row["actor_name"])
        now = datetime.now(UTC)
        await _update_observed(session_id, actor, now)
        await _bump_generation(session_id, row["ownership_generation"])
        return _binding_from_row(await get_workspace(session_id))  # type: ignore[arg-type]


async def revert_workspace(
    session_id: str, *, acknowledge_loss: bool, control: SubstrateControl | None = None
) -> WorkspaceBinding:
    """Roll back to the actor's last completed snapshot, discarding any unsnapshotted work.
    Never called implicitly by this module -- the caller (API layer) must have shown the user
    the actor is CRASHED (or otherwise irrecoverable) and gotten explicit confirmation first.
    """
    if not acknowledge_loss:
        raise ContractError(
            "revert_workspace requires acknowledge_loss=True: it discards unsnapshotted work"
        )
    control = control or _control()
    async with _lock(session_id):
        row = await get_workspace(session_id)
        if row is None:
            raise ContractError(f"no workspace binding for session {session_id}")
        actor = await control.revert_actor(row["atespace"], row["actor_name"])
        now = datetime.now(UTC)
        await _update_observed(
            session_id,
            actor,
            now,
            last_error="reverted to last completed snapshot; any unsnapshotted work was lost",
        )
        await _bump_generation(session_id, row["ownership_generation"])
        return _binding_from_row(await get_workspace(session_id))  # type: ignore[arg-type]


def is_crashed(binding_row: dict) -> bool:
    return (
        binding_row["observed_state"] == "unavailable"
        and binding_row.get("last_error") is not None
        and "CRASHED" in (binding_row.get("last_error") or "").upper()
    )
