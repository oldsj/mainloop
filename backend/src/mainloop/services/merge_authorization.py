"""Template-keyed merge mappings and server-resolved proposal enrichment."""

import asyncio
import json
import logging
import os
from datetime import datetime, timezone

from mainloop.runtime.hitl_correlation import canonical_operation
from mainloop.runtime.hitl_observer import observer
from mainloop.runtime.kagent_client import KagentError
from mainloop.runtime.policy import PolicyError
from mainloop.services import merge
from pydantic import TypeAdapter, ValidationError

from models.agent_tools import PreparePullRequestMerge
from models.hitl import (
    MergeInvocation,
    TemplateMappingEvidence,
    TemplateMergeConfiguration,
    ToolApprovalRequest,
)

logger = logging.getLogger(__name__)
MAX_CONFIG_BYTES = 131072
MAX_CONFIGURATIONS = 100
MERGE_TOOL_SUFFIX = "merge_pull_request_with_approval"


def parse_configurations(raw: str | None = None) -> list[TemplateMergeConfiguration]:
    """Parse the bounded operator allowlist, explicitly rejecting the retired shape."""
    raw = os.environ.get("MAINLOOP_MERGE_CONFIGURATIONS", "[]") if raw is None else raw
    if not isinstance(raw, str) or len(raw.encode("utf-8")) > MAX_CONFIG_BYTES:
        raise ValueError("MAINLOOP_MERGE_CONFIGURATIONS exceeds 128 KiB")
    try:
        value = json.loads(raw)
    except (json.JSONDecodeError, UnicodeError) as exc:
        raise ValueError("MAINLOOP_MERGE_CONFIGURATIONS must be a JSON array") from exc
    if not isinstance(value, list):
        raise ValueError("MAINLOOP_MERGE_CONFIGURATIONS must be a JSON array")
    if any(
        isinstance(item, dict)
        and {"owner_id", "binding_id", "runtime_session_id", "prepared_revision"}
        & item.keys()
        for item in value
    ):
        raise ValueError(
            "legacy per-session merge configuration is unsupported; use template_name and provider mappings"
        )
    if len(value) > MAX_CONFIGURATIONS:
        raise ValueError("MAINLOOP_MERGE_CONFIGURATIONS exceeds 100 entries")
    try:
        return TypeAdapter(list[TemplateMergeConfiguration]).validate_python(value)
    except ValidationError as exc:
        raise ValueError(
            "MAINLOOP_MERGE_CONFIGURATIONS contains an invalid template mapping"
        ) from exc


async def resolve_template_mapping(
    conn,
    owner: str,
    binding_id: str | None,
    runtime_session_id: str | None,
    *,
    leaf=None,
    service=None,
) -> tuple[TemplateMergeConfiguration, TemplateMappingEvidence] | None:
    """Resolve a template mapping only from a live owned Session and its Agent."""
    if not merge.enabled() or not owner or not binding_id or not runtime_session_id:
        return None
    try:
        configurations = parse_configurations()
        if not configurations:
            return None
        service = service or observer()
        if service.owner != owner or not service.creator:
            return None
        live_binding = await conn.fetchrow(
            """SELECT b.*,s.user_id,s.archived_at FROM native_bindings b
            JOIN sessions s ON s.id=b.session_id
            WHERE b.session_id=$1""",
            binding_id,
        )
        if (
            not live_binding
            or live_binding["user_id"] != owner
            or live_binding["archived_at"]
            or live_binding["kagent_deleted_at"]
            or live_binding["kagent_session_id"] != runtime_session_id
            or live_binding["kind"] not in ("claude", "codex")
        ):
            return None
        async with asyncio.timeout(10):
            live = await service.client.get_session(runtime_session_id)
            if live.id != runtime_session_id:
                return None
            observed = await service.owned(conn, live)
            if (
                observed.owner_id != owner
                or observed.runtime_session_id != runtime_session_id
                or observed.binding_id != binding_id
                or (
                    leaf is not None
                    and (
                        observed.endpoint != leaf.endpoint
                        or observed.context_id != leaf.context_id
                    )
                )
                or live.agent is None
            ):
                return None
            agent = await service.client.get_agent(live.agent)
        if agent.ref != live.agent or agent.inline_template or not agent.template_name:
            return None
        matches = [
            config
            for config in configurations
            if config.template_name == agent.template_name
            and config.provider == live_binding["kind"]
        ]
        if len(matches) != 1:
            return None
        config = matches[0]
        return config, config.evidence()
    except (
        KagentError,
        ValidationError,
        ValueError,
        TypeError,
        TimeoutError,
        OSError,
    ) as exc:
        # Config and kagent reads are fail-closed. Do not include values from the config in logs.
        logger.warning("Merge template mapping is unavailable (%s)", type(exc).__name__)
        return None


