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
import json
import logging
import uuid
from datetime import UTC, datetime, timedelta

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

from models import (
    CapabilityResult,
    CapabilityState,
    WorkspaceAgentKind,
    WorkspaceBinding,
    WorkspaceCondition,
    WorkspaceConditionStatus,
    WorkspaceDesiredState,
    WorkspaceLifecycle,
    WorkspaceManifest,
    WorkspaceObservedState,
    WorkspaceTransition,
)

logger = logging.getLogger(__name__)

_locks: dict[str, asyncio.Lock] = {}

LIFECYCLE_STATE = {
    ActorState.RUNNING: WorkspaceObservedState.RUNNING,
    ActorState.SUSPENDING: WorkspaceObservedState.SUSPENDING,
    ActorState.SUSPENDED: WorkspaceObservedState.SUSPENDED,
    ActorState.RESUMING: WorkspaceObservedState.RESUMING,
    ActorState.CRASHED: WorkspaceObservedState.FAILED,
    ActorState.DELETING: WorkspaceObservedState.FAILED,
    ActorState.PAUSED: WorkspaceObservedState.UNKNOWN,
    ActorState.PAUSING: WorkspaceObservedState.UNKNOWN,
    ActorState.REVERTING: WorkspaceObservedState.UNKNOWN,
    ActorState.UNSPECIFIED: WorkspaceObservedState.UNKNOWN,
}

BLOCKING_DELIVERY_STATES = {"recorded", "queued", "sending", "delivered", "uncertain"}


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
    await ensure_workspace_lifecycle(session_id)
    await _record_observation(session_id, actor=actor, binding_error=last_error)


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
            await ensure_workspace_lifecycle(session_id)
            await _record_observation(session_id, actor=actor)
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
) -> WorkspaceLifecycle:
    return await _request_workspace_state(
        session_id, WorkspaceDesiredState.RUNNING, control=control
    )


async def touch_workspace(workspace_id: str, *, reason: str) -> WorkspaceLifecycle:
    """Record turn or preview activity and wake a parked workspace when needed."""
    if reason not in {"turn", "delivery", "preview"}:
        raise ValueError("reason must be turn, delivery, or preview")
    lifecycle = await ensure_workspace_lifecycle(workspace_id)
    if lifecycle is None:
        raise ContractError(f"no workspace binding for {workspace_id}")
    async with db.connection() as conn:
        async with conn.transaction():
            binding = await conn.fetchrow(
                "SELECT workspace_id,desired_state FROM workspace_bindings WHERE workspace_id=$1 FOR UPDATE",
                workspace_id,
            )
            if binding is None:
                raise ContractError(f"no workspace binding for {workspace_id}")
            if binding.get("desired_state") == "deleting":
                raise ContractError("The workspace is being deleted.")
            current = await conn.fetchrow(
                """SELECT desired_state, observed_state FROM workspace_lifecycles
                   WHERE workspace_id=$1 FOR UPDATE""",
                workspace_id,
            )
            if current is None:
                raise ContractError(f"no workspace lifecycle for {workspace_id}")
            await conn.execute(
                "UPDATE workspace_lifecycles SET last_activity_at=NOW(), updated_at=NOW() WHERE workspace_id=$1",
                workspace_id,
            )
            should_wake = current["desired_state"] == "suspended" or current[
                "observed_state"
            ] in {"suspending", "suspended"}
    if should_wake:
        return await resume_workspace(workspace_id)
    return await get_workspace_lifecycle(workspace_id) or lifecycle


async def suspend_idle_workspaces() -> int:
    """Suspend idle dev workspaces via the normal generation and delivery fence."""
    async with db.connection() as conn:
        rows = await conn.fetch(
            """SELECT l.workspace_id
               FROM workspace_lifecycles l
               JOIN sessions s ON s.id=l.workspace_id
               LEFT JOIN LATERAL (
                   SELECT MAX(created_at) AS last_delivery_at
                   FROM native_deliveries WHERE session_id=l.workspace_id
               ) d ON TRUE
               WHERE l.desired_state='running' AND l.observed_state='running'
                 AND l.manifest->'dev' IS NOT NULL
                 AND COALESCE((l.manifest->'dev'->>'idle_timeout_minutes')::integer, 0) > 0
                 AND GREATEST(l.last_activity_at, COALESCE(d.last_delivery_at, l.last_activity_at))
                     < NOW() - ((l.manifest->'dev'->>'idle_timeout_minutes')::integer * INTERVAL '1 minute')"""
        )
    suspended = 0
    for row in rows:
        try:
            result = await suspend_workspace_if_idle(row["workspace_id"])
            if result is not None:
                suspended += 1
        except ContractError:
            # The fenced path records the reason; open deliveries are expected to be skipped.
            continue
        except Exception:
            logger.exception(
                "Idle workspace suspend failed for %s", row["workspace_id"]
            )
    return suspended


