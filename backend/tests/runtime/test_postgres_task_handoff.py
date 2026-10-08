"""Prepared PostgreSQL artifact durability regression; heavy lane must be granted."""

from mainloop.db import tasks as store
from mainloop.tasks.principal import TaskPrincipal
from tests.runtime import test_postgres_task_provisioning as s1
from tests.runtime.test_postgres_ledger import PostgresTestCase


class HandoffArtifactPostgresTests(PostgresTestCase):
    async def test_restart_replay_and_conflicting_checkpoint(self):
        principal = TaskPrincipal(self.user)
        async with self.pool.acquire() as conn, conn.transaction():
            operation, fresh = await store.begin_operation(
                conn,
                principal,
                "handoff-artifact-test",
                "retry",
                {"expected_version": 1},
            )
            self.assertTrue(fresh)
            artifact = await store.add_artifact(
                conn, operation.id, "checkpoint", {"sha": "a" * 40}
            )
        async with self.pool.acquire() as conn, conn.transaction():
            replay, fresh = await store.begin_operation(
                conn,
                principal,
                "handoff-artifact-test",
                "retry",
                {"expected_version": 1},
            )
            self.assertFalse(fresh)
            self.assertEqual(replay.id, operation.id)
            self.assertEqual(
                await store.add_artifact(
                    conn, replay.id, "checkpoint", {"sha": "a" * 40}
                ),
                artifact,
            )
            restored = await store.get_artifact(conn, artifact, principal)
            self.assertEqual(restored["payload"]["sha"], "a" * 40)
        async with self.pool.acquire() as conn, conn.transaction():
            with self.assertRaises(store.TaskError) as raised:
                await store.add_artifact(
                    conn, operation.id, "checkpoint", {"sha": "b" * 40}
                )
            self.assertEqual(raised.exception.code, "artifact_payload_conflict")

    async def test_durable_operation_cas_rejects_old_snapshot(self):
        principal = TaskPrincipal(self.user)
        async with self.pool.acquire() as conn, conn.transaction():
            operation, _ = await store.begin_operation(
                conn, principal, "handoff-cas-test", "retry", {}
            )
            draining = operation.model_copy(
                update={"state": "draining", "last_confirmed_step": "draining"}
            )
            await store.handoff_save(conn, operation, draining)
        async with self.pool.acquire() as conn, conn.transaction():
            with self.assertRaises(store.TaskError) as raised:
                await store.handoff_save(
                    conn, operation, operation.model_copy(update={"state": "completed"})
                )
            self.assertEqual(raised.exception.code, "stale_handoff_operation")
            self.assertEqual(
                (await store.handoff_operation(conn, operation.id)).last_confirmed_step,
                "draining",
            )

    async def test_concurrent_operation_cas_has_one_winner(self):
        import asyncio

        principal = TaskPrincipal(self.user)
        async with self.pool.acquire() as conn, conn.transaction():
            operation, _ = await store.begin_operation(
                conn, principal, "handoff-concurrent-test", "retry", {}
            )

        async def update(state):
            async with self.pool.acquire() as conn, conn.transaction():
                try:
                    await store.handoff_save(
                        conn, operation, operation.model_copy(update={"state": state})
                    )
                except store.TaskError as exc:
                    return exc.code
                return "saved"

        results = await asyncio.gather(update("draining"), update("blocked"))
        self.assertCountEqual(results, ["saved", "stale_handoff_operation"])


class RetentionArtifactPostgresTests(PostgresTestCase):
    async def test_fresh_schema_full_typed_receipts_replay_and_immutable_uniqueness(
        self,
    ):
        import uuid
        from datetime import UTC, datetime

        import asyncpg
        from pydantic import ValidationError

        from models.task import TaskArtifact
        from models.task_handoff import RetentionReceipt

        principal = TaskPrincipal(self.user)
        for character in ("\x00", "😀", '"'):
            async with self.pool.acquire() as conn, conn.transaction():
                operation, _ = await store.begin_operation(
                    conn, principal, uuid.uuid4().hex, "retry", {}
                )
                receipt = RetentionReceipt(
                    runtime_identity=character * 100,
                    attempt_id=character * 100,
                    session_id=character * 100,
                    action_id=character * 100,
                    qualification="offline_fake",
                    confirmed=True,
                    provenance=character * 2048,
                    observed_at=datetime.now(UTC),
                )
                payload = receipt.model_dump(mode="json")
                artifact_id = await store.add_artifact(
                    conn, operation.id, "retention_receipt", payload
                )
                self.assertEqual(
                    await store.add_artifact(
                        conn, operation.id, "retention_receipt", payload
                    ),
                    artifact_id,
                )
                artifact = TaskArtifact.model_validate(
                    await store.get_artifact(conn, artifact_id, principal)
                )
                self.assertEqual(artifact.kind, "retention_receipt")
                self.assertEqual(
                    RetentionReceipt.model_validate(artifact.payload), receipt
                )
                self.assertLessEqual(
                    len(("retention-receipt:" + artifact_id).encode()), 2048
                )
                with self.assertRaises(store.TaskError) as conflict:
                    await store.add_artifact(
                        conn,
                        operation.id,
                        "retention_receipt",
                        {**payload, "confirmed": False},
                    )
                self.assertEqual(conflict.exception.code, "artifact_payload_conflict")
            async with self.pool.acquire() as conn:
                for statement in (
                    "UPDATE task_artifacts SET content=content WHERE id=$1",
                    "DELETE FROM task_artifacts WHERE id=$1",
                ):
                    with self.assertRaises(asyncpg.RaiseError):
                        async with conn.transaction():
                            await conn.execute(statement, artifact_id)
                with self.assertRaises(asyncpg.UniqueViolationError):
                    async with conn.transaction():
                        await conn.execute(
                            "INSERT INTO task_artifacts(id,operation_id,kind,content,sha256) SELECT $1,operation_id,kind,content,sha256 FROM task_artifacts WHERE id=$2",
                            uuid.uuid4().hex,
                            artifact_id,
                        )
                with self.assertRaises(asyncpg.CheckViolationError):
                    async with conn.transaction():
                        await store.add_artifact(
                            conn, operation.id, "retention_receipt:alias", payload
                        )
                with self.assertRaises(ValidationError):
                    TaskArtifact.model_validate(
                        {**artifact.model_dump(), "kind": "retention_receipt:alias"}
                    )
                restored = TaskArtifact.model_validate(
                    await store.get_artifact(conn, artifact_id, principal)
                )
                self.assertEqual(restored, artifact)


