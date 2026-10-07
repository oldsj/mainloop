"""Pure HITL correlation/receipt contract; inputs marked verified are server-only.

No gateway lookup, prompts, or native-traversal attestation occurs here. b2 must
resolve ownership, pinned configuration and associations before calling this seam.
"""

from collections.abc import Callable

from models.hitl import (
    AskUserRequest,
    AskUserResponse,
    CallSnapshot,
    ContinuationIdentity,
    DecisionReceipt,
    HITLRequest,
    HITLResponse,
    LeafIdentity,
    MergeInvocation,
    MergeReceiptKey,
    TaskIdentity,
    ToolApprovalRequest,
    ToolApprovalResponse,
    TrustedToolMapping,
    VerifiedAssociation,
    VerifiedLeafConfiguration,
    normalized_hash,
)


def task_identity(value: TaskIdentity) -> TaskIdentity:
    return TaskIdentity(
        **{name: getattr(value, name) for name in TaskIdentity.model_fields}
    )


def canonical_operation(
    public_name: str, configuration: VerifiedLeafConfiguration | None
) -> TrustedToolMapping | None:
    """Exact match only; unknown or colliding aliases cannot authorize a merge."""
    if configuration is None:
        return None
    matches = [m for m in configuration.mappings if m.public_name() == public_name]
    if len(matches) != 1:
        return None
    match = matches[0]
    if (
        match.provider != configuration.provider
        or match.prepared_revision != configuration.prepared_revision
    ):
        return None
    return match


def verified_route(
    owner_id: str,
    outer: TaskIdentity,
    leaf: TaskIdentity,
    associations: tuple[VerifiedAssociation, ...],
) -> bool:
    """Require a trusted chain for propagated requests, never payload lookup hints."""
    outer, leaf = task_identity(outer), task_identity(leaf)
    if outer == leaf:
        return True
    visited = set()
    current = outer
    while current != leaf:
        key = current.model_dump_json()
        if key in visited:
            return False
        visited.add(key)
        links = [
            a for a in associations if a.owner_id == owner_id and a.outer == current
        ]
        if len(links) != 1:
            return False
        current = links[0].leaf
    return True


def build_decision_receipt(
    *,
    action_id: str,
    owner_id: str,
    outbound_message_id: str,
    outer: ContinuationIdentity,
    request: HITLRequest,
    leaf_task: TaskIdentity,
    leaf_request: HITLRequest,
    leaf_binding_id: str | None,
    response: HITLResponse,
    associations: tuple[VerifiedAssociation, ...] = (),
    configuration: VerifiedLeafConfiguration | None = None,
    validate_proposal: Callable[[MergeReceiptKey, bool], None] | None = None,
) -> DecisionReceipt:
    """Build a complete immutable decision after fresh server-side task resolution.

    validate_proposal must reject wrong owner/binding or stale proposal facts for
    approval. c supplies this server lookup. No receipt submission API exists in b1.
    """
    if isinstance(response, ToolApprovalResponse):
        # Revalidate even internal model_copy/model_construct callers before consent.
        response = ToolApprovalResponse.model_validate_json(response.model_dump_json())
    if outer.request_hash != normalized_hash(request.model_dump(mode="json")):
        raise ValueError("Outer request changed")
    if not verified_route(owner_id, outer, leaf_task, associations):
        raise ValueError("Nested continuation mapping unavailable")
    nested = request.nested
    if task_identity(outer) != leaf_task:
        if (
            nested is None
            or nested.task_id != leaf_task.task_id
            or nested.context_id != leaf_task.context_id
        ):
            raise ValueError("Nested identity mismatch")
    elif nested is not None:
        raise ValueError("Nested request requires a separately verified leaf")
    if leaf_request.nested is not None:
        raise ValueError("Resolve to the terminal leaf request")
    if nested is None and request != leaf_request:
        raise ValueError("Direct request mismatch")
    if configuration is not None and (
        configuration.owner_id != owner_id
        or configuration.binding_id != leaf_binding_id
        or configuration.runtime_session_id != leaf_task.runtime_session_id
    ):
        raise ValueError("Configuration does not belong to the leaf")
    leaf_hash = normalized_hash(leaf_request.model_dump(mode="json"))

    def leaf(pending_id: str) -> LeafIdentity:
        return LeafIdentity(
            **leaf_task.model_dump(),
            owner_id=owner_id,
            binding_id=leaf_binding_id,
            pending_request_id=pending_id,
            request_hash=leaf_hash,
        )

    calls = []
    if isinstance(request, ToolApprovalRequest):
        if not isinstance(leaf_request, ToolApprovalRequest) or not isinstance(
            response, ToolApprovalResponse
        ):
            raise ValueError("Wrong response kind")
        tools = nested.tools if nested else request.tools
        if tools != leaf_request.tools:
            raise ValueError("Leaf tool payload mismatch")
        decisions = {a.id: a for a in response.approvals}
        if len(decisions) != len(response.approvals) or set(decisions) != {
            t.id for t in tools
        }:
            raise ValueError("Decisions must cover every pending tool exactly once")
        for tool in tools:
            mapping = canonical_operation(tool.name, configuration)
            merge_key = None
            if mapping is not None:
                invocation = MergeInvocation.model_validate(tool.args)
                if not leaf_binding_id or validate_proposal is None:
                    raise ValueError("Merge proposal resolver required")
                merge_key = MergeReceiptKey(
                    owner_id=owner_id,
                    leaf_binding_id=leaf_binding_id,
                    leaf_runtime_session_id=leaf_task.runtime_session_id,
                    proposal_id=invocation.proposal_id,
                    invocation_request_id=invocation.request_id,
                )
                validate_proposal(merge_key, decisions[tool.id].approved)
            calls.append(
                CallSnapshot(
                    leaf=leaf(tool.id),
                    call_id=tool.call_id,
                    tool_name=tool.name,
                    arguments_hash=normalized_hash(tool.args),
                    approved=decisions[tool.id].approved,
                    configuration=configuration if mapping else None,
                    mapping=mapping,
                    merge_key=merge_key,
                )
            )
    else:
        if not isinstance(leaf_request, AskUserRequest) or not isinstance(
            response, AskUserResponse
        ):
            raise ValueError("Wrong response kind")
        if nested is not None and len(nested.tools) != 1:
            raise ValueError("Nested question must identify exactly one child request")
        response_id = (
            nested.tools[0].id if nested and len(nested.tools) == 1 else request.id
        )
        if (
            response.id != response_id
            or leaf_request.id != response_id
            or request.questions != leaf_request.questions
            or len(response.answers) != len(request.questions)
        ):
            raise ValueError("Question response correlation mismatch")
        for question, answer in zip(request.questions, response.answers, strict=True):
            if not answer.answer or (not question.multiple and len(answer.answer) != 1):
                raise ValueError("Incomplete question answer")
        calls.append(CallSnapshot(leaf=leaf(response_id), arguments_hash=leaf_hash))
    return DecisionReceipt(
        action_id=action_id,
        owner_id=owner_id,
        request_hash=outer.request_hash,
        request=request,
        response=response,
        outer=outer,
        calls=tuple(calls),
        associations=associations,
        outbound_message_id=outbound_message_id,
    )


