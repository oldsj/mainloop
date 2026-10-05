"""Branch workspaces on kagent Sessions.

A workspace is a native agent session whose kagent Session was created with a ``workspace``
(repository, ref, branch, depth), so the repository is in the harness before the first turn. This
module owns what is specific to that: creation, the lifecycle read from kagent, suspend and
resume, the preview idle-out, and deletion. Turn delivery stays in ``native_sessions``.

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
from datetime import UTC, datetime

from mainloop.db import db
from mainloop.runtime import native_sessions as ns
from mainloop.runtime.kagent_client import (
    KagentError,
    KagentSession,
    RuntimeOperation,
    RuntimeState,
    SessionError,
)
from mainloop.sse import notify_workspace_updated

from models import (
    WorkspaceAgentKind,
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

_SELECT = """
SELECT w.session_id, w.repo, w.ref, w.branch, w.depth, w.ports, w.idle_timeout_minutes,
       w.last_active_at, w.idle_suspended_at, w.created_at,
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
        agent_kind=WorkspaceAgentKind(row["kind"]),
        dev=WorkspaceDev(
            ports=tuple(WorkspacePort(**port) for port in ports or []),
            idle_timeout_minutes=row["idle_timeout_minutes"],
        ),
    )


def state_of(session: KagentSession) -> tuple[WorkspaceObservedState, str | None]:
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
    if session.state == RuntimeState.READY:
        return WorkspaceObservedState.RUNNING, None
    return WorkspaceObservedState.UNKNOWN, "kagent reported no state."


async def _observe(
    kagent_session_id: str | None, session: KagentSession | None = None
) -> tuple[WorkspaceObservedState, str | None]:
    if session is None:
        if kagent_session_id is None:
            return (
                WorkspaceObservedState.UNKNOWN,
                "The kagent Session has not been created; refresh to retry.",
            )
        try:
            session = await ns.get_client().get_session(kagent_session_id)
        except SessionError as exc:
            if exc.grpc_status == 5:
                return (
                    WorkspaceObservedState.UNKNOWN,
                    "kagent no longer has the Session; the next message replaces it.",
                )
            return WorkspaceObservedState.UNKNOWN, f"kagent: {exc}"
        except KagentError as exc:
            return WorkspaceObservedState.UNKNOWN, f"kagent unreachable: {exc}"
    return state_of(session)


async def _lifecycle(row, session: KagentSession | None = None) -> WorkspaceLifecycle:
    state, detail = await _observe(row["kagent_session_id"], session)
    return WorkspaceLifecycle(
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


async def preview_row(workspace_id: str, user_id: str) -> dict | None:
    """Return the owner's workspace with a live kagent Session, or None (for the preview proxy)."""
    try:
        row = await _owned_row(workspace_id, user_id)
    except WorkspaceNotFound:
        return None
    if row["kagent_session_id"] is None:
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


async def create(
    user_id: str, project_id: str, manifest: WorkspaceManifest
) -> WorkspaceLifecycle:
    """Create the session rows and the kagent Session carrying the workspace.

    The rows are durable before kagent is called, and the create request id is stable, so a
    create whose outcome is unknown is reconciled by ``refresh`` rather than repeated. kagent
    refusing the request (for example a repository host outside the harness's allowed origins)
    removes the rows again and raises ``WorkspaceRejected``.
    """
    workspace_id = str(uuid.uuid4())
    conversation_id = str(uuid.uuid4())
    now = datetime.now(UTC)
    title = f"{project_id} · {manifest.branch}"
    async with db.connection() as conn:
        async with conn.transaction():
            thread = await conn.fetchrow(
                "SELECT id FROM main_threads WHERE user_id=$1 ORDER BY created_at LIMIT 1",
                user_id,
            )
            thread_id = thread["id"] if thread else str(uuid.uuid4())
            if thread is None:
                await conn.execute(
                    "INSERT INTO main_threads (id,user_id) VALUES ($1,$2)",
                    thread_id,
                    user_id,
                )
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
                "Branch development workspace",
                "Development workspace",
                conversation_id,
                now,
                manifest.repo_url,
                project_id,
                manifest.branch,
                manifest.ref or None,
            )
            await conn.execute(
                """INSERT INTO workspaces
                   (session_id,repo,ref,branch,depth,ports,idle_timeout_minutes,created_at)
                   VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,$8)""",
                workspace_id,
                manifest.repo_url,
                manifest.ref,
                manifest.branch,
                manifest.depth,
                json.dumps([p.model_dump(mode="json") for p in manifest.dev.ports]),
                manifest.dev.idle_timeout_minutes,
                now,
            )
            await ns.create_binding(workspace_id, manifest.agent_kind.value, conn=conn)
    await _create_session(workspace_id, user_id, reject_removes_rows=True)
    return await get(workspace_id, user_id)


async def _create_session(
    workspace_id: str, user_id: str, *, reject_removes_rows: bool
) -> None:
    async with ns._lock(workspace_id):
        binding = await ns.get_binding(workspace_id)
        if binding is None or binding["kagent_session_id"] is not None:
            return
        try:
            session = await ns._create_bound_session(binding)
        except SessionError as exc:
            if exc.grpc_status in _REJECTED:
                if reject_removes_rows:
                    await _delete_rows(workspace_id)
                raise WorkspaceRejected(str(exc)) from exc
            logger.warning(
                "kagent create for workspace %s unconfirmed: %s", workspace_id, exc
            )
            return
        except KagentError as exc:
            logger.warning(
                "kagent create for workspace %s unconfirmed: %s", workspace_id, exc
            )
            return
        await ns.ledger.update_binding(
            workspace_id, kagent_session_id=session.id, standing_hash=None
        )


async def refresh(workspace_id: str, user_id: str) -> WorkspaceLifecycle:
    """Re-read the lifecycle from kagent, first retrying a create whose outcome was unknown."""
    row = await _owned_row(workspace_id, user_id)
    if row["kagent_session_id"] is None:
        await _create_session(workspace_id, user_id, reject_removes_rows=False)
        row = await _owned_row(workspace_id, user_id)
    return await _lifecycle(row)


async def _delete_rows(workspace_id: str) -> None:
    async with db.connection() as conn:
        async with conn.transaction():
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
        if binding is not None and binding["kagent_session_id"] is not None:
            try:
                await ns.get_client().delete_session(binding["kagent_session_id"])
            except SessionError as exc:
                if exc.grpc_status != 5:
                    raise WorkspaceUnconfirmed(
                        "kagent did not confirm workspace deletion; refresh before retrying."
                    ) from exc
            except KagentError as exc:
                raise WorkspaceUnconfirmed(
                    "kagent did not confirm workspace deletion; refresh before retrying."
                ) from exc
        await _delete_rows(workspace_id)


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
    async with ns._lock(row["session_id"]):
        client = ns.get_client()
        session = await client.get_session(row["kagent_session_id"])
        # Only a suspended Session is resumed. ResumeSession on a Ready one does not wake a
        # quiesced actor (kagent); a turn or a preview request wakes it.
        if session.state == RuntimeState.SUSPENDED:
            return await client.resume_session(row["kagent_session_id"]), True
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