async def suspend_workspace_if_idle(
    workspace_id: str, *, control: SubstrateControl | None = None
) -> WorkspaceLifecycle | None:
    """Recheck activity under the workspace row lock before reserving a suspend."""
    return await _request_workspace_state(
        workspace_id,
        WorkspaceDesiredState.SUSPENDED,
        control=control,
        only_if_idle=True,
    )


async def suspend_workspace(
    session_id: str, *, control: SubstrateControl | None = None
) -> WorkspaceLifecycle:
    return await _request_workspace_state(
        session_id, WorkspaceDesiredState.SUSPENDED, control=control
    )


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


def lifecycle_state(actor_state: ActorState) -> WorkspaceObservedState:
    """Project Substrate actor status without treating unknown states as healthy."""
    return LIFECYCLE_STATE[actor_state]


def suspend_fence_reason(delivery_states: set[str]) -> str | None:
    """Return why parking is unsafe while the durable delivery ledger is open."""
    if "recorded" in delivery_states:
        return "DeliveryRecorded"
    if delivery_states & {"sending", "delivered"}:
        return "TurnInFlight"
    if "uncertain" in delivery_states:
        return "DeliveryUncertain"
    if "queued" in delivery_states:
        return "DeliveryQueued"
    return None


def _suspend_fence_detail(reason: str) -> str:
    return {
        "DeliveryRecorded": "A recorded delivery must be reconciled before the workspace can be suspended.",
        "TurnInFlight": "A native turn is still in flight; wait for it to finish before suspending.",
        "DeliveryUncertain": "A delivery outcome is uncertain; reconcile it before suspending.",
        "DeliveryQueued": "A queued delivery must be handled before the workspace can be suspended.",
    }[reason]


def _json_value(value):
    if isinstance(value, str):
        return json.loads(value)
    return value


def _condition(
    condition_type: str,
    status: WorkspaceConditionStatus,
    reason: str,
    message: str,
    at: datetime,
) -> WorkspaceCondition:
    return WorkspaceCondition(
        type=condition_type,
        status=status,
        reason=reason,
        message=message,
        last_transition_time=at,
    )


def _conditions_for(
    *,
    observed: WorkspaceObservedState,
    desired: WorkspaceDesiredState,
    snapshot_ref: str | None,
    reason: str,
    message: str,
    at: datetime,
    previous: tuple[WorkspaceCondition, ...] = (),
    operation_status: WorkspaceConditionStatus | None = None,
    operation_reason: str | None = None,
    operation_message: str | None = None,
    suspend_allowed: tuple[bool, str, str] | None = None,
) -> tuple[WorkspaceCondition, ...]:
    available_status = {
        WorkspaceObservedState.RUNNING: WorkspaceConditionStatus.TRUE,
        WorkspaceObservedState.SUSPENDED: WorkspaceConditionStatus.FALSE,
        WorkspaceObservedState.FAILED: WorkspaceConditionStatus.FALSE,
    }.get(observed, WorkspaceConditionStatus.UNKNOWN)
    parked_status = (
        WorkspaceConditionStatus.TRUE
        if observed == WorkspaceObservedState.SUSPENDED and snapshot_ref
        else (
            WorkspaceConditionStatus.FALSE
            if observed
            in (WorkspaceObservedState.RUNNING, WorkspaceObservedState.FAILED)
            or observed == WorkspaceObservedState.SUSPENDED
            else WorkspaceConditionStatus.UNKNOWN
        )
    )
    desired_status = (
        WorkspaceConditionStatus.TRUE
        if observed.value == desired.value
        else (
            WorkspaceConditionStatus.FALSE
            if observed
            in (WorkspaceObservedState.RUNNING, WorkspaceObservedState.SUSPENDED)
            else WorkspaceConditionStatus.UNKNOWN
        )
    )
    specs = [
        ("Available", available_status, reason, message),
        (
            "Parked",
            parked_status,
            (
                "SnapshotConfirmed"
                if parked_status == WorkspaceConditionStatus.TRUE
                else (
                    "SnapshotReferenceMissing"
                    if observed == WorkspaceObservedState.SUSPENDED
                    else reason
                )
            ),
            (
                "The suspended workspace has a recorded snapshot."
                if parked_status == WorkspaceConditionStatus.TRUE
                else (
                    "Substrate reported suspension without a snapshot reference."
                    if observed == WorkspaceObservedState.SUSPENDED
                    else message
                )
            ),
        ),
        (
            "DesiredState",
            desired_status,
            (
                "DesiredStateObserved"
                if desired_status == WorkspaceConditionStatus.TRUE
                else reason
            ),
            f"Desired {desired.value}; observed {observed.value}.",
        ),
        (
            "ControlOperation",
            operation_status
            or (
                WorkspaceConditionStatus.TRUE
                if observed
                in (WorkspaceObservedState.RUNNING, WorkspaceObservedState.SUSPENDED)
                and desired_status == WorkspaceConditionStatus.TRUE
                else WorkspaceConditionStatus.UNKNOWN
            ),
            operation_reason or reason,
            operation_message or message,
        ),
    ]
    if suspend_allowed is not None:
        allowed, allowed_reason, allowed_message = suspend_allowed
        specs.append(
            (
                "SuspendAllowed",
                (
                    WorkspaceConditionStatus.TRUE
                    if allowed
                    else WorkspaceConditionStatus.FALSE
                ),
                allowed_reason,
                allowed_message,
            )
        )

    old = {item.type: item for item in previous}
    result = []
    for condition_type, status, item_reason, item_message in specs:
        prior = old.get(condition_type)
        changed = (
            prior is None
            or prior.status != status
            or prior.reason != item_reason
            or prior.message != item_message
        )
        result.append(
            _condition(
                condition_type,
                status,
                item_reason,
                item_message,
                at if changed else prior.last_transition_time,
            )
        )
    return tuple(result)


