"""Task linkage through existing attempt, PR-creation and immutable merge records.

Projection fields are never authority. External writes use the same ordered locks
as source revocation; no new ledger, runtime owner or dispatcher is introduced.
"""

import asyncio
from contextlib import asynccontextmanager
from datetime import UTC, datetime
from functools import wraps
from weakref import WeakKeyDictionary

from mainloop.db import tasks as store
from mainloop.push_gate import lifecycle as push
from mainloop.runtime.policy import PolicyError
from mainloop.tasks import lifecycle

# Mirror the durable tree lock before taking a pool connection. Duplicate task
# calls waiting for that lock must not exhaust the pool needed by its holder.
# PostgreSQL remains the authority across processes; these locks hold no facts.
_tree_locks = WeakKeyDictionary()


@asynccontextmanager
async def guard(database, binding, project_id):
    # Policy -> tree -> publication -> runtime -> admission -> rows. Locks are
    # session locks; HTTP has no open database transaction. Revocation takes these
    # same locks, including the ancestor tree key for a delegated child.
    async with database.connection() as conn:
        facts = await lifecycle.facts(conn, binding["session_id"])
        if facts is None:
            await lifecycle.check(conn, binding["session_id"], "submit")
        root = (
            await conn.fetchval(
                "SELECT root_task_id FROM tasks WHERE id=$1", facts["task_id"]
            )
            if facts
            else None
        )
        actual = await conn.fetchval(
            "SELECT project_id FROM sessions WHERE id=$1", binding["session_id"]
        )
        if actual is not None and actual != project_id:
            raise PolicyError("ownership", "publication project differs from workspace")
    if facts is None:
        # Preserve existing ordinary PR/merge concurrency and authority checks.
        yield
        return
    locks = _tree_locks.setdefault(asyncio.get_running_loop(), {})
    lock = locks.setdefault(root, asyncio.Lock())
    async with lock, database.connection() as conn:
        async with push.locked(conn, binding["session_id"]), lifecycle.locked(
            conn, binding["session_id"]
        ):
            try:
                await lifecycle.check(conn, binding["session_id"], "submit")
            except lifecycle.LifecycleDenied:
                raise PolicyError(
                    "ownership", "task writer is no longer current"
                ) from None
            yield


def guarded(function=None, *, before=None, schema=None, replay=None):
    """Public tool boundary; do not nest decorated tools while holding a guard."""
    if function is None:
        return lambda function: guarded(
            function, before=before, schema=schema, replay=replay
        )

    @wraps(function)
    async def call(binding, arguments, *args, **kwargs):
        from mainloop.db import db

        if before:
            before()
        if schema:
            schema.model_validate(arguments)
        if replay:
            result = await replay(binding, arguments, *args, **kwargs)
            if result is not None:
                return result
        project_id = arguments.get("project_id")
        if project_id is None:
            async with db.connection() as conn:
                project_id = await conn.fetchval(
                    "SELECT facts->>'project_id' FROM merge_proposals WHERE id=$1 AND owner_id=$2",
                    arguments.get("proposal_id"),
                    binding["user_id"],
                )
        try:
            async with guard(db, binding, project_id):
                return await function(binding, arguments, *args, **kwargs)
        except PolicyError:
            if replay:
                # A competing invocation may have completed while this caller
                # waited for the writer lock. Return only its immutable result.
                result = await replay(binding, arguments, *args, **kwargs)
                if result is not None:
                    return result
            raise

    return call


async def current(conn, binding):
    """Only this binding's exact current attempt; ordinary sessions return None."""
    row = await lifecycle.facts(conn, binding["session_id"])
    if row is None:
        await lifecycle.check(conn, binding["session_id"], "submit")
        return None
    try:
        principal = await lifecycle.authenticate_binding(conn, binding)
    except lifecycle.LifecycleDenied:
        raise PolicyError("ownership", "task attempt authority changed") from None
    task = await store.get_task(conn, principal.task_id, principal)
    attempt = await lifecycle.load_attempt(conn, principal.attempt_id)
    if task.mode != "code" or attempt.workspace_id != binding["session_id"]:
        raise PolicyError("ownership", "publication requires this coding workspace")
    return task, attempt


def task_facts(project):
    """Pin existing SQL-resolved scope in the immutable merge proposal."""
    if not project.get("attempt_id"):
        return {}
    return {
        "task_id": project["task_id"],
        "attempt_id": project["attempt_id"],
        "workspace_id": project["attempt_workspace_id"],
        "writer_generation": project["attempt_writer_generation"],
    }