def validate_receipt(receipt: DecisionReceipt) -> None:
    """Validate internal snapshot consistency before durable recording/lookup."""
    if isinstance(receipt.response, ToolApprovalResponse):
        ToolApprovalResponse.model_validate_json(receipt.response.model_dump_json())
    keys = [c.leaf.key() for c in receipt.calls]
    if (
        len(set(keys)) != len(keys)
        or receipt.request_hash != receipt.outer.request_hash
    ):
        raise ValueError("Invalid receipt membership")
    if normalized_hash(receipt.request.model_dump(mode="json")) != receipt.request_hash:
        raise ValueError("Receipt request snapshot mismatch")
    if isinstance(receipt.response, ToolApprovalResponse):
        if not isinstance(receipt.request, ToolApprovalRequest):
            raise ValueError("Receipt request/response kind mismatch")
        tools = (
            receipt.request.nested.tools
            if receipt.request.nested
            else receipt.request.tools
        )
        if {tool.id for tool in tools} != {
            call.leaf.pending_request_id for call in receipt.calls
        }:
            raise ValueError("Receipt pending tool coverage mismatch")
        for call in receipt.calls:
            tool = next(t for t in tools if t.id == call.leaf.pending_request_id)
            if (
                call.call_id != tool.call_id
                or call.tool_name != tool.name
                or call.arguments_hash != normalized_hash(tool.args)
            ):
                raise ValueError("Receipt tool snapshot mismatch")
        if len(receipt.response.approvals) != len(receipt.calls):
            raise ValueError("Receipt decision coverage mismatch")
        approved_ids = {
            approval.id for approval in receipt.response.approvals if approval.approved
        }
        if not set(receipt.response.reviewed_context).issubset(approved_ids):
            raise ValueError("Reviewed context must belong to an approved tool")
    elif (
        len(receipt.calls) != 1
        or receipt.calls[0].leaf.pending_request_id != receipt.response.id
    ):
        raise ValueError("Receipt question correlation mismatch")
    for call in receipt.calls:
        if call.leaf.owner_id != receipt.owner_id or not verified_route(
            receipt.owner_id, receipt.outer, call.leaf, receipt.associations
        ):
            raise ValueError("Unverified receipt route")
        if isinstance(receipt.response, ToolApprovalResponse):
            matches = [
                a
                for a in receipt.response.approvals
                if a.id == call.leaf.pending_request_id
            ]
            if len(matches) != 1 or matches[0].approved != call.approved:
                raise ValueError("Receipt decision mismatch")
        elif call.merge_key is not None or call.approved is not None:
            raise ValueError("Questions cannot authorize merges")
        if call.merge_key is None:
            if call.mapping is not None:
                raise ValueError("Mapping without merge identity")
            continue
        key, config = call.merge_key, call.configuration
        if config is None or call.mapping is None or call.tool_name is None:
            raise ValueError("Missing merge configuration")
        if (
            key.owner_id != receipt.owner_id
            or key.leaf_binding_id != call.leaf.binding_id
            or key.leaf_runtime_session_id != call.leaf.runtime_session_id
            or config.owner_id != key.owner_id
            or config.binding_id != key.leaf_binding_id
            or config.runtime_session_id != key.leaf_runtime_session_id
            or canonical_operation(call.tool_name, config) != call.mapping
            or normalized_hash(
                {
                    "proposal_id": key.proposal_id,
                    "request_id": key.invocation_request_id,
                }
            )
            != call.arguments_hash
        ):
            raise ValueError("Merge receipt identity mismatch")