def _transition(
    previous: WorkspaceLifecycle | None,
    state: WorkspaceObservedState,
    reason: str,
    at: datetime,
) -> WorkspaceTransition | None:
    if previous is not None and previous.observed_state == state:
        return previous.last_transition
    return WorkspaceTransition(
        from_state=previous.observed_state if previous else None,
        to_state=state,
        reason=reason,
        occurred_at=at,
    )


def _actor_reason(state: ActorState) -> tuple[str, str]:
    messages = {
        ActorState.RUNNING: (
            "ActorRunning",
            "Substrate reports the workspace actor running.",
        ),
        ActorState.RESUMING: (
            "ActorResuming",
            "Substrate is restoring the workspace actor.",
        ),
        ActorState.SUSPENDING: (
            "ActorSuspending",
            "Substrate is suspending the workspace actor.",
        ),
        ActorState.SUSPENDED: (
            "ActorSuspended",
            "Substrate reports the workspace actor suspended.",
        ),
        ActorState.CRASHED: (
            "ActorCrashed",
            "Substrate reports the workspace actor crashed.",
        ),
        ActorState.DELETING: (
            "ActorDeleting",
            "Substrate reports the workspace actor deleting.",
        ),
        ActorState.PAUSED: (
            "UnexpectedPaused",
            "Substrate reported PAUSED; it is not confirmed parked.",
        ),
        ActorState.PAUSING: (
            "UnexpectedPausing",
            "Substrate reported PAUSING; lifecycle is not confirmed.",
        ),
        ActorState.REVERTING: (
            "ActorReverting",
            "Substrate is reverting the workspace actor.",
        ),
        ActorState.UNSPECIFIED: (
            "ActorStateUnknown",
            "Substrate did not report a recognized actor state.",
        ),
    }
    return messages[state]


def _lifecycle_from_row(row: dict) -> WorkspaceLifecycle:
    payload = {
        "workspace_id": row["workspace_id"],
        "session_id": row["workspace_id"],
        "desired_state": row["desired_state"],
        "observed_state": row["observed_state"],
        "manifest": _json_value(row["manifest"]),
        "conditions": _json_value(row["conditions"]),
        "last_transition": (
            _json_value(row["last_transition"]) if row.get("last_transition") else None
        ),
        "operation_id": row.get("operation_id"),
        "snapshot_ref": row.get("snapshot_ref"),
        "last_activity_at": row.get("last_activity_at"),
        "ownership_generation": row["ownership_generation"],
        "updated_at": row["updated_at"],
    }
    return WorkspaceLifecycle.model_validate_json(
        json.dumps(payload, default=lambda value: value.isoformat())
    )


