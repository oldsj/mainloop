"""Merge proposals and at-most-once dispatch. PostgreSQL locks never span HTTP."""

import asyncio
import json
import logging
import os
import re
import uuid
from datetime import datetime, timezone

from mainloop.db import db
from mainloop.db import tasks as task_store
from mainloop.db.hitl import lookup_merge_receipt
from mainloop.runtime.policy import PolicyError
from mainloop.services.github_creation import GitHubError
from mainloop.services.github_merge import GitHubMergeClient
from mainloop.services.merge_summary import build_summary
from mainloop.services.workspace_authority import (
    ScopeUnavailable,
    resolve_project_authority,
)
from mainloop.tasks import publication
from mainloop.tasks.projection import ci_state
from pydantic import ValidationError

from models.agent_tools import MergePullRequestWithApproval, PreparePullRequestMerge
from models.hitl import MERGE_OPERATION, MergeReceiptKey, normalized_hash
from models.merge_policy import PROTECTED_GLOBS_VERSION

logger = logging.getLogger(__name__)
CI_POLL_SECONDS = 10.0
EVALUATION_BUDGET_SECONDS = 20.0
STATUS_BUDGET_SECONDS = 2.0


def enabled():
    return os.environ.get("MAINLOOP_MERGE_TOOLS_ENABLED") == "true"


def require_enabled():
    if not enabled():
        raise PolicyError(
            "disabled", "merge tools require verified deployment enablement"
        )


def decode(value):
    return json.loads(value) if isinstance(value, str) else value


def pinned(facts):
    return {
        k: v
        for k, v in facts.items()
        if k not in ("ci", "mergeable", "mapping_evidence")
    }


async def authority(conn, binding, project_id, branch=None):
    try:
        resolved = await resolve_project_authority(
            conn,
            binding,
            project_id,
            branch=branch,
            require_runtime=True,
        )
    except ScopeUnavailable:
        resolved = None
    if not resolved:
        raise PolicyError("ownership", "no live runtime binding for this project")
    project, name = resolved
    return project, name


async def read_evidence(binding, body):
    async with db.connection() as conn:
        project, name = await authority(conn, binding, body.project_id)
    github = None
    try:
        async with asyncio.timeout(60), GitHubMergeClient(name) as github:
            facts = await github.evidence(name, body.pr_number, body.expected_sha)
    except (
        GitHubError,
        ValidationError,
        KeyError,
        TypeError,
        ValueError,
        TimeoutError,
    ) as error:
        # Fixed step labels and class names only: no exception text, traceback,
        # response body, repository input or credential-bearing request.
        logger.warning(
            "GitHub merge evidence unavailable: step=%s exception=%s",
            getattr(github, "evidence_step", "client"),
            type(error).__name__,
        )
        raise PolicyError(
            "github", "complete GitHub merge evidence unavailable"
        ) from None
    async with db.connection() as conn:
        current, fresh_name = await authority(
            conn, binding, body.project_id, facts["head"]
        )
    if (
        fresh_name != name
        or project["kagent_session_id"] != current["kagent_session_id"]
    ):
        raise PolicyError("ownership", "project or runtime identity changed")
    facts.update(
        project_id=body.project_id,
        policy=current["merge_policy"],
        policy_version=current["merge_policy_version"],
        globs_version=PROTECTED_GLOBS_VERSION,
        **publication.task_facts(current),
    )
    facts["route"] = (
        "approval"
        if facts["policy"] == "approval" or facts["protected_matches"]
        else "auto"
    )
    return current, facts


async def lock_candidate(conn, owner, project_id, repository_id, number):
    # Same order in preparation, decision recording, policy updates and merge claim.
    await conn.fetchrow(
        "SELECT id FROM projects WHERE id=$1 AND user_id=$2 FOR UPDATE",
        project_id,
        owner,
    )
    await conn.execute(
        "SELECT pg_advisory_xact_lock(hashtextextended($1,0))",
        f"merge:{owner}:{repository_id}:{number}",
    )


async def rejection(conn, candidate_id):
    return await conn.fetchval(
        """SELECT 1 FROM native_hitl_response_members m JOIN merge_proposals p
        ON p.id=m.call_snapshot->'merge_key'->>'proposal_id'
        WHERE p.candidate_id=$1 AND m.owner_id=p.owner_id
        AND m.call_snapshot->'merge_key'->>'operation'='mainloop.merge_pull_request_with_approval.v1'
        AND m.call_snapshot->>'approved'='false' LIMIT 1""",
        candidate_id,
    )


async def proposal(conn, owner, proposal_id):
    row = await conn.fetchrow(
        "SELECT * FROM merge_proposals WHERE owner_id=$1 AND id=$2", owner, proposal_id
    )
    if not row:
        raise PolicyError("proposal", "unknown owned merge proposal")
    value = dict(row)
    return {
        **value,
        "facts": decode(value["facts"]),
        "presentation": (
            decode(value.get("presentation")) if value.get("presentation") else None
        ),
    }


