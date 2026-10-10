"""Application seam shared by owner REST and binding-scoped MCP.

Ports have no default runtime implementation. Disconnected mutations persist their
idempotent blocked operation but never reserve capacity, create sessions or dispatch.
"""

import asyncio
import logging
from dataclasses import dataclass, field
from typing import Protocol

from asyncpg import Connection
from mainloop.config import settings
from mainloop.db import tasks as store
from mainloop.db.postgres import Database
from mainloop.providers import qualify_task_profile, registry
from mainloop.runtime.policy import PolicyError
from mainloop.tasks import lifecycle, projection
from mainloop.tasks.events import dispatch_committed_events
from mainloop.tasks.principal import TaskPrincipal

from models.task import (
    Task,
    TaskAction,
    TaskArtifact,
    TaskCreate,
    TaskEligibility,
    TaskOperation,
    TaskReassign,
    TaskReport,
    TaskView,
)

logger = logging.getLogger(__name__)


class ProvisioningPort(Protocol):
    async def create(
        self,
        conn: Connection,
        principal: TaskPrincipal,
        request: TaskCreate,
        operation: TaskOperation,
    ) -> TaskOperation: ...
    async def cancel(
        self,
        conn: Connection,
        principal: TaskPrincipal,
        task: Task,
        request: TaskAction,
        operation: TaskOperation,
    ) -> TaskOperation: ...
    async def reconcile(self, database: Database, operation: TaskOperation) -> None: ...


class HandoffPort(Protocol):
    async def start(
        self,
        conn: Connection,
        principal: TaskPrincipal,
        task: Task,
        request: TaskAction | TaskReassign,
        operation: TaskOperation,
    ) -> TaskOperation: ...
    async def reconcile(self, database: Database, operation: TaskOperation) -> None: ...


class ProjectionPort(Protocol):
    async def refresh(self, database: Database, task_id: str) -> None: ...


@dataclass
class TaskPorts:
    provisioning: ProvisioningPort | None = None
    handoff: HandoffPort | None = None
    projection: ProjectionPort | None = None
    _projection_cursor: str = field(default="", init=False, repr=False)


ports = TaskPorts()


async def select_profile(conn, principal, request, role, *, allow_fixture=False):
    identifier = request.provider_profile_id
    source = "explicit"
    if request.project_id:
        await store.project(conn, request.project_id, principal.owner_id)
    if identifier is None and request.project_id:
        preference = await store.preference(
            conn, request.project_id, principal.owner_id
        )
        identifier = preference.profile_id
        source = "project_default"
    if identifier is None:
        identifier = settings.task_default_provider_profile_id
        source = "installation_default"
    if principal.role == "supervisor":
        parent = await store.get_task(conn, principal.task_id, principal, lock=True)
        if request.project_id != parent.project_id:
            raise store.TaskError(403, "inherited_project_required")
        if parent.provider_constraint:
            if request.provider_profile_id not in (None, parent.provider_constraint):
                raise store.TaskError(403, "inherited_provider_constraint")
            identifier, source = parent.provider_constraint, "inherited"
    try:
        profile = registry().resolve(identifier, role, selecting=True)
        return (
            qualify_task_profile(
                profile, role, request.mode, allow_fixture=allow_fixture
            ),
            source,
        )
    except ValueError as exc:
        raise store.TaskError(422, str(exc)) from exc


