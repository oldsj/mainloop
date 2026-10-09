"""Branch workspaces on kagent Sessions.

A workspace is a native agent session whose kagent Session was created with a ``workspace``
(repository, ref, branch, depth), so the repository is in the harness before the first turn. This
module owns what is specific to that: creation (and the automatic retry of a create kagent did
not confirm), the lifecycle read from kagent, suspend and resume, the preview idle-out, and
deletion. Turn delivery stays in ``native_sessions``.

Suspend and resume are kagent's ``SuspendSession`` and ``ResumeSession``. Mainloop keeps no copy
of the lifecycle state: it is read from ``GetSession`` each time, so it cannot drift.

Idle-out is Mainloop's job. With ``snapshotPolicy.onQuiesce: Full`` the router wakes a suspended
actor on a preview request without telling kagent and nothing puts it back to sleep, so a preview
resumes the Session through kagent first (``wake_for_preview``), and the idle check suspends
the Session once there has been no preview traffic, resume or turn for the workspace's idle
timeout. Suspending takes the
same in-process lock as ``submit_message`` and ``_deliver`` and refuses while a turn is open or
queued. The MCP container is a second writer outside that lock, so a message recorded during a
suspend is possible; a turn that arrives during or after a suspend finds the Session suspended
and resumes it before sending.
"""

from __future__ import annotations

import asyncio
import json
import logging
import uuid
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta

from mainloop.config import settings
from mainloop.db import db
from mainloop.push_gate import lifecycle as push_lifecycle
from mainloop.runtime import native_sessions as ns
from mainloop.runtime.kagent_client import (
    KagentError,
    KagentSession,
    OutcomeUnknown,
    RuntimeOperation,
    RuntimeState,
    ServiceConfigurationError,
    SessionError,
    Unreachable,
)
from mainloop.services import github_checkout
from mainloop.services.github_repo import (
    GithubRepo,
    InvalidGithubRepo,
    parse_github_repo,
)
from mainloop.sse import notify_workspace_updated
from mainloop.tasks import lifecycle

from models import (
    WorkspaceDev,
    WorkspaceLifecycle,
    WorkspaceManifest,
    WorkspaceObservedState,
    WorkspacePort,
)

logger = logging.getLogger(__name__)

# CreateSession statuses that mean kagent rejected the request itself (bad workspace, an origin
# the harness does not allow, no permission) rather than failed to answer.
_REJECTED = (3, 7, 16)
# CreateSession statuses that retrying the same request cannot fix: the rejections above, plus
# NOT_FOUND (kagent deleted the Session it had reserved for this request id, so the id is used
# up), ALREADY_EXISTS (the request id is bound to a different create) and UNIMPLEMENTED.
# Anything else (UNAVAILABLE while the environment snapshot is prepared, FAILED_PRECONDITION
# while no prepared revision is ready, a deadline, an unreachable gateway, an unobserved reply)
# is retried automatically.
_PERMANENT = (*_REJECTED, 5, 6, 12)
_GRPC_NAMES = {
    2: "unknown error",
    3: "invalid argument",
    4: "deadline exceeded",
    5: "not found",
    6: "already exists",
    7: "permission denied",
    8: "resource exhausted",
    9: "failed precondition",
    10: "aborted",
    12: "unimplemented",
    13: "internal error",
    14: "unavailable",
    16: "unauthenticated",
}


def _create_reason(exc: Exception) -> str:
    """Return a classified, owner-safe reason for an unconfirmed create (raw text is logged)."""
    if isinstance(exc, SessionError):
        if exc.grpc_status in _GRPC_NAMES:
            return f"kagent {_GRPC_NAMES[exc.grpc_status]} (gRPC {exc.grpc_status})"
        return "kagent Session error"
    if isinstance(exc, ServiceConfigurationError):
        return "kagent refused Mainloop's configuration"
    if isinstance(exc, Unreachable):
        return "kagent unreachable"
    if isinstance(exc, OutcomeUnknown):
        return "kagent reply not observed"
    if isinstance(exc, KagentError):
        return "kagent error"
    if isinstance(exc, WorkspaceConflict):
        return "the workspace's task attempt is not live"
    return f"Mainloop error ({type(exc).__name__})"


_SELECT = """
SELECT w.session_id, w.repo, w.ref, w.branch, w.depth, w.ports, w.idle_timeout_minutes,
       w.last_active_at, w.idle_suspended_at, w.created_at, w.development_environment,
       w.create_attempts, w.create_error, w.create_stopped,
       s.user_id, s.conversation_id, n.kind, n.kagent_session_id,
       GREATEST(w.last_active_at, w.created_at,
                (SELECT max(d.updated_at) FROM native_deliveries d
                 WHERE d.session_id=w.session_id)) AS last_activity_at
FROM workspaces w
JOIN sessions s ON s.id=w.session_id
JOIN native_bindings n ON n.session_id=w.session_id
"""


class WorkspaceNotFound(LookupError):
    pass


class WorkspaceConflict(Exception):
    """The workspace is busy or in a state that refuses the operation."""


class WorkspaceRejected(Exception):
    """kagent refused to create the Session (for example the repository host is not allowed)."""


class WorkspaceUnconfirmed(Exception):
    """kagent did not confirm the operation; refresh before retrying."""


# --------------------------------------------------------------------------------------------
# Reading
# --------------------------------------------------------------------------------------------