class RetentionUpgradePostgresTests(PostgresTestCase):
    async def test_existing_three_kind_schema_upgrade_is_idempotent_and_preserves_audit(
        self,
    ):
        import uuid

        import asyncpg
        from mainloop.db.task_schema import TASK_MIGRATION_SQL

        from models.task import TaskArtifact

        principal = TaskPrincipal(self.user)
        async with self.pool.acquire() as conn, conn.transaction():
            operation, _ = await store.begin_operation(
                conn, principal, uuid.uuid4().hex, "retry", {}
            )
            old_id = await store.add_artifact(
                conn, operation.id, "checkpoint", {"sha": "a" * 40}
            )
            old = await store.get_artifact(conn, old_id, principal)
            await conn.execute(
                """
                ALTER TABLE task_artifacts ADD CONSTRAINT fixture_legacy_kind_check
                CHECK(kind IN ('checkpoint','handoff_manifest','unverified_provider_summary'));
                ALTER TABLE task_artifacts DROP CONSTRAINT task_artifacts_kind_check;
                ALTER TABLE task_artifacts RENAME CONSTRAINT fixture_legacy_kind_check TO task_artifacts_kind_check;
            """
            )
        async with self.pool.acquire() as conn:
            with self.assertRaises(asyncpg.CheckViolationError):
                async with conn.transaction():
                    await store.add_artifact(
                        conn, operation.id, "retention_receipt", {}
                    )
            await conn.execute(TASK_MIGRATION_SQL)
            first = await conn.fetchrow(
                "SELECT oid,convalidated FROM pg_constraint WHERE conrelid='task_artifacts'::regclass AND conname='task_artifacts_kind_check'"
            )
            self.assertTrue(first["convalidated"])
            await conn.execute(TASK_MIGRATION_SQL)
            second = await conn.fetchrow(
                "SELECT oid,convalidated FROM pg_constraint WHERE conrelid='task_artifacts'::regclass AND conname='task_artifacts_kind_check'"
            )
            self.assertEqual(first, second)
            self.assertEqual(await store.get_artifact(conn, old_id, principal), old)
            async with conn.transaction():
                artifact_id = await store.add_artifact(
                    conn,
                    operation.id,
                    "retention_receipt",
                    {"fixture": "upgraded storage"},
                )
                artifact = TaskArtifact.model_validate(
                    await store.get_artifact(conn, artifact_id, principal)
                )
                self.assertEqual(artifact.kind, "retention_receipt")
            with self.assertRaises(asyncpg.RaiseError):
                async with conn.transaction():
                    await conn.execute("DELETE FROM task_artifacts WHERE id=$1", old_id)
            self.assertEqual(
                await conn.fetchval(
                    "SELECT count(*) FROM pg_trigger WHERE tgrelid='task_artifacts'::regclass AND tgname='immutable_task_artifact' AND NOT tgisinternal"
                ),
                1,
            )


