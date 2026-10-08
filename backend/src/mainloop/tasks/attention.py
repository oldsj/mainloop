"""Task-tree links to existing HITL cards. No receipt copying or new cards."""

from mainloop.db import tasks as store
from mainloop.runtime.policy import PolicyError
from mainloop.tasks import lifecycle, publication
from mainloop.tasks.principal import TaskPrincipal
from mainloop.tasks.projection import persist

from models.hitl import HITLProjection, ToolApprovalRequest


def owns_leaf(leaf, *, owner_id, binding_id, runtime_session_id):
    return (leaf.owner_id, leaf.binding_id, leaf.runtime_session_id) == (
        owner_id,
        binding_id,
        runtime_session_id,
    )


async def pending(conn, task, attempt, runtime_id):
    """Resolve only current owned native leaves; a parent never becomes the leaf."""
    rows = await conn.fetch(
        """SELECT r.id,r.snapshot FROM native_hitl_requests r
           JOIN queue_items q ON q.hitl_request_id=r.id
           WHERE r.owner_id=$1 AND NOT r.superseded AND q.status='pending'
             AND r.snapshot->>'availability'='pending' ORDER BY r.id""",
        task.owner_id,
    )
    result = set()
    projection = await store.projection(conn, task.id)
    for row in rows:
        request = HITLProjection.model_validate(store.decode(row["snapshot"]))
        for leaf in request.leaves:
            if not owns_leaf(
                leaf,
                owner_id=task.owner_id,
                binding_id=attempt.binding_id,
                runtime_session_id=runtime_id,
            ):
                continue
            if await conn.fetchval(
                "SELECT 1 FROM native_hitl_response_members WHERE owner_id=$1 AND leaf_key=$2",
                task.owner_id,
                leaf.key(),
            ):
                continue
            stale = False
            if isinstance(request.payload, ToolApprovalRequest):
                tools = (
                    request.payload.nested.tools
                    if request.payload.nested
                    else request.payload.tools
                )
                for tool in tools:
                    pid = (
                        tool.args.get("proposal_id")
                        if isinstance(tool.args, dict)
                        else None
                    )
                    proposal = (
                        await conn.fetchrow(
                            """SELECT p.facts,p.binding_id,c.active_proposal_id,c.state
                           FROM merge_proposals p JOIN merge_requests c ON c.id=p.candidate_id
                           WHERE p.id=$1 AND p.owner_id=$2""",
                            pid,
                            task.owner_id,
                        )
                        if pid
                        else None
                    )
                    if proposal:
                        facts = store.decode(proposal["facts"])
                        stale |= (
                            proposal["binding_id"] != attempt.binding_id
                            or proposal["active_proposal_id"] != pid
                            or proposal["state"] not in ("prepared", "evaluating")
                            or facts.get("attempt_id") != attempt.id
                            or facts.get("writer_generation")
                            != attempt.writer_generation
                            or (
                                projection.pr_number == facts["pr_number"]
                                and projection.pr_head_sha is not None
                                and projection.pr_head_sha != facts["head_sha"]
                            )
                        )
            if not stale:
                result.add(row["id"])
    return tuple(sorted(result))


async def refresh(database, binding_id):
    """Roll up presentation atomically; reuse the observer's canonical owner card.

    Run after the observer's HITL transaction, never with its receipt/leaf locks
    held. Read/decision integration may call the same helper after receipt writes.
    """
    async with database.connection() as conn:
        row = await conn.fetchrow(
            """SELECT b.*,s.user_id,t.project_id FROM task_attempts a JOIN tasks t ON t.id=a.task_id
               JOIN native_bindings b ON b.session_id=a.binding_id
               JOIN sessions s ON s.id=b.session_id
               WHERE a.binding_id=$1 AND t.current_attempt_id=a.id AND a.state='active'""",
            binding_id,
        )
    if not row:
        return
    binding = dict(row)
    try:
        async with publication.guard(database, binding, row["project_id"]):
            async with database.connection() as conn, conn.transaction():
                await store.admission_lock(conn)
                principal = await lifecycle.authenticate_binding(conn, binding)
                tasks = await conn.fetch(
                    "SELECT id FROM tasks WHERE owner_id=$1 AND root_task_id=$2 ORDER BY id",
                    principal.owner_id,
                    principal.root_task_id,
                )
                states = []
                for item in tasks:
                    task = await store.get_task(
                        conn, item["id"], TaskPrincipal(principal.owner_id), lock=True
                    )
                    attempt = (
                        await lifecycle.load_attempt(conn, task.current_attempt_id)
                        if task.current_attempt_id
                        else None
                    )
                    runtime = (
                        await conn.fetchval(
                            "SELECT kagent_session_id FROM native_bindings WHERE session_id=$1 AND token_hash IS NOT NULL AND kagent_deleted_at IS NULL",
                            attempt.binding_id,
                        )
                        if attempt
                        and attempt.state == "active"
                        and task.status not in ("completed", "failed", "cancelled")
                        else None
                    )
                    ids = await pending(conn, task, attempt, runtime) if runtime else ()
                    states.append((task, ids))
                # Each ancestor presents the same canonical cards as its subtree.
                # Rollup never grants it a leaf's continuation or merge receipt.
                by_id = {task.id: task for task, _ in states}
                rolled_up = {task.id: set(ids) for task, ids in states}
                for task, ids in states:
                    parent_id = task.parent_task_id
                    while parent_id in by_id:
                        rolled_up[parent_id].update(ids)
                        parent_id = by_id[parent_id].parent_task_id
                # Emit one changed projection event per task, including the root.
                states.sort(key=lambda item: item[0].parent_task_id is None)
                for task, _ in states:
                    ids = tuple(sorted(rolled_up[task.id]))
                    task = await lifecycle.load_task(conn, task.id)
                    value = await store.projection(conn, task.id)
                    if value.pending_approval_ids == ids:
                        continue
                    value = value.model_copy(update={"pending_approval_ids": ids})
                    await persist(
                        conn,
                        task,
                        value,
                        f"attention:{task.current_attempt_id}:{task.version}:{store.digest(ids)}",
                        notify_parent=False,
                        attention=True,
                    )
    except (PolicyError, lifecycle.LifecycleDenied):
        # A revoked/superseded source creates no new attention association.
        return