async def unresolved_intents(conn, task, attempt):
    """Read unresolved publication intents from the existing ledgers.

    Caller holds source policy/tree/publication/runtime locks and admission before
    releasing/transferring its writer claim. Include unassociated PR intents: a
    missing attempt reference is not evidence that no external write occurred.
    """
    store.require_transaction(conn)
    if task.mode != "code":
        return ()
    creations = await conn.fetch(
        """SELECT id FROM pr_creations WHERE user_id=$1 AND project_id=$2
           AND head=$3 AND state='uncertain' ORDER BY id""",
        task.owner_id,
        task.project_id,
        task.checkout.branch,
    )
    merges = await conn.fetch(
        """SELECT c.intent_id FROM merge_requests c
           JOIN merge_proposals p ON p.id=c.active_proposal_id
           WHERE c.owner_id=$1 AND p.facts->>'project_id'=$2
             AND (p.binding_id=$3 OR p.facts->>'head'=$4)
             AND c.state IN ('merging','uncertain') AND c.intent_id IS NOT NULL
           ORDER BY c.intent_id""",
        task.owner_id,
        task.project_id,
        attempt.binding_id if attempt else None,
        task.checkout.branch,
    )
    from mainloop.push_gate.store import unresolved_for_branch

    git = await unresolved_for_branch(
        conn,
        task.owner_id,
        (
            await conn.fetchval(
                "SELECT full_name FROM projects WHERE id=$1", task.project_id
            )
        ),
        task.checkout.branch,
    )
    return tuple(
        sorted(
            {
                *git,
                *(f"pr-creation:{row['id']}" for row in creations),
                *(f"merge-intent:{row['intent_id']}" for row in merges),
            }
        )
    )


async def bind_creation(database, binding, creation, *, newly_claimed=False):
    """Associate the existing intent with its sole attempt before network dispatch.

    An uncertain pre-S4 creation without an association cannot be adopted. This
    avoids attributing an earlier writer's unknown POST to a successor.
    """
    async with database.connection() as conn, conn.transaction():
        await store.admission_lock(conn)
        owned = await current(conn, binding)
        if owned is None:
            return
        task, attempt = owned
        ref = f"pr-creation:{creation['id']}"
        await conn.execute("SELECT pg_advisory_xact_lock(hashtextextended($1,0))", ref)
        existing = await conn.fetch(
            "SELECT id FROM task_attempts WHERE snapshot->'evidence_refs' ? $1", ref
        )
        if existing and any(row["id"] != attempt.id for row in existing):
            raise PolicyError("ownership", "PR intent belongs to another task attempt")
        if (
            creation["project_id"] != task.project_id
            or creation["head"] != task.checkout.branch
        ):
            raise PolicyError("ownership", "PR intent differs from task checkout")
        if ref not in attempt.evidence_refs:
            if not newly_claimed:
                raise PolicyError(
                    "ownership", "existing PR intent lacks this attempt's association"
                )
            await lifecycle.save_attempt(
                conn,
                attempt.model_copy(
                    update={"evidence_refs": (*attempt.evidence_refs, ref)}
                ),
            )


async def attach_creation(database, binding, creation, repository):
    """Project only a verified, durable result of this attempt's existing intent."""
    from mainloop.tasks.projection import persist

    async with database.connection() as conn, conn.transaction():
        await store.admission_lock(conn)
        owned = await current(conn, binding)
        if owned is None:
            return
        task, attempt = owned
        if f"pr-creation:{creation['id']}" not in attempt.evidence_refs:
            raise PolicyError(
                "ownership", "PR intent has no current-attempt association"
            )
        row = await conn.fetchrow(
            "SELECT * FROM pr_creations WHERE id=$1", creation["id"]
        )
        if not row or row["user_id"] != task.owner_id or row["state"] != "created":
            raise PolicyError("publication", "verified PR result unavailable")
        result = store.decode(row["result"])
        if (
            result["head_sha"] != row["expected_sha"]
            or row["head"] != task.checkout.branch
        ):
            raise PolicyError("publication", "PR result differs from persisted intent")
        value = (await store.projection(conn, task.id)).model_copy(
            update={
                "repository": repository.lower(),
                "branch": row["head"],
                "pr_number": result["pr_number"],
                "pr_url": result["url"],
                "pr_head_sha": result["head_sha"],
                "pr_state": "unknown",
                "ci_state": "unknown",
                "ci_head_sha": None,
                "merge_state": None,
                "merge_proposal_id": None,
                "pending_approval_ids": (),
                "observed_at": datetime.now(UTC),
            }
        )
        await persist(conn, task, value, f"pr-created:{creation['id']}")


