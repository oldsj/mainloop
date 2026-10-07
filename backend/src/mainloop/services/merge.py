"""Merge proposals and at-most-once dispatch. PostgreSQL locks never span HTTP."""

import asyncio
import json
import os
import re
import uuid
from datetime import datetime, timezone

from mainloop.db import db
from mainloop.db.hitl import lookup_merge_receipt
from mainloop.runtime.policy import PolicyError
from mainloop.services.github_creation import GitHubError
from mainloop.services.github_merge import GitHubMergeClient
from mainloop.services.merge_summary import build_summary
from mainloop.services.workspace_authority import (
    ScopeUnavailable,
    resolve_project_authority,
)
from pydantic import ValidationError

from models.agent_tools import MergePullRequestWithApproval, PreparePullRequestMerge
from models.hitl import MergeReceiptKey, normalized_hash
from models.merge_policy import PROTECTED_GLOBS_VERSION


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
    try:
        async with asyncio.timeout(60), GitHubMergeClient() as github:
            facts = await github.evidence(name, body.pr_number, body.expected_sha)
    except (
        GitHubError,
        ValidationError,
        KeyError,
        TypeError,
        ValueError,
        TimeoutError,
    ):
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
            return prepared_result(await proposal(conn, owner, prior["proposal_id"]))
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
            "UPDATE merge_requests SET active_proposal_id=$2,state='prepared',deadline=NULL WHERE id=$1",
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
        return prepared_result(await proposal(conn, owner, pid))


async def validate_binding(conn, binding, p):
    current, name = await authority(
        conn, binding, p["facts"]["project_id"], p["facts"]["head"]
    )
    if (
        p["binding_id"] != binding["session_id"]
        or p["runtime_session_id"] != current["kagent_session_id"]
        or p["facts"]["repository"] != name
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


def state_result(state, pid):
    return {
        "text": f"Merge {state}; proposal {pid}. Reuse the same invocation ID to reconcile.",
        "state": state,
        "proposal_id": pid,
    }


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
    return decode(stored) if stored else result


async def finish(conn, candidate, result):
    # Outcome and deterministic informational inbox publication share one transaction.
    prior = await conn.fetchval(
        "SELECT result FROM merge_requests WHERE id=$1 AND state='merged'",
        candidate["id"],
    )
    if prior:
        return decode(prior)
    await conn.execute(
        "UPDATE merge_requests SET state='merged',result=$2::jsonb WHERE id=$1",
        candidate["id"],
        json.dumps(result),
    )
    result = await remember_result(conn, candidate["active_proposal_id"], result)
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
        VALUES($1,$2,$3,'notification','Pull request merged',$4,$5::jsonb) ON CONFLICT DO NOTHING""",
        f"merge-outcome:{candidate['intent_id']}",
        thread,
        candidate["owner_id"],
        result["text"],
        json.dumps(result),
    )
    return result


async def reconcile(binding, p, candidate):
    if candidate["state"] == "merged":
        return decode(candidate["result"])
    # A matching PR reports an outcome, not proof of which actor merged it. Never
    # resend a PUT, including a crash between intent commit and network dispatch.
    facts = p["facts"]
    try:
        async with GitHubMergeClient() as github:
            pr = await github.pull(facts["repository"], facts["pr_number"])
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
            async with db.connection() as conn, conn.transaction():
                await lock_candidate(
                    conn,
                    binding["user_id"],
                    facts["project_id"],
                    facts["repository_id"],
                    facts["pr_number"],
                )
                result = await finish(conn, candidate, result)
            return result
    except (GitHubError, ValidationError):
        pass
    return state_result("uncertain", p["id"])


async def execute(binding, arguments, *, approved):
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
            await conn.execute(
                "UPDATE merge_requests SET state='evaluating',deadline=COALESCE(deadline,now()+interval '30 minutes') WHERE id=$1",
                candidate["id"],
            )
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
            elif not fresh["ci"]["green"] or not fresh["mergeable"]:
                if fresh["ci"].get("pending") or not fresh["mergeable"]:
                    return state_result("evaluating", p["id"])
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
                    "UPDATE merge_requests SET state='uncertain',intent_id=$2,receipt_action_id=$3,intent_invocation_id=$4 WHERE id=$1 RETURNING *",
                    candidate["id"],
                    intent,
                    receipt.action_id if receipt else None,
                    body.request_id,
                )
            )
    if existing:
        return await reconcile(binding, p, existing)
    # Durable uncertainty precedes the only network write. Cancellation/crash is
    # conservative even if no bytes were sent; a lease cannot authorize a replay.
    try:
        async with GitHubMergeClient() as github:
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
    async with db.connection() as conn, conn.transaction():
        await lock_candidate(
            conn, owner, facts["project_id"], facts["repository_id"], facts["pr_number"]
        )
        outcome = await finish(conn, candidate, outcome)
    return outcome


async def auto_merge(binding, arguments):
    result = await prepare(binding, arguments)
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
