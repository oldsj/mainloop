"""Read observations and transactional task events; no turns or merge dispatch."""

from datetime import UTC, datetime, timedelta

from mainloop.db import tasks as store
from mainloop.runtime.policy import PolicyError
from mainloop.tasks.principal import TaskPrincipal

MAX_CI_AGE = timedelta(minutes=5)


def ci_state(ci, head_sha, *, now=None):
    """Only complete, fresh evidence for this exact head can be successful."""
    now = now or datetime.now(UTC)
    if not ci or ci.get("head_sha") != head_sha or ci.get("complete") is not True:
        return "unknown"
    try:
        captured = datetime.fromisoformat(ci["captured_at"].replace("Z", "+00:00"))
        if captured.tzinfo is None or not timedelta(0) <= now - captured <= MAX_CI_AGE:
            return "unknown"
    except (KeyError, ValueError, TypeError, AttributeError):
        return "unknown"
    if ci.get("blocked") is True:
        return "failure"
    if ci.get("pending") is True:
        return "pending"
    if ci.get("green") is True:
        return "success"
    return "unknown"


def observed(previous, *, repository, branch, number, pr, ci, observed_at):
    """Require the PR identity to match the task's durable linkage."""
    if (
        pr.number != number
        or pr.head.ref != branch
        or any(
            ref.repo.full_name.lower() != repository.lower()
            for ref in (pr.head, pr.base)
        )
        or pr.head.repo.id != pr.base.repo.id
    ):
        raise PolicyError("ownership", "PR observation differs from task workspace")
    state = "merged" if pr.merged else pr.state
    if state not in ("open", "closed", "merged"):
        state = "unknown"
    changed = previous.pr_head_sha != pr.head.sha
    return previous.model_copy(
        update={
            "repository": repository.lower(),
            "branch": branch,
            "pr_number": number,
            "pr_url": f"https://github.com/{repository}/pull/{number}",
            "pr_head_sha": pr.head.sha,
            "pr_state": state,
            "ci_state": ci_state(ci, pr.head.sha, now=observed_at),
            "ci_head_sha": ci.get("head_sha") if ci else None,
            "observed_at": observed_at,
            **(
                {
                    "merge_proposal_id": None,
                    "merge_state": None,
                    "pending_approval_ids": (),
                }
                if changed
                else {}
            ),
        }
    )


async def persist(
    conn,
    task,
    value,
    event_key,
    *,
    completed=False,
    notify_parent=True,
    attention=False,
):
    """Existing update_projection commits projection/version/event together.

    Called under publication/runtime guard and the global admission lock. The
    owner principal here is internal; no client may supply projection evidence.
    Completion shares that one version/event and leaves attempt capacity held.
    """
    store.require_transaction(conn)
    principal = TaskPrincipal(task.owner_id)
    fresh = await store.get_task(conn, task.id, principal, lock=True)
    if await conn.fetchval(
        "SELECT 1 FROM task_events WHERE task_id=$1 AND event_key=$2",
        task.id,
        event_key,
    ):
        return fresh
    if (fresh.version, fresh.current_attempt_id) != (
        task.version,
        task.current_attempt_id,
    ):
        raise store.TaskError(409, "stale_task_attempt")
    updated = await store.update_projection(
        conn,
        principal,
        task.id,
        task.version,
        task.current_attempt_id,
        value,
        event_key,
    )
    state_update = None
    if completed:
        if (
            value.pr_state != "merged"
            or value.ci_state != "success"
            or value.ci_head_sha != value.pr_head_sha
        ):
            raise PolicyError(
                "publication", "task completion needs verified merge and exact CI"
            )
        updated = updated.model_copy(update={"status": "completed", "reason": None})
        state_update = "completed"
    elif attention and updated.status not in ("completed", "failed", "cancelled"):
        if value.pending_approval_ids:
            updated = updated.model_copy(
                update={"status": "waiting", "reason": "approval"}
            )
            state_update = "waiting"
        elif updated.reason == "approval":
            updated = updated.model_copy(update={"status": "running", "reason": None})
            state_update = "running"
    if state_update:
        await conn.execute(
            "UPDATE tasks SET status=$4,snapshot=$2::jsonb WHERE id=$1 AND version=$3",
            updated.id,
            updated.model_dump_json(),
            updated.version,
            state_update,
        )
    if task.parent_task_id and notify_parent:
        parent = await store.get_task(conn, task.parent_task_id, principal, lock=True)
        key = f"child:{task.id}:{event_key}"
        if not await conn.fetchval(
            "SELECT 1 FROM task_events WHERE task_id=$1 AND event_key=$2",
            parent.id,
            key,
        ):
            await store.save_task(
                conn,
                parent.model_copy(
                    update={
                        "version": parent.version + 1,
                        "updated_at": datetime.now(UTC),
                    }
                ),
                parent.version,
                key,
            )
    return updated