def prepared_result(p):
    facts = {
        key: value
        for key, value in p["facts"].items()
        if key not in ("description", "files")
    }
    return {
        "text": f"Merge proposal {p['id']}; required route: {p['facts']['route']}",
        "state": "prepared",
        "proposal_id": p["id"],
        **facts,
        "summary": p.get("presentation"),
        "summary_digest": p.get("summary_digest"),
    }


@publication.guarded(before=require_enabled, schema=PreparePullRequestMerge)
async def prepare(binding, arguments):
    require_enabled()
    body = PreparePullRequestMerge.model_validate(arguments)
    owner = binding["user_id"]
    digest = normalized_hash({**body.model_dump(), "binding_id": binding["session_id"]})
    async with db.connection() as conn:
        await authority(conn, binding, body.project_id)
        prior = await conn.fetchrow(
            "SELECT * FROM merge_tool_requests WHERE owner_id=$1 AND request_id=$2",
            owner,
            body.request_id,
        )
        if prior:
            if prior["payload_hash"] != digest:
                raise PolicyError("conflict", "request ID has different arguments")
            p = await proposal(conn, owner, prior["proposal_id"])
            await validate_binding(conn, binding, p)
            return prepared_result(p)
    project, facts = await read_evidence(binding, body)
    # Capture the template mapping reference when the immutable proposal is made.
    # The same trusted reads run again when the owner responds. With an empty config,
    # the resolver returns before contacting kagent.
    from mainloop.services.merge_authorization import resolve_template_mapping

    async with db.connection() as conn:
        resolved = await resolve_template_mapping(
            conn,
            owner,
            binding["session_id"],
            project["kagent_session_id"],
        )
    facts["mapping_evidence"] = (
        resolved[1].model_dump(mode="json") if resolved else None
    )
    async with db.connection() as conn, conn.transaction():
        await task_store.admission_lock(conn)
        await lock_candidate(
            conn, owner, body.project_id, facts["repository_id"], body.pr_number
        )
        # IDs serialize independently of candidate identity.
        await conn.execute(
            "SELECT pg_advisory_xact_lock(hashtextextended($1,0))",
            f"merge-request:{owner}:{body.request_id}",
        )
        prior = await conn.fetchrow(
            "SELECT * FROM merge_tool_requests WHERE owner_id=$1 AND request_id=$2",
            owner,
            body.request_id,
        )
        if prior:
            if prior["payload_hash"] != digest:
                raise PolicyError("conflict", "request ID has different arguments")
            p = await proposal(conn, owner, prior["proposal_id"])
            await validate_binding(conn, binding, p)
            return prepared_result(p)
        current, name = await authority(conn, binding, body.project_id, facts["head"])
        if (
            name != facts["repository"]
            or current["merge_policy_version"] != facts["policy_version"]
            or current["kagent_session_id"] != project["kagent_session_id"]
        ):
            raise PolicyError("stale", "policy or authority changed during preparation")
        rows = await conn.fetch(
            "SELECT * FROM merge_requests WHERE owner_id=$1 AND repository_id=$2 AND pr_number=$3",
            owner,
            facts["repository_id"],
            body.pr_number,
        )
        candidate = next((r for r in rows if r["head_sha"] == body.expected_sha), None)
        if any(r["state"] in ("merging", "uncertain", "merged") for r in rows):
            raise PolicyError(
                "intent", "existing merge intent permits reconciliation only"
            )
        if candidate and candidate["project_id"] != body.project_id:
            raise PolicyError(
                "ownership", "candidate is already bound to another project"
            )
        if candidate and await rejection(conn, candidate["id"]):
            raise PolicyError("rejected", "owner rejected this candidate head")
        if candidate and candidate["state"] in ("evaluating", "superseded"):
            raise PolicyError(
                "attempt", "prior attempt is active or head is superseded"
            )
        cid = candidate["id"] if candidate else str(uuid.uuid4())
        pid = str(uuid.uuid4())
        presentation, summary_digest = build_summary(facts, pid)
        await conn.execute(
            "UPDATE merge_requests SET state='superseded' WHERE owner_id=$1 AND repository_id=$2 AND pr_number=$3 AND head_sha<>$4 AND state IN ('prepared','evaluating','blocked','expired')",
            owner,
            facts["repository_id"],
            body.pr_number,
            body.expected_sha,
        )
        if not candidate:
            await conn.execute(
                "INSERT INTO merge_requests(id,owner_id,project_id,repository_id,pr_number,head_sha,state) VALUES($1,$2,$3,$4,$5,$6,'prepared')",
                cid,
                owner,
                body.project_id,
                facts["repository_id"],
                body.pr_number,
                body.expected_sha,
            )
        await conn.execute(
            "INSERT INTO merge_proposals(id,candidate_id,owner_id,binding_id,runtime_session_id,facts,presentation,summary_digest) VALUES($1,$2,$3,$4,$5,$6::jsonb,$7::jsonb,$8)",
            pid,
            cid,
            owner,
            binding["session_id"],
            project["kagent_session_id"],
            json.dumps(facts),
            json.dumps(presentation),
            summary_digest,
        )
        await conn.execute(
            "UPDATE merge_requests SET active_proposal_id=$2,state='prepared',deadline=NULL,receipt_action_id=NULL,intent_invocation_id=NULL,result=NULL WHERE id=$1",
            cid,
            pid,
        )
        await conn.execute(
            "INSERT INTO merge_tool_requests VALUES($1,$2,$3,$4)",
            owner,
            body.request_id,
            digest,
            pid,
        )
        p = await proposal(conn, owner, pid)
        await publication.attach_proposal(conn, binding, p)
        return prepared_result(p)