def _manifest(row) -> WorkspaceManifest:
    ports = row["ports"]
    if isinstance(ports, str):
        ports = json.loads(ports)
    return WorkspaceManifest(
        repo_url=row["repo"],
        ref=row["ref"],
        branch=row["branch"],
        depth=row["depth"],
        agent_kind=row["kind"],
        development_environment=(
            json.loads(row["development_environment"])
            if isinstance(row.get("development_environment"), str)
            else row.get("development_environment")
        ),
        dev=WorkspaceDev(
            ports=tuple(WorkspacePort(**port) for port in ports or []),
            idle_timeout_minutes=row["idle_timeout_minutes"],
        ),
    )


def state_of(
    session: KagentSession, *, prepare_state: str | None = None
) -> tuple[WorkspaceObservedState, str | None]:
    """Map a kagent Session to a workspace state, with kagent's reason when it gave one."""
    if session.state == RuntimeState.FAILED:
        return (
            WorkspaceObservedState.FAILED,
            session.failure_message
            or session.failure_reason
            or "kagent reports failure",
        )
    if session.state in (RuntimeState.DELETING, RuntimeState.DELETED):
        return (
            WorkspaceObservedState.UNKNOWN,
            "kagent has deleted the Session; the next message replaces it.",
        )
    if session.state == RuntimeState.CREATING or session.operation in (
        RuntimeOperation.CREATE,
        RuntimeOperation.RESUME,
    ):
        return WorkspaceObservedState.RESUMING, "Starting."
    if session.operation == RuntimeOperation.SUSPEND:
        return WorkspaceObservedState.SUSPENDING, "Suspending."
    if session.state == RuntimeState.SUSPENDED:
        return WorkspaceObservedState.SUSPENDED, None
    if (
        settings.git_transport_enabled
        and settings.push_gate_enabled
        and session.state == RuntimeState.READY
        and session.operation == RuntimeOperation.NONE
    ):
        receipt = session.workspace_preparation
        if prepare_state == "failed" or (
            receipt
            and (receipt.historical or receipt.classification == "definite-failure")
        ):
            return (
                WorkspaceObservedState.FAILED,
                "Workspace preparation failed; replace the session",
            )
        if prepare_state == "requested" or (
            receipt and receipt.classification in ("pending", "uncertain")
        ):
            return WorkspaceObservedState.RESUMING, "Preparing workspace"
    if session.state == RuntimeState.READY:
        return WorkspaceObservedState.RUNNING, None
    return WorkspaceObservedState.UNKNOWN, "kagent reported no state."


async def _observe(
    kagent_session_id: str | None,
    session: KagentSession | None = None,
    *,
    workspace_id: str | None = None,
) -> tuple[WorkspaceObservedState, str | None]:
    if session is None:
        if kagent_session_id is None:
            return (
                WorkspaceObservedState.UNKNOWN,
                "The kagent Session has not been created yet; retrying automatically.",
            )
        try:
            session = await ns.get_client().get_session(kagent_session_id)
        except SessionError as exc:
            if exc.grpc_status == 5:
                if workspace_id is not None:
                    await _revoke_publication(workspace_id, kagent_session_id)
                return (
                    WorkspaceObservedState.UNKNOWN,
                    "kagent no longer has the Session; the next message replaces it.",
                )
            return WorkspaceObservedState.UNKNOWN, f"kagent: {exc}"
        except KagentError as exc:
            return WorkspaceObservedState.UNKNOWN, f"kagent unreachable: {exc}"
    if workspace_id is not None and session.state in (
        RuntimeState.FAILED,
        RuntimeState.DELETING,
        RuntimeState.DELETED,
    ):
        await _revoke_publication(workspace_id, kagent_session_id)
    if workspace_id is not None and (
        session.development_environment is not None
        or session.runtime_composition is not None
    ):
        await ns.ledger.record_composition(workspace_id, session)
    prepare_state = None
    if (
        workspace_id is not None
        and settings.git_transport_enabled
        and settings.push_gate_enabled
    ):
        async with db.connection() as conn:
            preparation = await conn.fetchrow(
                """SELECT e.prepare_state,e.create_request_id,b.session_id,b.kagent_request_id
                FROM git_enrollments e JOIN native_bindings b ON b.session_id=e.binding_id
                WHERE e.binding_id=$1 AND b.kagent_session_id=$2 AND e.revoked_at IS NULL""",
                workspace_id,
                session.id,
            )
            if preparation and preparation["create_request_id"] == ns._request_id(
                dict(preparation)
            ):
                prepare_state = preparation["prepare_state"]
    return state_of(session, prepare_state=prepare_state)


async def _revoke_publication(
    workspace_id: str, observed_runtime_id: str | None
) -> None:
    async with db.connection() as conn:
        async with push_lifecycle.locked(conn, workspace_id):
            current_runtime_id = await conn.fetchval(
                "SELECT kagent_session_id FROM native_bindings WHERE session_id=$1",
                workspace_id,
            )
            if (
                observed_runtime_id is not None
                and current_runtime_id == observed_runtime_id
            ):
                await push_lifecycle.revoke_locked(conn, workspace_id)


