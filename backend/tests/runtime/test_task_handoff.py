"""Offline evidence regressions; these are never live qualification."""

import unittest
from datetime import UTC, datetime, timedelta

from mainloop.db.tasks import TaskError
from mainloop.tasks.checkpoint import verify_checkpoint
from mainloop.tasks.handoff import STEPS, advance, require_fence
from pydantic import ValidationError

from models.task import TaskOperation
from models.task_handoff import CheckpointEvidence, SourceFenceEvidence


class HandoffEvidenceTests(unittest.TestCase):
    def checkpoint(self, **changes):
        return CheckpointEvidence.model_validate(
            {
                "repository": "example/app",
                "branch": "feature/task",
                "remote_sha": "a" * 40,
                "committed_checkpoint": True,
                "operation_id": "operation",
                "attempt_id": "source",
                "session_id": "source-session",
                "binding_id": "binding",
                "runtime_identity": "native-source",
                "writer_generation": 3,
                "qualification": "offline_fake",
                "provenance": "fake:remote-reader",
                "git_dispatch": "settled",
                "merge_dispatch": "settled",
                "evidence_ref": "fake:checkpoint",
                "observed_at": datetime.now(UTC),
                **changes,
            }
        )

    def test_checkpoint_requires_exact_preserved_remote_commit(self):
        self.assertEqual(
            verify_checkpoint(
                self.checkpoint(), repository="example/app", branch="feature/task"
            ),
            "a" * 40,
        )
        for changes in (
            {"committed_checkpoint": False},
            {"git_dispatch": "unknown"},
            {"merge_dispatch": "unknown"},
            {"repository": "other/app"},
            {"branch": "feature/sibling"},
        ):
            with self.subTest(changes=changes), self.assertRaises(TaskError):
                verify_checkpoint(
                    self.checkpoint(**changes),
                    repository="example/app",
                    branch="feature/task",
                )

    def test_remote_sha_and_ref_validation(self):
        for changes in ({"remote_sha": "HEAD"}, {"branch": "feature/task~1"}):
            with self.assertRaises(ValidationError):
                self.checkpoint(**changes)

    def fence(self, **changes):
        return SourceFenceEvidence.model_validate(
            {
                "attempt_id": "source",
                "session_id": "source-session",
                "binding_id": "binding",
                "writer_generation": 3,
                "provenance": "fake:runtime",
                "observed_at": datetime.now(UTC),
                "runtime_identity": "native-source",
                "operation_id": "operation",
                "native_dispatch_settled": True,
                "git_dispatch_settled": True,
                "merge_dispatch_settled": True,
                "credentials_revoked": True,
                "runtime_quiescent": True,
                "preview_closed": True,
                "children_drained": True,
                "pending_hitl_resolved": True,
                "qualification": "offline_fake",
                "evidence_ref": "fake:fence",
                **changes,
            }
        )

    def require(self, evidence, live=False):
        require_fence(
            evidence,
            operation_id="operation",
            attempt_id="source",
            binding_id="binding",
            generation=3,
            live=live,
        )

    def test_fake_cannot_qualify_live_and_every_fence_fact_is_required(self):
        self.require(self.fence())
        with self.assertRaises(TaskError):
            self.require(self.fence(), live=True)
        for field in (
            "native_dispatch_settled",
            "git_dispatch_settled",
            "merge_dispatch_settled",
            "credentials_revoked",
            "runtime_quiescent",
            "preview_closed",
            "children_drained",
            "pending_hitl_resolved",
        ):
            with self.subTest(field=field), self.assertRaises(TaskError):
                self.require(self.fence(**{field: False}))
        for changes in (
            {"writer_generation": 2},
            {"attempt_id": "old"},
            {"binding_id": "sibling"},
            {"operation_id": "another"},
            {"qualification": "unsupported"},
        ):
            with self.assertRaises(TaskError):
                self.require(self.fence(**changes))

    def test_restart_uses_last_confirmed_step_and_cannot_skip_fencing(self):
        now = datetime.now(UTC)
        operation = TaskOperation(
            id="operation",
            owner_id="owner",
            principal_key="owner",
            request_id="request",
            request_digest="digest",
            kind="retry",
            created_at=now,
            updated_at=now,
        )
        with self.assertRaises(TaskError):
            advance(operation, "target_creating")
        for step in STEPS[1:]:
            uncertain = operation.model_copy(update={"state": "uncertain"})
            operation = advance(uncertain, step)
            self.assertEqual(operation.last_confirmed_step, step)
            self.assertEqual(advance(operation, step), operation)


