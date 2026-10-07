"""Operator-pinned merge configuration and server-resolved proposal enrichment.

No observed tool name, gateway metadata or agent argument imports configuration.
The allowlist is an operator-supplied JSON array of VerifiedLeafConfiguration snapshots,
including evidence references for each retained binding/runtime/prepared revision.
"""

import os

from mainloop.runtime.hitl_correlation import canonical_operation
from mainloop.runtime.policy import PolicyError
from mainloop.services import merge
from pydantic import TypeAdapter, ValidationError

from models.agent_tools import PreparePullRequestMerge
from models.hitl import (
    MergeInvocation,
    ObservedSession,
    ToolApprovalRequest,
    VerifiedLeafConfiguration,
)


async def configuration(conn, owner, leaf, binding):
    if not merge.enabled() or not binding:
        return None
    raw = os.environ.get("MAINLOOP_MERGE_CONFIGURATIONS", "[]")
    if len(raw.encode()) > 131072:
        return None
    try:
        configs = TypeAdapter(list[VerifiedLeafConfiguration]).validate_json(raw)
        if len(configs) > 100:
            return None
        stored = await conn.fetchval(
            "SELECT snapshot FROM native_observed_sessions WHERE gateway=$1 AND runtime_session_id=$2 AND owner_id=$3",
            leaf.gateway,
            leaf.runtime_session_id,
            owner,
        )
        if not stored:
            return None
        session = ObservedSession.model_validate(merge.decode(stored))
        matches = [
            c
            for c in configs
            if c.owner_id == owner
            and c.binding_id == binding
            and c.runtime_session_id == leaf.runtime_session_id
            and c.prepared_revision == session.prepared_revision
        ]
        live = await conn.fetchrow(
            "SELECT b.*,s.user_id FROM native_bindings b JOIN sessions s ON s.id=b.session_id WHERE b.session_id=$1 AND s.user_id=$2",
            binding,
            owner,
        )
        if (
            len(matches) != 1
            or session.binding_id != binding
            or session.endpoint != leaf.endpoint
            or not live
            or live["kagent_session_id"] != leaf.runtime_session_id
            or live["kind"] != matches[0].provider
        ):
            return None
        return matches[0]
    except (ValidationError, ValueError, TypeError):
        return None


async def decision_inputs(conn, owner, leaf, request, binding, response):
    config = await configuration(conn, owner, leaf, binding)
    prepared = {}
    if not config or not isinstance(request, ToolApprovalRequest):
        return config, prepared
    if not hasattr(response, "approvals"):
        raise ValueError("Wrong response kind")
    decisions = {d.id: d.approved for d in response.approvals}
    for tool in request.tools:
        if canonical_operation(tool.name, config) is None:
            continue
        args = MergeInvocation.model_validate(tool.args)
        p = await merge.proposal(conn, owner, args.proposal_id)
        if (
            p["binding_id"] != binding
            or p["runtime_session_id"] != leaf.runtime_session_id
        ):
            raise PolicyError("ownership", "merge proposal belongs to another leaf")
        fresh = None
        if decisions.get(tool.id):
            live = await conn.fetchrow(
                "SELECT b.*,s.user_id FROM native_bindings b JOIN sessions s ON s.id=b.session_id WHERE b.session_id=$1",
                binding,
            )
            await merge.validate_binding(conn, dict(live), p)
            _, fresh = await merge.read_evidence(
                dict(live),
                PreparePullRequestMerge(
                    project_id=p["facts"]["project_id"],
                    pr_number=p["facts"]["pr_number"],
                    expected_sha=p["facts"]["head_sha"],
                    request_id=args.request_id,
                ),
            )
        prepared[p["id"]] = (p, fresh)
    return config, prepared


async def lock_decision(conn, owner, prepared):
    # Lock projects first in sorted order, then candidate locks, across a batch.
    proposals = [p for p, _ in prepared.values()]
    for project in sorted({p["facts"]["project_id"] for p in proposals}):
        await conn.fetchrow(
            "SELECT id FROM projects WHERE id=$1 AND user_id=$2 FOR UPDATE",
            project,
            owner,
        )
    candidates = {}
    for p in sorted(
        proposals, key=lambda p: (p["facts"]["repository_id"], p["facts"]["pr_number"])
    ):
        f = p["facts"]
        await merge.lock_candidate(
            conn, owner, f["project_id"], f["repository_id"], f["pr_number"]
        )
        candidate = await conn.fetchrow(
            "SELECT * FROM merge_requests WHERE id=$1", p["candidate_id"]
        )
        live = await conn.fetchrow(
            "SELECT b.*,s.user_id FROM native_bindings b JOIN sessions s ON s.id=b.session_id WHERE b.session_id=$1",
            p["binding_id"],
        )
        if not live:
            raise ValueError("Original merge binding disappeared")
        try:
            await merge.validate_binding(conn, dict(live), p)
        except PolicyError as exc:
            raise ValueError(exc.message) from None
        policy = await conn.fetchrow(
            "SELECT merge_policy_version FROM projects WHERE id=$1", f["project_id"]
        )
        candidates[p["id"]] = (
            candidate,
            policy,
            await merge.rejection(conn, p["candidate_id"]),
        )

    def validate(key, approved):
        if key.proposal_id not in prepared:
            raise ValueError("Unresolved merge proposal")
        p, fresh = prepared[key.proposal_id]
        candidate, policy, rejected = candidates[p["id"]]
        if (
            p["owner_id"] != key.owner_id
            or p["binding_id"] != key.leaf_binding_id
            or p["runtime_session_id"] != key.leaf_runtime_session_id
        ):
            raise ValueError("Proposal leaf mismatch")
        if candidate["state"] in ("merging", "uncertain", "merged"):
            raise ValueError("Merge intent already claimed; cannot change its decision")
        if approved and (
            rejected
            or candidate["active_proposal_id"] != p["id"]
            or candidate["state"] != "prepared"
            or fresh is None
            or merge.pinned(fresh) != merge.pinned(p["facts"])
            or policy["merge_policy_version"] != p["facts"]["policy_version"]
        ):
            raise ValueError("Merge proposal is stale; prepare a fresh operation")

    return validate


async def enrichment(conn, projection):
    # Shared HITL view data only; no alternative decision actions or lifecycle.
    values = []
    for leaf in projection.leaves:
        config = await configuration(conn, projection.owner_id, leaf, leaf.binding_id)
        request = projection.payload
        if not config or not isinstance(request, ToolApprovalRequest):
            continue
        for tool in (request.nested.tools if request.nested else request.tools):
            if (
                tool.id != leaf.pending_request_id
                or canonical_operation(tool.name, config) is None
            ):
                continue
            try:
                args = MergeInvocation.model_validate(tool.args)
                p = await merge.proposal(conn, projection.owner_id, args.proposal_id)
                if (
                    p["binding_id"] != leaf.binding_id
                    or p["runtime_session_id"] != leaf.runtime_session_id
                ):
                    continue
                candidate = await conn.fetchrow(
                    "SELECT active_proposal_id,state FROM merge_requests WHERE id=$1",
                    p["candidate_id"],
                )
                policy = await conn.fetchval(
                    "SELECT merge_policy_version FROM projects WHERE id=$1",
                    p["facts"]["project_id"],
                )
                values.append(
                    {
                        "tool_id": tool.id,
                        "proposal_id": p["id"],
                        **p["facts"],
                        "stale": candidate["active_proposal_id"] != p["id"]
                        or policy != p["facts"]["policy_version"],
                    }
                )
            except (ValueError, PolicyError):
                continue
    return values