async def configuration(conn, owner, leaf, binding, *, service=None):
    if not binding:
        return None
    resolved = await resolve_template_mapping(
        conn,
        owner,
        binding,
        leaf.runtime_session_id,
        leaf=leaf,
        service=service,
    )
    return resolved


def possible_merge_tool(tool) -> bool:
    """Suffix is only a fail-closed warning; canonical authority uses an exact mapping."""
    return isinstance(tool.name, str) and tool.name.endswith(MERGE_TOOL_SUFFIX)


async def decision_inputs(
    conn, owner, leaf, request, binding, response, *, service=None
):
    resolved = await configuration(conn, owner, leaf, binding, service=service)
    config, mapping_evidence = resolved if resolved else (None, None)
    prepared = {}
    if not isinstance(request, ToolApprovalRequest):
        return config, mapping_evidence, prepared
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
    return config, mapping_evidence, prepared


def approval_unavailable(p, candidate, policy_version, rejected):
    """Return the stored refusal reason for cards and locked approval validation."""
    if not candidate or candidate["active_proposal_id"] != p["id"]:
        return "Merge proposal was replaced; prepare a fresh operation"
    if rejected:
        return "Merge candidate was rejected; prepare a fresh operation"
    if (
        candidate["state"] in ("prepared", "evaluating")
        and candidate["deadline"]
        and candidate["deadline"] <= datetime.now(timezone.utc)
    ):
        return "Merge proposal expired; prepare a fresh operation"
    if candidate["state"] != "prepared":
        if candidate["state"] == "evaluating":
            return (
                "Merge is evaluating CI; a second approval is unavailable. "
                "Any recorded consent remains bound to its original invocation. "
                "Prepare a fresh operation if it expires."
            )
        return f"Merge proposal is {candidate['state']}; prepare a fresh operation"
    if policy_version != p["facts"]["policy_version"]:
        return "Merge policy changed; prepare a fresh operation"
    return None


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
        if candidate and candidate["state"] in ("merging", "uncertain", "merged"):
            raise ValueError("Merge intent already claimed; cannot change its decision")
        if approved:
            reason = approval_unavailable(
                p, candidate, policy["merge_policy_version"], rejected
            )
            if reason:
                raise ValueError(reason)
        if approved and (
            fresh is None or merge.pinned(fresh) != merge.pinned(p["facts"])
        ):
            raise ValueError("Merge proposal is stale; prepare a fresh operation")

    return validate


async def enrichment(conn, projection, *, service=None):
    """Shared HITL view data; merge suffixes only locate unavailable display context."""
    values = []
    request = projection.payload
    if not isinstance(request, ToolApprovalRequest):
        return values
    for leaf in projection.leaves:
        resolved = await configuration(
            conn, projection.owner_id, leaf, leaf.binding_id, service=service
        )
        config, mapping_evidence = resolved if resolved else (None, None)
        for tool in (request.nested.tools if request.nested else request.tools):
            if tool.id != leaf.pending_request_id:
                continue
            mapped = canonical_operation(tool.name, config) is not None
            if not mapped and not possible_merge_tool(tool):
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
                    "SELECT active_proposal_id,state,deadline FROM merge_requests WHERE id=$1",
                    p["candidate_id"],
                )
                policy = await conn.fetchval(
                    "SELECT merge_policy_version FROM projects WHERE id=$1",
                    p["facts"]["project_id"],
                )
                stored_evidence = p["facts"].get("mapping_evidence")
                evidence_changed = (
                    not mapped
                    or mapping_evidence is None
                    or stored_evidence != mapping_evidence.model_dump(mode="json")
                )
                freshness_reason = (
                    "The reviewed template mapping changed or is unavailable. Reject the call or refresh the request."
                    if evidence_changed
                    else approval_unavailable(
                        p,
                        candidate,
                        policy,
                        await merge.rejection(conn, p["candidate_id"]),
                    )
                )
                values.append(
                    {
                        "tool_id": tool.id,
                        "proposal_id": p["id"],
                        **p["facts"],
                        "stale": freshness_reason is not None,
                        "state": candidate["state"] if candidate else None,
                        "deadline": (
                            candidate["deadline"].isoformat()
                            if candidate and candidate["deadline"]
                            else None
                        ),
                        "mapping_unavailable": evidence_changed,
                        "freshness_reason": freshness_reason,
                    }
                )
            except (ValueError, PolicyError):
                continue
    return values
