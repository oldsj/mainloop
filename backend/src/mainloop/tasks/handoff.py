"""Durable committed handoff coordinator. External adapters are explicitly qualified.

No default runtime fence or target readiness is inferred from kagent suspension.
Each effect reconciles its stable original action ID; no uncertain effect is replayed.
"""

from datetime import UTC, datetime
from typing import Protocol

from mainloop.db import tasks as store
from mainloop.providers import qualify_task_profile, registry
from mainloop.push_gate import lifecycle as push_lifecycle
from mainloop.tasks import lifecycle, provisioning
from mainloop.tasks.checkpoint import persist_manifest, verify_checkpoint, verify_scope

from models.task import TaskEligibility, TaskOperation
from models.task_handoff import (
    AdapterCapabilities,
    CheckpointEvidence,
    ContinuationManifest,
    SourceFenceEvidence,
    SuccessorResult,
)

STEPS = (
    "requested",
    "draining",
    "checkpoint_required",
    "checkpoint_verified",
    "source_fencing",
    "source_fenced",
    "target_creating",
    "target_ready",
    "completed",
)


class HandoffRuntime(Protocol):
    """Implementations receive stable IDs, reconcile lost replies and never blind retry.

    Source fence re-observes the remote checkpoint under exclusive source locks.
    Target preparation must create read-only, verify checkout/environment, then issue
    fresh scoped credentials only through admit_target after coordinator validation.
    Readiness never executes the first brief itself.
    """

    capabilities: AdapterCapabilities

    async def drain_children(
        self, conn, task, source, operation, action_id: str
    ) -> bool: ...
    async def fence(
        self, conn, task, source, operation, checkpoint, action_id: str
    ) -> SourceFenceEvidence: ...
    async def prepare_target(
        self, conn, task, target, operation, checkpoint, action_id: str
    ) -> SuccessorResult: ...
    async def admit_target(
        self, conn, task, target, operation, checkpoint, action_id: str
    ) -> SuccessorResult: ...

    async def finish_target(
        self, conn, task, target, operation, action_id: str
    ) -> bool: ...


def require_fence(
    evidence: SourceFenceEvidence,
    *,
    operation_id: str,
    attempt_id: str,
    binding_id: str,
    generation: int,
    live: bool,
) -> None:
    if (
        evidence.operation_id,
        evidence.attempt_id,
        evidence.binding_id,
        evidence.writer_generation,
    ) != (operation_id, attempt_id, binding_id, generation):
        raise store.TaskError(409, "fence_identity_mismatch")
    if evidence.qualification != ("qualified_live" if live else "offline_fake"):
        raise store.TaskError(409, "fence_unqualified")
    if not all(
        (
            evidence.native_dispatch_settled,
            evidence.git_dispatch_settled,
            evidence.merge_dispatch_settled,
            evidence.credentials_revoked,
            evidence.runtime_quiescent,
            evidence.preview_closed,
            evidence.children_drained,
            evidence.pending_hitl_resolved,
        )
    ):
        raise store.TaskError(409, "source_not_quiescent")


def advance(operation: TaskOperation, step: str) -> TaskOperation:
    current = operation.last_confirmed_step
    if step == current:
        return operation
    if (
        current not in STEPS
        or step not in STEPS
        or STEPS.index(step) != STEPS.index(current) + 1
    ):
        raise store.TaskError(409, "invalid_handoff_transition")
    return operation.model_copy(
        update={
            "state": step,
            "last_confirmed_step": step,
            "reason": None,
            "updated_at": datetime.now(UTC),
        }
    )