async def validate_binding(conn, binding, p):
    current, name = await authority(
        conn, binding, p["facts"]["project_id"], p["facts"]["head"]
    )
    if (
        p["binding_id"] != binding["session_id"]
        or p["runtime_session_id"] != current["kagent_session_id"]
        or p["facts"]["repository"] != name
        or publication.task_facts(current)
        != {
            key: p["facts"][key]
            for key in ("task_id", "attempt_id", "workspace_id", "writer_generation")
            if key in p["facts"]
        }
    ):
        raise PolicyError("ownership", "proposal belongs to another binding or runtime")
    return current


def merged_result(p, sha, source):
    facts = p["facts"]
    url = f"https://github.com/{facts['repository']}/pull/{facts['pr_number']}"
    return {
        "text": f"Merged PR {facts['pr_number']}: {url}",
        "state": "merged",
        "proposal_id": p["id"],
        "pr_number": facts["pr_number"],
        "url": url,
        "head_sha": facts["head_sha"],
        "base": facts["base"],
        "merge_sha": sha,
        "outcome_source": source,
    }


def state_result(state, pid, deadline=None, *, approved=False):
    text = f"Merge {state}; proposal {pid}."
    if state == "evaluating" and approved:
        text += (
            f" Mainloop will complete this exact merge under the original consent if CI "
            f"and all gates pass before {deadline.isoformat()}. Do not call the merge "
            "tool again; use get_pull_request_merge_status with the same proposal_id "
            "and request_id to read the outcome."
        )
    elif state in ("uncertain", "merging"):
        text += " Mainloop will reconcile read-only; no merge PUT will be repeated."
    elif state == "evaluating":
        text += " Reuse the same invocation ID to re-evaluate."
    return {
        "text": text,
        "state": state,
        "proposal_id": pid,
        "deadline": deadline.isoformat() if deadline else None,
    }


async def project_state(conn, pid, state):
    """Project stored evaluation state only onto its exact current task attempt."""
    from mainloop.tasks import attention, lifecycle
    from mainloop.tasks.principal import TaskPrincipal
    from mainloop.tasks.projection import persist

    p = await conn.fetchrow(
        "SELECT owner_id,facts FROM merge_proposals WHERE id=$1", pid
    )
    facts = decode(p["facts"])
    if not facts.get("task_id"):
        return
    task = await task_store.get_task(
        conn, facts["task_id"], TaskPrincipal(p["owner_id"])
    )
    attempt = await lifecycle.load_attempt(conn, facts["attempt_id"])
    if (
        task.current_attempt_id != facts["attempt_id"]
        or attempt.workspace_id != facts["workspace_id"]
        or attempt.writer_generation != facts["writer_generation"]
    ):
        return
    previous = await task_store.projection(conn, task.id)
    if previous.merge_proposal_id != pid:
        return
    ids = previous.pending_approval_ids
    if state in ("blocked", "expired") and attempt.state == "active":
        runtime = await conn.fetchval(
            "SELECT kagent_session_id FROM native_bindings WHERE session_id=$1",
            attempt.binding_id,
        )
        if runtime:
            # Recover links that an earlier projection dropped for stale merge
            # eligibility. Unanswered native retries still require attention.
            ids = tuple(
                sorted(
                    set(ids)
                    | set(await attention.pending(conn, task, attempt, runtime))
                )
            )
    if state in ("blocked", "expired") and ids:
        # Remove only canonical views tied to this merge's recorded response,
        # and only if all their leaves were answered. Preserve other questions,
        # mixed requests with unanswered leaves and IDs lacking such evidence.
        answered = await conn.fetch(
            """SELECT DISTINCT a.request_id FROM native_hitl_aliases a
               JOIN native_hitl_response_members m ON m.leaf_key=a.leaf_key
               WHERE a.request_id=ANY($1::text[]) AND m.owner_id=$2
                 AND m.call_snapshot->'merge_key'->>'proposal_id'=$3
                 AND m.call_snapshot->'merge_key'->>'operation'=$4
                 AND NOT EXISTS (
                   SELECT 1 FROM native_hitl_aliases remaining
                   LEFT JOIN native_hitl_response_members response
                     ON response.leaf_key=remaining.leaf_key AND response.owner_id=$2
                   WHERE remaining.request_id=a.request_id AND response.leaf_key IS NULL
                 )""",
            ids,
            p["owner_id"],
            pid,
            MERGE_OPERATION,
        )
        removed = {row["request_id"] for row in answered}
        ids = tuple(value for value in ids if value not in removed)
    value = previous.model_copy(
        update={
            "merge_state": state,
            "pending_approval_ids": ids,
        }
    )
    await persist(conn, task, value, f"merge-state:{pid}:{state}", attention=True)