class CoordinatorTests(unittest.IsolatedAsyncioTestCase):
    """Execute the actual coordinator with transaction/runtime fakes, no native calls."""

    async def exercise(
        self,
        source_provider,
        target_provider,
        *,
        crash=None,
        no_start=False,
        invalid_target=False,
        target_absent=False,
    ):
        import copy
        from contextlib import ExitStack, asynccontextmanager
        from unittest.mock import AsyncMock, patch

        from mainloop.providers import registry
        from mainloop.tasks import handoff as module

        from models.task import Task, TaskAttempt, TaskCheckout
        from models.task_handoff import AdapterCapabilities, SuccessorResult
        from models.workspace import WorkspaceEnvironment

        now = datetime.now(UTC)
        environment = WorkspaceEnvironment(
            environment_id="env",
            version_id="v1",
            image="example/env@sha256:" + "b" * 64,
            platform="linux/amd64",
            policy_identity="policy",
        )
        profile = registry().resolve(target_provider, "supervisor")
        source_profile = registry().resolve(source_provider, "supervisor")
        task = Task(
            id="task",
            owner_id="owner",
            root_task_id="task",
            title="Task",
            brief="Preserve caller instructions",
            mode="code",
            project_id="project",
            assigned_profile_id=source_provider,
            selection_source="explicit",
            accepted_environment=environment,
            checkout=TaskCheckout(branch="feature/task", ref="a" * 40),
            current_attempt_id=None if no_start else "source",
            created_at=now,
            updated_at=now,
        )
        source = TaskAttempt(
            id="source",
            task_id="task",
            number=1,
            profile_id=source_provider,
            native_provider=source_profile.native_provider,
            configuration_revision=source_profile.configuration_revision,
            agent_ref=source_profile.agents["supervisor"],
            role="supervisor",
            depth=1,
            session_id="source-session",
            binding_id="source-session",
            workspace_id="source-session",
            writer_generation=3,
            state="failed" if no_start else "active",
            environment=environment,
            initial_ref="a" * 40,
            evidence_refs=(module.lifecycle.CREATE_REJECTED,) if no_start else (),
            created_at=now,
            updated_at=now,
        )
        operation = TaskOperation(
            id="operation",
            owner_id="owner",
            principal_key="owner",
            request_id="request",
            request_digest="digest",
            kind="retry" if source_provider == target_provider else "reassign",
            task_id="task",
            source_attempt_id="source",
            created_at=now,
            updated_at=now,
            request_payload={
                "_s3": {
                    "profile": profile.model_dump(mode="json"),
                    "expected_version": 1,
                    "source_runtime": (
                        "no-start:source" if no_start else "native-source"
                    ),
                    "no_start": no_start,
                }
            },
        )
        state = {
            "task": task,
            "source": source,
            "operation": operation,
            "artifacts": {},
            "target": None,
        }
        effects = {}
        crash_used = False
        writer_grants = {"source": not no_start}

        @asynccontextmanager
        async def lock(*args, **kwargs):
            yield

        class Conn:
            @asynccontextmanager
            async def transaction(self):
                previous = copy.deepcopy(state)
                try:
                    yield
                except BaseException:
                    state.clear()
                    state.update(previous)
                    raise

            async def fetch(self, query, *args):
                return []

            async def fetchval(self, query, *args):
                if "task_artifacts" in query:
                    return state["artifacts"][args[0]]
                if "kagent_session_id" in query:
                    return (
                        "native-target"
                        if args[0] == "target-session"
                        else "native-source"
                    )
                return True

        conn = Conn()

        class Database:
            @asynccontextmanager
            async def connection(self):
                yield conn

        async def load_task(*args, **kwargs):
            return state["task"]

        async def load_attempt(conn, attempt_id, **kwargs):
            return state[attempt_id]

        async def save(conn, previous, updated):
            if state["operation"] != previous:
                raise TaskError(409, "stale_handoff_operation")
            state["task"] = state["task"].model_copy(
                update={"version": state["task"].version + 1}
            )
            metadata = updated.request_payload.get("_s3")
            if metadata is not None:
                updated = updated.model_copy(
                    update={
                        "request_payload": {
                            **updated.request_payload,
                            "_s3": {
                                **metadata,
                                "expected_version": state["task"].version,
                            },
                        }
                    }
                )
            state["operation"] = updated

        async def artifact(conn, operation_id, kind, payload):
            import json

            content = json.dumps(payload, sort_keys=True)
            if kind in state["artifacts"] and state["artifacts"][kind] != content:
                raise TaskError(409, "artifact_payload_conflict")
            state["artifacts"][kind] = content
            return kind

        async def drain(conn, source):
            writer_grants["source"] = False
            if not no_start:
                state["source"] = source.model_copy(update={"state": "draining"})

        async def settle(conn, source, evidence):
            self.assertFalse(writer_grants["source"])
            state["source"] = source.model_copy(
                update={"state": "superseded", "superseded_at": now}
            )
            state["task"] = state["task"].model_copy(
                update={"current_attempt_id": None, "version": 2}
            )

        async def enroll(conn, task, source, profile, operation, checkpoint):
            self.assertEqual(state["source"].state, "superseded")
            self.assertEqual(checkpoint.remote_sha, "a" * 40)
            self.assertEqual(task.accepted_environment, environment)
            self.assertIsNone(task.current_attempt_id)
            target = source.model_copy(
                update={
                    "id": "target",
                    "number": 2,
                    "state": "creating",
                    "profile_id": profile.id,
                    "native_provider": profile.native_provider,
                    "session_id": "target-session",
                    "binding_id": "target-session",
                    "workspace_id": "target-session",
                    "writer_generation": 4,
                    "predecessor_id": "source",
                    "superseded_at": None,
                }
            )
            state["target"] = target
            state["task"] = task.model_copy(
                update={
                    "current_attempt_id": "target",
                    "assigned_profile_id": profile.id,
                    "checkout": task.checkout.model_copy(
                        update={"ref": checkpoint.remote_sha}
                    ),
                    "version": 3,
                }
            )
            return state["task"], target

        async def activate(conn, task, target, operation):
            self.assertTrue(writer_grants["target"])
            state["target"] = target.model_copy(
                update={"state": "active", "brief_delivery_id": "first-brief"}
            )

        async def external(kind, action_id, produce):
            nonlocal crash_used
            if action_id not in effects:
                effects[action_id] = produce()
            if crash == kind and not crash_used:
                crash_used = True
                raise RuntimeError("lost fake reply after committed external effect")
            return effects[action_id]

        def scope(attempt, runtime):
            return dict(
                operation_id="operation",
                attempt_id=attempt.id,
                session_id=attempt.session_id,
                binding_id=attempt.binding_id,
                writer_generation=attempt.writer_generation,
                runtime_identity=runtime,
                qualification="offline_fake",
                provenance="fake:adapter",
                observed_at=now,
            )

        class Reader:
            async def read(self, conn, task, source, operation, action_id):
                return await external(
                    "checkpoint",
                    action_id,
                    lambda: CheckpointEvidence(
                        **scope(
                            source, "no-start:source" if no_start else "native-source"
                        ),
                        repository="example/app",
                        branch="feature/task",
                        remote_sha="a" * 40,
                        committed_checkpoint=True,
                        no_start_initial_ref="a" * 40 if no_start else None,
                        git_dispatch="settled",
                        merge_dispatch="settled",
                        evidence_ref="fake:checkpoint",
                    ),
                )

        class Runtime:
            capabilities = AdapterCapabilities(
                qualification="offline_fake",
                provenance="fake:capabilities",
                source_fence=True,
                preview_closure=True,
                exact_checkout=True,
                reconcile_original_create=True,
            )

            async def fence(self, conn, task, source, operation, checkpoint, action_id):
                return await external(
                    "fence",
                    action_id,
                    lambda: SourceFenceEvidence(
                        **scope(
                            source, "no-start:source" if no_start else "native-source"
                        ),
                        native_dispatch_settled=True,
                        git_dispatch_settled=True,
                        merge_dispatch_settled=True,
                        credentials_revoked=True,
                        runtime_quiescent=True,
                        preview_closed=True,
                        children_drained=True,
                        pending_hitl_resolved=True,
                        evidence_ref="fake:fence",
                    ),
                )

            async def prepare_target(
                self, conn, task, target, operation, checkpoint, action_id
            ):
                return await external(
                    "create",
                    action_id,
                    lambda: SuccessorResult(
                        **scope(target, "native-target"),
                        outcome="absent" if target_absent else "ready",
                        checkpoint_sha=("b" if invalid_target else "a") * 40,
                        environment=environment,
                        repository="example/app",
                        branch="feature/task",
                        checkout_verified=True,
                        environment_verified=True,
                        evidence_ref="fake:ready",
                    ),
                )

            async def admit_target(
                self, conn, task, target, operation, checkpoint, action_id
            ):
                writer_grants["target"] = True
                prepared = await self.prepare_target(
                    conn, task, target, operation, checkpoint, f"{operation.id}:create"
                )
                return await external(
                    "grant",
                    action_id,
                    lambda: prepared.model_copy(
                        update={
                            "grant_confirmed": True,
                            "grant_ref": "fake:target-own-grant",
                        }
                    ),
                )

            async def finish_target(self, conn, task, target, operation, action_id):
                return await external("admit", action_id, lambda: True)

        with ExitStack() as stack:
            for target, replacement in (
                (
                    "store.handoff_operation",
                    AsyncMock(side_effect=lambda *a, **k: state["operation"]),
                ),
                ("store.handoff_save", save),
                ("store.handoff_pending", AsyncMock(return_value=False)),
                ("store.handoff_children_drained", AsyncMock(return_value=True)),
                ("store.admission_lock", AsyncMock()),
                ("store.project", AsyncMock(return_value={"full_name": "example/app"})),
                ("store.add_artifact", artifact),
                ("lifecycle.load_task", load_task),
                ("lifecycle.load_attempt", load_attempt),
                ("lifecycle.drain_handoff", drain),
                ("lifecycle.supersede_handoff", settle),
                ("lifecycle.locked", lock),
                ("push_lifecycle.locked", lock),
                ("provisioning.enroll_successor", enroll),
                ("provisioning.activate_successor", activate),
                ("provisioning.admit_successor", AsyncMock()),
            ):
                stack.enter_context(
                    patch("mainloop.tasks.handoff." + target, replacement)
                )
            coordinator = module.Handoff(Runtime(), Reader(), live=False)
            for _ in range(12):
                try:
                    await coordinator.reconcile(Database(), state["operation"])
                except RuntimeError:
                    pass
                if state["operation"].state in ("completed", "blocked"):
                    break
        if invalid_target or target_absent:
            self.assertEqual(state["operation"].state, "blocked")
            self.assertEqual(state["target"].state, "creating")
            self.assertFalse(writer_grants["source"])
            self.assertNotIn("target", writer_grants)
            return
        self.assertEqual(state["operation"].state, "completed")
        self.assertEqual(state["target"].profile_id, target_provider)
        self.assertEqual(state["target"].predecessor_id, "source")
        self.assertEqual(state["target"].writer_generation, 4)
        self.assertFalse(writer_grants["source"])
        self.assertEqual(len(effects), 5)
        if crash:
            self.assertTrue(crash_used)
        for action in ("submit", "resume", "preview", "create"):
            self.assertIsNotNone(
                module.lifecycle.denial({"state": "superseded"}, action)
            )

    async def test_both_directions_and_same_provider(self):
        for source, target in (
            ("claude", "codex"),
            ("codex", "claude"),
            ("claude", "claude"),
        ):
            with self.subTest(source=source, target=target):
                await self.exercise(source, target)

    async def test_each_external_lost_reply_reconciles_original_action(self):
        for step in ("checkpoint", "fence", "create", "grant", "admit"):
            with self.subTest(step=step):
                await self.exercise("codex", "claude", crash=step)

    async def test_definite_no_start_retry_uses_last_attempt(self):
        await self.exercise("codex", "codex", no_start=True)

    async def test_start_rejects_concurrent_operation_before_effects(self):
        from types import SimpleNamespace
        from unittest.mock import AsyncMock, patch

        from mainloop.tasks.handoff import Handoff

        task = SimpleNamespace(
            id="task",
            version=1,
            current_attempt_id="source",
            mode="code",
            status="running",
        )
        request = SimpleNamespace(expected_version=1, expected_attempt_id="source")
        operation = SimpleNamespace(id="operation", kind="retry")
        conn = SimpleNamespace(is_in_transaction=lambda: True)
        with patch(
            "mainloop.tasks.handoff.store.get_task", AsyncMock(return_value=task)
        ), patch(
            "mainloop.tasks.handoff.store.handoff_pending", AsyncMock(return_value=True)
        ), self.assertRaises(
            TaskError
        ) as raised:
            await Handoff().start(conn, SimpleNamespace(), task, request, operation)
        self.assertEqual(raised.exception.code, "handoff_in_progress")

    def test_live_defaults_are_unqualified(self):
        from mainloop.tasks.handoff import Handoff

        self.assertFalse(Handoff().qualified())

    async def test_wrong_checkout_and_target_absence_never_admit_or_revive_source(self):
        await self.exercise("claude", "codex", invalid_target=True)
        await self.exercise("claude", "codex", target_absent=True)