class CoordinatorPostgresTests(s1.TaskProvisioningPostgresTests):
    def coordinator(self, *, crash=None, outcome="ready", hitl_pending=False):
        import uuid
        from datetime import UTC, datetime
        from types import SimpleNamespace

        from mainloop.push_gate import lifecycle as push_lifecycle
        from mainloop.tasks import lifecycle
        from mainloop.tasks.handoff import Handoff

        from models.task_handoff import (
            AdapterCapabilities,
            CheckpointEvidence,
            SourceFenceEvidence,
            SuccessorResult,
        )

        case = self
        effects = {}
        used = set()
        calls = []

        async def effect(step, action, factory):
            calls.append((step, action))
            if action not in effects:
                effects[action] = await factory()
            if crash == step and step not in used:
                used.add(step)
                raise RuntimeError("lost fixture reply after effect")
            return effects[action]

        async def scope(conn, attempt, operation):
            native_id = await conn.fetchval(
                "SELECT kagent_session_id FROM native_bindings WHERE session_id=$1",
                attempt.binding_id,
            )
            return dict(
                operation_id=operation.id,
                attempt_id=attempt.id,
                session_id=attempt.session_id,
                binding_id=attempt.binding_id,
                runtime_identity=native_id or f"no-start:{attempt.id}",
                writer_generation=attempt.writer_generation,
                qualification="offline_fake",
                provenance="fixture:pg-native",
                observed_at=datetime.now(UTC),
            )

        class Reader:
            async def read(self, conn, task, attempt, operation, action):
                async def observe():
                    return CheckpointEvidence(
                        **await scope(conn, attempt, operation),
                        repository="example/app",
                        branch=task.checkout.branch,
                        remote_sha="a" * 40,
                        committed_checkpoint=True,
                        no_start_initial_ref=(
                            attempt.initial_ref
                            if operation.request_payload["_s3"]["no_start"]
                            else None
                        ),
                        git_dispatch="settled",
                        merge_dispatch="settled",
                        evidence_ref="fixture:remote-checkpoint",
                    )

                value = await effect("checkpoint", action, observe)
                return value.model_copy(update={"observed_at": datetime.now(UTC)})

        class Runtime:
            capabilities = AdapterCapabilities(
                qualification="offline_fake",
                provenance="fixture:pg-adapter",
                source_fence=True,
                preview_closure=True,
                exact_checkout=True,
                reconcile_original_create=True,
            )

            async def drain_children(self, conn, task, source, operation, action):
                async def drain():
                    # Qualified fake has no external in-flight child actions. Use S1
                    # lifecycle/credentials on this connection, never a second owner.
                    rows = await conn.fetch(
                        "SELECT a.id FROM task_attempts a JOIN tasks t ON t.id=a.task_id WHERE t.parent_task_id=$1 AND a.state IN ('creating','active','draining')",
                        task.id,
                    )
                    from mainloop.push_gate import lifecycle as push_lifecycle

                    for row in rows:
                        child = await lifecycle.load_attempt(conn, row["id"])
                        async with push_lifecycle.locked(conn, child.session_id):
                            async with conn.transaction():
                                await lifecycle.drain_handoff(conn, child)
                                await lifecycle.settle(
                                    conn,
                                    child.id,
                                    "cancelled",
                                    evidence="fixture:child-quiescent",
                                )
                    return True

                return await effect("children", action, drain)

            async def fence(self, conn, task, source, operation, checkpoint, action):
                async def fence():
                    case.assertIsNone(
                        await conn.fetchval(
                            "SELECT token_hash FROM native_bindings WHERE session_id=$1",
                            source.binding_id,
                        )
                    )
                    await conn.execute(
                        "UPDATE native_deliveries SET state='cancelled' WHERE session_id=$1 AND state IN ('recorded','queued')",
                        source.session_id,
                    )
                    return SourceFenceEvidence(
                        **await scope(conn, source, operation),
                        native_dispatch_settled=True,
                        git_dispatch_settled=True,
                        merge_dispatch_settled=True,
                        credentials_revoked=True,
                        runtime_quiescent=True,
                        preview_closed=True,
                        children_drained=True,
                        pending_hitl_resolved=not hitl_pending,
                        evidence_ref="fixture:pg-quiescent",
                    )

                value = await effect("fence", action, fence)
                return value.model_copy(update={"observed_at": datetime.now(UTC)})

            async def prepare_target(
                self, conn, task, target, operation, checkpoint, action
            ):
                async def prepare():
                    native_id = "fixture-native-" + uuid.uuid4().hex
                    # This models the native service's original-create reconciliation;
                    # it does not call a live provider or publish credentials.
                    await conn.execute(
                        "UPDATE native_bindings SET kagent_session_id=$2 WHERE session_id=$1",
                        target.binding_id,
                        native_id,
                    )
                    workspace = await conn.fetchrow(
                        "SELECT * FROM workspaces WHERE session_id=$1",
                        target.session_id,
                    )
                    case.assertEqual(
                        (workspace["ref"], workspace["branch"]),
                        (checkpoint.remote_sha, task.checkout.branch),
                    )
                    return SuccessorResult(
                        **await scope(conn, target, operation),
                        outcome=outcome,
                        checkpoint_sha=checkpoint.remote_sha,
                        repository=checkpoint.repository,
                        branch=checkpoint.branch,
                        environment=target.environment,
                        checkout_verified=True,
                        environment_verified=True,
                        evidence_ref="fixture:pg-ready",
                    )

                value = await effect("create", action, prepare)
                return value.model_copy(update={"observed_at": datetime.now(UTC)})

            async def admit_target(
                self, conn, task, target, operation, checkpoint, action
            ):
                async def grant():
                    case.assertEqual(
                        await conn.fetchval(
                            "SELECT state FROM task_attempts WHERE id=$1", target.id
                        ),
                        "active",
                    )
                    for action_name in ("create", "submit", "resume", "preview"):
                        with case.assertRaises(lifecycle.LifecycleDenied) as pending:
                            await lifecycle.check(conn, target.session_id, action_name)
                        case.assertEqual(
                            pending.exception.code, "handoff_admission_pending"
                        )
                    # Uses real push grant resolution and generation binding, no flag activation.
                    await push_lifecycle.enroll(conn, target.session_id)
                    value = await self.prepare_target(
                        conn,
                        task,
                        target,
                        operation,
                        checkpoint,
                        f"{operation.id}:create",
                    )
                    return value.model_copy(
                        update={
                            "grant_confirmed": True,
                            "grant_ref": f"fixture:push-grant:{target.session_id}",
                        }
                    )

                value = await effect("grant", action, grant)
                return value.model_copy(update={"observed_at": datetime.now(UTC)})

            async def finish_target(self, conn, task, target, operation, action):
                async def brief():
                    current = await lifecycle.load_attempt(conn, target.id)
                    row = await conn.fetchrow(
                        "SELECT * FROM native_deliveries WHERE message_id=$1",
                        current.brief_delivery_id,
                    )
                    case.assertEqual(row["session_id"], target.session_id)
                    case.assertEqual(
                        await conn.fetchval(
                            "SELECT count(*) FROM native_deliveries d JOIN messages m ON m.id=d.message_id WHERE d.session_id=$1 AND d.source='brief'",
                            target.session_id,
                        ),
                        1,
                    )
                    # Fake native receipt for the exact durable message identity.
                    await conn.execute(
                        "UPDATE native_deliveries SET state='completed',task_id=$2 WHERE message_id=$1",
                        current.brief_delivery_id,
                        "fixture-receipt-" + current.brief_delivery_id,
                    )
                    return True

                return await effect("brief", action, brief)

        reader, runtime = Reader(), Runtime()
        return Handoff(runtime, reader, live=False), SimpleNamespace(
            effects=effects, calls=calls, runtime=runtime, reader=reader
        )

    async def run_handoff(
        self,
        source_provider,
        target_provider,
        *,
        crash=None,
        no_start=False,
        with_child=False,
        brief=None,
    ):
        import uuid
        from unittest.mock import patch

        from mainloop.config import settings
        from mainloop.db import db
        from mainloop.tasks import lifecycle
        from mainloop.tasks.handoff import Handoff
        from mainloop.tasks.service import TaskPorts, mutate

        from models.task import TaskAction, TaskReassign

        with patch.object(settings, "push_gate_enabled", True):
            creation = self.request(provider=source_provider)
            if brief is not None:
                creation = creation.model_copy(update={"brief": brief})
            _, task, source = await self.create_task(creation, ready=not no_start)
            if no_start:
                async with self.pool.acquire() as conn, conn.transaction():
                    await lifecycle.save_attempt(
                        conn,
                        source.model_copy(
                            update={
                                "state": "draining",
                                "evidence_refs": (lifecycle.CREATE_REJECTED,),
                            }
                        ),
                    )
                    await conn.execute(
                        "UPDATE native_bindings SET token_hash=NULL WHERE session_id=$1",
                        source.binding_id,
                    )
                    await lifecycle.settle(
                        conn, source.id, "failed", evidence=lifecycle.CREATE_REJECTED
                    )
                async with self.pool.acquire() as conn:
                    task = await lifecycle.load_task(conn, task.id)
                    source = await lifecycle.load_attempt(conn, source.id)
            child = None
            if with_child:
                child_principal = await self.principal(source)
                _, _, child = await self.create_task(
                    self.request(provider=source_provider), principal=child_principal
                )
            coordinator, external = self.coordinator(crash=crash)
            kind = "retry" if source_provider == target_provider else "reassign"
            data = dict(
                request_id=uuid.uuid4().hex,
                expected_version=task.version,
                expected_attempt_id=task.current_attempt_id,
            )
            request = (
                TaskAction(**data)
                if kind == "retry"
                else TaskReassign(**data, target_profile_id=target_provider)
            )
            installed = TaskPorts(provisioning=self.worker, handoff=coordinator)
            async with self.pool.acquire() as conn, conn.transaction():
                operation = await mutate(
                    conn,
                    self.owner,
                    kind,
                    request,
                    task_id=task.id,
                    installed_ports=installed,
                )
            # Request replay is unchanged even after task version and attempt move.
            async with self.pool.acquire() as conn, conn.transaction():
                replay = await mutate(
                    conn,
                    self.owner,
                    kind,
                    request,
                    task_id=task.id,
                    installed_ports=installed,
                )
                self.assertEqual(replay.id, operation.id)
            await self.pool.execute(
                "UPDATE tasks SET projection=$2::jsonb WHERE id=$1",
                task.id,
                '{"pr_number":7,"merge_proposal_id":"old-consent"}',
            )
            for _ in range(14):
                # A fresh coordinator per pass models backend restarts, with the
                # external fake retaining only its own native action identities.
                coordinator = Handoff(external.runtime, external.reader, live=False)
                try:
                    await coordinator.reconcile(db, operation)
                except RuntimeError:
                    pass
                async with self.pool.acquire() as conn:
                    current = await store.handoff_operation(conn, operation.id)
                    if current.state in ("completed", "blocked"):
                        break
            self.assertEqual(current.state, "completed", current.model_dump())
            async with self.pool.acquire() as conn:
                target = await lifecycle.load_attempt(conn, current.target_attempt_id)
                old = await lifecycle.load_attempt(conn, source.id)
                task_now = await lifecycle.load_task(conn, task.id)
                self.assertEqual(
                    (old.state, old.successor_id, target.predecessor_id),
                    ("superseded", target.id, source.id),
                )
                self.assertEqual(target.profile_id, target_provider)
                self.assertEqual(target.writer_generation, source.writer_generation + 1)
                self.assertEqual(task_now.current_attempt_id, target.id)
                self.assertEqual(task_now.checkout.ref, "a" * 40)
                self.assertEqual(target.environment, source.environment)
                self.assertEqual(
                    await conn.fetchval(
                        "SELECT projection FROM tasks WHERE id=$1", task.id
                    ),
                    "{}",
                )
                self.assertNotEqual(target.session_id, source.session_id)
                self.assertNotEqual(
                    await conn.fetchval(
                        "SELECT kagent_session_id FROM native_bindings WHERE session_id=$1",
                        target.binding_id,
                    ),
                    await conn.fetchval(
                        "SELECT kagent_session_id FROM native_bindings WHERE session_id=$1",
                        source.binding_id,
                    ),
                )
                self.assertIsNone(
                    await conn.fetchval(
                        "SELECT token_hash FROM native_bindings WHERE session_id=$1",
                        source.binding_id,
                    )
                )
                grant = await conn.fetchval(
                    "SELECT grant_data FROM push_grants WHERE id=$1 AND revoked_at IS NULL",
                    target.session_id,
                )
                self.assertEqual(store.decode(grant)["attempt_id"], target.id)
                self.assertEqual(
                    store.decode(grant)["writer_generation"], target.writer_generation
                )
                for action in ("submit", "resume", "preview", "create"):
                    with self.assertRaises(lifecycle.LifecycleDenied):
                        await lifecycle.check(conn, source.session_id, action)
                self.assertGreater(
                    await conn.fetchval(
                        "SELECT count(*) FROM task_events WHERE task_id=$1", task.id
                    ),
                    3,
                )
                if child is not None:
                    self.assertEqual(
                        (await lifecycle.load_attempt(conn, child.id)).state,
                        "cancelled",
                    )
                    self.assertIsNone(
                        await conn.fetchval(
                            "SELECT token_hash FROM native_bindings WHERE session_id=$1",
                            child.binding_id,
                        )
                    )
                with self.assertRaises(store.TaskError) as changed:
                    async with conn.transaction():
                        await mutate(
                            conn,
                            self.owner,
                            kind,
                            request.model_copy(
                                update={
                                    "expected_version": request.expected_version + 1
                                }
                            ),
                            task_id=task.id,
                            installed_ports=installed,
                        )
                self.assertEqual(changed.exception.code, "request_payload_conflict")
            # Settle the scenario through S1 rather than raising capacity limits.
            async with self.pool.acquire() as conn, conn.transaction():
                latest_task = await lifecycle.load_task(conn, task.id)
                cancel = await mutate(
                    conn,
                    self.owner,
                    "cancel",
                    TaskAction(
                        request_id=uuid.uuid4().hex,
                        expected_version=latest_task.version,
                        expected_attempt_id=target.id,
                    ),
                    task_id=task.id,
                    installed_ports=installed,
                )
            await self.worker.reconcile(db, cancel)
            return current, target

    async def test_s3_real_enrollment_both_directions_retry_and_no_start(self):
        for source, target, no_start in (
            ("claude", "codex", False),
            ("codex", "claude", False),
            ("claude", "claude", False),
            ("codex", "codex", True),
        ):
            with self.subTest(source=source, target=target, no_start=no_start):
                await self.run_handoff(source, target, no_start=no_start)

    async def test_s3_database_restart_after_every_external_boundary(self):
        for crash in ("checkpoint", "fence", "create", "grant", "brief"):
            with self.subTest(crash=crash):
                await self.run_handoff("codex", "claude", crash=crash)

    async def test_s3_supervisor_children_drain_before_transfer(self):
        await self.run_handoff("codex", "claude", with_child=True)

    async def test_s3_native_hitl_blocks_transfer_and_keeps_generation(self):
        import uuid

        from mainloop.db import db
        from mainloop.tasks.service import TaskPorts, mutate

        from models.task import TaskReassign

        _, task, source = await self.create_task(self.request(provider="codex"))
        coordinator, _ = self.coordinator(hitl_pending=True)
        async with self.pool.acquire() as conn, conn.transaction():
            operation = await mutate(
                conn,
                self.owner,
                "reassign",
                TaskReassign(
                    request_id=uuid.uuid4().hex,
                    expected_version=task.version,
                    expected_attempt_id=source.id,
                    target_profile_id="claude",
                ),
                task_id=task.id,
                installed_ports=TaskPorts(handoff=coordinator),
            )
        for _ in range(7):
            await coordinator.reconcile(db, operation)
        async with self.pool.acquire() as conn:
            current = await store.handoff_operation(conn, operation.id)
            self.assertEqual(current.state, "blocked")
            self.assertEqual(current.last_confirmed_step, "source_fencing")
            self.assertIsNone(current.target_attempt_id)
            claim = await conn.fetchrow(
                "SELECT held,generation FROM workspace_writer_claims WHERE attempt_id=$1",
                source.id,
            )
            self.assertTrue(claim["held"])
            self.assertEqual(claim["generation"], source.writer_generation)

    async def test_s3_concurrent_retry_and_reassign_have_one_operation(self):
        import asyncio
        import uuid

        from mainloop.tasks.service import TaskPorts, mutate

        from models.task import TaskAction, TaskReassign

        _, task, source = await self.create_task(self.request(provider="codex"))
        coordinator, _ = self.coordinator()

        async def start(kind):
            request = dict(
                request_id=uuid.uuid4().hex,
                expected_version=task.version,
                expected_attempt_id=source.id,
            )
            request = (
                TaskAction(**request)
                if kind == "retry"
                else TaskReassign(**request, target_profile_id="claude")
            )
            try:
                async with self.pool.acquire() as conn, conn.transaction():
                    value = await mutate(
                        conn,
                        self.owner,
                        kind,
                        request,
                        task_id=task.id,
                        installed_ports=TaskPorts(handoff=coordinator),
                    )
            except store.TaskError as exc:
                return exc.code
            return value.id

        outcomes = await asyncio.gather(start("retry"), start("reassign"))
        self.assertEqual(
            sum(
                value in ("stale_task_attempt", "handoff_in_progress")
                for value in outcomes
            ),
            1,
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM task_operations WHERE task_id=$1 AND kind IN ('retry','reassign')",
                task.id,
            ),
            1,
        )

    async def test_s3_retention_archive_failure_success_and_audit_persistence(self):
        from datetime import UTC, datetime

        from mainloop.db import db
        from mainloop.tasks import lifecycle
        from mainloop.tasks.retention import RetentionPolicy, reconcile_retention

        from models.task_handoff import RetentionReceipt

        current, target = await self.run_handoff("claude", "codex")
        async with self.pool.acquire() as conn, conn.transaction():
            source = await lifecycle.load_attempt(conn, current.source_attempt_id)
            await lifecycle.save_attempt(
                conn,
                source.model_copy(
                    update={"superseded_at": datetime(2026, 1, 31, tzinfo=UTC)}
                ),
            )
        calls = []

        class Port:
            confirmed = False
            lost_reply = True
            receipt = None

            async def safe_to_cleanup(self, conn, attempt):
                return True

            async def delete_native(self, conn, attempt, action_id):
                calls.append(action_id)
                if self.confirmed and self.receipt is not None:
                    return self.receipt
                receipt = RetentionReceipt(
                    attempt_id=attempt.id,
                    session_id=attempt.session_id,
                    runtime_identity=await conn.fetchval(
                        "SELECT kagent_session_id FROM native_bindings WHERE session_id=$1",
                        attempt.binding_id,
                    ),
                    action_id=action_id,
                    qualification="offline_fake",
                    confirmed=self.confirmed,
                    provenance="😀" * 2048,
                    observed_at=datetime.now(UTC),
                )
                if self.confirmed:
                    self.receipt = receipt
                    if self.lost_reply:
                        self.lost_reply = False
                        raise RuntimeError("lost confirmed cleanup reply")
                return receipt

        port = Port()
        await reconcile_retention(db, RetentionPolicy(), port, live=False)
        self.assertEqual(calls, [])
        self.assertIsNotNone(
            await self.pool.fetchval(
                "SELECT archived_at FROM sessions WHERE id=$1", source.session_id
            )
        )
        for _ in range(2):
            await reconcile_retention(db, RetentionPolicy(), port, live=False)
        async with self.pool.acquire() as conn:
            self.assertIsNone(
                (await lifecycle.load_attempt(conn, source.id)).native_deleted_at
            )
        self.assertEqual(len(calls), 2)
        self.assertEqual(calls[0], calls[1])
        port.confirmed = True
        with self.assertRaises(RuntimeError):
            await reconcile_retention(db, RetentionPolicy(), port, live=False)
        self.assertEqual(calls[-1], calls[0])
        await reconcile_retention(db, RetentionPolicy(), port, live=False)
        async with self.pool.acquire() as conn:
            source = await lifecycle.load_attempt(conn, source.id)
            self.assertIsNotNone(source.native_deleted_at)
            receipts = [
                ref
                for ref in source.evidence_refs
                if ref.startswith("retention-receipt:")
            ]
            self.assertEqual(len(receipts), 1)
            self.assertEqual(
                RetentionReceipt.model_validate_json(
                    await conn.fetchval(
                        "SELECT content FROM task_artifacts WHERE id=$1",
                        receipts[0].removeprefix("retention-receipt:"),
                    )
                ).action_id,
                calls[0],
            )
            self.assertIsNotNone(
                await conn.fetchval(
                    "SELECT conversation_id FROM sessions WHERE id=$1",
                    source.session_id,
                )
            )
            self.assertEqual(
                await conn.fetchval(
                    "SELECT count(*) FROM task_artifacts WHERE operation_id=$1",
                    current.id,
                ),
                3,
            )
            self.assertEqual(
                RetentionReceipt.model_validate_json(
                    await conn.fetchval(
                        "SELECT content FROM task_artifacts WHERE id=$1",
                        receipts[0].removeprefix("retention-receipt:"),
                    )
                ),
                port.receipt,
            )
            self.assertEqual(calls[-1], calls[0])
            self.assertIsNotNone(
                await conn.fetchval(
                    "SELECT generation FROM workspace_writer_claims WHERE owner_id=$1 AND repository='example/app'",
                    self.user,
                )
            )

    async def test_actual_successor_ledger_brief_is_byte_bounded_without_duplication(
        self,
    ):
        for brief in ("x" * 9000, "x" * 16384, "😀" * 4096):
            with self.subTest(bytes=len(brief.encode("utf-8"))):
                _, target = await self.run_handoff("claude", "codex", brief=brief)
                text = await self.pool.fetchval(
                    "SELECT content FROM messages WHERE id=$1", target.brief_delivery_id
                )
                self.assertEqual(text, brief)
                self.assertLessEqual(len(text.encode("utf-8")), 16384)

    async def prepare_cancel_window(self, step, *, no_start=False):
        import uuid
        from unittest.mock import patch

        from mainloop.config import settings
        from mainloop.db import db
        from mainloop.tasks import lifecycle
        from mainloop.tasks.service import TaskPorts, mutate

        from models.task import TaskAction

        setting = patch.object(settings, "push_gate_enabled", True)
        setting.start()
        self.addCleanup(setting.stop)
        _, task, source = await self.create_task(
            self.request(provider="codex"), ready=not no_start
        )
        if no_start:
            async with self.pool.acquire() as conn, conn.transaction():
                await lifecycle.save_attempt(
                    conn,
                    source.model_copy(
                        update={
                            "state": "draining",
                            "evidence_refs": (lifecycle.CREATE_REJECTED,),
                        }
                    ),
                )
                await conn.execute(
                    "UPDATE native_bindings SET token_hash=NULL WHERE session_id=$1",
                    source.binding_id,
                )
                await lifecycle.settle(
                    conn, source.id, "failed", evidence=lifecycle.CREATE_REJECTED
                )
            async with self.pool.acquire() as conn:
                task = await lifecycle.load_task(conn, task.id)
                source = await lifecycle.load_attempt(conn, source.id)
        coordinator, external = self.coordinator()
        installed = TaskPorts(provisioning=self.worker, handoff=coordinator)
        async with self.pool.acquire() as conn, conn.transaction():
            operation = await mutate(
                conn,
                self.owner,
                "retry",
                TaskAction(
                    request_id=uuid.uuid4().hex,
                    expected_version=task.version,
                    expected_attempt_id=task.current_attempt_id,
                ),
                task_id=task.id,
                installed_ports=installed,
            )
        for _ in range(10):
            async with self.pool.acquire() as conn:
                operation = await store.handoff_operation(conn, operation.id)
            if operation.last_confirmed_step == step:
                return operation, source, external, installed
            self.assertNotIn(operation.state, ("completed", "blocked"))
            await coordinator.reconcile(db, operation)
        self.fail(f"Did not reach committed {step}")

    async def cancel_null_current_task(self, task_id, installed):
        import uuid

        from mainloop.tasks import lifecycle
        from mainloop.tasks.service import mutate

        from models.task import TaskAction

        async with self.pool.acquire() as conn, conn.transaction():
            task = await lifecycle.load_task(conn, task_id)
            self.assertIsNone(task.current_attempt_id)
            cancel = await mutate(
                conn,
                self.owner,
                "cancel",
                TaskAction(
                    request_id=uuid.uuid4().hex,
                    expected_version=task.version,
                    expected_attempt_id=None,
                ),
                task_id=task.id,
                installed_ports=installed,
            )
            stopped = await lifecycle.load_task(conn, task_id)
            row = await conn.fetchrow(
                "SELECT projection FROM tasks WHERE id=$1", task_id
            )
        self.assertEqual(cancel.state, "completed")
        self.assertEqual(stopped.status, "cancelled")
        return cancel, stopped, row["projection"]

    async def assert_no_successor_after_stop(
        self, operation, source, stopped, projection
    ):
        from mainloop.tasks import lifecycle

        async with self.pool.acquire() as conn:
            actual = await lifecycle.load_task(conn, stopped.id)
            self.assertEqual(
                actual, stopped
            )  # Includes terminal version/reason/current attempt.
            self.assertEqual(
                await conn.fetchval(
                    "SELECT projection FROM tasks WHERE id=$1", stopped.id
                ),
                projection,
            )
            self.assertEqual(
                await conn.fetchval(
                    "SELECT count(*) FROM task_attempts WHERE task_id=$1", stopped.id
                ),
                1,
            )
            old = await lifecycle.load_attempt(conn, source.id)
            self.assertIsNone(old.successor_id)
            self.assertIn(old.state, ("failed", "superseded"))
            self.assertIsNone(
                await conn.fetchval(
                    "SELECT token_hash FROM native_bindings WHERE session_id=$1",
                    source.binding_id,
                )
            )
            self.assertFalse(
                await conn.fetchval(
                    "SELECT held FROM workspace_writer_claims WHERE owner_id=$1 AND repository='example/app' AND branch=$2",
                    self.user,
                    stopped.checkout.branch,
                )
            )
            self.assertEqual(
                await conn.fetchval(
                    "SELECT count(*) FROM push_grants WHERE attempt_id IN (SELECT id FROM task_attempts WHERE task_id=$1) AND attempt_id<>$2",
                    stopped.id,
                    source.id,
                ),
                0,
            )
            self.assertEqual(
                await conn.fetchval(
                    "SELECT count(*) FROM native_deliveries WHERE session_id IN (SELECT session_id FROM task_attempts WHERE task_id=$1) AND session_id<>$2",
                    stopped.id,
                    source.session_id,
                ),
                0,
            )
            current = await store.handoff_operation(conn, operation.id)
            self.assertEqual(current.state, "blocked")
            self.assertIsNone(current.target_attempt_id)

    async def test_completed_cancel_at_source_fenced_never_enrolls_successor(self):
        from mainloop.db import db
        from mainloop.tasks.handoff import Handoff

        operation, source, external, installed = await self.prepare_cancel_window(
            "source_fenced"
        )
        cancel, stopped, projection = await self.cancel_null_current_task(
            operation.task_id, installed
        )
        calls = tuple(external.calls)
        for _ in range(4):
            await Handoff(external.runtime, external.reader, live=False).reconcile(
                db, operation
            )
        await self.assert_no_successor_after_stop(
            operation, source, stopped, projection
        )
        self.assertEqual(tuple(external.calls), calls)
        async with self.pool.acquire() as conn:
            self.assertEqual(
                (await store.handoff_operation(conn, cancel.id)).state, "completed"
            )

    async def assert_completed_cancel_no_start_window(self, step):
        from mainloop.db import db
        from mainloop.tasks.handoff import Handoff

        operation, source, external, installed = await self.prepare_cancel_window(
            step, no_start=True
        )
        cancel, stopped, projection = await self.cancel_null_current_task(
            operation.task_id, installed
        )
        calls = tuple(external.calls)
        for _ in range(4):
            await Handoff(external.runtime, external.reader, live=False).reconcile(
                db, operation
            )
        await self.assert_no_successor_after_stop(
            operation, source, stopped, projection
        )
        self.assertEqual(tuple(external.calls), calls)
        async with self.pool.acquire() as conn:
            self.assertEqual(
                (await store.handoff_operation(conn, cancel.id)).state, "completed"
            )

    async def test_completed_cancel_no_start_requested(self):
        await self.assert_completed_cancel_no_start_window("requested")

    async def test_completed_cancel_no_start_checkpoint_required(self):
        await self.assert_completed_cancel_no_start_window("checkpoint_required")

    async def test_completed_cancel_no_start_checkpoint_verified(self):
        await self.assert_completed_cancel_no_start_window("checkpoint_verified")

    async def test_completed_cancel_no_start_source_fencing(self):
        await self.assert_completed_cancel_no_start_window("source_fencing")

    async def test_completed_cancel_no_start_source_fenced(self):
        await self.assert_completed_cancel_no_start_window("source_fenced")

    async def test_lost_reply_and_task_error_projection_preserve_completed_cancel(self):
        from mainloop.db import db
        from mainloop.tasks.handoff import Handoff

        for error in (
            RuntimeError("lost checkpoint reply"),
            store.TaskError(409, "checkpoint_required"),
        ):
            with self.subTest(error=type(error).__name__):
                operation, source, external, installed = (
                    await self.prepare_cancel_window(
                        "checkpoint_required", no_start=True
                    )
                )
                case = self
                stopped = None
                projection = None

                class Reader:
                    async def read(
                        self,
                        *args,
                        case=case,
                        task_id=operation.task_id,
                        installed=installed,
                        error=error,
                    ):
                        nonlocal stopped, projection
                        _, stopped, projection = await case.cancel_null_current_task(
                            task_id, installed
                        )
                        raise error

                coordinator = Handoff(external.runtime, Reader(), live=False)
                if isinstance(error, RuntimeError):
                    with self.assertRaises(RuntimeError):
                        await coordinator.reconcile(db, operation)
                else:
                    await coordinator.reconcile(db, operation)
                async with self.pool.acquire() as conn:
                    from mainloop.tasks import lifecycle

                    self.assertEqual(
                        await lifecycle.load_task(conn, stopped.id), stopped
                    )
                for _ in range(3):
                    await Handoff(
                        external.runtime, external.reader, live=False
                    ).reconcile(db, operation)
                await self.assert_no_successor_after_stop(
                    operation, source, stopped, projection
                )

    async def test_stale_task_version_before_successor_enrollment_is_rejected(self):
        from mainloop.db import db
        from mainloop.tasks import lifecycle
        from mainloop.tasks.handoff import Handoff

        operation, source, external, _ = await self.prepare_cancel_window(
            "source_fenced"
        )
        async with self.pool.acquire() as conn, conn.transaction():
            task = await lifecycle.load_task(conn, operation.task_id, lock=True)
            await store.save_task(
                conn,
                task.model_copy(update={"version": task.version + 1}),
                task.version,
                "fixture:owner-change",
            )
        for _ in range(4):
            await Handoff(external.runtime, external.reader, live=False).reconcile(
                db, operation
            )
        async with self.pool.acquire() as conn:
            self.assertEqual(
                await conn.fetchval(
                    "SELECT count(*) FROM task_attempts WHERE task_id=$1", task.id
                ),
                1,
            )
            self.assertIsNone(
                (await lifecycle.load_task(conn, task.id)).current_attempt_id
            )
            current = await store.handoff_operation(conn, operation.id)
            self.assertEqual(current.state, "blocked")
            self.assertEqual(
                current.request_payload["_s3"]["failure_code"], "stale_task_attempt"
            )
        self.assertFalse(
            any(step in ("create", "grant", "brief") for step, _ in external.calls)
        )

    async def prepare_retention_source(self):
        from datetime import UTC, datetime

        from mainloop.tasks import lifecycle

        operation, _ = await self.run_handoff("claude", "codex")
        async with self.pool.acquire() as conn, conn.transaction():
            source = await lifecycle.load_attempt(conn, operation.source_attempt_id)
            source = await lifecycle.save_attempt(
                conn,
                source.model_copy(
                    update={
                        "superseded_at": datetime(2026, 1, 31, tzinfo=UTC),
                        "archived_at": datetime(2026, 2, 21, tzinfo=UTC),
                    }
                ),
            )
            runtime_identity = await conn.fetchval(
                "SELECT kagent_session_id FROM native_bindings WHERE session_id=$1",
                source.binding_id,
            )
        return operation, source, runtime_identity

    async def test_retention_storage_preflight_blocks_cleanup(self):
        from unittest.mock import AsyncMock, patch

        from mainloop.db import db
        from mainloop.tasks import lifecycle
        from mainloop.tasks.retention import RetentionPolicy, reconcile_retention

        for unsupported in ("schema", "model"):
            with self.subTest(unsupported=unsupported):
                operation, source, _ = await self.prepare_retention_source()
                port = AsyncMock()
                port.safe_to_cleanup.return_value = True
                port.delete_native.side_effect = AssertionError(
                    "unsupported storage invoked cleanup"
                )
                if unsupported == "schema":
                    await self.pool.execute(
                        "ALTER TABLE task_artifacts ADD CONSTRAINT fixture_receipt_storage_disabled CHECK(kind <> 'retention_receipt') NOT VALID"
                    )
                try:
                    if unsupported == "model":
                        with patch(
                            "mainloop.tasks.retention.TaskArtifact.model_validate",
                            side_effect=ValueError("fixture old model kind"),
                        ):
                            await reconcile_retention(
                                db, RetentionPolicy(), port, live=False
                            )
                    else:
                        await reconcile_retention(
                            db, RetentionPolicy(), port, live=False
                        )
                finally:
                    if unsupported == "schema":
                        await self.pool.execute(
                            "ALTER TABLE task_artifacts DROP CONSTRAINT fixture_receipt_storage_disabled"
                        )
                port.safe_to_cleanup.assert_awaited()
                port.delete_native.assert_not_called()
                async with self.pool.acquire() as conn:
                    self.assertEqual(
                        await lifecycle.load_attempt(conn, source.id), source
                    )
                    self.assertEqual(
                        await conn.fetchval(
                            "SELECT count(*) FROM task_artifacts WHERE operation_id=$1",
                            operation.id,
                        ),
                        2,
                    )

    async def test_retention_receipt_commit_failure_reuses_original_receipt(self):
        from datetime import UTC, datetime
        from unittest.mock import patch

        from mainloop.db import db
        from mainloop.tasks import lifecycle
        from mainloop.tasks.retention import RetentionPolicy, reconcile_retention

        from models.task_handoff import RetentionReceipt

        operation, source, runtime_identity = await self.prepare_retention_source()
        receipt = RetentionReceipt(
            attempt_id=source.id,
            session_id=source.session_id,
            runtime_identity=runtime_identity,
            action_id=f"task-retention:{source.id}:delete",
            qualification="offline_fake",
            confirmed=True,
            provenance="😀" * 2048,
            observed_at=datetime.now(UTC),
        )
        calls = []
        effects = set()

        class Port:
            async def safe_to_cleanup(self, conn, attempt):
                return True

            async def delete_native(self, conn, attempt, action_id):
                calls.append(action_id)
                effects.add(action_id)
                return receipt

        port = Port()
        case = self

        async def interrupted_commit(conn, attempt):
            case.assertIsNotNone(attempt.native_deleted_at)
            case.assertEqual(
                await conn.fetchval(
                    "SELECT count(*) FROM task_artifacts WHERE operation_id=$1 AND kind='retention_receipt'",
                    operation.id,
                ),
                1,
            )
            raise RuntimeError("fixture interrupted receipt commit")

        with patch(
            "mainloop.tasks.lifecycle.save_attempt", side_effect=interrupted_commit
        ):
            with self.assertRaises(RuntimeError):
                await reconcile_retention(db, RetentionPolicy(), port, live=False)
        async with self.pool.acquire() as conn:
            self.assertEqual(await lifecycle.load_attempt(conn, source.id), source)
            self.assertEqual(
                await conn.fetchval(
                    "SELECT count(*) FROM task_artifacts WHERE operation_id=$1",
                    operation.id,
                ),
                2,
            )
        await reconcile_retention(db, RetentionPolicy(), port, live=False)
        self.assertEqual(calls, [receipt.action_id, receipt.action_id])
        self.assertEqual(effects, {receipt.action_id})
        async with self.pool.acquire() as conn:
            after = await lifecycle.load_attempt(conn, source.id)
            self.assertEqual(after.native_deleted_at, receipt.observed_at)
            self.assertEqual(len(after.evidence_refs), len(source.evidence_refs) + 1)
            artifact_id = after.evidence_refs[-1].removeprefix("retention-receipt:")
            self.assertEqual(
                RetentionReceipt.model_validate_json(
                    await conn.fetchval(
                        "SELECT content FROM task_artifacts WHERE id=$1", artifact_id
                    )
                ),
                receipt,
            )
            self.assertEqual(
                await conn.fetchval(
                    "SELECT count(*) FROM task_artifacts WHERE operation_id=$1",
                    operation.id,
                ),
                3,
            )
        await reconcile_retention(db, RetentionPolicy(), port, live=False)
        self.assertEqual(len(calls), 2)