async def notify_owner(conn, candidate, result):
    thread = await conn.fetchval(
        "SELECT id FROM main_threads WHERE user_id=$1 ORDER BY created_at LIMIT 1",
        candidate["owner_id"],
    )
    if not thread:
        thread = str(
            uuid.uuid5(uuid.NAMESPACE_URL, f"main-thread:{candidate['owner_id']}")
        )
        await conn.execute(
            "INSERT INTO main_threads(id,user_id) VALUES($1,$2) ON CONFLICT DO NOTHING",
            thread,
            candidate["owner_id"],
        )
    await conn.execute(
        """INSERT INTO queue_items(id,main_thread_id,user_id,item_type,title,content,context)
        VALUES($1,$2,$3,'notification',$4,$5,$6::jsonb) ON CONFLICT DO NOTHING""",
        f"merge-outcome:{candidate['intent_id'] or result['proposal_id']}",
        thread,
        candidate["owner_id"],
        f"Pull request {result['state']}",
        result["text"],
        json.dumps(result),
    )


async def remember_result(conn, pid, result):
    await conn.execute(
        """INSERT INTO merge_proposal_results(proposal_id,deadline,result)
        SELECT $1,deadline,$2::jsonb FROM merge_requests WHERE active_proposal_id=$1
        ON CONFLICT DO NOTHING""",
        pid,
        json.dumps(result),
    )
    stored = await conn.fetchval(
        "SELECT result FROM merge_proposal_results WHERE proposal_id=$1", pid
    )
    result = decode(stored) if stored else result
    if result["state"] in ("blocked", "expired"):
        await project_state(conn, pid, result["state"])
        candidate = await conn.fetchrow(
            "SELECT * FROM merge_requests WHERE active_proposal_id=$1", pid
        )
        if candidate:
            await notify_owner(conn, candidate, result)
    return result


async def finish(conn, candidate, result):
    # Outcome and deterministic informational inbox publication share one transaction.
    prior = await conn.fetchval(
        "SELECT result FROM merge_requests WHERE id=$1 AND state='merged'",
        candidate["id"],
    )
    if prior:
        return decode(prior)
    await publication.settle_merge(conn, candidate, result)
    await conn.execute(
        "UPDATE merge_requests SET state='merged',result=$2::jsonb WHERE id=$1",
        candidate["id"],
        json.dumps(result),
    )
    result = await remember_result(conn, candidate["active_proposal_id"], result)
    await notify_owner(conn, candidate, result)
    return result


async def reconcile(binding, p, candidate):
    if candidate["state"] == "merged":
        return decode(candidate["result"])
    # A matching PR reports an outcome, not proof of which actor merged it. Never
    # resend a PUT, including a crash between intent commit and network dispatch.
    facts = p["facts"]
    try:
        async with GitHubMergeClient(facts["repository"]) as github:
            pr = await github.pull(facts["repository"], facts["pr_number"])
            ci = (
                await github.checks(
                    facts["repository"], facts["head_sha"], facts["base"]
                )
                if facts.get("task_id")
                else None
            )
        if (
            pr.merged
            and pr.head.sha == facts["head_sha"]
            and pr.head.ref == facts["head"]
            and pr.base.ref == facts["base"]
            and pr.number == facts["pr_number"]
            and pr.merge_commit_sha
            and re.fullmatch(r"[0-9a-f]{40}", pr.merge_commit_sha)
            and all(
                ref.repo.id == facts["repository_id"]
                and ref.repo.full_name.lower() == facts["repository"].lower()
                for ref in (pr.head, pr.base)
            )
        ):
            result = merged_result(p, pr.merge_commit_sha, "github_pr_observation")
            if facts.get("task_id"):
                if ci_state(ci, facts["head_sha"]) != "success":
                    return state_result("uncertain", p["id"])
                result["ci_observation"] = ci
            async with db.connection() as conn, conn.transaction():
                await task_store.admission_lock(conn)
                await lock_candidate(
                    conn,
                    binding["user_id"],
                    facts["project_id"],
                    facts["repository_id"],
                    facts["pr_number"],
                )
                result = await finish(conn, candidate, result)
            return result
    except (GitHubError, ValidationError, PolicyError):
        pass
    return state_result("uncertain", p["id"])


