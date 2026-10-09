"""Committed tree verification; actor clean status is explicitly untrusted."""

from datetime import datetime, timedelta
from typing import Protocol

from mainloop.db import tasks as store

from models.task_handoff import CheckpointEvidence, ContinuationManifest, EvidenceScope


class RemoteCheckpointReader(Protocol):
    """Fixed trusted origin, redirects disabled; persisted identity only.

    Reader must reconcile the original checkpoint action, never dispatch a second
    uncertain checkpoint turn or push. Runtime/model assertions are not remote proof.
    """

    async def read(
        self, conn, task, attempt, operation, action_id: str
    ) -> CheckpointEvidence: ...


def verify_scope(
    value: EvidenceScope,
    *,
    operation,
    attempt,
    runtime_identity: str,
    now: datetime,
    live: bool,
    max_age_seconds: int = 60,
) -> None:
    actual = (
        value.operation_id,
        value.attempt_id,
        value.session_id,
        value.binding_id,
        value.runtime_identity,
        value.writer_generation,
    )
    expected = (
        operation.id,
        attempt.id,
        attempt.session_id,
        attempt.binding_id,
        runtime_identity,
        attempt.writer_generation,
    )
    if actual != expected:
        raise store.TaskError(409, "evidence_identity_mismatch")
    if value.qualification != ("qualified_live" if live else "offline_fake"):
        raise store.TaskError(409, "evidence_unqualified")
    age = now - value.observed_at
    if age < timedelta(0) or age > timedelta(seconds=max_age_seconds):
        raise store.TaskError(409, "evidence_stale")


def verify_checkpoint(
    value: CheckpointEvidence, *, repository: str, branch: str
) -> str:
    if (value.repository, value.branch) != (repository, branch):
        raise store.TaskError(409, "checkpoint_identity_mismatch")
    if value.git_dispatch != "settled" or value.merge_dispatch != "settled":
        raise store.TaskError(409, "checkpoint_dispatch_uncertain")
    if not value.committed_checkpoint:
        raise store.TaskError(409, "checkpoint_required")
    return value.remote_sha


async def persist_manifest(conn, value: ContinuationManifest) -> str:
    return await store.add_artifact(
        conn, value.operation_id, "handoff_manifest", value.model_dump(mode="json")
    )


class FixedOriginCheckpointReader:
    """Compose a configured native checkpoint observer with existing bounded GitHub reads.

    No client is constructed until read(), so source preparation has no credentials
    or live effects. GitHubCreationClient disables redirects and environment proxies.
    """

    def __init__(self, observer, *, client_factory=None):
        self.observer = observer
        self.client_factory = client_factory

    async def read(self, conn, task, attempt, operation, action_id):
        from mainloop.services.github_creation import GitHubCreationClient
        from mainloop.services.github_repo import parse_github_repo

        project = await store.project(conn, task.project_id, task.owner_id)
        repository = parse_github_repo(project["full_name"]).full_name.lower()
        if parse_github_repo(project["html_url"]).full_name.lower() != repository:
            raise store.TaskError(409, "project_repository_mismatch")
        value = await self.observer.checkpoint(
            conn, task, attempt, operation, action_id
        )
        verify_checkpoint(value, repository=repository, branch=task.checkout.branch)
        factory = self.client_factory or GitHubCreationClient
        async with factory(repository) as client:
            remote_repo = await client.repo(repository)
            if (
                remote_repo.full_name.lower() != repository
                or remote_repo.default_branch == task.checkout.branch
            ):
                raise store.TaskError(409, "checkpoint_repository_mismatch")
            if value.no_start_initial_ref is not None:
                from urllib.parse import quote

                from mainloop.services.github_creation import Commit

                commit = Commit.model_validate(
                    await client._request(
                        "GET",
                        f"/repos/{repository}/commits/{quote(value.no_start_initial_ref, safe='')}",
                    )
                )
                remote_sha = commit.sha
            else:
                branch = await client.branch(repository, task.checkout.branch)
                if branch.name != task.checkout.branch:
                    raise store.TaskError(409, "checkpoint_branch_mismatch")
                remote_sha = branch.commit.sha
            if remote_sha != value.remote_sha:
                raise store.TaskError(409, "checkpoint_not_pushed")
        # A remote SHA read does not refresh native Git/merge observations.
        verify_scope(
            value,
            operation=operation,
            attempt=attempt,
            runtime_identity=value.runtime_identity,
            now=datetime.now().astimezone(),
            live=value.qualification == "qualified_live",
        )
        return value