def _manifest_from_session(row: dict) -> WorkspaceManifest:
    raw_kind = row.get("agent_kind")
    agent_kinds = (
        (WorkspaceAgentKind(raw_kind),)
        if raw_kind in {kind.value for kind in WorkspaceAgentKind}
        else ()
    )
    return WorkspaceManifest(
        repo_url=row.get("repo_url"),
        branch=row.get("branch_name") or row.get("base_branch") or "main",
        agent_kinds=agent_kinds,
        skills=(),
        mcp_servers=(),
        egress_allowlist=(),
        resource_class="default",
        dev=None,
    )


async def _get_lifecycle_row(workspace_id: str) -> dict | None:
    async with db.connection() as conn:
        row = await conn.fetchrow(
            """SELECT l.*, b.ownership_generation, b.external_snapshot_uri
               FROM workspace_lifecycles l
               JOIN workspace_bindings b USING (workspace_id)
               WHERE l.workspace_id=$1""",
            workspace_id,
        )
    return dict(row) if row else None


async def get_workspace_lifecycle(workspace_id: str) -> WorkspaceLifecycle | None:
    row = await _get_lifecycle_row(workspace_id)
    return _lifecycle_from_row(row) if row else None


async def ensure_workspace_lifecycle(workspace_id: str) -> WorkspaceLifecycle | None:
    """Backfill a declarative manifest and lifecycle record for an existing actor binding."""
    async with db.connection() as conn:
        row = await conn.fetchrow(
            """SELECT b.*, s.repo_url, s.branch_name, s.base_branch, n.kind AS agent_kind
               FROM workspace_bindings b
               JOIN sessions s ON s.id=b.workspace_id
               LEFT JOIN native_bindings n ON n.session_id=b.workspace_id
               WHERE b.workspace_id=$1""",
            workspace_id,
        )
    if row is None:
        return None
    binding = dict(row)
    existing = await get_workspace_lifecycle(workspace_id)
    if existing:
        return existing

    at = datetime.now(UTC)
    initial_state = (
        WorkspaceObservedState.RUNNING
        if binding["observed_state"] == "ready"
        else WorkspaceObservedState.UNKNOWN
    )
    reason = (
        "LifecycleInitialized"
        if initial_state == WorkspaceObservedState.UNKNOWN
        else "ActorRunning"
    )
    message = (
        "Lifecycle tracking was initialized; refresh status to inspect Substrate."
        if initial_state == WorkspaceObservedState.UNKNOWN
        else "Substrate previously reported the workspace ready."
    )
    manifest = _manifest_from_session(binding)
    conditions = _conditions_for(
        observed=initial_state,
        desired=WorkspaceDesiredState.RUNNING,
        snapshot_ref=binding.get("external_snapshot_uri"),
        reason=reason,
        message=message,
        at=at,
    )
    async with db.connection() as conn:
        await conn.execute(
            """INSERT INTO workspace_lifecycles
               (workspace_id, desired_state, observed_state, manifest, conditions, snapshot_ref,
                updated_at)
               VALUES ($1,$2,$3,$4::jsonb,$5::jsonb,$6,$7)
               ON CONFLICT (workspace_id) DO NOTHING""",
            workspace_id,
            WorkspaceDesiredState.RUNNING.value,
            initial_state.value,
            json.dumps(manifest.model_dump(mode="json")),
            json.dumps([condition.model_dump(mode="json") for condition in conditions]),
            binding.get("external_snapshot_uri"),
            at,
        )
    return await get_workspace_lifecycle(workspace_id)


async def list_workspace_lifecycles(user_id: str) -> list[WorkspaceLifecycle]:
    async with db.connection() as conn:
        rows = await conn.fetch(
            """SELECT b.workspace_id FROM workspace_bindings b
               JOIN sessions s ON s.id=b.workspace_id
               WHERE s.user_id=$1 ORDER BY s.created_at DESC""",
            user_id,
        )
    for row in rows:
        await ensure_workspace_lifecycle(row["workspace_id"])
    async with db.connection() as conn:
        lifecycle_rows = await conn.fetch(
            """SELECT l.*, b.ownership_generation, b.external_snapshot_uri
               FROM workspace_lifecycles l
               JOIN workspace_bindings b USING (workspace_id)
               JOIN sessions s ON s.id=b.workspace_id
               WHERE s.user_id=$1 ORDER BY s.created_at DESC""",
            user_id,
        )
    return [_lifecycle_from_row(dict(row)) for row in lifecycle_rows]