async def read(conn, principal, task_id):
    task = await store.get_task(conn, task_id, principal)
    operation_rows = await conn.fetch(
        "SELECT snapshot FROM task_operations WHERE task_id=$1 ORDER BY created_at,id",
        task.id,
    )
    artifact_rows = await conn.fetch(
        "SELECT a.id FROM task_artifacts a JOIN task_operations o ON o.id=a.operation_id WHERE o.task_id=$1 ORDER BY a.created_at,a.id",
        task.id,
    )
    report_rows = await conn.fetch(
        "SELECT snapshot FROM task_reports WHERE task_id=$1 ORDER BY created_at,id",
        task.id,
    )
    # Later slices provide per-task eligibility after durable fence/publication checks.
    return TaskView(
        task=task,
        attempts=tuple(await store.attempts(conn, task.id)),
        operations=tuple(
            TaskOperation.model_validate(store.decode(r["snapshot"]))
            for r in operation_rows
        ),
        artifacts=tuple(
            [
                TaskArtifact.model_validate(
                    await store.get_artifact(conn, r["id"], principal)
                )
                for r in artifact_rows
            ]
        ),
        reports=tuple(
            TaskReport.model_validate(store.decode(r["snapshot"])) for r in report_rows
        ),
        projection=await projection.read(conn, task),
        actions={
            name: TaskEligibility(reason=reason)
            for name, reason in (
                ("retry", "handoff_unavailable"),
                ("reassign", "handoff_unavailable"),
                ("cancel", "cancel_unavailable"),
            )
        },
    )


async def read_native(conn, principal, task_id):
    """Binding-visible facts and exact linked continuation, without private history."""
    task, artifacts, reports = await store.load_continuation(conn, task_id, principal)
    current_reports = []
    if task.current_attempt_id is not None:
        for row in await conn.fetch(
            """SELECT * FROM task_reports WHERE task_id=$1 AND attempt_id=$2
               ORDER BY created_at,id""",
            task.id,
            task.current_attempt_id,
        ):
            payload = store.decode(row["snapshot"])
            if store.digest(payload) != row["request_digest"]:
                raise store.TaskError(409, "report_integrity_error")
            value = TaskReport.model_validate(payload)
            if (value.task_id, value.attempt_id, value.request_id) != (
                task.id,
                task.current_attempt_id,
                row["request_id"],
            ):
                raise store.TaskError(409, "continuation_identity_mismatch")
            current_reports.append(value)
    linked_operation = artifacts[0].operation_id if artifacts else None
    operation_rows = await conn.fetch(
        "SELECT * FROM task_operations WHERE task_id=$1 ORDER BY created_at,id", task.id
    )
    operations = []
    for row in operation_rows:
        value = TaskOperation.model_validate(store.decode(row["snapshot"]))
        if (
            any(
                getattr(value, key) != row[key]
                for key in ("id", "owner_id", "task_id", "attempt_id", "kind", "state")
            )
            or value.owner_id != task.owner_id
        ):
            raise store.TaskError(409, "continuation_identity_mismatch")
        operations.append(
            value.model_copy(
                update={
                    "request_payload": {},
                    "principal_key": "",
                    "request_digest": "",
                    "checkpoint_ref": (
                        value.checkpoint_ref if value.id == linked_operation else None
                    ),
                    "manifest_ref": (
                        value.manifest_ref if value.id == linked_operation else None
                    ),
                }
            )
        )
    return TaskView(
        task=task,
        attempts=tuple(
            value.model_copy(
                update={
                    "evidence_refs": (),
                    "result_ref": None,
                    "retention_hold": None,
                    "brief_delivery_id": None,
                    "checkpoint_ref": (
                        value.checkpoint_ref
                        if artifacts and value.id == task.current_attempt_id
                        else None
                    ),
                    "manifest_ref": (
                        value.manifest_ref
                        if artifacts and value.id == task.current_attempt_id
                        else None
                    ),
                }
            )
            for value in await store.attempts(conn, task.id)
        ),
        operations=tuple(operations),
        artifacts=artifacts,
        reports=(*reports, *current_reports),
        projection=(await projection.read(conn, task)).model_copy(
            update={"pending_approval_ids": ()}
        ),
        actions={
            name: TaskEligibility(reason=reason)
            for name, reason in (
                ("retry", "handoff_unavailable"),
                ("reassign", "handoff_unavailable"),
                ("cancel", "cancel_unavailable"),
            )
        },
    )