def _pending_create(row) -> tuple[WorkspaceObservedState, str]:
    """Return the state of a workspace whose kagent Session is not confirmed yet."""
    error, stopped = row.get("create_error"), row.get("create_stopped")
    created_at = row.get("created_at")
    if stopped == "rejected":
        return (
            WorkspaceObservedState.FAILED,
            f"kagent refused to create the Session ({error}). Refresh to try again "
            "once the cause is fixed.",
        )
    if stopped == "gave_up":
        return (
            WorkspaceObservedState.FAILED,
            f"kagent did not create the Session after {row.get('create_attempts')} "
            f"attempts ({error}); automatic retries stopped. Refresh to try again.",
        )
    if error:
        return (
            WorkspaceObservedState.RESUMING,
            f"Waiting for kagent to create the Session ({error}); retrying automatically.",
        )
    if (
        created_at is not None
        and (datetime.now(UTC) - created_at).total_seconds()
        >= settings.workspace_create_retry_window_seconds
    ):
        # Older than the retry window with no recorded outcome: ``due_creates`` skips it.
        return (
            WorkspaceObservedState.UNKNOWN,
            "kagent never confirmed this workspace's Session; refresh to retry.",
        )
    return (
        WorkspaceObservedState.RESUMING,
        "The kagent Session has not been created yet; retrying automatically.",
    )


async def _lifecycle(row, session: KagentSession | None = None) -> WorkspaceLifecycle:
    if session is None and row["kagent_session_id"] is None:
        state, detail = _pending_create(row)
    else:
        state, detail = await _observe(
            row["kagent_session_id"], session, workspace_id=row["session_id"]
        )
    async with db.connection() as conn:
        mode, reason = await push_lifecycle.projection(conn, row["session_id"])
    return WorkspaceLifecycle(
        publication_mode=mode,
        publication_reason=reason,
        workspace_id=row["session_id"],
        session_id=row["session_id"],
        observed_state=state,
        detail=detail,
        manifest=_manifest(row),
        last_activity_at=row["last_activity_at"],
        updated_at=datetime.now(UTC),
    )


async def _owned_row(workspace_id: str, user_id: str, *, conn=None):
    query = (
        _SELECT
        + """WHERE w.session_id=$1 AND s.user_id=$2 AND s.archived_at IS NULL
               AND n.kagent_deleted_at IS NULL"""
    )
    if conn is None:
        async with db.connection() as connection:
            row = await connection.fetchrow(query, workspace_id, user_id)
    else:
        row = await conn.fetchrow(query, workspace_id, user_id)
    if row is None:
        raise WorkspaceNotFound("Workspace not found")
    return row


async def get(workspace_id: str, user_id: str) -> WorkspaceLifecycle:
    return await _lifecycle(await _owned_row(workspace_id, user_id))


async def list_for(user_id: str) -> list[WorkspaceLifecycle]:
    async with db.connection() as conn:
        rows = await conn.fetch(
            _SELECT
            + """WHERE s.user_id=$1 AND s.archived_at IS NULL AND n.kagent_deleted_at IS NULL
                 ORDER BY w.created_at DESC""",
            user_id,
        )
    return list(await asyncio.gather(*(_lifecycle(row) for row in rows)))


async def publish(user_id: str, lifecycle: WorkspaceLifecycle) -> None:
    await notify_workspace_updated(user_id, lifecycle.model_dump(mode="json"))


# --------------------------------------------------------------------------------------------
# Preview support
# --------------------------------------------------------------------------------------------


async def preview_row(workspace_id: str, user_id: str, *, conn=None) -> dict | None:
    """Return the owner's workspace with a live kagent Session, or None (for the preview proxy)."""
    try:
        row = await _owned_row(workspace_id, user_id, conn=conn)
    except WorkspaceNotFound:
        return None
    if row["kagent_session_id"] is None or not await lifecycle.permitted(
        workspace_id, "preview", conn=conn
    ):
        return None
    ports = row["ports"]
    if isinstance(ports, str):
        ports = json.loads(ports)
    return {
        "workspace_id": row["session_id"],
        "kagent_session_id": row["kagent_session_id"],
        "ports": {int(p["number"]): p["name"] for p in ports or []},
    }


async def touch(workspace_id: str) -> None:
    """Record preview traffic (or a resume): it restarts the idle debounce."""
    async with db.connection() as conn:
        await conn.execute(
            "UPDATE workspaces SET last_active_at=NOW() WHERE session_id=$1",
            workspace_id,
        )


# --------------------------------------------------------------------------------------------
# Create and delete
# --------------------------------------------------------------------------------------------


async def project_for(user_id: str, project_id: str) -> dict | None:
    """Return the owner's project (its clone URL and default branch), or None."""
    async with db.connection() as conn:
        row = await conn.fetchrow(
            "SELECT id, html_url, default_branch FROM projects WHERE id=$1 AND user_id=$2",
            project_id,
            user_id,
        )
    return dict(row) if row else None


async def project_for_repo(user_id: str, repo: GithubRepo) -> dict:
    """Find or create the owner's project for a GitHub repository (same shape as ``project_for``)."""
    project = await db.get_or_create_project(user_id, repo)
    return {
        "id": project.id,
        "html_url": project.html_url,
        "default_branch": project.default_branch,
    }


_RESOLVE = object()


@dataclass(frozen=True)
class Enrolled:
    workspace_id: str
    manifest: WorkspaceManifest | None
    generation: int | None