async def completed_replay(binding, arguments, *, approved, reevaluate=False):
    """Return an immutable outcome after product completion, without execution.

    Completion closes submit authority. A lost tool response must still be
    recoverable by the same live binding/current writer; this never claims an
    intent or sends a PUT. Superseded/revoked sources still fail closed.
    """
    async with db.connection() as conn:
        row = await conn.fetchrow(
            """SELECT p.id,p.runtime_session_id,r.result FROM merge_proposals p
               JOIN merge_proposal_results r ON r.proposal_id=p.id
               JOIN native_bindings b ON b.session_id=p.binding_id
               JOIN sessions s ON s.id=b.session_id
               JOIN task_attempts a ON a.binding_id=b.session_id
               JOIN tasks t ON t.id=a.task_id
               JOIN workspace_writer_claims c ON c.attempt_id=a.id
               WHERE p.id=$1 AND p.owner_id=$2 AND p.binding_id=$3
                 AND b.token_hash=$4 AND b.kagent_session_id=p.runtime_session_id
                 AND s.user_id=$2 AND t.owner_id=$2
                 AND b.kagent_deleted_at IS NULL AND s.archived_at IS NULL
                 AND t.current_attempt_id=a.id AND t.status='completed'
                 AND p.facts->>'attempt_id'=a.id AND a.state='active'
                 AND c.held AND c.generation=a.writer_generation
                 AND p.facts->>'writer_generation'=c.generation::text
                 AND p.facts->>'workspace_id'=a.workspace_id
                 AND r.result->>'state'='merged'""",
            arguments["proposal_id"],
            binding["user_id"],
            binding["session_id"],
            binding.get("token_hash"),
        )
        if row is None:
            return None
        from mainloop.tasks import lifecycle

        try:
            await lifecycle.authenticate_binding(
                conn, binding, allow_completed_read=True
            )
        except (lifecycle.LifecycleDenied, ValueError):
            return None
        digest = normalized_hash(arguments)
        prior = await conn.fetchrow(
            "SELECT payload_hash FROM merge_tool_invocations WHERE owner_id=$1 AND request_id=$2",
            binding["user_id"],
            arguments["request_id"],
        )
        if prior and prior["payload_hash"] != digest:
            raise PolicyError("conflict", "invocation ID has different arguments")
        if approved:
            key = MergeReceiptKey(
                owner_id=binding["user_id"],
                leaf_binding_id=binding["session_id"],
                leaf_runtime_session_id=row["runtime_session_id"],
                proposal_id=row["id"],
                invocation_request_id=arguments["request_id"],
            )
            if not await lookup_merge_receipt(conn, key, digest):
                raise PolicyError("consent", "exact owner decision receipt required")
        return decode(row["result"])