class ManifestPostgresTests(PostgresTestCase):
    async def test_bounded_manifest_and_unverified_note_cannot_overflow_or_mutate(self):
        import uuid

        from mainloop.tasks.checkpoint import persist_manifest
        from pydantic import ValidationError

        from models.task_handoff import ContinuationManifest
        from models.workspace import WorkspaceEnvironment

        environment = WorkspaceEnvironment(
            environment_id="env",
            version_id="version",
            image="example/image@sha256:" + "a" * 64,
            platform="linux/amd64",
            policy_identity="policy",
        )
        principal = TaskPrincipal(self.user)
        async with self.pool.acquire() as conn, conn.transaction():
            operation, _ = await store.begin_operation(
                conn, principal, uuid.uuid4().hex, "retry", {}
            )
            data = dict(
                task_id="task",
                operation_id=operation.id,
                predecessor_id="source",
                target_profile_id="codex",
                repository="example/app",
                branch="feature/task",
                checkpoint_sha="a" * 40,
                environment=environment,
                caller_instructions="Owner instructions",
            )
            manifest = ContinuationManifest(**data, unverified_note="A provider claim")
            artifact = await persist_manifest(conn, manifest)
            self.assertEqual(await persist_manifest(conn, manifest), artifact)
            with self.assertRaises(store.TaskError) as conflict:
                await persist_manifest(
                    conn, manifest.model_copy(update={"unverified_note": "Different"})
                )
            self.assertEqual(conflict.exception.code, "artifact_payload_conflict")
            with self.assertRaises(ValidationError):
                ContinuationManifest(**data, pending_hitl=["receipt"])
        async with self.pool.acquire() as conn, conn.transaction():
            operation, _ = await store.begin_operation(
                conn, principal, uuid.uuid4().hex, "retry", {}
            )
            oversized = ContinuationManifest(
                **{**data, "operation_id": operation.id},
                report_refs=tuple("e" * 2048 for _ in range(64)),
            )
            with self.assertRaises(store.TaskError) as bound:
                await persist_manifest(conn, oversized)
            self.assertEqual(bound.exception.code, "artifact_too_large")
            self.assertEqual(
                await conn.fetchval(
                    "SELECT count(*) FROM task_artifacts WHERE operation_id=$1",
                    operation.id,
                ),
                0,
            )
            with self.assertRaises(store.TaskError):
                await store.add_artifact(
                    conn,
                    operation.id,
                    "unverified_provider_summary",
                    {"note": "🙂" * 4000},
                )