async def attach_proposal(conn, binding, proposal):
    from mainloop.tasks.projection import ci_state, persist

    owned = await current(conn, binding)
    if owned is None:
        return
    task, attempt = owned
    facts = proposal["facts"]
    if facts.get("attempt_id") != attempt.id:
        raise PolicyError("ownership", "proposal has no current task association")
    previous = await store.projection(conn, task.id)
    value = previous.model_copy(
        update={
            "repository": facts["repository"].lower(),
            "branch": facts["head"],
            "pr_number": facts["pr_number"],
            "pr_url": f"https://github.com/{facts['repository']}/pull/{facts['pr_number']}",
            "pr_head_sha": facts["head_sha"],
            "pr_state": "open",
            "ci_state": ci_state(facts["ci"], facts["head_sha"]),
            "ci_head_sha": facts["ci"].get("head_sha"),
            "merge_state": "prepared",
            "merge_proposal_id": proposal["id"],
            "pending_approval_ids": (),
            "observed_at": datetime.now(UTC),
        }
    )
    await persist(conn, task, value, f"merge-proposal:{proposal['id']}")


async def settle_merge(conn, candidate, result):
    """Settle product state/event with the merge outcome; retain runtime capacity.

    Runtime fencing/deletion remains S1/S3's job. A verified merge never implies
    that the native runtime is gone or that a branch claim can be released.
    """
    from mainloop.tasks.projection import ci_state, persist

    p = await conn.fetchrow(
        "SELECT binding_id,facts FROM merge_proposals WHERE id=$1",
        candidate["active_proposal_id"],
    )
    facts = store.decode(p["facts"])
    if not facts.get("task_id"):
        return
    binding = await conn.fetchrow(
        """SELECT b.*,s.user_id FROM native_bindings b JOIN sessions s ON s.id=b.session_id
           WHERE b.session_id=$1""",
        p["binding_id"],
    )
    owned = await current(conn, dict(binding))
    task, attempt = owned
    if task_facts(
        {
            "task_id": task.id,
            "attempt_id": attempt.id,
            "attempt_workspace_id": attempt.workspace_id,
            "attempt_writer_generation": attempt.writer_generation,
        }
    ) != {
        key: facts[key]
        for key in ("task_id", "attempt_id", "workspace_id", "writer_generation")
    }:
        raise PolicyError("ownership", "merge outcome belongs to another attempt")
    claim = store.decode(candidate["result"]) if candidate["result"] else {}
    evidence = claim.get("claim_evidence")
    ci = result.get("ci_observation") or (evidence or {}).get("ci")
    if (
        not evidence
        or evidence["head_sha"] != facts["head_sha"]
        or ci_state(ci, facts["head_sha"]) != "success"
    ):
        raise PolicyError(
            "publication", "exact-head successful claim evidence unavailable"
        )
    policy = await conn.fetchrow(
        "SELECT merge_policy,merge_policy_version FROM projects WHERE id=$1 AND user_id=$2",
        task.project_id,
        task.owner_id,
    )
    # The immutable proposal and durable intent record the policy accepted at
    # dispatch. Later policy edits cannot undo an already-dispatched merge.
    if policy is None:
        raise PolicyError("ownership", "merge project is no longer owner-owned")
    if facts["route"] == "approval" and not candidate["receipt_action_id"]:
        raise PolicyError("consent", "merge has no claimed leaf receipt")
    value = (await store.projection(conn, task.id)).model_copy(
        update={
            "repository": facts["repository"].lower(),
            "branch": facts["head"],
            "pr_number": facts["pr_number"],
            "pr_url": result["url"],
            "pr_head_sha": facts["head_sha"],
            "pr_state": "merged",
            "ci_state": "success",
            "ci_head_sha": facts["head_sha"],
            "merge_state": "merged",
            "merge_proposal_id": candidate["active_proposal_id"],
            "pending_approval_ids": (),
            "observed_at": datetime.fromisoformat(
                ci["captured_at"].replace("Z", "+00:00")
            ),
        }
    )
    await persist(
        conn, task, value, f"merge-settled:{candidate['intent_id']}", completed=True
    )