async def _save_lifecycle(conn, lifecycle: WorkspaceLifecycle) -> None:
    await conn.execute(
        """UPDATE workspace_lifecycles
           SET desired_state=$2, observed_state=$3, conditions=$4::jsonb,
               last_transition=$5::jsonb, operation_id=$6, snapshot_ref=$7, updated_at=$8
           WHERE workspace_id=$1""",
        lifecycle.workspace_id,
        lifecycle.desired_state.value,
        lifecycle.observed_state.value,
        json.dumps([item.model_dump(mode="json") for item in lifecycle.conditions]),
        (
            json.dumps(lifecycle.last_transition.model_dump(mode="json"))
            if lifecycle.last_transition
            else None
        ),
        lifecycle.operation_id,
        lifecycle.snapshot_ref,
        lifecycle.updated_at,
    )


async def _record_observation(
    workspace_id: str,
    *,
    actor: ActorRecord | None = None,
    failure: tuple[WorkspaceObservedState, str, str] | None = None,
    binding_error: str | None = None,
    desired_state: WorkspaceDesiredState | None = None,
    operation_status: WorkspaceConditionStatus | None = None,
    operation_reason: str | None = None,
    operation_message: str | None = None,
    suspend_allowed: tuple[bool, str, str] | None = None,
) -> WorkspaceLifecycle:
    previous = await ensure_workspace_lifecycle(workspace_id)
    if previous is None:
        raise ContractError(f"no workspace binding for {workspace_id}")
    at = datetime.now(UTC)
    if actor is not None:
        state = lifecycle_state(actor.state)
        reason, message = _actor_reason(actor.state)
        snapshot_ref = (
            actor.external_snapshot_uri
            if state == WorkspaceObservedState.SUSPENDED
            else actor.external_snapshot_uri or previous.snapshot_ref
        )
        observed_state_text = OBSERVED_STATE[actor.state]
        last_error = (
            binding_error
            if binding_error is not None
            else CRASHED_NOTE if actor.state == ActorState.CRASHED else None
        )
        if actor.state in (ActorState.PAUSED, ActorState.DELETING):
            last_error = reason
        async with db.connection() as conn:
            await conn.execute(
                """UPDATE workspace_bindings
                   SET observed_state=$2, observed_at=$3, external_snapshot_uri=$4,
                       last_error=$5, updated_at=NOW() WHERE workspace_id=$1""",
                workspace_id,
                observed_state_text,
                at,
                actor.external_snapshot_uri,
                last_error,
            )
    else:
        if failure is None:
            raise ValueError("actor or failure observation is required")
        state, reason, message = failure
        snapshot_ref = previous.snapshot_ref

    desired = desired_state or previous.desired_state
    stable = state in (
        WorkspaceObservedState.RUNNING,
        WorkspaceObservedState.SUSPENDED,
        WorkspaceObservedState.FAILED,
    )
    operation_id = None if stable else previous.operation_id
    conditions = _conditions_for(
        observed=state,
        desired=desired,
        snapshot_ref=snapshot_ref,
        reason=reason,
        message=message,
        at=at,
        previous=previous.conditions,
        operation_status=operation_status,
        operation_reason=operation_reason,
        operation_message=operation_message,
        suspend_allowed=suspend_allowed,
    )
    updated = WorkspaceLifecycle(
        workspace_id=workspace_id,
        session_id=workspace_id,
        desired_state=desired,
        observed_state=state,
        manifest=previous.manifest,
        conditions=conditions,
        last_transition=_transition(previous, state, reason, at),
        operation_id=operation_id,
        snapshot_ref=snapshot_ref,
        ownership_generation=previous.ownership_generation,
        updated_at=at,
    )
    async with db.connection() as conn:
        await _save_lifecycle(conn, updated)
    return await get_workspace_lifecycle(workspace_id) or updated


async def refresh_workspace_lifecycle(
    workspace_id: str, *, control: SubstrateControl | None = None
) -> WorkspaceLifecycle:
    """Observe the actor once; transport errors remain unknown until a later refresh."""
    control = control or _control()
    async with _lock(workspace_id):
        row = await get_workspace(workspace_id)
        if row is None:
            raise ContractError(f"no workspace binding for {workspace_id}")
        await ensure_workspace_lifecycle(workspace_id)
        try:
            actor = await control.get_actor(row["atespace"], row["actor_name"])
        except Exception:
            return await _record_observation(
                workspace_id,
                failure=(
                    WorkspaceObservedState.UNKNOWN,
                    "ObservationUncertain",
                    "Substrate could not confirm workspace status. Refresh again before retrying an operation.",
                ),
            )
        if actor is None:
            return await _record_observation(
                workspace_id,
                failure=(
                    WorkspaceObservedState.FAILED,
                    "ActorMissing",
                    "The workspace binding exists but Substrate returned no actor.",
                ),
            )
        return await _record_observation(workspace_id, actor=actor)


