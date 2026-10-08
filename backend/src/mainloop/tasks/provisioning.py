"""Task provisioning: one creation path for supervisor and child attempts.

``create`` runs inside the REST/MCP transaction ``service.mutate`` opened. It writes the task, the
attempt (with its capacity slot and branch claim) and the session, checkout, environment and
identity rows together, so a rejected request leaves nothing behind. Everything that talks to
kagent or Kubernetes happens afterwards in ``reconcile``, one idempotent step at a time, keyed by
the operation:

1. create the kagent Session (stable request id; a lost response is retried, never repeated);
2. publish the scoped credential and confirm the Session can take a turn;
3. in one transaction, revalidate the parent's authority, record the single first brief and move
   the attempt to ``active``.

Any definite failure, a parent that lost authority, or a cancel goes the other way: the attempt
drains, its identity is revoked, the runtime is confirmed gone, and only then is the branch claim
released and the capacity slot given back (``lifecycle.settle``). The attempt row is kept either
way, so a failed or rejected attempt stays auditable after its workspace rows are cleaned.

The role, parent, depth and topic come from the authenticated principal. Nothing a caller sends
selects a credential reference or an AgentRef.
"""

from __future__ import annotations

import logging
import uuid
from datetime import UTC, datetime

from mainloop.config import settings
from mainloop.db import db
from mainloop.db import tasks as store
from mainloop.push_gate import lifecycle as push_lifecycle
from mainloop.runtime import native_sessions as ns
from mainloop.runtime import workspaces
from mainloop.runtime.agent_credentials import reference_from_data, revoke
from mainloop.runtime.kagent_client import (
    KagentError,
    RuntimeState,
    SessionError,
)
from mainloop.services.github_repo import parse_github_repo
from mainloop.tasks import lifecycle
from mainloop.tasks.principal import TaskPrincipal
from mainloop.tasks.service import ports, select_profile

from models import SessionStatus, WorkspaceManifest
from models.task import Task, TaskAttempt, TaskCreate, TaskOperation

logger = logging.getLogger(__name__)

# CreateSession statuses that mean kagent rejected the request itself.
_REJECTED = (3, 7, 16)


class _RuntimeLost(Exception):
    """The attempt's kagent Session is gone or failed; a delegated runtime is not replaced."""


def _grant_kind(task: Task) -> str:
    return "workspace" if task.mode == "code" else "coordination"


def scope_evidence(task: Task, attempt: TaskAttempt) -> tuple[str, ...]:
    """Non-secret facts of what the attempt was given, for diagnosing a grant."""
    refs = [f"grant:{attempt.role}/{_grant_kind(task)}"]
    if task.mode == "code" and task.checkout is not None:
        refs.append(f"checkout:{task.checkout.branch}@{task.checkout.ref}")
        refs.append(f"writer-generation:{attempt.writer_generation}")
    if attempt.environment is not None:
        refs.append(f"environment:{attempt.environment.environment_id}")
    return tuple(refs)