async def enroll_session(
    conn,
    *,
    user_id: str,
    kind: str,
    role: str,
    mcp_grant_kind: str,
    manifest: WorkspaceManifest | None = None,
    project_id: str | None = None,
    session_id: str | None = None,
    parent_session_id: str | None = None,
    topic_id: str | None = None,
    title: str | None = None,
    description: str = "Branch development workspace",
    prompt: str = "Development workspace",
    environment=_RESOLVE,
    claim_branch: bool = False,
    checkout_resolved: bool = False,
) -> Enrolled:
    """Write the session, checkout and native identity rows in the caller's transaction.

    This is the one enrollment path: ordinary workspace creation and task provisioning both use
    it, so the checkout, environment, identity and branch claim are always written together.
    Role, parent, topic and grant come from the caller's server-side context, never from a
    request body. ``manifest`` is None for a coordination session, which has no checkout.
    ``environment`` is resolved from the project unless the caller supplies the one a task
    accepted. ``claim_branch`` takes the owner workspace's writer claim (a delegated attempt's
    claim is taken by its admission instead).

    With both Git gates enabled, resolve a new checkout before writing any rows. Task callers
    supply an already resolved initial checkout or a verified handoff remote SHA; the owner
    repo route resolves before its project insert/touch.

    Nothing here calls kagent or publishes a credential; those happen after the commit.
    """
    from mainloop.db import tasks as store

    store.require_transaction(conn)
    workspace_id = session_id or str(uuid.uuid4())
    conversation_id = str(uuid.uuid4())
    now = datetime.now(UTC)
    generation = None
    resolved = None
    repository = None
    if manifest is not None:
        if project_id is None:
            raise WorkspaceConflict("A workspace needs a project.")
        if claim_branch:
            # Same order as task admission: the global admission lock first, then the project.
            await store.admission_lock(conn)
        project = await conn.fetchrow(
            "SELECT id,full_name,html_url FROM projects WHERE id=$1 AND user_id=$2 FOR NO KEY UPDATE",
            project_id,
            user_id,
        )
        if project is None:
            raise WorkspaceConflict(
                "Workspace project must belong to the current owner."
            )
        try:
            project_repo = parse_github_repo(project["full_name"])
            url_repo = parse_github_repo(manifest.repo_url)
            html_repo = parse_github_repo(project["html_url"])
        except (InvalidGithubRepo, TypeError):
            raise WorkspaceConflict(
                "Workspace repository must match an owner-owned GitHub project."
            ) from None
        if (
            project_repo.full_name.lower() != url_repo.full_name.lower()
            or project_repo.full_name.lower() != html_repo.full_name.lower()
        ):
            raise WorkspaceConflict(
                "Workspace repository must match an owner-owned GitHub project."
            )
        repository = project_repo.full_name.lower()
        if (
            settings.git_transport_enabled
            and settings.push_gate_enabled
            and not checkout_resolved
        ):
            try:
                sha = await github_checkout.resolve_checkout_ref(
                    repository, manifest.ref
                )
            except github_checkout.CheckoutRefUnavailable as exc:
                raise WorkspaceRejected(str(exc)) from exc
            manifest = manifest.model_copy(update={"ref": sha})
        if environment is _RESOLVE:
            from mainloop.db.environments import EnvironmentError
            from mainloop.environments.resolution import resolve

            try:
                resolved = await resolve(conn, project_id, user_id)
            except EnvironmentError as exc:
                raise WorkspaceRejected(str(exc)) from exc
        else:
            resolved = environment
        manifest = manifest.model_copy(update={"development_environment": resolved})
    thread = await conn.fetchrow(
        "SELECT id FROM main_threads WHERE user_id=$1 ORDER BY created_at LIMIT 1",
        user_id,
    )
    thread_id = thread["id"] if thread else str(uuid.uuid4())
    if thread is None:
        await conn.execute(
            "INSERT INTO main_threads (id,user_id) VALUES ($1,$2)", thread_id, user_id
        )
    title = title or (f"{project_id} · {manifest.branch}" if manifest else "Task")
    await conn.execute(
        "INSERT INTO conversations (id,user_id,title) VALUES ($1,$2,$3)",
        conversation_id,
        user_id,
        title,
    )
    await conn.execute(
        """INSERT INTO sessions
           (id,user_id,main_thread_id,title,description,prompt,conversation_id,
            status,created_at,repo_url,project_id,branch_name,base_branch)
           VALUES ($1,$2,$3,$4,$5,$6,$7,'active',$8,$9,$10,$11,$12)""",
        workspace_id,
        user_id,
        thread_id,
        title,
        description,
        prompt,
        conversation_id,
        now,
        manifest.repo_url if manifest else None,
        project_id,
        manifest.branch if manifest else None,
        manifest.ref if manifest else "main",
    )
    if manifest is not None:
        await conn.execute(
            """INSERT INTO workspaces
               (session_id,repo,ref,branch,depth,ports,idle_timeout_minutes,created_at,development_environment)
               VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,$8,$9::jsonb)""",
            workspace_id,
            manifest.repo_url,
            manifest.ref,
            manifest.branch,
            manifest.depth,
            json.dumps([p.model_dump(mode="json") for p in manifest.dev.ports]),
            manifest.dev.idle_timeout_minutes,
            now,
            resolved.model_dump_json() if resolved else None,
        )
    await ns.create_binding(
        workspace_id,
        kind,
        role=role,
        parent_session_id=parent_session_id,
        topic_id=topic_id,
        mcp_grant_kind=mcp_grant_kind,
        conn=conn,
    )
    if claim_branch and manifest is not None:
        try:
            generation = await store.reserve_writer(
                conn,
                owner_id=user_id,
                repository=repository,
                branch=manifest.branch,
                binding_id=workspace_id,
            )
        except store.TaskError as exc:
            raise WorkspaceConflict(
                f"Another writer already owns branch {manifest.branch} ({exc.code})."
            ) from exc
    return Enrolled(workspace_id, manifest, generation)