async def _record_operation_failure(
    workspace_id: str,
    *,
    reason: str,
    message: str,
    uncertain: bool,
) -> WorkspaceLifecycle:
    previous = await ensure_workspace_lifecycle(workspace_id)
    if previous is None:
        raise ContractError(f"no workspace binding for {workspace_id}")
    at = datetime.now(UTC)
    state = WorkspaceObservedState.UNKNOWN if uncertain else previous.observed_state
    conditions = _conditions_for(
        observed=state,
        desired=previous.desired_state,
        snapshot_ref=previous.snapshot_ref,
        reason=reason,
        message=message,
        at=at,
        previous=previous.conditions,
        operation_status=(
            WorkspaceConditionStatus.UNKNOWN
            if uncertain
            else WorkspaceConditionStatus.FALSE
        ),
        operation_reason=reason,
        operation_message=message,
    )
    updated = WorkspaceLifecycle(
        workspace_id=previous.workspace_id,
        session_id=previous.session_id,
        desired_state=previous.desired_state,
        observed_state=state,
        manifest=previous.manifest,
        conditions=conditions,
        last_transition=_transition(previous, state, reason, at),
        operation_id=previous.operation_id if uncertain else None,
        snapshot_ref=previous.snapshot_ref,
        ownership_generation=previous.ownership_generation,
        updated_at=at,
    )
    async with db.connection() as conn:
        await _save_lifecycle(conn, updated)
    return await get_workspace_lifecycle(workspace_id) or updated


async def _delivery_states(workspace_id: str, *, conn=None) -> set[str]:
    if conn is None:
        async with db.connection() as connection:
            rows = await connection.fetch(
                """SELECT state FROM native_deliveries
                   WHERE session_id=$1 AND state = ANY($2)""",
                workspace_id,
                list(BLOCKING_DELIVERY_STATES),
            )
    else:
        rows = await conn.fetch(
            """SELECT state FROM native_deliveries
               WHERE session_id=$1 AND state = ANY($2)""",
            workspace_id,
            list(BLOCKING_DELIVERY_STATES),
        )
    return {row["state"] for row in rows}


async def _record_suspend_fence(
    workspace_id: str,
    reason: str,
    message: str,
    *,
    previous: WorkspaceLifecycle | None = None,
    conn=None,
) -> WorkspaceLifecycle:
    previous = previous or await ensure_workspace_lifecycle(workspace_id)
    if previous is None:
        raise ContractError(f"no workspace binding for {workspace_id}")
    at = datetime.now(UTC)
    conditions = _conditions_for(
        observed=previous.observed_state,
        desired=previous.desired_state,
        snapshot_ref=previous.snapshot_ref,
        reason=reason,
        message=message,
        at=at,
        previous=previous.conditions,
        operation_status=WorkspaceConditionStatus.FALSE,
        operation_reason=reason,
        operation_message=message,
        suspend_allowed=(False, reason, message),
    )
    updated = WorkspaceLifecycle(
        **{
            **previous.model_dump(),
            "conditions": conditions,
            "updated_at": at,
        }
    )
    if conn is None:
        async with db.connection() as connection:
            await _save_lifecycle(connection, updated)
    else:
        await _save_lifecycle(conn, updated)
    if conn is not None:
        return updated
    return await get_workspace_lifecycle(workspace_id) or updated