class ScopedEvidenceTests(unittest.TestCase):
    checkpoint = HandoffEvidenceTests.checkpoint

    def test_stale_and_wrong_native_identity_are_rejected(self):
        from datetime import timedelta
        from types import SimpleNamespace

        from mainloop.tasks.checkpoint import verify_scope

        now = datetime.now(UTC)
        attempt = SimpleNamespace(
            id="source",
            session_id="source-session",
            binding_id="binding",
            writer_generation=3,
        )
        operation = SimpleNamespace(id="operation")
        for changes in (
            {"runtime_identity": "other-native"},
            {"session_id": "sibling"},
            {"observed_at": now - timedelta(seconds=61)},
            {"observed_at": now + timedelta(seconds=1)},
        ):
            with self.subTest(changes=changes), self.assertRaises(TaskError):
                verify_scope(
                    self.checkpoint(**changes),
                    operation=operation,
                    attempt=attempt,
                    runtime_identity="native-source",
                    now=now,
                    live=False,
                )


class FixedOriginReaderTests(unittest.IsolatedAsyncioTestCase):
    async def test_redirect_is_rejected_and_only_fixed_origin_is_requested(self):
        from types import SimpleNamespace
        from unittest.mock import AsyncMock, patch

        import httpx
        from mainloop.services.github_creation import GitHubCreationClient, GitHubError
        from mainloop.tasks.checkpoint import FixedOriginCheckpointReader

        evidence = HandoffEvidenceTests().checkpoint()
        observer = SimpleNamespace(checkpoint=AsyncMock(return_value=evidence))
        requests = []

        def respond(request):
            requests.append(request)
            return httpx.Response(
                302, headers={"Location": "https://untrusted.invalid/checkpoint"}
            )

        transport = httpx.MockTransport(respond)
        reader = FixedOriginCheckpointReader(
            observer, client_factory=lambda: GitHubCreationClient(transport=transport)
        )
        with patch(
            "mainloop.tasks.checkpoint.store.project",
            AsyncMock(
                return_value={
                    "full_name": "example/app",
                    "html_url": "https://github.com/example/app",
                }
            ),
        ), patch(
            "mainloop.services.github_creation.settings.github_token",
            "sanitized-test-token",
        ), self.assertRaises(
            GitHubError
        ):
            await reader.read(
                None,
                SimpleNamespace(
                    project_id="project",
                    owner_id="owner",
                    checkout=SimpleNamespace(branch="feature/task"),
                ),
                None,
                None,
                "operation:checkpoint",
            )
        self.assertEqual(len(requests), 1)
        self.assertEqual(requests[0].url.host, "api.github.com")