async def mutate(
    conn, principal, kind, request, *, task_id=None, installed_ports=ports
):
    await store.admission_lock(conn)
    await store.validate_principal(conn, principal)
    if principal.role not in ("owner", "main", "supervisor"):
        raise store.TaskError(403, "task_management_denied")
    if kind != "create" and task_id is None:
        raise store.TaskError(422, "task_id_required")
    # Keep global -> request -> task locking, but authorize the target before
    # inserting its FK reference. Replays still bypass stale-version checks.
    await store.lock_operation_request(conn, principal, request.request_id)
    task = None
    if kind != "create":
        task = await store.get_task(conn, task_id, principal, manage=True, lock=True)
    operation, fresh = await store.begin_operation(
        conn,
        principal,
        request.request_id,
        kind,
        request.model_dump(mode="json"),
        task_id=task_id,
    )
    if not fresh:
        return operation
    if kind != "create":
        if (task.version, task.current_attempt_id) != (
            request.expected_version,
            request.expected_attempt_id,
        ):
            raise store.TaskError(409, "stale_task_attempt")
    if kind == "create" and request.project_id:
        await store.project(conn, request.project_id, principal.owner_id)
    if principal.role == "supervisor" and kind == "create":
        parent = await store.get_task(conn, principal.task_id, principal, lock=True)
        if request.project_id != parent.project_id:
            raise store.TaskError(403, "inherited_project_required")
        if parent.provider_constraint and request.provider_profile_id not in (
            None,
            parent.provider_constraint,
        ):
            raise store.TaskError(403, "inherited_provider_constraint")
    if (
        kind == "reassign"
        and principal.role == "supervisor"
        and task.provider_constraint
    ):
        if request.target_profile_id != task.provider_constraint:
            raise store.TaskError(403, "inherited_provider_constraint")
    port = (
        installed_ports.provisioning
        if kind in ("create", "cancel")
        else installed_ports.handoff
    )
    if port is None:
        reason = {
            "create": "provisioning_unavailable",
            "cancel": "cancel_unavailable",
            "retry": "handoff_unavailable",
            "reassign": "handoff_unavailable",
        }[kind]
        operation = operation.model_copy(update={"state": "blocked", "reason": reason})
        await store.save_operation(conn, operation)
        return operation
    if kind == "create":
        return await port.create(conn, principal, request, operation)
    if kind == "cancel":
        return await port.cancel(conn, principal, task, request, operation)
    return await port.start(conn, principal, task, request, operation)


async def reconcile_once(database, *, installed_ports=ports):
    # No new scheduler or native writer. Later slices install idempotent reconciliation
    # keyed by the existing operation ID. Unavailable operations are not dispatched.
    async with database.connection() as conn:
        rows = await conn.fetch(
            "SELECT snapshot FROM task_operations WHERE state NOT IN ('completed','blocked') ORDER BY created_at,id"
        )
    from models.task import TaskOperation

    for row in rows:
        operation = TaskOperation.model_validate(store.decode(row["snapshot"]))
        port = (
            installed_ports.provisioning
            if operation.kind in ("create", "cancel")
            else installed_ports.handoff
        )
        if port is not None:
            await port.reconcile(database, operation)

    from mainloop.tasks.reports import dispatch_pending

    await reconcile_failed_deliveries(database)
    await dispatch_pending(database)
    await reconcile_projections(database, installed_ports)


async def reconcile_failed_deliveries(database):
    """Recover authoritative FAILED first/sole briefs after create completion."""
    from mainloop.push_gate import lifecycle as push_lifecycle

    async with database.connection() as conn:
        rows = await conn.fetch(
            """SELECT a.id,a.binding_id,d.message_id
               FROM task_attempts a JOIN tasks t ON t.current_attempt_id=a.id
               JOIN native_deliveries d ON d.session_id=a.binding_id
               WHERE a.state='active' AND d.state='failed'
                 AND d.message_id=a.snapshot->>'brief_delivery_id' AND d.source='brief'
                 AND d.task_id IS NOT NULL
                 AND d.evidence_ref='a2a:task/' || d.task_id || '#failed'
                 AND NOT EXISTS (SELECT 1 FROM native_deliveries other
                   WHERE other.session_id=d.session_id AND other.message_id<>d.message_id)
                 AND t.status NOT IN ('completed','failed','cancelled')
               ORDER BY a.id LIMIT 10"""
        )
    for row in rows:
        try:
            async with database.connection() as conn:
                async with (
                    push_lifecycle.locked(conn, row["binding_id"]),
                    lifecycle.locked(conn, row["binding_id"]),
                    conn.transaction(),
                ):
                    await lifecycle.block_failed_delivery(
                        conn, row["id"], row["message_id"]
                    )
        except Exception:
            logger.exception(
                "Task delivery failure reconciliation failed: %s", row["id"]
            )