@publication.guarded(
    before=require_enabled, schema=MergePullRequestWithApproval, replay=completed_replay
)
async def execute_once(binding, arguments, *, approved, reevaluate=False):
    require_enabled()
    body = MergePullRequestWithApproval.model_validate(arguments)
    owner = binding["user_id"]
    digest = normalized_hash(body.model_dump())
    async with db.connection() as conn:
        p = await proposal(conn, owner, body.proposal_id)
        await validate_binding(conn, binding, p)
    facts = p["facts"]
    key = MergeReceiptKey(
        owner_id=owner,
        leaf_binding_id=binding["session_id"],
        leaf_runtime_session_id=p["runtime_session_id"],
        proposal_id=p["id"],
        invocation_request_id=body.request_id,
    )
    async with db.connection() as conn, conn.transaction():
        await task_store.admission_lock(conn)
        await lock_candidate(
            conn, owner, facts["project_id"], facts["repository_id"], facts["pr_number"]
        )
        current = await validate_binding(conn, binding, p)
        candidate = await conn.fetchrow(
            "SELECT * FROM merge_requests WHERE id=$1", p["candidate_id"]
        )
        receipt = await lookup_merge_receipt(conn, key, digest) if approved else None
        if approved and not receipt:
            raise PolicyError("consent", "exact owner decision receipt required")
        await conn.execute(
            "SELECT pg_advisory_xact_lock(hashtextextended($1,0))",
            f"merge-invocation:{owner}:{body.request_id}",
        )
        prior = await conn.fetchrow(
            "SELECT * FROM merge_tool_invocations WHERE owner_id=$1 AND request_id=$2",
            owner,
            body.request_id,
        )
        if prior and prior["payload_hash"] != digest:
            raise PolicyError("conflict", "invocation ID has different arguments")
        if not prior:
            await conn.execute(
                "INSERT INTO merge_tool_invocations VALUES($1,$2,$3,$4)",
                owner,
                body.request_id,
                digest,
                p["id"],
            )
        stored_result = await conn.fetchval(
            "SELECT result FROM merge_proposal_results WHERE proposal_id=$1", p["id"]
        )
        if stored_result:
            return decode(stored_result)
        if candidate["active_proposal_id"] != p["id"]:
            raise PolicyError("stale", "merge proposal was replaced")
        if candidate["state"] in ("merging", "uncertain", "merged"):
            existing = dict(candidate)
        else:
            existing = None
            if not approved and (
                facts["route"] != "auto"
                or current["merge_policy"] != "auto"
                or facts["protected_matches"]
            ):
                raise PolicyError(
                    "approval_required",
                    "use the protected merge tool with this proposal",
                )
            if candidate["active_proposal_id"] != p["id"] or await rejection(
                conn, candidate["id"]
            ):
                raise PolicyError("stale", "proposal replaced or candidate rejected")
            if candidate["state"] in ("blocked", "expired", "superseded"):
                result = state_result(candidate["state"], p["id"])
                if candidate["state"] in ("blocked", "expired"):
                    return await remember_result(conn, p["id"], result)
                return result
            if candidate["deadline"] and candidate["deadline"] <= datetime.now(
                timezone.utc
            ):
                await conn.execute(
                    "UPDATE merge_requests SET state='expired' WHERE id=$1",
                    candidate["id"],
                )
                return await remember_result(
                    conn, p["id"], state_result("expired", p["id"])
                )
            if approved and candidate["state"] == "evaluating" and not reevaluate:
                return state_result(
                    "evaluating", p["id"], candidate["deadline"], approved=True
                )
            candidate = await conn.fetchrow(
                """UPDATE merge_requests SET state='evaluating',
                   deadline=COALESCE(deadline,now()+interval '30 minutes'),
                   receipt_action_id=COALESCE(receipt_action_id,$2),
                   intent_invocation_id=COALESCE(intent_invocation_id,$3)
                   WHERE id=$1 RETURNING *""",
                candidate["id"],
                receipt.action_id if receipt else None,
                body.request_id if approved else None,
            )
            await project_state(conn, p["id"], "evaluating")
    if existing:
        return await reconcile(binding, p, existing)
    try:
        _, fresh = await read_evidence(
            binding,
            PreparePullRequestMerge(
                project_id=facts["project_id"],
                pr_number=facts["pr_number"],
                expected_sha=facts["head_sha"],
                request_id=body.request_id,
            ),
        )
    except PolicyError:
        async with db.connection() as conn, conn.transaction():
            await task_store.admission_lock(conn)
            await lock_candidate(
                conn,
                owner,
                facts["project_id"],
                facts["repository_id"],
                facts["pr_number"],
            )
            closed = await conn.fetchval(
                "UPDATE merge_requests SET state='blocked' WHERE id=$1 AND state='evaluating' AND active_proposal_id=$2 RETURNING id",
                p["candidate_id"],
                p["id"],
            )
            if closed:
                return await remember_result(
                    conn, p["id"], state_result("blocked", p["id"])
                )
        raise
    async with db.connection() as conn, conn.transaction():
        await task_store.admission_lock(conn)
        await lock_candidate(
            conn, owner, facts["project_id"], facts["repository_id"], facts["pr_number"]
        )
        current = await validate_binding(conn, binding, p)
        candidate = await conn.fetchrow(
            "SELECT * FROM merge_requests WHERE id=$1", p["candidate_id"]
        )
        stored_result = await conn.fetchval(
            "SELECT result FROM merge_proposal_results WHERE proposal_id=$1", p["id"]
        )
        if stored_result:
            return decode(stored_result)
        if candidate["active_proposal_id"] != p["id"]:
            raise PolicyError("stale", "merge proposal was replaced")
        if candidate["state"] in ("merging", "uncertain", "merged"):
            existing = dict(candidate)
        else:
            existing = None
            if candidate["active_proposal_id"] != p["id"] or await rejection(
                conn, candidate["id"]
            ):
                raise PolicyError("stale", "proposal replaced or rejected")
            if candidate["state"] != "evaluating":
                result = state_result(candidate["state"], p["id"])
                if candidate["state"] in ("blocked", "expired"):
                    return await remember_result(conn, p["id"], result)
                return result
            state = None
            if candidate["deadline"] <= datetime.now(timezone.utc):
                state = "expired"
            elif (
                pinned(facts) != pinned(fresh)
                or current["merge_policy_version"] != facts["policy_version"]
            ):
                state = "blocked"
            elif ci_state(fresh["ci"], facts["head_sha"]) == "failure":
                state = "blocked"
            elif (
                ci_state(fresh["ci"], facts["head_sha"]) != "success"
                or not fresh["mergeable"]
            ):
                if fresh["ci"].get("pending") or not fresh["mergeable"]:
                    return state_result(
                        "evaluating", p["id"], candidate["deadline"], approved=approved
                    )
                if ci_state(fresh["ci"], facts["head_sha"]) == "unknown":
                    return state_result(
                        "evaluating", p["id"], candidate["deadline"], approved=approved
                    )
                state = "blocked"
            if state:
                await conn.execute(
                    "UPDATE merge_requests SET state=$2 WHERE id=$1",
                    candidate["id"],
                    state,
                )
                return await remember_result(
                    conn, p["id"], state_result(state, p["id"])
                )
            if approved and not await lookup_merge_receipt(conn, key, digest):
                raise PolicyError("consent", "exact consent unavailable at claim")
            intent = str(uuid.uuid4())
            candidate = dict(
                await conn.fetchrow(
                    "UPDATE merge_requests SET state='uncertain',intent_id=$2,receipt_action_id=$3,intent_invocation_id=$4,result=$5::jsonb WHERE id=$1 RETURNING *",
                    candidate["id"],
                    intent,
                    receipt.action_id if receipt else None,
                    body.request_id,
                    json.dumps(
                        {
                            "claim_evidence": {
                                "head_sha": facts["head_sha"],
                                "ci_state": ci_state(fresh["ci"], facts["head_sha"]),
                                "ci": fresh["ci"],
                            }
                        }
                    ),
                )
            )
    if existing:
        return await reconcile(binding, p, existing)
    # Durable uncertainty precedes the only network write. Cancellation/crash is
    # conservative even if no bytes were sent; a lease cannot authorize a replay.
    try:
        async with GitHubMergeClient(facts["repository"]) as github:
            result = await github.merge(
                facts["repository"], facts["pr_number"], facts["head_sha"]
            )
        if (
            result.get("merged") is not True
            or not isinstance(result.get("sha"), str)
            or re.fullmatch(r"[0-9a-f]{40}", result["sha"]) is None
        ):
            return state_result("uncertain", p["id"])
    except (GitHubError, ValidationError, AttributeError):
        return state_result("uncertain", p["id"])
    outcome = merged_result(p, result["sha"], "merge_response")
    try:
        async with db.connection() as conn, conn.transaction():
            await task_store.admission_lock(conn)
            await lock_candidate(
                conn,
                owner,
                facts["project_id"],
                facts["repository_id"],
                facts["pr_number"],
            )
            outcome = await finish(conn, candidate, outcome)
    except PolicyError:
        # The remote write may already have succeeded. Keep its durable intent
        # for read-only reconciliation; never manufacture task completion.
        return state_result("uncertain", p["id"])
    return outcome