class ComposedCheckpointFreshnessTests(unittest.IsolatedAsyncioTestCase):
    async def test_matching_remote_sha_never_refreshes_native_observation(self):
        from types import SimpleNamespace
        from unittest.mock import AsyncMock, patch

        from mainloop.tasks.checkpoint import FixedOriginCheckpointReader, verify_scope

        task = SimpleNamespace(
            project_id="project",
            owner_id="owner",
            checkout=SimpleNamespace(branch="feature/task"),
        )
        for seconds in (-61, 30, 0):
            with self.subTest(offset=seconds):
                value = HandoffEvidenceTests().checkpoint(
                    observed_at=datetime.now(UTC) + timedelta(seconds=seconds)
                )
                attempt = SimpleNamespace(
                    id=value.attempt_id,
                    session_id=value.session_id,
                    binding_id=value.binding_id,
                    writer_generation=value.writer_generation,
                )
                operation = SimpleNamespace(id=value.operation_id)
                client = AsyncMock()
                client.__aenter__.return_value = client
                client.repo.return_value = SimpleNamespace(
                    full_name="example/app", default_branch="main"
                )
                client.branch.return_value = SimpleNamespace(
                    name="feature/task", commit=SimpleNamespace(sha=value.remote_sha)
                )
                reader = FixedOriginCheckpointReader(
                    SimpleNamespace(checkpoint=AsyncMock(return_value=value)),
                    client_factory=lambda client=client: client,
                )
                with patch(
                    "mainloop.tasks.checkpoint.store.project",
                    AsyncMock(
                        return_value={
                            "full_name": "example/app",
                            "html_url": "https://github.com/example/app",
                        }
                    ),
                ):
                    if seconds:
                        with self.assertRaises(TaskError) as raised:
                            result = await reader.read(
                                None, task, attempt, operation, "checkpoint"
                            )
                            verify_scope(
                                result,
                                operation=operation,
                                attempt=attempt,
                                runtime_identity=value.runtime_identity,
                                now=datetime.now(UTC),
                                live=False,
                            )
                        self.assertEqual(raised.exception.code, "evidence_stale")
                    else:
                        result = await reader.read(
                            None, task, attempt, operation, "checkpoint"
                        )
                        self.assertEqual(result.observed_at, value.observed_at)
                client.branch.assert_awaited_once()