async def create(
    user_id: str,
    project_id: str,
    manifest: WorkspaceManifest,
    *,
    checkout_resolved: bool = False,
) -> WorkspaceLifecycle:
    """Create the session rows and the kagent Session carrying the workspace.

    The rows are durable before kagent is called, and the create request id is stable, so a
    create whose outcome is unknown is retried under that id (by ``retry_creates``, or by
    ``refresh``) and returns the same Session rather than a second one. kagent
    refusing the request (for example a repository host outside the harness's allowed origins)
    removes the rows again and raises ``WorkspaceRejected``. The workspace takes the branch's
    writer claim in the same transaction, so a second writer on the branch (the default branch
    included) is refused with ``WorkspaceConflict``.

    ``checkout_resolved`` is server-internal: the owner repo route supplies the SHA it verified
    before creating or touching the project.
    """
    from mainloop.providers import registry

    try:
        profile = registry().resolve(manifest.agent_kind, "agent", selecting=True)
    except ValueError as exc:
        raise WorkspaceRejected(str(exc)) from exc
    manifest = manifest.model_copy(update={"agent_kind": profile.id})
    async with db.connection() as conn:
        async with conn.transaction():
            enrolled = await enroll_session(
                conn,
                user_id=user_id,
                kind=manifest.agent_kind,
                role="agent",
                mcp_grant_kind="workspace",
                manifest=manifest,
                project_id=project_id,
                claim_branch=True,
                checkout_resolved=checkout_resolved,
            )
    workspace_id = enrolled.workspace_id
    await _create_session(workspace_id, user_id, reject_removes_rows=True)
    return await get(workspace_id, user_id)


def _create_retry_fields(state: dict, error: str, *, permanent: bool) -> dict:
    """Return the retry state after one more unconfirmed create: the next retry, or a stop."""
    now = datetime.now(UTC)
    attempts = state["create_attempts"] + 1
    first = state["create_first_failed_at"] or now
    fields = {
        "create_attempts": attempts,
        "create_first_failed_at": first,
        "create_retry_at": None,
        "create_error": error[:500],
        "create_stopped": None,
    }
    if permanent:
        fields["create_stopped"] = "rejected"
    elif (
        now - first
    ).total_seconds() >= settings.workspace_create_retry_window_seconds:
        fields["create_stopped"] = "gave_up"
    else:
        delay = min(
            settings.workspace_create_retry_initial_seconds * 2 ** (attempts - 1),
            settings.workspace_create_retry_max_seconds,
        )
        fields["create_retry_at"] = now + timedelta(seconds=delay)
    return fields


async def _record_create_failure(
    workspace_id: str, exc: Exception, *, permanent: bool
) -> None:
    binding = await ns.get_binding(workspace_id)
    if binding is not None and binding["kagent_session_id"] is not None:
        return  # a concurrent create confirmed the Session; its state is already cleared
    state = await ns.ledger.create_retry(workspace_id)
    if state is None:
        return
    await ns.ledger.set_create_retry(
        workspace_id,
        **_create_retry_fields(state, _create_reason(exc), permanent=permanent),
    )


_CREATE_RESET = {
    "create_attempts": 0,
    "create_first_failed_at": None,
    "create_retry_at": None,
    "create_error": None,
    "create_stopped": None,
}


async def _create_session(
    workspace_id: str,
    user_id: str,
    *,
    reject_removes_rows: bool,
    restart_retries: bool = False,
) -> None:
    """Create the binding's kagent Session under its stable request id, or confirm it exists.

    An unconfirmed outcome is recorded on the workspace: a transient one schedules the next
    automatic retry (``retry_creates``), a permanent one or one past the retry window stops
    them. ``restart_retries`` (a manual refresh) starts a fresh retry window first.
    """
    async with ns._lock(workspace_id):
        binding = await ns.get_binding(workspace_id)
        if binding is None:
            return
        if restart_retries and binding["kagent_session_id"] is None:
            await ns.ledger.set_create_retry(workspace_id, **_CREATE_RESET)
        if binding["kagent_session_id"] is not None:
            async with db.connection() as conn:
                issuance = await conn.fetchval(
                    "SELECT issuance_id FROM git_enrollments WHERE binding_id=$1 AND revoked_at IS NULL",
                    workspace_id,
                )
            if issuance is None:
                return
            session = await ns._live_session(binding["kagent_session_id"])
            if session is not None and session.state not in (
                RuntimeState.FAILED,
                RuntimeState.DELETED,
            ):
                from mainloop.push_gate.credentials import ready_for_binding

                async with db.connection() as conn:
                    async with lifecycle.guard(workspace_id, "create", conn=conn):
                        await ready_for_binding(conn, workspace_id, session)
            return
        try:
            if (
                binding.get("mcp_grant_kind") == "workspace"
                and binding.get("token_hash") is None
            ):
                session = await ns.reconcile_revoked_workspace_creation(binding)
            else:
                session = await ns._create_bound_session(binding)
        except lifecycle.LifecycleDenied as exc:
            raise WorkspaceConflict(
                f"This workspace's task attempt is not live ({exc.code})."
            ) from exc
        except SessionError as exc:
            if exc.grpc_status in _REJECTED and reject_removes_rows:
                await _delete_rows(
                    workspace_id, evidence=f"kagent-rejected:{exc.grpc_status}"
                )
                raise WorkspaceRejected(str(exc)) from exc
            await _record_create_failure(
                workspace_id,
                exc,
                permanent=exc.grpc_status in _PERMANENT or ns._create_hit_deleted(exc),
            )
            if exc.grpc_status in _REJECTED:
                raise WorkspaceRejected(str(exc)) from exc
            logger.warning(
                "kagent create for workspace %s unconfirmed: %s", workspace_id, exc
            )
            return
        except KagentError as exc:
            await _record_create_failure(
                workspace_id,
                exc,
                permanent=isinstance(exc, ServiceConfigurationError),
            )
            logger.warning(
                "kagent create for workspace %s unconfirmed: %s", workspace_id, exc
            )
            return
        await ns.ledger.update_binding(
            workspace_id, kagent_session_id=session.id, standing_hash=None
        )
        if binding.get("token_hash"):
            from mainloop.push_gate.credentials import ready_for_binding

            async with db.connection() as conn:
                async with lifecycle.guard(workspace_id, "create", conn=conn):
                    await ready_for_binding(conn, workspace_id, session)