async def status(binding, arguments):
    """Bound stored-result reads and return a retriable failure on contention."""
    try:
        async with asyncio.timeout(STATUS_BUDGET_SECONDS):
            return await _status(binding, arguments)
    except TimeoutError:
        raise PolicyError(
            "status_timeout",
            "Merge status read timed out. Retry get_pull_request_merge_status with the "
            "same proposal_id and request_id; Mainloop's merge continuation is unaffected.",
        ) from None


async def _status(binding, arguments):
    """Read persisted state without GitHub access, approval, or merge execution."""
    require_enabled()
    body = MergePullRequestWithApproval.model_validate(arguments)
    replay = await completed_replay(binding, body.model_dump(), approved=True)
    if replay is not None:
        return replay
    async with db.connection() as conn:
        p = await proposal(conn, binding["user_id"], body.proposal_id)
        await validate_binding(conn, binding, p)
        invocation = await conn.fetchrow(
            "SELECT * FROM merge_tool_invocations WHERE owner_id=$1 AND request_id=$2",
            binding["user_id"],
            body.request_id,
        )
        if not invocation or invocation["payload_hash"] != normalized_hash(
            body.model_dump()
        ):
            raise PolicyError("invocation", "exact existing merge invocation required")
        stored = await conn.fetchval(
            "SELECT result FROM merge_proposal_results WHERE proposal_id=$1", p["id"]
        )
        if stored:
            return decode(stored)
        candidate = await conn.fetchrow(
            "SELECT * FROM merge_requests WHERE id=$1", p["candidate_id"]
        )
        if candidate["active_proposal_id"] != p["id"]:
            return state_result("superseded", p["id"])
        return state_result(
            candidate["state"],
            p["id"],
            candidate["deadline"],
            approved=bool(candidate["receipt_action_id"]),
        )


async def completed_status_binding(binding):
    """Allow only immutable merge-result reads for an exact completed writer."""
    async with db.connection() as conn:
        rows = await conn.fetch(
            """SELECT p.id,c.intent_invocation_id,a.depth FROM merge_proposals p
               JOIN merge_requests c ON c.active_proposal_id=p.id
               JOIN task_attempts a ON a.id=p.facts->>'attempt_id'
               JOIN tasks t ON t.id=a.task_id
               WHERE p.owner_id=$1 AND p.binding_id=$2 AND c.state='merged'
                 AND t.current_attempt_id=a.id AND t.status='completed'
               ORDER BY p.created_at DESC LIMIT 1""",
            binding["user_id"],
            binding["session_id"],
        )
    for row in rows:
        if row["intent_invocation_id"] and await completed_replay(
            binding,
            {"proposal_id": row["id"], "request_id": row["intent_invocation_id"]},
            approved=True,
        ):
            return row["depth"]
    return None


async def execute(binding, arguments, *, approved, reevaluate=False):
    """Run one bounded pass; PostgreSQL retains consent and work across restarts."""
    try:
        async with asyncio.timeout(EVALUATION_BUDGET_SECONDS):
            return await execute_once(
                binding, arguments, approved=approved, reevaluate=reevaluate
            )
    except TimeoutError:
        # Cancellation before intent commit leaves evaluation eligible. After
        # commit it leaves uncertainty, even if dispatch never reached GitHub.
        return await status(binding, arguments)


async def close_evaluation(binding, pid):
    """Fail closed after authority refusal without touching a replacement attempt."""
    async with db.connection() as conn, conn.transaction():
        await task_store.admission_lock(conn)
        p = await proposal(conn, binding["user_id"], pid)
        facts = p["facts"]
        await lock_candidate(
            conn,
            binding["user_id"],
            facts["project_id"],
            facts["repository_id"],
            facts["pr_number"],
        )
        row = await conn.fetchrow(
            """UPDATE merge_requests SET state=CASE WHEN deadline<=now() THEN 'expired' ELSE 'blocked' END
               WHERE id=$1 AND active_proposal_id=$2 AND state='evaluating' RETURNING state""",
            p["candidate_id"],
            pid,
        )
        if row:
            return await remember_result(conn, pid, state_result(row["state"], pid))