PROJECTION_RECONCILE_LIMIT = 10
# New refreshes start only within this admission window of a pass...
PROJECTION_RECONCILE_BUDGET_SECONDS = 2.0
# ...but an admitted refresh may finish its whole observation chain. A cold
# GitHub observation makes ~17 sequential calls (5 permission-scoped token
# mints), each separately bounded at 15s; 60s covers that at ~3.5s per call
# plus persistence, while one stuck refresh delays the reconciler by at most this.
PROJECTION_REFRESH_DEADLINE_SECONDS = 60.0


async def reconcile_projections(database, installed_ports):
    """Rotate current code tasks through the installed observer, never dispatch merges."""
    if installed_ports.projection is None:
        return
    loop = asyncio.get_running_loop()
    started = loop.time()
    try:
        async with asyncio.timeout(PROJECTION_RECONCILE_BUDGET_SECONDS):
            async with database.connection() as conn:
                rows = await conn.fetch(
                    """SELECT t.id FROM tasks t
                       JOIN task_attempts a ON a.id=t.current_attempt_id
                       JOIN native_bindings b ON b.session_id=a.binding_id
                       JOIN sessions s ON s.id=b.session_id
                       WHERE t.mode='code' AND a.state='active'
                         AND t.status NOT IN ('completed','failed','cancelled')
                         AND b.token_hash IS NOT NULL AND b.kagent_deleted_at IS NULL
                         AND s.archived_at IS NULL
                       ORDER BY (t.id <= $1),t.id LIMIT $2""",
                    installed_ports._projection_cursor,
                    PROJECTION_RECONCILE_LIMIT,
                )
    except TimeoutError:
        logger.warning("Task projection selection timed out")
        return
    for row in rows:
        if loop.time() - started >= PROJECTION_RECONCILE_BUDGET_SECONDS:
            return
        # Advance before work: a slow, revoked or failing source cannot
        # monopolize the next pass. The port revalidates live authority.
        installed_ports._projection_cursor = row["id"]
        refresh_started = loop.time()
        deadline = asyncio.timeout(PROJECTION_REFRESH_DEADLINE_SECONDS)
        try:
            async with deadline:
                await installed_ports.projection.refresh(database, row["id"])
        except TimeoutError:
            if not deadline.expired():
                logger.exception("Task projection refresh failed: %s", row["id"])
                continue
            logger.warning(
                "Task projection refresh timed out: task=%s step=%s elapsed=%.1fs",
                row["id"],
                getattr(installed_ports.projection, "step", "unknown"),
                loop.time() - refresh_started,
            )
        except (PolicyError, lifecycle.LifecycleDenied):
            logger.debug("Task projection source lost authority: %s", row["id"])
        except Exception:
            logger.exception("Task projection refresh failed: %s", row["id"])


class SSETaskEventSink:
    async def publish(self, owner_id, event):
        from mainloop.sse import SSEEvent, event_bus

        await event_bus.publish_to_user(
            owner_id,
            SSEEvent(
                event="task:updated",
                data=event.model_dump(mode="json"),
                id=event.event_id,
            ),
        )


async def reconciliation_dispatcher(database):
    sink = SSETaskEventSink()
    while True:
        try:
            await reconcile_once(database)
            await dispatch_committed_events(database, sink)
        except asyncio.CancelledError:
            raise
        except Exception:
            logger.exception(
                "Task reconciliation failed; committed operations remain recoverable"
            )
        await asyncio.sleep(settings.task_reconcile_seconds)