async def refresh(workspace_id: str, user_id: str) -> WorkspaceLifecycle:
    """Re-read the lifecycle from kagent, first retrying a create whose outcome was unknown.

    A manual retry also restarts automatic retries that had stopped.
    """
    row = await _owned_row(workspace_id, user_id)
    if row["kagent_session_id"] is None or settings.git_transport_enabled:
        await _create_session(
            workspace_id, user_id, reject_removes_rows=False, restart_retries=True
        )
        row = await _owned_row(workspace_id, user_id)
    return await _lifecycle(row)


async def _delete_rows(workspace_id: str, *, evidence: str | None = None) -> None:
    """Delete the session rows and release the workspace's branch claim.

    ``evidence`` is the confirmed absence of the runtime (kagent deleted the Session or refused
    the create); the claim is released only with it. Task attempt rows are not touched: the
    attempt keeps its audit after its workspace resources are gone.
    """
    if evidence is None:
        # A direct cleanup caller must confirm deletion through the same safe path.
        # Never fabricate fence evidence merely to make a branch claim releasable.
        session = await db.get_session(workspace_id)
        if session is not None:
            await delete(workspace_id, session.user_id)
        return
    # Revoke before deleting the binding. Cleanup has an independent durable tombstone, so a
    # Kubernetes Secret outage cannot leave an active bearer or erase the retry record.
    from mainloop.db import tasks as store
    from mainloop.runtime.agent_credentials import revoke

    await revoke(workspace_id)
    async with db.connection() as conn:
        async with push_lifecycle.locked(conn, workspace_id, revoke=True):
            async with conn.transaction():
                from mainloop.push_gate import store as push_store

                scope = await conn.fetchrow(
                    "SELECT s.user_id,p.full_name,w.branch FROM sessions s JOIN projects p ON p.id=s.project_id JOIN workspaces w ON w.session_id=s.id WHERE s.id=$1",
                    workspace_id,
                )
                if scope:
                    await push_store.assert_branch_resolved(
                        conn, scope["user_id"], scope["full_name"], scope["branch"]
                    )
                claim = await conn.fetchrow(
                    """SELECT owner_id,repository,branch,generation
                       FROM workspace_writer_claims WHERE binding_id=$1 AND held""",
                    workspace_id,
                )
                if claim is not None:
                    await store.release_writer(
                        conn,
                        owner_id=claim["owner_id"],
                        repository=claim["repository"],
                        branch=claim["branch"],
                        generation=claim["generation"],
                        binding_id=workspace_id,
                        fence_evidence_ref=evidence,
                    )
                conversation_id = await conn.fetchval(
                    "SELECT conversation_id FROM sessions WHERE id=$1", workspace_id
                )
                await conn.execute(
                    "DELETE FROM native_deliveries WHERE session_id=$1", workspace_id
                )
                await conn.execute(
                    "DELETE FROM native_bindings WHERE session_id=$1", workspace_id
                )
                await conn.execute(
                    "DELETE FROM workspaces WHERE session_id=$1", workspace_id
                )
                await conn.execute("DELETE FROM sessions WHERE id=$1", workspace_id)
                await conn.execute(
                    """DELETE FROM messages
                       WHERE conversation_id=$1
                         AND NOT EXISTS (
                             SELECT 1 FROM sessions s WHERE s.anchor_message_id=messages.id
                         )""",
                    conversation_id,
                )
                await conn.execute(
                    """DELETE FROM conversations c WHERE c.id=$1
                       AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.conversation_id=c.id)""",
                    conversation_id,
                )


async def delete(workspace_id: str, user_id: str) -> None:
    """Delete the kagent Session, then the workspace's rows.

    The rows stay until kagent confirms, so a failed or unknown delete can be retried without
    orphaning a Session. DeleteSession is idempotent and NotFound counts as deleted.
    """
    await _owned_row(workspace_id, user_id)
    if not await lifecycle.permitted(workspace_id, "delete"):
        raise WorkspaceConflict(
            "A task attempt's workspace is deleted only after the attempt has ended."
        )
    # A create whose outcome is unknown may have made a Session Mainloop never recorded. The
    # create is idempotent on its stored request id, so repeat it (as ``refresh`` does) and
    # delete what it returns. A rejected retry does not prove an earlier unknown request
    # created nothing (permissions/configuration may have changed in the meantime).
    binding = await ns.get_binding(workspace_id)
    if binding is not None and binding["kagent_session_id"] is None:
        try:
            await _create_session(workspace_id, user_id, reject_removes_rows=False)
        except WorkspaceRejected as exc:
            raise WorkspaceUnconfirmed(
                "kagent rejected reconciliation of an unknown create; the earlier Session "
                "may still exist. Restore the create configuration and refresh before deleting."
            ) from exc
    async with ns._lock(workspace_id):
        if await ns.ledger.active_count(workspace_id):
            raise WorkspaceConflict(
                "An open delivery must be reconciled before deleting this workspace."
            )
        binding = await ns.get_binding(workspace_id)
        if binding is not None and binding["kagent_session_id"] is None:
            raise WorkspaceUnconfirmed(
                "kagent did not confirm whether this workspace's Session exists; "
                "refresh before retrying."
            )
        evidence = "no-kagent-session"
        if binding is not None and binding["kagent_session_id"] is not None:
            from mainloop.runtime.agent_credentials import revoke

            evidence = f"kagent-deleted:{binding['kagent_session_id']}"
            # Stop accepting the cached bearer before asking kagent to delete its runtime.
            await revoke(workspace_id)
            try:
                deleted = await ns.get_client().delete_session(
                    binding["kagent_session_id"]
                )
                if (
                    deleted.id != binding["kagent_session_id"]
                    or deleted.state != RuntimeState.DELETED
                    or not deleted.settled
                ):
                    raise WorkspaceUnconfirmed(
                        "kagent workspace deletion is still pending."
                    )
            except SessionError as exc:
                if exc.grpc_status != 5:
                    raise WorkspaceUnconfirmed(
                        "kagent did not confirm workspace deletion; refresh before retrying."
                    ) from exc
            except KagentError as exc:
                raise WorkspaceUnconfirmed(
                    "kagent did not confirm workspace deletion; refresh before retrying."
                ) from exc
        await _delete_rows(workspace_id, evidence=evidence)