class Projection:
    """S0 refresh port, for installation by serialized integration only."""

    async def refresh(self, database, task_id):
        from mainloop.services.github_creation import GitHubError
        from mainloop.services.github_merge import GitHubMergeClient
        from mainloop.tasks import publication
        from pydantic import ValidationError

        async with database.connection() as conn:
            row = await conn.fetchrow(
                """SELECT b.*,s.user_id,t.project_id FROM tasks t
                   JOIN task_attempts a ON a.id=t.current_attempt_id
                   JOIN native_bindings b ON b.session_id=a.binding_id
                   JOIN sessions s ON s.id=b.session_id WHERE t.id=$1""",
                task_id,
            )
        if not row:
            return
        binding = dict(row)
        async with publication.guard(database, binding, row["project_id"]):
            async with database.connection() as conn:
                task, attempt = await publication.current(conn, binding)
                previous = await store.projection(conn, task.id)
                # Projection is only a cache: reconstruct linkage from the immutable
                # proposal or this attempt's verified PR-creation record.
                proposals = await conn.fetch(
                    "SELECT facts FROM merge_proposals WHERE binding_id=$1 AND owner_id=$2 ORDER BY created_at DESC",
                    attempt.binding_id,
                    task.owner_id,
                )
                linkage = next(
                    (
                        store.decode(p["facts"])
                        for p in proposals
                        if store.decode(p["facts"]).get("attempt_id") == attempt.id
                    ),
                    None,
                )
                if linkage:
                    repository, number = linkage["repository"], linkage["pr_number"]
                else:
                    ids = [
                        ref.removeprefix("pr-creation:")
                        for ref in attempt.evidence_refs
                        if ref.startswith("pr-creation:")
                    ]
                    creation = await conn.fetchrow(
                        "SELECT result,repo_id FROM pr_creations WHERE id=ANY($1::text[]) AND user_id=$2 AND project_id=$3 AND head=$4 AND state='created' ORDER BY created_at DESC LIMIT 1",
                        ids,
                        task.owner_id,
                        task.project_id,
                        task.checkout.branch,
                    )
                    if not creation:
                        return
                    result = store.decode(creation["result"])
                    project = await store.project(conn, task.project_id, task.owner_id)
                    repository, number = project["full_name"], result["pr_number"]
                    linkage = {"repository_id": creation["repo_id"]}
            now = datetime.now(UTC)
            try:
                async with GitHubMergeClient() as github:
                    pr, ci = await github.observation(repository, number)
                if any(
                    ref.repo.id != linkage["repository_id"]
                    for ref in (pr.head, pr.base)
                ):
                    raise PolicyError("ownership", "PR repository ID changed")
                value = observed(
                    previous,
                    repository=repository,
                    branch=task.checkout.branch,
                    number=number,
                    pr=pr,
                    ci=ci,
                    observed_at=datetime.now(UTC),
                )
            except (GitHubError, ValidationError, PolicyError, TimeoutError):
                value = previous.model_copy(
                    update={
                        "ci_state": "unknown",
                        "ci_head_sha": None,
                        "pr_state": "unknown",
                        "observed_at": now,
                    }
                )
            # No task completion from observations alone: the existing merge service
            # must settle its durable intent, consent and policy first.
            async with database.connection() as conn, conn.transaction():
                await store.admission_lock(conn)
                task, _ = await publication.current(conn, binding)
                current = await store.projection(conn, task.id)
                comparable = value.model_dump(exclude={"observed_at"})
                if comparable == current.model_dump(exclude={"observed_at"}):
                    # Refresh age without duplicate events/parent notifications.
                    await conn.execute(
                        "UPDATE tasks SET projection=$2::jsonb WHERE id=$1",
                        task.id,
                        value.model_dump_json(),
                    )
                    return
                await persist(
                    conn,
                    task,
                    value,
                    f"pr-observed:{task.current_attempt_id}:{task.version}:{store.digest(comparable)}",
                )


async def read(conn, task):
    """Read-port integration helper: DB only, with stale CI masked to unknown.

    Caller must authorize ``task`` through db.tasks.get_task first. No GET may
    install a scheduler, send a native turn or call the merge service.
    """
    value = await store.projection(conn, task.id)
    if value.ci_state == "success" and (
        value.ci_head_sha != value.pr_head_sha
        or value.observed_at is None
        or not timedelta(0) <= datetime.now(UTC) - value.observed_at <= MAX_CI_AGE
    ):
        value = value.model_copy(update={"ci_state": "unknown"})
    if task.current_attempt_id is None:
        return value.model_copy(
            update={"merge_proposal_id": None, "pending_approval_ids": ()}
        )
    from mainloop.push_gate.lifecycle import projection as publication_mode
    from mainloop.tasks.lifecycle import load_attempt

    attempt = await load_attempt(conn, task.current_attempt_id)
    if attempt and attempt.binding_id:
        mode, _ = await publication_mode(conn, attempt.binding_id)
        value = value.model_copy(update={"publication_state": mode})
    rows = await conn.fetch(
        """SELECT p.id,p.facts,c.state,c.active_proposal_id FROM merge_proposals p
           JOIN merge_requests c ON c.id=p.candidate_id
           WHERE p.owner_id=$1 AND p.facts->>'task_id'=$2
             AND p.facts->>'attempt_id'=$3 ORDER BY p.created_at DESC,p.id DESC""",
        task.owner_id,
        task.id,
        task.current_attempt_id,
    )
    for row in rows:
        facts = store.decode(row["facts"])
        if (facts["pr_number"], facts["head_sha"]) == (
            value.pr_number,
            value.pr_head_sha,
        ):
            return value.model_copy(
                update={
                    "merge_state": row["state"],
                    "merge_proposal_id": (
                        row["id"] if row["active_proposal_id"] == row["id"] else None
                    ),
                }
            )
    return value