async def _reserve_operation(
    previous: WorkspaceLifecycle,
    desired_state: WorkspaceDesiredState,
    *,
    only_if_idle: bool = False,
) -> WorkspaceLifecycle | None:
    at = datetime.now(UTC)
    transitional = (
        WorkspaceObservedState.SUSPENDING
        if desired_state == WorkspaceDesiredState.SUSPENDED
        else WorkspaceObservedState.RESUMING
    )
    operation_id = str(uuid.uuid4())
    reason = (
        "SuspendRequested"
        if desired_state == WorkspaceDesiredState.SUSPENDED
        else "ResumeRequested"
    )
    message = (
        f"Mainloop recorded a request to make the workspace {desired_state.value}."
    )
    conditions = _conditions_for(
        observed=transitional,
        desired=desired_state,
        snapshot_ref=previous.snapshot_ref,
        reason=reason,
        message=message,
        at=at,
        previous=previous.conditions,
        operation_status=WorkspaceConditionStatus.UNKNOWN,
        operation_reason=reason,
        operation_message=message,
        suspend_allowed=(
            (
                True,
                "NoOpenDelivery",
                "No open or uncertain delivery blocks suspension.",
            )
            if desired_state == WorkspaceDesiredState.SUSPENDED
            else None
        ),
    )
    updated = WorkspaceLifecycle(
        workspace_id=previous.workspace_id,
        session_id=previous.session_id,
        desired_state=desired_state,
        observed_state=transitional,
        manifest=previous.manifest,
        conditions=conditions,
        last_transition=_transition(previous, transitional, reason, at),
        operation_id=operation_id,
        snapshot_ref=previous.snapshot_ref,
        ownership_generation=previous.ownership_generation,
        updated_at=at,
    )
    fence_detail: str | None = None
    async with db.connection() as conn:
        async with conn.transaction():
            binding = await conn.fetchrow(
                """SELECT ownership_generation,desired_state FROM workspace_bindings
                   WHERE workspace_id=$1 FOR UPDATE""",
                previous.workspace_id,
            )
            if binding is None:
                raise ContractError(f"no workspace binding for {previous.workspace_id}")
            if binding.get("desired_state") == "deleting":
                raise ContractError("The workspace is being deleted.")
            if binding["ownership_generation"] != previous.ownership_generation:
                raise StaleOwnership(
                    f"workspace {previous.workspace_id} changed during lifecycle request"
                )
            if only_if_idle:
                if desired_state != WorkspaceDesiredState.SUSPENDED:
                    raise ValueError("only_if_idle applies to suspension")
                dev = previous.manifest.dev
                if dev is None:
                    return None
                activity = await conn.fetchrow(
                    """SELECT last_activity_at,
                              (SELECT MAX(created_at) FROM native_deliveries
                               WHERE session_id=$1) AS last_delivery_at
                       FROM workspace_lifecycles WHERE workspace_id=$1 FOR UPDATE""",
                    previous.workspace_id,
                )
                if activity is None:
                    raise ContractError(
                        f"no workspace lifecycle for {previous.workspace_id}"
                    )
                last_active = max(
                    value
                    for value in (
                        activity["last_activity_at"],
                        activity["last_delivery_at"],
                    )
                    if value is not None
                )
                if last_active > at - timedelta(minutes=dev.idle_timeout_minutes):
                    return None
            if desired_state == WorkspaceDesiredState.SUSPENDED:
                fence_reason = suspend_fence_reason(
                    await _delivery_states(previous.workspace_id, conn=conn)
                )
                if fence_reason:
                    fence_detail = _suspend_fence_detail(fence_reason)
                    await _record_suspend_fence(
                        previous.workspace_id,
                        fence_reason,
                        fence_detail,
                        previous=previous,
                        conn=conn,
                    )
            if fence_detail is None:
                tag = await conn.execute(
                    """UPDATE workspace_bindings
                       SET ownership_generation=ownership_generation+1, updated_at=NOW()
                       WHERE workspace_id=$1 AND ownership_generation=$2""",
                    previous.workspace_id,
                    previous.ownership_generation,
                )
                if tag.endswith(" 0"):
                    raise StaleOwnership(
                        f"workspace {previous.workspace_id} changed during lifecycle request"
                    )
                await _save_lifecycle(conn, updated)
    if fence_detail is not None:
        raise ContractError(fence_detail)
    return await get_workspace_lifecycle(previous.workspace_id) or updated