class Provisioning:
    """The ``ProvisioningPort`` implementation."""

    # ---- in the request transaction ------------------------------------------------------

    async def create(
        self,
        conn,
        principal: TaskPrincipal,
        request: TaskCreate,
        operation: TaskOperation,
    ) -> TaskOperation:
        store.require_transaction(conn)
        if principal.role == "supervisor":
            role, depth = "child", 2
            parent = await store.get_task(conn, principal.task_id, principal, lock=True)
            parent_session = await self._live_parent_session(conn, parent)
            parent_id, root_id = parent.id, parent.root_task_id
            topic_id = request.topic_id or parent.topic_id
        elif principal.role in ("owner", "main"):
            role, depth = "supervisor", 1
            parent = None
            parent_session = principal.binding_id if principal.role == "main" else None
            parent_id, root_id = None, None
            topic_id = request.topic_id
        else:
            raise store.TaskError(403, "task_management_denied")

        profile, source = await select_profile(conn, principal, request, role)
        constraint = None
        if parent is not None and parent.provider_constraint:
            constraint = parent.provider_constraint
        elif source == "explicit" and parent is None:
            constraint = profile.id

        environment = None
        manifest = None
        if request.mode == "code":
            project = await store.project(conn, request.project_id, principal.owner_id)
            from mainloop.db.environments import EnvironmentError
            from mainloop.environments.resolution import resolve

            try:
                environment = await resolve(
                    conn, request.project_id, principal.owner_id
                )
            except EnvironmentError as exc:
                raise store.TaskError(422, "environment_unavailable") from exc
            repository = parse_github_repo(project["full_name"]).full_name
            manifest = WorkspaceManifest(
                repo_url=f"https://github.com/{repository}",
                ref=request.checkout.ref,
                branch=request.checkout.branch,
                depth=request.checkout.depth,
                agent_kind=profile.id,
            )

        now = datetime.now(UTC)
        task_id = uuid.uuid4().hex
        task = Task(
            id=task_id,
            owner_id=principal.owner_id,
            project_id=request.project_id,
            topic_id=topic_id,
            parent_task_id=parent_id,
            root_task_id=root_id or task_id,
            creator_binding_id=principal.binding_id,
            title=request.title,
            brief=request.brief,
            mode=request.mode,
            assigned_profile_id=profile.id,
            selection_source=source,
            provider_constraint=constraint,
            accepted_environment=environment,
            checkout=request.checkout,
            created_at=now,
            updated_at=now,
        )
        await store.insert_task(conn, task)
        task, attempt = await store.admit_attempt(
            conn, task, profile, role=role, depth=depth
        )
        try:
            enrolled = await workspaces.enroll_session(
                conn,
                user_id=principal.owner_id,
                kind=profile.id,
                role=role,
                mcp_grant_kind=_grant_kind(task),
                manifest=manifest,
                project_id=task.project_id,
                session_id=attempt.id,
                parent_session_id=parent_session,
                topic_id=task.topic_id,
                title=task.title,
                description=f"Task {role} attempt",
                prompt=task.brief,
                environment=environment,
                claim_branch=False,
            )
        except (workspaces.WorkspaceConflict, workspaces.WorkspaceRejected) as exc:
            raise store.TaskError(422, "workspace_unavailable") from exc
        attempt = await lifecycle.save_attempt(
            conn,
            attempt.model_copy(
                update={
                    "session_id": enrolled.workspace_id,
                    "binding_id": enrolled.workspace_id,
                    "workspace_id": enrolled.workspace_id,
                    "evidence_refs": scope_evidence(task, attempt),
                }
            ),
        )
        operation = operation.model_copy(
            update={
                "task_id": task.id,
                "attempt_id": attempt.id,
                "target_attempt_id": attempt.id,
                "state": "target_creating",
                "last_confirmed_step": "requested",
            }
        )
        await store.save_operation(conn, operation)
        return operation

    async def _live_parent_session(self, conn, parent: Task) -> str:
        """Prove the creating supervisor's session is active in this transaction."""
        row = await conn.fetchrow(
            """SELECT a.id,a.state,a.session_id FROM task_attempts a
               WHERE a.id=$1 AND a.role='supervisor' FOR SHARE""",
            parent.current_attempt_id,
        )
        if row is None or row["state"] != "active" or not row["session_id"]:
            raise store.TaskError(409, "parent_not_active")
        return row["session_id"]

    async def cancel(
        self,
        conn,
        principal: TaskPrincipal,
        task: Task,
        request,
        operation: TaskOperation,
    ) -> TaskOperation:
        """Record the intent; the reconciler drains, fences and settles the attempt.

        The request transaction already holds the admission lock, and draining takes the push
        locks first, so the drain is not done here.
        """
        attempt_id = task.current_attempt_id
        if attempt_id is None:
            if task.status not in ("completed", "failed", "cancelled"):
                updated = task.model_copy(
                    update={
                        "status": "cancelled",
                        "reason": None,
                        "version": task.version + 1,
                        "updated_at": datetime.now(UTC),
                    }
                )
                await store.save_task(conn, updated, task.version, "cancelled")
            state = "completed"
        else:
            state = "draining"
        operation = operation.model_copy(
            update={
                "attempt_id": attempt_id,
                "source_attempt_id": attempt_id,
                "state": state,
            }
        )
        await store.save_operation(conn, operation)
        return operation

    # ---- reconciliation ------------------------------------------------------------------

    async def reconcile(self, database, operation: TaskOperation) -> None:
        if operation.kind not in ("create", "cancel"):
            return
        key = f"task-operation:{operation.id}"
        async with database.connection() as conn:
            if not await conn.fetchval(
                "SELECT pg_try_advisory_lock(hashtextextended($1,0))", key
            ):
                return  # another process is reconciling this operation
            try:
                await self._step(operation.id)
            finally:
                await conn.fetchval(
                    "SELECT pg_advisory_unlock(hashtextextended($1,0))", key
                )

    async def _current(self, operation_id: str):
        async with db.connection() as conn:
            row = await conn.fetchrow(
                "SELECT snapshot FROM task_operations WHERE id=$1", operation_id
            )
            if row is None:
                return None, None
            operation = TaskOperation.model_validate(store.decode(row["snapshot"]))
            if operation.attempt_id is None:
                return operation, None
            attempt = await lifecycle.load_attempt(conn, operation.attempt_id)
            return operation, attempt

    async def _step(self, operation_id: str) -> None:
        operation, attempt = await self._current(operation_id)
        if operation is None or operation.state in ("completed", "blocked"):
            return
        if attempt is None:
            await self._complete(operation_id)
            return
        if operation.kind == "cancel":
            await self._abort(operation_id, attempt, "cancelled", discard=False)
            return
        if attempt.state == "active":
            await self._enroll_active(operation, attempt)
        elif attempt.state in lifecycle.FINAL:
            await self._complete(operation_id)
        elif attempt.state == "creating":
            if lifecycle.CREATE_REJECTED in attempt.evidence_refs:
                # Recover older persisted evidence too: this identity is closed to create.
                await self._abort(operation_id, attempt, "failed", discard=True)
            else:
                await self._advance(operation, attempt)
        elif attempt.state in ("draining", "fenced"):
            await self._abort(operation_id, attempt, "failed", discard=True)

    async def _advance(self, operation: TaskOperation, attempt: TaskAttempt) -> None:
        sid = attempt.session_id
        try:
            async with ns._lock(sid):
                binding = await ns.get_binding(sid)
                if binding is None:
                    raise RuntimeError("the attempt's binding is missing")
                session = await self._ensure_session(binding)
                async with lifecycle.guard(sid, "create"):
                    session = await self._ready(session)
                if session is None:
                    return
        except _RuntimeLost as exc:
            await self._abort(
                operation.id, attempt, "failed", discard=True, evidence=str(exc)
            )
            return
        except SessionError as exc:
            if exc.grpc_status in _REJECTED:
                await self._abort(
                    operation.id,
                    attempt,
                    "failed",
                    discard=True,
                    evidence=f"kagent-rejected:{exc.grpc_status}",
                )
                return
            await self._unconfirmed(operation, f"kagent create unconfirmed: {exc}")
            return
        except KagentError as exc:
            async with db.connection() as conn:
                current = await lifecycle.load_attempt(conn, attempt.id)
            if current and lifecycle.CREATE_REJECTED in current.evidence_refs:
                await self._abort(
                    operation.id,
                    current,
                    "failed",
                    discard=True,
                    evidence=lifecycle.CREATE_REJECTED,
                )
                return
            await self._unconfirmed(operation, f"kagent create unconfirmed: {exc}")
            return
        except (
            Exception
        ) as exc:  # noqa: BLE001 - e.g. a Secret outage; the next pass retries
            await self._unconfirmed(operation, f"provisioning step failed: {exc}")
            return
        await self._activate(operation.id, attempt)

    async def _ensure_session(self, binding: dict):
        """Create the attempt's kagent Session, or recognize the one a lost reply made."""
        if binding["kagent_session_id"] is not None:
            live = await ns._live_session(binding["kagent_session_id"])
            if live is None:
                raise _RuntimeLost("runtime-lost")
            await ns.validate_bound_session(binding, live)
            return live
        session = await ns._create_bound_session(binding)
        await ns.ledger.update_binding(
            binding["session_id"], kagent_session_id=session.id, standing_hash=None
        )
        return session

    async def _ready(self, session):
        try:
            return await ns.get_client().ensure_ready(
                session, timeout=settings.kagent_session_ready_timeout_seconds
            )
        except SessionError:
            live = await ns._live_session(session.id)
            if live is not None and live.state != RuntimeState.FAILED:
                return None  # still starting; the next pass looks again
            raise _RuntimeLost("runtime-failed") from None

    async def _unconfirmed(self, operation: TaskOperation, detail: str) -> None:
        ns._log_step_failure(
            "task_provisioning", operation.attempt_id, RuntimeError(detail)
        )
        async with db.connection() as conn, conn.transaction():
            current = await conn.fetchval(
                "SELECT snapshot FROM task_operations WHERE id=$1 FOR UPDATE",
                operation.id,
            )
            value = TaskOperation.model_validate(store.decode(current))
            if value.state in ("completed", "blocked"):
                return
            await store.save_operation(
                conn,
                value.model_copy(
                    update={"state": "uncertain", "reason": "reconciliation"}
                ),
            )

    async def _complete(self, operation_id: str, conn=None) -> None:
        async def run(c):
            row = await c.fetchval(
                "SELECT snapshot FROM task_operations WHERE id=$1 FOR UPDATE",
                operation_id,
            )
            value = TaskOperation.model_validate(store.decode(row))
            if value.state not in ("completed", "blocked"):
                await store.save_operation(
                    c, value.model_copy(update={"state": "completed", "reason": None})
                )

        if conn is not None:
            await run(conn)
            return
        async with db.connection() as c, c.transaction():
            await run(c)

    # ---- activation ----------------------------------------------------------------------

    async def _activate(self, operation_id: str, attempt: TaskAttempt) -> None:
        """Revalidate authority, record the one brief and go active; one transaction.

        Admission, the task row and the attempt row are locked, so a cancel, a fence or the
        parent losing authority commits strictly before (and denies) or after (and drains an
        attempt that already holds its brief). The brief is the only way work starts.
        """
        message_id = None
        text = None
        denied = None
        async with db.connection() as conn, conn.transaction():
            await store.admission_lock(conn)
            task = await lifecycle.load_task(conn, attempt.task_id, lock=True)
            current = await lifecycle.load_attempt(conn, attempt.id, lock=True)
            if current is None or current.state != "creating":
                return
            denied = await self._authority_denial(conn, task, current)
            if denied is None:
                conversation_id = await conn.fetchval(
                    "SELECT conversation_id FROM sessions WHERE id=$1",
                    current.session_id,
                )
                text = task.brief
                message_id = await ns.ledger.insert_brief(
                    conn,
                    session_id=current.session_id,
                    conversation_id=conversation_id,
                    text=text,
                )
                await lifecycle.save_attempt(
                    conn,
                    current.model_copy(
                        update={"state": "active", "brief_delivery_id": message_id}
                    ),
                )
                await store.save_task(
                    conn,
                    task.model_copy(
                        update={
                            "status": "running",
                            "version": task.version + 1,
                            "updated_at": datetime.now(UTC),
                        }
                    ),
                    task.version,
                    f"attempt:{current.id}:active",
                )
                value = TaskOperation.model_validate(
                    store.decode(
                        await conn.fetchval(
                            "SELECT snapshot FROM task_operations WHERE id=$1 FOR UPDATE",
                            operation_id,
                        )
                    )
                )
                await store.save_operation(
                    conn,
                    value.model_copy(
                        update={
                            "state": "target_ready",
                            "last_confirmed_step": "target_ready",
                            "reason": None,
                        }
                    ),
                )
        if denied is not None:
            await self._abort(
                operation_id, attempt, "cancelled", discard=True, evidence=denied
            )
            return
        operation, active = await self._current(operation_id)
        await self._enroll_active(operation, active)
        ns._spawn_deliver(attempt.session_id, message_id, text)

    async def _enroll_active(
        self, operation: TaskOperation, attempt: TaskAttempt
    ) -> None:
        """Recover the post-activation step; the brief's ledger intent already exists.

        Keep the operation pending until enrollment succeeds. A crash after issuance
        retries the idempotent enrollment, without recording or replaying another brief.
        """
        try:
            async with db.connection() as conn:
                async with (
                    push_lifecycle.locked(conn, attempt.session_id),
                    lifecycle.locked(conn, attempt.session_id),
                ):
                    await lifecycle.check(conn, attempt.session_id, "submit")
                    if await conn.fetchval(
                        """SELECT EXISTS(SELECT 1 FROM task_operations WHERE attempt_id=$1
                           AND kind='cancel' AND state NOT IN ('completed','blocked'))""",
                        attempt.id,
                    ):
                        return
                    await push_lifecycle.enroll(conn, attempt.session_id)
                    async with conn.transaction():
                        await self._complete(operation.id, conn)
        except lifecycle.LifecycleDenied:
            # Draining/cancelled attempts must not recover publication authority.
            await self._complete(operation.id)
        except (
            Exception
        ) as exc:  # noqa: BLE001 - retry the durable post-activation intent
            await self._unconfirmed(
                operation, f"post-activation enrollment unconfirmed: {exc}"
            )

    async def _authority_denial(
        self, conn, task: Task, attempt: TaskAttempt
    ) -> str | None:
        if task.current_attempt_id != attempt.id or task.status in lifecycle.FINAL:
            return "attempt-not-current"
        if await conn.fetchval(
            """SELECT EXISTS(SELECT 1 FROM task_operations
               WHERE task_id=$1 AND kind='cancel' AND state NOT IN ('completed','blocked'))""",
            task.id,
        ):
            return "cancellation-requested"
        if not await conn.fetchval(
            """SELECT b.token_hash IS NOT NULL AND b.kagent_deleted_at IS NULL
                      AND s.archived_at IS NULL
               FROM native_bindings b JOIN sessions s ON s.id=b.session_id
               WHERE b.session_id=$1""",
            attempt.session_id,
        ):
            return "binding-authority-lost"
        if task.mode == "code":
            claim = await conn.fetchrow(
                "SELECT held,generation FROM workspace_writer_claims WHERE attempt_id=$1",
                attempt.id,
            )
            if (
                claim is None
                or not claim["held"]
                or claim["generation"] != attempt.writer_generation
            ):
                return "stale-writer-generation"
        if attempt.role == "child":
            parent = await conn.fetchrow(
                """SELECT pa.state,pa.role,pa.id,pt.current_attempt_id,pt.owner_id,
                          pt.project_id,pt.parent_task_id,pc.held,pc.generation,
                          pa.writer_generation,pt.mode,pb.token_hash,pb.kagent_deleted_at,
                          ps.archived_at,pt.root_task_id,pa.depth
                   FROM tasks pt JOIN task_attempts pa ON pa.id=pt.current_attempt_id
                   JOIN native_bindings pb ON pb.session_id=pa.binding_id
                   JOIN sessions ps ON ps.id=pb.session_id
                   LEFT JOIN workspace_writer_claims pc ON pc.attempt_id=pa.id
                   WHERE pt.id=$1 FOR SHARE OF pa,pb,ps,pt""",
                task.parent_task_id,
            )
            if (
                parent is None
                or parent["state"] != "active"
                or parent["role"] != "supervisor"
                or parent["parent_task_id"] is not None
                or parent["depth"] != 1
                or parent["root_task_id"] != task.root_task_id
                or parent["project_id"] != task.project_id
                or not parent["token_hash"]
                or parent["kagent_deleted_at"] is not None
                or parent["archived_at"] is not None
                or parent["owner_id"] != task.owner_id
                or (
                    parent["mode"] == "code"
                    and not (
                        parent["held"]
                        and parent["generation"] == parent["writer_generation"]
                    )
                )
            ):
                return "parent-authority-lost"
        return None

    # ---- abort: drain, revoke, fence, settle ---------------------------------------------

    async def _abort(
        self,
        operation_id: str,
        attempt: TaskAttempt,
        final: str,
        *,
        discard: bool,
        evidence: str | None = None,
    ) -> bool:
        """End an attempt. True once settled; False leaves it draining for the next pass.

        The runtime is confirmed gone before the claim is released. Order: (1) stop new work and
        revoke the push grant under the lifecycle locks, with the durable state change; (2) revoke
        the bearer and Secret; (3) delete the Session, or, for a create whose reply was lost,
        retry the same frozen request to learn whether one exists; (4) settle.
        """
        sid = attempt.session_id
        async with db.connection() as conn:
            async with push_lifecycle.locked(conn, sid, revoke=True):
                async with lifecycle.locked(conn, sid), conn.transaction():
                    await store.admission_lock(conn)
                    if final == "failed" and await conn.fetchval(
                        """SELECT EXISTS(SELECT 1 FROM task_operations
                           WHERE attempt_id=$1 AND kind='cancel'
                             AND state NOT IN ('completed','blocked'))""",
                        attempt.id,
                    ):
                        # A create operation recovering the drain must respect an already
                        # committed cancellation, rather than turn it into startup failure.
                        final = "cancelled"
                    moved = await lifecycle.transition(
                        conn,
                        attempt.id,
                        "draining",
                        from_states=("creating", "active", "draining"),
                    )
        if moved is None:
            async with db.connection() as conn:
                moved = await lifecycle.load_attempt(conn, attempt.id)
            if moved is None or moved.state in lifecycle.FINAL:
                await self._complete(operation_id)
                return True
        await revoke(sid)
        fence = await self._fence_runtime(sid)
        if fence is None:
            operation, _ = await self._current(operation_id)
            if operation is not None:
                await self._unconfirmed(operation, "runtime fence unconfirmed")
            return False
        evidence = evidence or fence
        had_work = moved.brief_delivery_id is not None
        await ns.ledger.fail_open(sid, f"task attempt {final}")
        await db.update_session(
            sid,
            status=(
                SessionStatus.FAILED if final == "failed" else SessionStatus.CANCELLED
            ),
            error=evidence if final != "cancelled" else None,
        )
        async with db.connection() as conn:
            async with push_lifecycle.locked(conn, sid, revoke=True):
                async with lifecycle.locked(conn, sid), conn.transaction():
                    settled = await lifecycle.settle(
                        conn, attempt.id, final, evidence=fence
                    )
                    await self._complete(operation_id, conn)
        if settled is not None and discard and not had_work:
            # Nothing ran: the workspace rows have no value. The attempt row stays as the audit.
            await workspaces._delete_rows(sid, evidence=fence)
        return True

    async def _fence_runtime(self, sid: str) -> str | None:
        """Confirm the attempt's runtime is gone; the evidence string, or None if unconfirmed."""
        async with ns._lock(sid):
            async with db.connection() as conn:
                row = await conn.fetchrow(
                    "SELECT id FROM task_attempts WHERE binding_id=$1", sid
                )
                attempt = await lifecycle.load_attempt(conn, row["id"]) if row else None
            binding = await ns.get_binding(sid)
            if binding is None:
                return None  # Missing durable identity does not prove runtime absence.
            client = ns.get_client()
            kagent_id = binding["kagent_session_id"]
            if (
                kagent_id is None
                and attempt
                and lifecycle.CREATE_REJECTED in attempt.evidence_refs
            ):
                return lifecycle.CREATE_REJECTED
            try:
                if kagent_id is None:
                    reference = reference_from_data(binding["credential_ref"])
                    if reference is None:
                        return None
                    try:
                        # The create may have succeeded with its reply lost. The identity is
                        # revoked, so retry only the frozen request (same id, same checkout,
                        # same credential reference); never add anything to it.
                        found = await ns._create_session_with_credentials(
                            binding,
                            (reference,),
                            require_workspace=bool(await ns.ledger.get_workspace(sid)),
                        )
                    except SessionError as exc:
                        if ns._create_hit_deleted(exc):
                            return "kagent-deleted:create-request"
                        raise
                    kagent_id = found.id
                deleted = await client.delete_session(kagent_id)
                if (
                    deleted.id != kagent_id
                    or deleted.state != RuntimeState.DELETED
                    or not deleted.settled
                ):
                    return None
            except SessionError as exc:
                # NOT_FOUND proves absence only for DeleteSession on a known runtime,
                # not for a create retry whose template or permissions may have changed.
                if exc.grpc_status != 5 or kagent_id is None:
                    ns._log_step_failure("task_fence", sid, exc)
                    return None
            except KagentError as exc:
                ns._log_step_failure("task_fence", sid, exc)
                return None
            await ns.ledger.mark_kagent_deleted(sid)
            return f"kagent-deleted:{kagent_id}"


def install() -> Provisioning:
    """Install task ports at the existing startup seam."""
    from mainloop.tasks.projection import Projection

    ports.provisioning = Provisioning()
    ports.projection = Projection()
    ports._projection_cursor = ""
    return ports.provisioning