async def retry_creates() -> list[str]:
    """Retry the creates kagent did not confirm, when due; returns the ids now created.

    Each retry is the same idempotent ``CreateSession`` (stable request id, same checkout and
    credential reference) that **refresh** sends, under the same per-session lock, so it can
    only ever return the one Session. The owner sees each attempt's outcome.
    """
    created = []
    for row in await ns.ledger.due_creates(
        settings.workspace_create_retry_initial_seconds,
        settings.workspace_create_retry_window_seconds,
    ):
        workspace_id, user_id = row["session_id"], row["user_id"]
        try:
            await _create_session(workspace_id, user_id, reject_removes_rows=False)
        except WorkspaceRejected:
            pass  # recorded as a stop; the owner sees it below
        except WorkspaceConflict as exc:
            await _record_create_failure(workspace_id, exc, permanent=True)
        except (
            Exception
        ) as exc:  # noqa: BLE001 - e.g. a Secret outage; retried with backoff
            ns._log_step_failure("workspace_create", workspace_id, exc)
            await _record_create_failure(workspace_id, exc, permanent=False)
        binding = await ns.get_binding(workspace_id)
        if binding is not None and binding["kagent_session_id"] is not None:
            created.append(workspace_id)
        try:
            await publish(user_id, await get(workspace_id, user_id))
        except WorkspaceNotFound:
            pass
    return created


# --------------------------------------------------------------------------------------------
# Suspend and resume
# --------------------------------------------------------------------------------------------


async def _is_idle(session_id: str) -> bool:
    """Report whether the workspace's last activity is older than its idle timeout, read now."""
    async with db.connection() as conn:
        return bool(
            await conn.fetchval(
                """SELECT GREATEST(w.last_active_at, w.created_at,
                                   (SELECT max(d.updated_at) FROM native_deliveries d
                                    WHERE d.session_id=w.session_id))
                          < NOW() - make_interval(mins => w.idle_timeout_minutes)
                   FROM workspaces w WHERE w.session_id=$1""",
                session_id,
            )
        )


async def suspend_if_quiet(
    session_id: str, *, only_if_idle: bool = False
) -> KagentSession | None:
    """SuspendSession unless a turn is open or queued; None when there is no Session.

    This is the one place that suspends. The per-session lock orders it against
    ``submit_message`` (which records a delivery under the same lock) and ``_deliver`` (which
    claims ``sending`` under it): whichever comes second sees the other's effect. A delivery
    recorded after the check waits for the lock, finds the Session suspended and resumes it.

    ``only_if_idle`` is for the idle check, which chose the workspace some time ago: a preview
    that touched it since (the proxy touches before it wakes, so before it needs this lock) must
    win, or the suspend would land under a preview and the router would wake the actor again
    behind kagent. Activity since raises ``WorkspaceConflict``.
    """
    async with ns._lock(session_id):
        binding = await ns.get_binding(session_id)
        if binding is None or binding["kagent_session_id"] is None:
            return None
        if binding["role"] == "main":
            # Only workspaces have an idle timeout; the main thread stays resident.
            raise WorkspaceConflict("The main thread is never suspended.")
        if await ns.ledger.active_count(session_id):
            raise WorkspaceConflict(
                "A turn is in flight; suspend the workspace once it has finished."
            )
        if only_if_idle and not await _is_idle(session_id):
            raise WorkspaceConflict("The workspace was active since the idle check.")
        return await ns.get_client().suspend_session(binding["kagent_session_id"])


async def _kagent_call(workspace_id: str, user_id: str, call):
    row = await _owned_row(workspace_id, user_id)
    if row["kagent_session_id"] is None:
        raise WorkspaceConflict(
            "The kagent Session has not been created; refresh first."
        )
    try:
        session = await call(row)
    except lifecycle.LifecycleDenied as exc:
        raise WorkspaceConflict(
            f"This workspace's task attempt is not live ({exc.code})."
        ) from exc
    except SessionError as exc:
        raise WorkspaceConflict(f"kagent refused: {exc}") from exc
    except KagentError as exc:
        raise WorkspaceUnconfirmed(
            f"kagent did not confirm the operation ({exc}); refresh before retrying."
        ) from exc
    return await _lifecycle(row, session)