async def _request_workspace_state(
    workspace_id: str,
    desired_state: WorkspaceDesiredState,
    *,
    control: SubstrateControl | None = None,
    only_if_idle: bool = False,
) -> WorkspaceLifecycle | None:
    control = control or _control()
    async with _lock(workspace_id):
        previous = await ensure_workspace_lifecycle(workspace_id)
        row = await get_workspace(workspace_id)
        if previous is None or row is None:
            raise ContractError(f"no workspace binding for {workspace_id}")
        if (
            previous.desired_state == desired_state
            and previous.observed_state.value == desired_state.value
            and previous.operation_id is None
        ):
            return previous

        # A pending operation means a previous control call may have timed out. Its original
        # intent is already durable; inspect that actor before deciding whether a new operation
        # can be recorded. First attempts persist intent before the first Substrate call.
        had_pending_operation = previous.operation_id is not None
        if not had_pending_operation:
            reserved = (
                await _reserve_operation(previous, desired_state, only_if_idle=True)
                if only_if_idle
                else await _reserve_operation(previous, desired_state)
            )
            if reserved is None:
                return None
            previous = reserved

        try:
            actor = await control.get_actor(row["atespace"], row["actor_name"])
        except Exception:
            return await _record_operation_failure(
                workspace_id,
                reason="ObservationUncertain",
                message="Substrate status is unknown. Refresh status before retrying the operation.",
                uncertain=True,
            )
        if actor is None:
            return await _record_observation(
                workspace_id,
                failure=(
                    WorkspaceObservedState.FAILED,
                    "ActorMissing",
                    "The workspace binding exists but Substrate returned no actor.",
                ),
                desired_state=desired_state,
            )

        observed = lifecycle_state(actor.state)
        if observed.value == desired_state.value:
            if had_pending_operation and previous.desired_state != desired_state:
                # Finish reconciling the old intent, then persist the new request. The observed
                # actor already matches it, so no second control mutation is needed.
                previous = await _record_observation(
                    workspace_id, actor=actor, desired_state=previous.desired_state
                )
                await _reserve_operation(previous, desired_state)
            return await _record_observation(
                workspace_id, actor=actor, desired_state=desired_state
            )
        if observed in (
            WorkspaceObservedState.SUSPENDING,
            WorkspaceObservedState.RESUMING,
            WorkspaceObservedState.UNKNOWN,
            WorkspaceObservedState.FAILED,
        ):
            reason, message = _actor_reason(actor.state)
            return await _record_observation(
                workspace_id,
                actor=actor,
                operation_status=WorkspaceConditionStatus.UNKNOWN,
                operation_reason=reason,
                operation_message=message,
                desired_state=(
                    previous.desired_state
                    if had_pending_operation and previous.desired_state != desired_state
                    else desired_state
                ),
            )

        if had_pending_operation:
            # The inspected actor is stable in the opposite state. Retire the uncertain
            # operation record before persisting a fresh attempt; this is the required
            # inspect-before-retry fence.
            previous = await _record_observation(
                workspace_id, actor=actor, desired_state=previous.desired_state
            )
            previous = await _reserve_operation(previous, desired_state)

        if desired_state == WorkspaceDesiredState.SUSPENDED:
            fence_reason = suspend_fence_reason(await _delivery_states(workspace_id))
            if fence_reason:
                detail = _suspend_fence_detail(fence_reason)
                await _record_observation(
                    workspace_id,
                    actor=actor,
                    desired_state=desired_state,
                    operation_status=WorkspaceConditionStatus.FALSE,
                    operation_reason=fence_reason,
                    operation_message=detail,
                    suspend_allowed=(False, fence_reason, detail),
                )
                raise ContractError(detail)

        try:
            if desired_state == WorkspaceDesiredState.SUSPENDED:
                actor = await control.suspend_actor(row["atespace"], row["actor_name"])
            else:
                actor = await control.resume_actor(row["atespace"], row["actor_name"])
        except TransportError:
            return await _record_operation_failure(
                workspace_id,
                reason="OperationOutcomeUnknown",
                message="Substrate did not confirm the operation. Refresh status before retrying; Mainloop will not replay it automatically.",
                uncertain=True,
            )
        except RuntimeError:
            return await _record_operation_failure(
                workspace_id,
                reason="OperationRejected",
                message="Substrate rejected the lifecycle request. Refresh status before retrying.",
                uncertain=False,
            )
        except Exception:
            return await _record_operation_failure(
                workspace_id,
                reason="OperationOutcomeUnknown",
                message="Substrate did not confirm the operation. Refresh status before retrying; Mainloop will not replay it automatically.",
                uncertain=True,
            )
        observed = lifecycle_state(actor.state)
        if observed.value == desired_state.value and (
            desired_state != WorkspaceDesiredState.SUSPENDED
            or actor.external_snapshot_uri is not None
        ):
            operation_status = WorkspaceConditionStatus.TRUE
            operation_reason = "OperationConfirmed"
            operation_message = (
                f"Substrate confirmed the workspace {desired_state.value}."
            )
        elif observed in (
            WorkspaceObservedState.RUNNING,
            WorkspaceObservedState.SUSPENDED,
            WorkspaceObservedState.FAILED,
        ):
            operation_status = WorkspaceConditionStatus.FALSE
            operation_reason = "DesiredStateNotReached"
            operation_message = f"Substrate observed {observed.value}; desired {desired_state.value} was not confirmed."
        else:
            operation_status = WorkspaceConditionStatus.UNKNOWN
            operation_reason = "ActorTransitionInProgress"
            operation_message = (
                "Substrate has not reached a stable state; refresh status to reconcile."
            )
        return await _record_observation(
            workspace_id,
            actor=actor,
            desired_state=desired_state,
            operation_status=operation_status,
            operation_reason=operation_reason,
            operation_message=operation_message,
        )
