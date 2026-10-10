"""Concurrent task persistence against disposable PostgreSQL; no live agents."""

import asyncio
import uuid
from datetime import UTC, datetime
from unittest.mock import patch

import asyncpg
import httpx
from fastapi import HTTPException
from mainloop import api
from mainloop.config import settings
from mainloop.db import db
from mainloop.db import tasks as store
from mainloop.db.postgres import MIGRATION_SQL, SCHEMA_SQL
from mainloop.identity import current_user
from mainloop.providers import registry
from mainloop.runtime import task_api
from mainloop.services.github_repo import parse_github_repo
from mainloop.tasks.events import dispatch_committed_events
from mainloop.tasks.principal import TaskPrincipal
from mainloop.tasks.service import mutate
from tests.runtime.test_postgres_ledger import PostgresTestCase

from models.task import ProjectProviderUpdate, Task, TaskCheckout, TaskCreate


class TaskPostgresTests(PostgresTestCase):
    async def asyncSetUp(self):
        await super().asyncSetUp()
        # Parent test classes retain their rows between tests; isolate our capacity
        # assertions without touching any other server/database.
        await self.pool.execute(
            "TRUNCATE tasks,task_attempts,workspace_writer_claims,task_events,task_operations CASCADE"
        )
        self.principal = TaskPrincipal(self.user)
        self.project = await db.get_or_create_project(
            self.user, parse_github_repo("example/app")
        )

    def task(self, *, branch=None, parent=None, owner=None, project=None):
        now = datetime.now(UTC)
        task_id = uuid.uuid4().hex
        return Task(
            id=task_id,
            owner_id=owner or self.user,
            project_id=project or self.project.id,
            parent_task_id=parent.id if parent else None,
            root_task_id=parent.root_task_id if parent else task_id,
            title="Task",
            brief="Brief",
            mode="code",
            assigned_profile_id="claude",
            selection_source="explicit",
            checkout=TaskCheckout(branch=branch or f"feature/{task_id}"),
            created_at=now,
            updated_at=now,
        )

    async def admit(self, task, *, per_parent_cap=3, global_cap=6):
        async with self.pool.acquire() as conn, conn.transaction():
            await store.admission_lock(conn)
            await store.insert_task(conn, task)
            return await store.admit_attempt(
                conn,
                task,
                registry().resolve("claude", "supervisor"),
                role="supervisor",
                depth=1,
                per_parent_cap=per_parent_cap,
                global_cap=global_cap,
            )

    async def test_fresh_schema_twice_no_change_and_no_backfill(self):
        async with self.pool.acquire() as conn:
            before = await conn.fetchval("SELECT count(*) FROM tasks")
            await conn.execute(SCHEMA_SQL)
            await conn.execute(MIGRATION_SQL)
            await conn.execute(SCHEMA_SQL)
            await conn.execute(MIGRATION_SQL)
            self.assertEqual(await conn.fetchval("SELECT count(*) FROM tasks"), before)

    async def test_capacity_rest_details_include_only_the_parent_admission_bucket(self):
        held = [await self.admit(self.task()) for _ in range(3)]
        other_project = await db.get_or_create_project(
            "other-owner", parse_github_repo("example/app")
        )
        await self.admit(
            self.task(owner="other-owner", project=other_project.id), global_cap=10
        )
        async with self.pool.acquire() as conn, conn.transaction():
            child = self.task(parent=held[0][0])
            await store.insert_task(conn, child)
            await store.admit_attempt(
                conn,
                child,
                registry().resolve("claude", "child"),
                role="child",
                depth=2,
                global_cap=10,
            )
        with self.assertRaises(HTTPException) as caught:
            async with task_api.transaction() as conn:
                task = self.task()
                await store.insert_task(conn, task)
                await store.admit_attempt(
                    conn,
                    task,
                    registry().resolve("claude", "supervisor"),
                    role="supervisor",
                    depth=1,
                    global_cap=10,
                )
        self.assertEqual(caught.exception.status_code, 409)
        detail = caught.exception.detail
        self.assertEqual(detail["reason"], "parent_capacity")
        self.assertEqual(
            {row["task_id"] for row in detail["held_tasks"]},
            {task.id for task, _ in held},
        )
        self.assertEqual(
            {row["held_attempt_id"] for row in detail["held_tasks"]},
            {attempt.id for _, attempt in held},
        )
        self.assertIn("task_cancel", detail["recovery"])
        self.assertEqual(await self.pool.fetchval("SELECT count(*) FROM tasks"), 5)

    async def test_concurrent_same_request_and_changed_payload(self):
        request = TaskCreate(
            request_id="same", title="T", brief="B", mode="coordination"
        )

        async def action(body):
            async with self.pool.acquire() as conn, conn.transaction():
                return await mutate(conn, self.principal, "create", body)

        a, b = await asyncio.gather(action(request), action(request))
        self.assertEqual(a.id, b.id)
        self.assertEqual(a.reason, "provisioning_unavailable")
        self.assertEqual(await self.pool.fetchval("SELECT count(*) FROM tasks"), 0)
        with self.assertRaises(store.TaskError) as caught:
            await action(request.model_copy(update={"brief": "changed"}))
        self.assertEqual(caught.exception.status, 409)
        result = await asyncio.gather(
            action(request.model_copy(update={"request_id": "new", "title": "a"})),
            action(request.model_copy(update={"request_id": "new", "title": "b"})),
            return_exceptions=True,
        )
        self.assertEqual(sum(isinstance(v, store.TaskError) for v in result), 1)

    async def test_atomic_capacity_and_rollback(self):
        result = await asyncio.gather(
            *(self.admit(self.task()) for _ in range(7)), return_exceptions=True
        )
        self.assertEqual(sum(isinstance(v, tuple) for v in result), 3)
        self.assertEqual(await self.pool.fetchval("SELECT count(*) FROM tasks"), 3)
        self.assertEqual(
            await self.pool.fetchval("SELECT count(*) FROM task_attempts"), 3
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM workspace_writer_claims WHERE held"
            ),
            3,
        )
        self.assertEqual(
            await self.pool.fetchval("SELECT count(*) FROM task_events"), 3
        )
        await self.pool.execute(
            "TRUNCATE tasks,task_attempts,workspace_writer_claims,task_events,task_operations CASCADE"
        )
        result = await asyncio.gather(
            *(
                self.admit(self.task(), per_parent_cap=10, global_cap=2)
                for _ in range(4)
            ),
            return_exceptions=True,
        )
        self.assertEqual(sum(isinstance(v, tuple) for v in result), 2)

    async def test_canonical_repository_collision_ordinary_and_delegated(self):
        async with self.pool.acquire() as conn, conn.transaction():
            generation = await store.reserve_writer(
                conn,
                owner_id=self.user,
                repository="https://github.com/EXAMPLE/APP.git",
                branch="feature/shared",
                binding_id="owner-workspace",
            )
        self.assertEqual(generation, 1)
        with self.assertRaises(store.TaskError):
            await self.admit(self.task(branch="feature/shared"))
        self.assertEqual(await self.pool.fetchval("SELECT count(*) FROM tasks"), 0)
        self.assertEqual(
            await self.pool.fetchval("SELECT count(*) FROM task_attempts"), 0
        )

    async def test_attempt_current_relation_and_immutable_routing(self):
        task, attempt = await self.admit(self.task())
        async with self.pool.acquire() as conn:
            with self.assertRaises(asyncpg.RaiseError):
                await conn.execute(
                    "UPDATE task_attempts SET configuration_revision='changed' WHERE id=$1",
                    attempt.id,
                )
            with self.assertRaises(asyncpg.UniqueViolationError):
                await conn.execute(
                    "INSERT INTO task_attempts SELECT * FROM task_attempts WHERE id=$1",
                    attempt.id,
                )
            with self.assertRaises(asyncpg.UniqueViolationError) as duplicate:
                await conn.execute(
                    """INSERT INTO task_attempts(id,task_id,number,profile_id,native_provider,configuration_revision,agent_ref,role,depth,state,snapshot)
                    SELECT $2,task_id,number+1,profile_id,native_provider,configuration_revision,agent_ref,role,depth,state,snapshot
                    FROM task_attempts WHERE id=$1""",
                    attempt.id,
                    uuid.uuid4().hex,
                )
            self.assertEqual(
                duplicate.exception.constraint_name, "task_one_unfenced_attempt"
            )
            other, _ = await self.admit(self.task())
            with self.assertRaises(asyncpg.ForeignKeyViolationError):
                await conn.execute(
                    "UPDATE tasks SET current_attempt_id=$2 WHERE id=$1",
                    other.id,
                    attempt.id,
                )
            with patch.object(settings, "provider_profiles", []), patch.object(
                settings, "kagent_supervisor_claude_agent", "changed-agent"
            ):
                stored = await store.attempts(conn, task.id)
                self.assertEqual(stored[0].agent_ref, attempt.agent_ref)
                self.assertEqual(
                    stored[0].configuration_revision, attempt.configuration_revision
                )

    async def test_owner_isolation_project_and_hierarchy(self):
        task, _ = await self.admit(self.task())
        async with self.pool.acquire() as conn:
            with self.assertRaises(store.TaskError):
                await store.get_task(conn, task.id, TaskPrincipal("different-owner"))
        with self.assertRaises(asyncpg.RaiseError):
            async with self.pool.acquire() as conn, conn.transaction():
                await store.insert_task(conn, self.task(owner="different-owner"))
        with self.assertRaises(asyncpg.ForeignKeyViolationError):
            async with self.pool.acquire() as conn, conn.transaction():
                await store.insert_task(
                    conn,
                    self.task(parent=task).model_copy(
                        update={"root_task_id": "missing"}
                    ),
                )

        foreign_topic = uuid.uuid4().hex
        await self.pool.execute(
            "INSERT INTO topics(id,user_id,name) VALUES($1,'other-owner','foreign')",
            foreign_topic,
        )
        with self.assertRaises(asyncpg.RaiseError):
            async with self.pool.acquire() as conn, conn.transaction():
                await store.insert_task(
                    conn, self.task().model_copy(update={"topic_id": foreign_topic})
                )

    async def test_preference_cas_and_owner_isolation(self):
        async with self.pool.acquire() as conn, conn.transaction():
            selected = await store.set_preference(
                conn,
                self.project.id,
                self.user,
                ProjectProviderUpdate(profile_id="codex", expected_version=0),
            )
            self.assertEqual(selected.version, 1)
        async with self.pool.acquire() as conn, conn.transaction():
            with self.assertRaises(store.TaskError):
                await store.set_preference(
                    conn,
                    self.project.id,
                    self.user,
                    ProjectProviderUpdate(profile_id="claude", expected_version=0),
                )
        async with self.pool.acquire() as conn:
            with self.assertRaises(store.TaskError):
                await store.preference(conn, self.project.id, "other")

    async def test_claim_fence_and_generation_cas(self):
        task, attempt = await self.admit(self.task(branch="feature/fence"))
        async with self.pool.acquire() as conn, conn.transaction():
            with self.assertRaises(store.TaskError):
                await store.release_writer(
                    conn,
                    owner_id=self.user,
                    repository="example/app",
                    branch=task.checkout.branch,
                    generation=1,
                    attempt_id=attempt.id,
                    fence_evidence_ref="fixture:confirmed-fence",
                )
            await conn.execute(
                "UPDATE task_attempts SET state='fenced',capacity_held=FALSE WHERE id=$1",
                attempt.id,
            )
            await store.release_writer(
                conn,
                owner_id=self.user,
                repository="example/app",
                branch=task.checkout.branch,
                generation=1,
                attempt_id=attempt.id,
                fence_evidence_ref="fixture:confirmed-fence",
            )
            generation = await store.reserve_writer(
                conn,
                owner_id=self.user,
                repository="example/app",
                branch=task.checkout.branch,
                binding_id="new-writer",
            )
            self.assertEqual(generation, 2)
            with self.assertRaises(store.TaskError):
                await store.release_writer(
                    conn,
                    owner_id=self.user,
                    repository="example/app",
                    branch=task.checkout.branch,
                    generation=1,
                    binding_id="new-writer",
                    fence_evidence_ref="fixture:confirmed-fence",
                )

    async def test_outbox_committed_only_and_api_reads(self):
        class Sink:
            def __init__(self):
                self.events = []

            async def publish(self, owner, event):
                self.events.append((owner, event))

        sink = Sink()
        async with self.pool.acquire() as conn, conn.transaction():
            await store.admission_lock(conn)
            task = self.task()
            await store.insert_task(conn, task)
            await store.admit_attempt(
                conn,
                task,
                registry().resolve("claude", "supervisor"),
                role="supervisor",
                depth=1,
            )
            await dispatch_committed_events(db, sink)
            self.assertEqual(sink.events, [])
        await dispatch_committed_events(db, sink)
        await dispatch_committed_events(db, sink)
        self.assertEqual(len(sink.events), 1)
        self.assertEqual(sink.events[0][1].type, "task:updated")
        api.app.dependency_overrides[current_user] = lambda: self.user
        try:
            with patch.object(settings, "api_hosts", "localhost"):
                async with httpx.AsyncClient(
                    transport=httpx.ASGITransport(app=api.app),
                    base_url="http://localhost",
                ) as client:
                    response = await client.get(f"/tasks/{task.id}")
                    self.assertEqual(response.status_code, 200, response.text)
                    self.assertFalse(response.json()["actions"]["retry"]["available"])
                    self.assertEqual(len((await client.get("/tasks")).json()), 1)
        finally:
            api.app.dependency_overrides.pop(current_user)

    async def test_projection_event_idempotency_and_stale_attempt(self):
        from models.task import TaskProjection

        task, attempt = await self.admit(self.task())
        async with self.pool.acquire() as conn, conn.transaction():
            updated = await store.update_projection(
                conn,
                self.principal,
                task.id,
                task.version,
                attempt.id,
                TaskProjection(ci_state="pending", ci_head_sha="a" * 40),
                "ci-observation-1",
            )
            again = await store.update_projection(
                conn,
                self.principal,
                task.id,
                task.version,
                attempt.id,
                TaskProjection(ci_state="pending", ci_head_sha="a" * 40),
                "ci-observation-1",
            )
            self.assertEqual(updated.version, again.version)
            with self.assertRaises(store.TaskError):
                await store.update_projection(
                    conn,
                    self.principal,
                    task.id,
                    updated.version,
                    "stale",
                    TaskProjection(ci_state="success"),
                    "ci-observation-2",
                )
            self.assertEqual(
                (await store.get_task(conn, task.id, self.principal)).status, "queued"
            )

    async def test_profile_precedence_and_invalid_production_default(self):
        from mainloop.tasks.service import select_profile

        request = TaskCreate(
            request_id="s",
            title="t",
            brief="b",
            mode="code",
            project_id=self.project.id,
            checkout=TaskCheckout(branch="feature/selection"),
        )
        # Mock only qualification, preserving real registry/ownership/selection behavior.
        with patch(
            "mainloop.tasks.service.qualify_task_profile",
            side_effect=lambda profile, *args, **kwargs: profile,
        ):
            async with self.pool.acquire() as conn, conn.transaction():
                profile, source = await select_profile(
                    conn, self.principal, request, "supervisor"
                )
                self.assertEqual(
                    (profile.id, source), ("claude", "installation_default")
                )
                await store.set_preference(
                    conn,
                    self.project.id,
                    self.user,
                    ProjectProviderUpdate(profile_id="codex", expected_version=0),
                )
                profile, source = await select_profile(
                    conn, self.principal, request, "supervisor"
                )
                self.assertEqual((profile.id, source), ("codex", "project_default"))
                profile, source = await select_profile(
                    conn,
                    self.principal,
                    request.model_copy(update={"provider_profile_id": "claude"}),
                    "supervisor",
                )
                self.assertEqual((profile.id, source), ("claude", "explicit"))
        async with self.pool.acquire() as conn:
            with self.assertRaises(store.TaskError):
                await select_profile(conn, self.principal, request, "supervisor")

    async def test_attempt_role_must_match_persisted_tree(self):
        parent, _ = await self.admit(self.task())
        child = self.task(parent=parent)
        async with self.pool.acquire() as conn, conn.transaction():
            await store.admission_lock(conn)
            await store.insert_task(conn, child)
            _, attempt = await store.admit_attempt(
                conn,
                child,
                registry().resolve("claude", "child"),
                role="child",
                depth=2,
            )
            self.assertEqual(attempt.depth, 2)
        grandchild = self.task(parent=child)
        with self.assertRaises(asyncpg.RaiseError):
            async with self.pool.acquire() as conn, conn.transaction():
                await store.insert_task(conn, grandchild)

    async def test_binding_scope_and_inherited_constraint(self):
        parent, parent_attempt = await self.admit(
            self.task().model_copy(update={"provider_constraint": "claude"})
        )
        child = self.task(parent=parent)
        async with self.pool.acquire() as conn, conn.transaction():
            await store.admission_lock(conn)
            await store.insert_task(conn, child)
            await store.admit_attempt(
                conn,
                child,
                registry().resolve("claude", "child"),
                role="child",
                depth=2,
            )
            binding_id = uuid.uuid4().hex
            thread_id = uuid.uuid4().hex
            conversation_id = uuid.uuid4().hex
            await conn.execute(
                "INSERT INTO main_threads(id,user_id) VALUES($1,$2)",
                thread_id,
                self.user,
            )
            await conn.execute(
                "INSERT INTO conversations(id,user_id,title) VALUES($1,$2,$3)",
                conversation_id,
                self.user,
                "Fixture",
            )
            await conn.execute(
                "INSERT INTO sessions(id,user_id,main_thread_id,title,description,prompt,conversation_id,status) VALUES($1,$2,$3,'Fixture','Fixture','Fixture',$4,'active')",
                binding_id,
                self.user,
                thread_id,
                conversation_id,
            )
            await conn.execute(
                "INSERT INTO native_bindings(session_id,kind,role,token_hash,mcp_grant_kind) VALUES($1,'claude','supervisor','fixture-hash','coordination')",
                binding_id,
            )
            await conn.execute(
                "UPDATE task_attempts SET binding_id=$2,session_id=$2,state='active' WHERE id=$1",
                parent_attempt.id,
                binding_id,
            )
        principal = TaskPrincipal(
            self.user,
            binding_id=binding_id,
            role="supervisor",
            task_id=parent.id,
            attempt_id=parent_attempt.id,
            project_id=self.project.id,
            root_task_id=parent.id,
            depth=1,
        )
        sibling, _ = await self.admit(self.task())
        async with self.pool.acquire() as conn:
            self.assertEqual(
                (await store.get_task(conn, child.id, principal, manage=True)).id,
                child.id,
            )
            with self.assertRaises(store.TaskError):
                await store.get_task(conn, sibling.id, principal)
            with self.assertRaises(store.TaskError):
                await store.get_task(conn, parent.id, principal, manage=True)
        async with self.pool.acquire() as conn, conn.transaction():
            request = TaskCreate(
                request_id="escape",
                title="t",
                brief="b",
                mode="coordination",
                project_id=self.project.id,
                provider_profile_id="codex",
            )
            with self.assertRaises(store.TaskError) as caught:
                await mutate(conn, principal, "create", request)
            self.assertEqual(caught.exception.code, "inherited_provider_constraint")
            # Simulate confirmed source fencing; its retained bearer cannot read/manage.
            await conn.execute(
                "UPDATE task_attempts SET state='fenced' WHERE id=$1", parent_attempt.id
            )
            with self.assertRaises(store.TaskError):
                await store.get_task(conn, child.id, principal)

    async def test_artifact_immutability_bounds_and_owner_scope(self):
        async with self.pool.acquire() as conn, conn.transaction():
            await store.admission_lock(conn)
            op, _ = await store.begin_operation(
                conn, self.principal, "artifact-operation", "create", {}
            )
            artifact = await store.add_artifact(
                conn, op.id, "handoff_manifest", {"head": "a" * 40}
            )
            self.assertEqual(
                artifact,
                await store.add_artifact(
                    conn, op.id, "handoff_manifest", {"head": "a" * 40}
                ),
            )
            with self.assertRaises(store.TaskError):
                await store.add_artifact(
                    conn, op.id, "handoff_manifest", {"head": "b" * 40}
                )
            with self.assertRaises(store.TaskError):
                await store.add_artifact(
                    conn, op.id, "unverified_provider_summary", {"summary": "é" * 4096}
                )
        async with self.pool.acquire() as conn:
            self.assertEqual(
                (await store.get_artifact(conn, artifact, self.principal))["payload"][
                    "head"
                ],
                "a" * 40,
            )
            with self.assertRaises(store.TaskError):
                await store.get_artifact(conn, artifact, TaskPrincipal("other"))
            with self.assertRaises(asyncpg.RaiseError):
                await conn.execute("DELETE FROM task_artifacts WHERE id=$1", artifact)
            with self.assertRaises(store.TaskError):
                await store.reserve_writer(
                    conn,
                    owner_id=self.user,
                    repository="example/app",
                    branch="feature/unlocked",
                    binding_id="unlocked",
                )

    async def test_connected_fake_same_request_reserves_one_attempt_and_writer(self):
        from mainloop.providers import (
            TASK_CODE_CAPABILITIES,
            TASK_REQUIRED_CAPABILITIES,
            qualify_task_profile,
        )
        from mainloop.tasks.service import TaskPorts

        from models import CapabilityResult

        test = self

        class FixtureProvisioner:
            calls = 0

            async def create(self, conn, principal, request, operation):
                self.calls += 1
                profile = (
                    registry()
                    .resolve("claude", "supervisor")
                    .model_copy(
                        update={
                            "capabilities": tuple(
                                CapabilityResult(
                                    capability=name,
                                    state="proved",
                                    scope="fixture",
                                    evidence_ref="fixture:persistence-only",
                                )
                                for name in sorted(
                                    TASK_REQUIRED_CAPABILITIES | TASK_CODE_CAPABILITIES
                                )
                            )
                        }
                    )
                )
                qualify_task_profile(profile, "supervisor", "code", allow_fixture=True)
                task = test.task(branch=request.checkout.branch)
                await store.insert_task(conn, task)
                task, attempt = await store.admit_attempt(
                    conn, task, profile, role="supervisor", depth=1
                )
                operation = operation.model_copy(
                    update={
                        "task_id": task.id,
                        "attempt_id": attempt.id,
                        "state": "target_creating",
                    }
                )
                await store.save_operation(conn, operation)
                return operation

        provisioner = FixtureProvisioner()
        installed = TaskPorts(provisioning=provisioner)
        request = TaskCreate(
            request_id="connected",
            title="t",
            brief="b",
            mode="code",
            project_id=self.project.id,
            checkout=TaskCheckout(branch="feature/connected"),
        )

        async def submit(body):
            async with self.pool.acquire() as conn, conn.transaction():
                return await mutate(
                    conn, self.principal, "create", body, installed_ports=installed
                )

        first, second = await asyncio.gather(submit(request), submit(request))
        self.assertEqual(first.id, second.id)
        self.assertEqual(first.attempt_id, second.attempt_id)
        self.assertEqual(first.request_payload, request.model_dump(mode="json"))
        self.assertEqual(provisioner.calls, 1)
        for query in (
            "SELECT count(*) FROM tasks",
            "SELECT count(*) FROM task_attempts",
            "SELECT count(*) FROM workspace_writer_claims",
            "SELECT count(*) FROM task_events",
        ):
            self.assertEqual(await self.pool.fetchval(query), 1)
        with self.assertRaises(store.TaskError):
            await submit(request.model_copy(update={"brief": "changed"}))
        self.assertEqual(provisioner.calls, 1)

    async def test_rest_actions_return_scoped_404_for_missing_and_foreign_tasks(self):
        other_owner = f"foreign-{uuid.uuid4().hex}"
        other_project = await db.get_or_create_project(
            other_owner, parse_github_repo("foreign/app")
        )
        foreign_task = self.task(owner=other_owner, project=other_project.id)
        async with self.pool.acquire() as conn, conn.transaction():
            await store.insert_task(conn, foreign_task)
        api.app.dependency_overrides[current_user] = lambda: self.user
        try:
            with patch.object(settings, "api_hosts", "localhost"):
                async with httpx.AsyncClient(
                    transport=httpx.ASGITransport(app=api.app),
                    base_url="http://localhost",
                ) as client:
                    for action in ("cancel", "retry", "reassign"):
                        for target in ("nonexistent-task", foreign_task.id):
                            body = {
                                "request_id": f"{action}-{target}",
                                "expected_version": 1,
                                "expected_attempt_id": None,
                            }
                            if action == "reassign":
                                body["target_profile_id"] = "codex"
                            with self.subTest(action=action, target=target):
                                response = await client.post(
                                    f"/tasks/{target}/{action}", json=body
                                )
                                self.assertEqual(
                                    response.status_code, 404, response.text
                                )
                                self.assertEqual(
                                    response.json(),
                                    {"detail": {"reason": "task_not_found"}},
                                )
                    self.assertEqual(
                        await self.pool.fetchval(
                            "SELECT count(*) FROM task_operations"
                        ),
                        0,
                    )
        finally:
            api.app.dependency_overrides.pop(current_user)

    async def test_rest_action_replays_preserve_identity_after_version_change(self):
        task, attempt = await self.admit(self.task())
        api.app.dependency_overrides[current_user] = lambda: self.user
        try:
            with patch.object(settings, "api_hosts", "localhost"):
                async with httpx.AsyncClient(
                    transport=httpx.ASGITransport(app=api.app),
                    base_url="http://localhost",
                ) as client:
                    for action in ("cancel", "retry", "reassign"):
                        body = {
                            "request_id": f"replay-{action}",
                            "expected_version": task.version,
                            "expected_attempt_id": attempt.id,
                        }
                        if action == "reassign":
                            body["target_profile_id"] = "codex"
                        first = await client.post(
                            f"/tasks/{task.id}/{action}", json=body
                        )
                        self.assertEqual(first.status_code, 202, first.text)
                        async with self.pool.acquire() as conn, conn.transaction():
                            updated = task.model_copy(
                                update={"version": task.version + 1}
                            )
                            await store.save_task(
                                conn, updated, task.version, f"changed-{action}"
                            )
                            task = updated
                        replay = await client.post(
                            f"/tasks/{task.id}/{action}", json=body
                        )
                        self.assertEqual(replay.status_code, 202, replay.text)
                        self.assertEqual(replay.json(), first.json())
                        changed = {**body, "expected_version": task.version}
                        conflict = await client.post(
                            f"/tasks/{task.id}/{action}", json=changed
                        )
                        self.assertEqual(conflict.status_code, 409, conflict.text)
        finally:
            api.app.dependency_overrides.pop(current_user)
