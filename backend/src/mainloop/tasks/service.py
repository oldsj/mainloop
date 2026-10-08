"""Application seam shared by owner REST and binding-scoped MCP.

Ports have no default runtime implementation. Disconnected mutations persist their
idempotent blocked operation but never reserve capacity, create sessions or dispatch.
"""

import asyncio
import logging
from dataclasses import dataclass
from typing import Protocol

from asyncpg import Connection
from mainloop.config import settings
from mainloop.db import tasks as store
from mainloop.db.postgres import Database
from mainloop.providers import qualify_task_profile, registry
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
        projection=await store.projection(conn, task.id),
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

    await dispatch_pending(database)


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