async def suspend(workspace_id: str, user_id: str) -> WorkspaceLifecycle:
    async def call(row):
        session = await suspend_if_quiet(row["session_id"])
        if session is None:
            raise WorkspaceConflict(
                "The kagent Session has not been created; refresh first."
            )
        return session

    return await _kagent_call(workspace_id, user_id, call)


async def _resume_if_suspended(row) -> tuple[KagentSession, bool]:
    """Return the Session, resumed first when suspended, and whether this call resumed it."""
    async with (
        ns._lock(row["session_id"]),
        lifecycle.guard(row["session_id"], "resume") as conn,
    ):
        client = ns.get_client()
        session = await client.get_session(row["kagent_session_id"])
        binding = await ns.get_binding(row["session_id"], conn=conn)
        if binding is None:
            raise WorkspaceUnconfirmed("Persisted workspace binding is unavailable.")
        await ns.validate_bound_session(binding, session, conn=conn)
        # Only a suspended Session is resumed. ResumeSession on a Ready one does not wake a
        # quiesced actor (kagent); a turn or a preview request wakes it.
        if session.state == RuntimeState.SUSPENDED:
            resumed = await client.resume_session(row["kagent_session_id"])
            await ns.validate_bound_session(binding, resumed, conn=conn)
            return resumed, True
        return session, False


async def resume(workspace_id: str, user_id: str) -> WorkspaceLifecycle:
    async def call(row):
        session, _ = await _resume_if_suspended(row)
        await touch(row["session_id"])
        return session

    return await _kagent_call(workspace_id, user_id, call)


# One in-flight wake per workspace: previews that arrive while it runs share it.
_waking: dict[str, asyncio.Task[bool]] = {}


async def _wake_for_preview(workspace_id: str, user_id: str) -> bool:
    resumed = False

    async def call(row):
        nonlocal resumed
        session, resumed = await _resume_if_suspended(row)
        if resumed:
            await touch(row["session_id"])
        return session

    lifecycle = await _kagent_call(workspace_id, user_id, call)
    if resumed:
        await publish(user_id, lifecycle)
    return resumed


async def wake_for_preview(workspace_id: str, user_id: str) -> bool:
    """Resume the workspace's Session through kagent when it is suspended; True if it did.

    With ``onQuiesce: Full`` the router wakes a suspended actor on a preview CONNECT without
    telling kagent, which would leave the Session ``suspended`` and invisible to idle-out. So a
    preview resumes it here first, then the router connects to an actor kagent knows is awake.
    Concurrent previews share one resume; a preview that gives up does not cancel it. Raises
    ``WorkspaceNotFound``, ``WorkspaceConflict`` or ``WorkspaceUnconfirmed`` when kagent could
    not be asked or refused, and the caller must not connect to the router then.
    """
    task = _waking.get(workspace_id)
    if task is None:
        task = asyncio.create_task(_wake_for_preview(workspace_id, user_id))
        _waking[workspace_id] = task

        def done(finished: asyncio.Task[bool]) -> None:
            if _waking.get(workspace_id) is finished:
                del _waking[workspace_id]
            if not finished.cancelled():
                finished.exception()  # retrieved, so an unwatched failure is not logged twice

        task.add_done_callback(done)
    return await asyncio.shield(task)


# --------------------------------------------------------------------------------------------
# Idle-out
# --------------------------------------------------------------------------------------------


async def suspend_idle() -> list[str]:
    """Suspend workspaces idle past their timeout; returns the ids suspended.

    Idle means no preview request, resume or turn activity for ``idle_timeout_minutes``. A
    workspace is a candidate again only after activity newer than its last idle suspend, so a
    suspended workspace is not polled every pass. A Session with a turn open or queued, or one
    kagent is already changing, is left for a later pass. The main thread has no workspace row
    and is excluded by role as well: it never idles out.
    """
    async with db.connection() as conn:
        rows = await conn.fetch(
            _SELECT  # nosec B608 - constant SQL fragments, nothing is interpolated
            + """WHERE s.archived_at IS NULL AND n.kagent_session_id IS NOT NULL
                   AND n.kagent_deleted_at IS NULL AND n.role <> 'main'
                   AND GREATEST(w.last_active_at, w.created_at,
                                (SELECT max(d.updated_at) FROM native_deliveries d
                                 WHERE d.session_id=w.session_id))
                       < NOW() - make_interval(mins => w.idle_timeout_minutes)
                   AND (w.idle_suspended_at IS NULL
                        OR w.idle_suspended_at < GREATEST(w.last_active_at, w.created_at,
                                (SELECT max(d.updated_at) FROM native_deliveries d
                                 WHERE d.session_id=w.session_id)))"""
        )
    suspended = []
    for row in rows:
        try:
            if await _idle_suspend(row["session_id"], row["kagent_session_id"]):
                suspended.append(row["session_id"])
                async with db.connection() as conn:
                    await conn.execute(
                        "UPDATE workspaces SET idle_suspended_at=NOW() WHERE session_id=$1",
                        row["session_id"],
                    )
                await publish(row["user_id"], await _lifecycle(row))
        except Exception as exc:
            ns._log_step_failure("suspend_idle", row["session_id"], exc)
    return suspended


async def _idle_suspend(session_id: str, kagent_session_id: str) -> bool:
    """Suspend the Session unless it is busy; True once it is suspended (now or already)."""
    try:
        session = await ns._live_session(kagent_session_id)
        if session is None or not session.settled:
            return False
        if session.state == RuntimeState.SUSPENDED:
            return True
        if session.state != RuntimeState.READY:
            return False
        return (await suspend_if_quiet(session_id, only_if_idle=True)) is not None
    except WorkspaceConflict:
        return False