async def reconcile_approved_merges():
    """Consume one due persisted consent through the existing backend reconciler."""
    if not enabled():
        return
    # Touch before work: interruption rotates behind untouched candidates across
    # processes/restarts. This is scheduling metadata, never a dispatch lease.
    async with db.connection() as conn, conn.transaction():
        row = await conn.fetchrow(
            """SELECT c.*,p.binding_id,consent.request_id AS original_request_id
               FROM merge_requests c JOIN merge_proposals p
               ON p.id=c.active_proposal_id
               JOIN LATERAL (
                   SELECT i.request_id FROM merge_tool_invocations i
                   JOIN native_hitl_response_members m ON m.owner_id=i.owner_id
                     AND m.call_snapshot->'merge_key'=jsonb_build_object(
                       'owner_id',p.owner_id,'leaf_binding_id',p.binding_id,
                       'leaf_runtime_session_id',p.runtime_session_id,'operation',$2::text,
                       'proposal_id',p.id,'invocation_request_id',i.request_id)
                     AND m.call_snapshot->>'approved'='true'
                     AND m.call_snapshot->>'arguments_hash'=i.payload_hash
                   WHERE i.owner_id=c.owner_id AND i.proposal_id=p.id
                     AND (c.intent_invocation_id IS NULL OR c.intent_invocation_id=i.request_id)
                     AND (c.receipt_action_id IS NULL OR c.receipt_action_id=m.action_id)
                   ORDER BY i.request_id LIMIT 1
               ) consent ON true
               WHERE c.state IN ('evaluating','uncertain','merging')
                 AND (c.deadline<=now() AND c.state='evaluating' OR
                      COALESCE((c.result->>'continuation_checked_at')::timestamptz,'epoch')
                      <=now()-$1::double precision*interval '1 second')
               ORDER BY COALESCE((c.result->>'continuation_checked_at')::timestamptz,'epoch'),c.id
               LIMIT 1 FOR UPDATE OF c SKIP LOCKED""",
            CI_POLL_SECONDS,
            MERGE_OPERATION,
        )
        if row is None:
            return
        await conn.execute(
            """UPDATE merge_requests SET result=jsonb_set(COALESCE(result,'{}'::jsonb),
               '{continuation_checked_at}',to_jsonb(now())) WHERE id=$1""",
            row["id"],
        )
        binding = await conn.fetchrow(
            "SELECT b.*,s.user_id FROM native_bindings b JOIN sessions s ON s.id=b.session_id WHERE b.session_id=$1",
            row["binding_id"],
        )
    if binding is None:
        binding = {"user_id": row["owner_id"], "session_id": row["binding_id"]}
    try:
        return await execute(
            dict(binding),
            {
                "proposal_id": row["active_proposal_id"],
                "request_id": row["original_request_id"],
            },
            approved=True,
            reevaluate=True,
        )
    except PolicyError as error:
        if error.code == "status_timeout":
            # A contended fallback read says nothing about consent or authority.
            # Keep the durable evaluation eligible for a later server pass.
            return None
        return await close_evaluation(binding, row["active_proposal_id"])


async def completed_auto_replay(binding, body):
    async with db.connection() as conn:
        prior = await conn.fetchrow(
            "SELECT payload_hash,proposal_id FROM merge_tool_requests WHERE owner_id=$1 AND request_id=$2",
            binding["user_id"],
            body.request_id,
        )
    if prior is None:
        return None
    result = await completed_replay(
        binding,
        {"proposal_id": prior["proposal_id"], "request_id": body.request_id},
        approved=False,
    )
    if result is None:
        return None
    digest = normalized_hash({**body.model_dump(), "binding_id": binding["session_id"]})
    if prior["payload_hash"] != digest:
        raise PolicyError("conflict", "request ID has different arguments")
    return result


async def auto_merge(binding, arguments):
    require_enabled()
    body = PreparePullRequestMerge.model_validate(arguments)
    replay = await completed_auto_replay(binding, body)
    if replay is not None:
        return replay
    try:
        result = await prepare(binding, arguments)
    except PolicyError:
        # A competing call may finish while preparation waits for the tree lock.
        replay = await completed_auto_replay(binding, body)
        if replay is not None:
            return replay
        raise
    if result["route"] == "approval":
        async with db.connection() as conn:
            p = await proposal(conn, binding["user_id"], result["proposal_id"])
            candidate = await conn.fetchrow(
                "SELECT * FROM merge_requests WHERE id=$1", p["candidate_id"]
            )
        if candidate["active_proposal_id"] == p["id"] and candidate["state"] in (
            "merging",
            "uncertain",
            "merged",
        ):
            async with publication.guard(db, binding, p["facts"]["project_id"]):
                return await reconcile(binding, p, dict(candidate))
        return {
            **result,
            "state": "approval_required",
            "tool": "merge_pull_request_with_approval",
        }
    return await execute(
        binding,
        {"proposal_id": result["proposal_id"], "request_id": arguments["request_id"]},
        approved=False,
    )