class Handoff:
    def __init__(self, runtime=None, checkpoint_reader=None, *, live=True):
        self.runtime = runtime
        self.checkpoint_reader = checkpoint_reader
        self.live = live

    def qualified(self):
        if self.runtime is None or self.checkpoint_reader is None:
            return False
        caps = self.runtime.capabilities
        return caps.qualification == (
            "qualified_live" if self.live else "offline_fake"
        ) and all(
            (
                caps.source_fence,
                caps.preview_closure,
                caps.exact_checkout,
                caps.reconcile_original_create,
            )
        )

    async def eligibility(self, conn, principal, task):
        """Read-only availability; never observes or dispatches a native runtime."""
        allowed = (
            self.qualified()
            and task.mode == "code"
            and principal.role in ("owner", "main", "supervisor")
        )
        if allowed:
            try:
                await store.get_task(conn, task.id, principal, manage=True)
            except store.TaskError:
                allowed = False
        if allowed:
            attempts = await store.attempts(conn, task.id)
            source = next(
                (a for a in attempts if a.id == task.current_attempt_id), None
            )
            if source is None and attempts:
                source = max(attempts, key=lambda a: a.number)
            allowed = source is not None and (
                source.state in ("active", "creating")
                or (
                    source.state == "failed"
                    and lifecycle.CREATE_REJECTED in source.evidence_refs
                )
            )
            allowed = allowed and not await store.handoff_pending(conn, task.id, "")
        return {
            name: TaskEligibility(
                available=allowed, reason=None if allowed else "handoff_unavailable"
            )
            for name in ("retry", "reassign")
        }

    async def start(self, conn, principal, task, request, operation):
        store.require_transaction(conn)
        task = await store.get_task(conn, task.id, principal, manage=True, lock=True)
        if (task.version, task.current_attempt_id) != (
            request.expected_version,
            request.expected_attempt_id,
        ):
            raise store.TaskError(409, "stale_task_attempt")
        if task.status in ("completed", "cancelled"):
            raise store.TaskError(409, "task_terminal")
        if operation.kind not in ("retry", "reassign") or task.mode != "code":
            raise store.TaskError(422, "code_handoff_required")
        if await store.handoff_pending(conn, task.id, operation.id):
            raise store.TaskError(409, "handoff_in_progress")
        if not self.qualified():
            blocked = operation.model_copy(
                update={
                    "source_attempt_id": task.current_attempt_id,
                    "state": "blocked",
                    "reason": "handoff_unavailable",
                }
            )
            await store.handoff_save(conn, operation, blocked)
            return await store.handoff_operation(conn, operation.id)
        attempts = await store.attempts(conn, task.id)
        source = next((a for a in attempts if a.id == task.current_attempt_id), None)
        if source is None and operation.kind == "retry" and attempts:
            source = max(attempts, key=lambda a: a.number)
            if (
                source.state != "failed"
                or lifecycle.CREATE_REJECTED not in source.evidence_refs
            ):
                raise store.TaskError(409, "failed_source_checkpoint_required")
        if source is None:
            raise store.TaskError(409, "handoff_source_missing")
        if source.state not in ("active", "creating", "failed"):
            raise store.TaskError(409, "source_not_retryable")
        no_start = (
            source.state == "failed"
            and lifecycle.CREATE_REJECTED in source.evidence_refs
        )
        if source.state == "failed" and not no_start:
            raise store.TaskError(409, "failed_source_checkpoint_required")
        native_id = await conn.fetchval(
            "SELECT kagent_session_id FROM native_bindings WHERE session_id=$1",
            source.binding_id,
        )
        if no_start and native_id is not None:
            raise store.TaskError(409, "no_start_identity_conflict")
        profile_id = (
            request.target_profile_id
            if operation.kind == "reassign"
            else source.profile_id
        )
        if (
            principal.role == "supervisor"
            and task.provider_constraint
            and profile_id != task.provider_constraint
        ):
            raise store.TaskError(403, "inherited_provider_constraint")
        profile = qualify_task_profile(
            registry().resolve(profile_id, source.role, selecting=True),
            source.role,
            task.mode,
            allow_fixture=not self.live,
        )
        payload = {
            **operation.request_payload,
            "_s3": {
                "profile": profile.model_dump(mode="json"),
                "expected_version": task.version,
                "source_runtime": native_id
                or (f"no-start:{source.id}" if no_start else None),
                "no_start": no_start,
            },
        }
        state = "requested" if self.qualified() else "blocked"
        value = operation.model_copy(
            update={
                "source_attempt_id": source.id,
                "request_payload": payload,
                "state": state,
                "reason": None if state == "requested" else "handoff_unavailable",
            }
        )
        await store.handoff_save(conn, operation, value)
        return await store.handoff_operation(conn, operation.id)

    async def reconcile(self, database, operation):
        if operation.kind not in ("retry", "reassign"):
            return
        async with database.connection() as conn:
            key = f"task-operation:{operation.id}"
            if not await conn.fetchval(
                "SELECT pg_try_advisory_lock(hashtextextended($1,0))", key
            ):
                return
            try:
                current = await store.handoff_operation(conn, operation.id)
                if current is None or current.state in ("completed", "blocked"):
                    return
                if not self.qualified():
                    await self._record(
                        conn,
                        current,
                        current.model_copy(
                            update={"state": "blocked", "reason": "handoff_unavailable"}
                        ),
                    )
                    return
                source = await lifecycle.load_attempt(conn, current.source_attempt_id)
                target = (
                    await lifecycle.load_attempt(conn, current.target_attempt_id)
                    if current.target_attempt_id
                    else None
                )
                sid = target.session_id if target else source.session_id
                async with push_lifecycle.locked(conn, sid), lifecycle.locked(
                    conn, sid
                ):
                    # Re-read after locks; rows are never held while acquiring runtime locks.
                    current = await store.handoff_operation(conn, operation.id)
                    try:
                        await self._step(conn, current)
                    except Exception as exc:
                        # Preserve the committed dispatch identity on every lost reply.
                        latest = await store.handoff_operation(conn, operation.id)
                        if latest is not None and latest.state not in (
                            "completed",
                            "blocked",
                        ):
                            await self._record(
                                conn,
                                latest,
                                latest.model_copy(
                                    update={
                                        "state": (
                                            "blocked"
                                            if isinstance(exc, store.TaskError)
                                            else "uncertain"
                                        ),
                                        "reason": "reconciliation",
                                        "request_payload": {
                                            **latest.request_payload,
                                            "_s3": {
                                                **latest.request_payload.get("_s3", {}),
                                                "failure_code": (
                                                    exc.code
                                                    if isinstance(exc, store.TaskError)
                                                    else "external_reply_uncertain"
                                                ),
                                            },
                                        },
                                    }
                                ),
                            )
                        if not isinstance(exc, store.TaskError):
                            raise
            finally:
                await conn.fetchval(
                    "SELECT pg_advisory_unlock(hashtextextended($1,0))", key
                )

    async def _record(self, conn, previous, value):
        async with conn.transaction():
            await store.handoff_save(conn, previous, value)

    async def _step(self, conn, operation):
        task = await lifecycle.load_task(conn, operation.task_id)
        if task.status in ("completed", "failed", "cancelled"):
            raise store.TaskError(409, "task_terminal")
        source = await lifecycle.load_attempt(conn, operation.source_attempt_id)
        step = operation.last_confirmed_step
        metadata = operation.request_payload["_s3"]
        expected_current = (
            operation.target_attempt_id if operation.target_attempt_id else source.id
        )
        if step != "source_fenced" and not (
            metadata["no_start"] and task.current_attempt_id is None
        ):
            if task.current_attempt_id != expected_current:
                raise store.TaskError(409, "stale_task_attempt")
        if await store.handoff_pending(conn, task.id, operation.id):
            raise store.TaskError(409, "competing_task_operation")
        if step == "requested":
            await self._record(conn, operation, advance(operation, "draining"))
            return
        if step == "draining":
            if not await store.handoff_children_drained(conn, task.id):
                if not await self.runtime.drain_children(
                    conn, task, source, operation, f"{operation.id}:children"
                ):
                    return
                if not await store.handoff_children_drained(conn, task.id):
                    return
            # Preserve sole-source authority until committed checkpoint exists.
            await self._record(
                conn, operation, advance(operation, "checkpoint_required")
            )
            return
        project = await store.project(conn, task.project_id, task.owner_id)
        repository = project["full_name"].lower()
        if step == "checkpoint_required":
            checkpoint = await self.checkpoint_reader.read(
                conn, task, source, operation, f"{operation.id}:checkpoint"
            )
            verify_scope(
                checkpoint,
                operation=operation,
                attempt=source,
                runtime_identity=metadata["source_runtime"],
                now=datetime.now(UTC),
                live=self.live,
            )
            verify_checkpoint(
                checkpoint, repository=repository, branch=task.checkout.branch
            )
            if metadata["no_start"]:
                if checkpoint.no_start_initial_ref != source.initial_ref:
                    raise store.TaskError(409, "no_start_ref_mismatch")
            elif checkpoint.no_start_initial_ref is not None:
                raise store.TaskError(409, "no_start_evidence_for_live_source")
            async with conn.transaction():
                await store.admission_lock(conn)
                await lifecycle.load_task(conn, task.id, lock=True)
                artifact = await store.add_artifact(
                    conn, operation.id, "checkpoint", checkpoint.model_dump(mode="json")
                )
                value = advance(operation, "checkpoint_verified").model_copy(
                    update={"checkpoint_ref": artifact}
                )
                await store.handoff_save(conn, operation, value)
            return
        checkpoint = CheckpointEvidence.model_validate(
            store.decode(
                await conn.fetchval(
                    "SELECT content FROM task_artifacts WHERE id=$1",
                    operation.checkpoint_ref,
                )
            )
        )
        if step == "checkpoint_verified":
            async with conn.transaction():
                if not await store.handoff_children_drained(conn, task.id):
                    raise store.TaskError(409, "children_not_drained")
                if task.version != metadata["expected_version"]:
                    raise store.TaskError(409, "stale_task_attempt")
                await lifecycle.drain_handoff(conn, source)
                locked_task = await lifecycle.load_task(conn, task.id, lock=True)
                if locked_task.version != metadata["expected_version"]:
                    raise store.TaskError(409, "stale_task_attempt")
                await store.handoff_save(
                    conn, operation, advance(operation, "source_fencing")
                )
            return
        if step == "source_fencing":
            evidence = await self.runtime.fence(
                conn, task, source, operation, checkpoint, f"{operation.id}:fence"
            )
            verify_scope(
                evidence,
                operation=operation,
                attempt=source,
                runtime_identity=metadata["source_runtime"],
                now=datetime.now(UTC),
                live=self.live,
            )
            require_fence(
                evidence,
                operation_id=operation.id,
                attempt_id=source.id,
                binding_id=source.binding_id,
                generation=source.writer_generation,
                live=self.live,
            )
            refreshed = await self.checkpoint_reader.read(
                conn, task, source, operation, f"{operation.id}:checkpoint"
            )
            verify_scope(
                refreshed,
                operation=operation,
                attempt=source,
                runtime_identity=metadata["source_runtime"],
                now=datetime.now(UTC),
                live=self.live,
            )
            if (
                verify_checkpoint(
                    refreshed, repository=repository, branch=task.checkout.branch
                )
                != checkpoint.remote_sha
            ):
                raise store.TaskError(409, "checkpoint_changed_after_fence")
            async with conn.transaction():
                await store.admission_lock(conn)
                if not await store.handoff_children_drained(conn, task.id):
                    raise store.TaskError(409, "children_not_drained")
                await lifecycle.supersede_handoff(conn, source, evidence.evidence_ref)
                await store.handoff_save(
                    conn, operation, advance(operation, "source_fenced")
                )
            return
        if step == "source_fenced":
            from models.provider import ProviderProfile

            profile = ProviderProfile.model_validate(metadata["profile"])
            async with conn.transaction():
                await store.admission_lock(conn)
                task = await lifecycle.load_task(conn, task.id, lock=True)
                store.handoff_require_admission(task, operation)
                source = await lifecycle.load_attempt(conn, source.id, lock=True)
                manifest = ContinuationManifest(
                    task_id=task.id,
                    operation_id=operation.id,
                    predecessor_id=source.id,
                    target_profile_id=profile.id,
                    repository=repository,
                    branch=task.checkout.branch,
                    checkpoint_sha=checkpoint.remote_sha,
                    environment=task.accepted_environment,
                    caller_instructions=task.brief,
                    report_refs=tuple(
                        f"task-report:{row['id']}"
                        for row in await conn.fetch(
                            "SELECT id FROM task_reports WHERE task_id=$1 AND attempt_id=$2 ORDER BY created_at,id LIMIT 64",
                            task.id,
                            source.id,
                        )
                    ),
                    unverified_note=checkpoint.unverified_note,
                )
                if checkpoint.unverified_note is not None:
                    await store.add_artifact(
                        conn,
                        operation.id,
                        "unverified_provider_summary",
                        {
                            "label": "unverified provider note",
                            "note": checkpoint.unverified_note,
                        },
                    )
                manifest_ref = await persist_manifest(conn, manifest)
                value = advance(operation, "target_creating").model_copy(
                    update={"manifest_ref": manifest_ref}
                )
                task, target = await provisioning.enroll_successor(
                    conn, task, source, profile, value, checkpoint
                )
                value = value.model_copy(
                    update={"target_attempt_id": target.id, "attempt_id": target.id}
                )
                await store.handoff_save(conn, operation, value)
            return
        target = await lifecycle.load_attempt(conn, operation.target_attempt_id)
        if step == "target_creating":
            result = await self.runtime.prepare_target(
                conn, task, target, operation, checkpoint, f"{operation.id}:create"
            )
            if result.outcome != "ready":
                await self._record(
                    conn,
                    operation,
                    operation.model_copy(
                        update={
                            "state": (
                                "blocked"
                                if result.outcome in ("definite_failure", "absent")
                                else "uncertain"
                            ),
                            "reason": "reconciliation",
                        }
                    ),
                )
                return
            identity = await conn.fetchval(
                "SELECT kagent_session_id FROM native_bindings WHERE session_id=$1",
                target.binding_id,
            )
            verify_scope(
                result,
                operation=operation,
                attempt=target,
                runtime_identity=identity,
                now=datetime.now(UTC),
                live=self.live,
            )
            if (
                not result.checkout_verified
                or not result.environment_verified
                or (
                    result.checkpoint_sha,
                    result.repository,
                    result.branch,
                    result.environment,
                )
                != (
                    checkpoint.remote_sha,
                    repository,
                    task.checkout.branch,
                    task.accepted_environment,
                )
            ):
                raise store.TaskError(409, "target_checkout_unverified")
            await self._record(conn, operation, advance(operation, "target_ready"))
            return
        if step == "target_ready":
            # Reconfirm read-only checkout before S1 active admission. Push grant
            # issuance requires active, but no first brief is recorded until it succeeds.
            ready = await self.runtime.prepare_target(
                conn, task, target, operation, checkpoint, f"{operation.id}:create"
            )
            identity = await conn.fetchval(
                "SELECT kagent_session_id FROM native_bindings WHERE session_id=$1",
                target.binding_id,
            )
            verify_scope(
                ready,
                operation=operation,
                attempt=target,
                runtime_identity=identity,
                now=datetime.now(UTC),
                live=self.live,
            )
            if (
                ready.outcome != "ready"
                or not ready.checkout_verified
                or not ready.environment_verified
                or (
                    ready.checkpoint_sha,
                    ready.repository,
                    ready.branch,
                    ready.environment,
                )
                != (
                    checkpoint.remote_sha,
                    repository,
                    task.checkout.branch,
                    task.accepted_environment,
                )
            ):
                raise store.TaskError(409, "target_checkout_unverified")
            async with conn.transaction():
                await store.admission_lock(conn)
                task = await lifecycle.load_task(conn, task.id, lock=True)
                await provisioning.admit_successor(conn, task, target)
            target = await lifecycle.load_attempt(conn, target.id)
            admitted = await self.runtime.admit_target(
                conn, task, target, operation, checkpoint, f"{operation.id}:grant"
            )
            identity = await conn.fetchval(
                "SELECT kagent_session_id FROM native_bindings WHERE session_id=$1",
                target.binding_id,
            )
            verify_scope(
                admitted,
                operation=operation,
                attempt=target,
                runtime_identity=identity,
                now=datetime.now(UTC),
                live=self.live,
            )
            if (
                admitted.outcome != "ready"
                or not admitted.grant_confirmed
                or admitted.grant_ref is None
                or not admitted.checkout_verified
                or not admitted.environment_verified
                or (
                    admitted.checkpoint_sha,
                    admitted.repository,
                    admitted.branch,
                    admitted.environment,
                )
                != (
                    checkpoint.remote_sha,
                    repository,
                    task.checkout.branch,
                    task.accepted_environment,
                )
            ):
                raise store.TaskError(409, "target_admission_unverified")
            async with conn.transaction():
                await store.admission_lock(conn)
                task = await lifecycle.load_task(conn, task.id, lock=True)
                await provisioning.activate_successor(conn, task, target, operation)
            target = await lifecycle.load_attempt(conn, target.id)
            if await self.runtime.finish_target(
                conn, task, target, operation, f"{operation.id}:brief"
            ):
                await self._record(conn, operation, advance(operation, "completed"))
