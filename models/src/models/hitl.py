"""Bounded kagent wire payloads and control-plane correlation snapshots.

These types do not verify ownership, delegation, or configuration provenance. Only
server resolvers may construct verified snapshots; agent payloads cannot grant authority.
"""

import hashlib
import json
from typing import Annotated, Literal

from pydantic import (
    BaseModel,
    ConfigDict,
    Field,
    JsonValue,
    TypeAdapter,
    model_validator,
)

HITL_EXTENSION = "https://kagent.dev/extensions/hitl/v1"
MERGE_OPERATION = "mainloop.merge_pull_request_with_approval.v1"
Identifier = Annotated[str, Field(min_length=1, max_length=2048)]
Digest = Annotated[str, Field(pattern=r"^[0-9a-f]{64}$")]


def normalized_hash(value: JsonValue) -> str:
    encoded = json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        allow_nan=False,
    )
    if len(encoded.encode()) > 131072:
        raise ValueError("HITL payload exceeds 128 KiB")
    return hashlib.sha256(encoded.encode()).hexdigest()


class WireModel(BaseModel):
    model_config = ConfigDict(extra="allow", strict=True)

    @model_validator(mode="after")
    def bounded(self):
        normalized_hash(self.model_dump(mode="json"))
        return self


class HITLTool(WireModel):
    id: Identifier
    call_id: Identifier
    name: Identifier
    args: dict[str, JsonValue]


class NestedHITLRequest(WireModel):
    subagent_name: str = Field(default="", max_length=2048)
    task_id: Identifier
    context_id: Identifier
    tools: list[HITLTool] = Field(min_length=1, max_length=100)


class ToolApprovalRequest(WireModel):
    type: Literal["tool_approval_request"]
    hint: str = Field(default="", max_length=4096)
    tools: list[HITLTool] = Field(min_length=1, max_length=100)
    nested: NestedHITLRequest | None = None

    @model_validator(mode="after")
    def unique_ids(self):
        for tools in (self.tools, self.nested.tools if self.nested else []):
            if len({t.id for t in tools}) != len(tools):
                raise ValueError("Duplicate pending tool IDs")
        return self


class HITLQuestion(WireModel):
    question: str = Field(min_length=1, max_length=8192)
    # The native Go encoder emits a nil slice as null for free-text questions.
    choices: list[Annotated[str, Field(max_length=4096)]] | None = Field(max_length=100)
    multiple: bool


class AskUserRequest(WireModel):
    type: Literal["ask_user_request"]
    id: Identifier
    questions: list[HITLQuestion] = Field(min_length=1, max_length=100)
    nested: NestedHITLRequest | None = None


HITLRequest = Annotated[
    ToolApprovalRequest | AskUserRequest, Field(discriminator="type")
]
REQUEST_ADAPTER = TypeAdapter(HITLRequest)


class ToolApproval(BaseModel):
    model_config = ConfigDict(extra="forbid", strict=True, frozen=True)
    id: Identifier
    approved: bool
    rejection_reason: str | None = Field(default=None, max_length=8192)

    @model_validator(mode="after")
    def approval_has_no_rejection_reason(self):
        if self.approved and self.rejection_reason:
            raise ValueError("Approved tools cannot have a rejection reason")
        return self


class ToolApprovalResponse(BaseModel):
    model_config = ConfigDict(extra="forbid", strict=True, frozen=True)
    type: Literal["tool_approval_response"]
    approvals: tuple[ToolApproval, ...] = Field(min_length=1, max_length=100)
    # Mainloop-only acknowledgement of the exact summary rendered by the owner.
    reviewed_context: dict[Identifier, Digest] = Field(
        default_factory=dict, max_length=100
    )


class AskUserAnswer(BaseModel):
    model_config = ConfigDict(extra="forbid", strict=True, frozen=True)
    answer: tuple[Annotated[str, Field(max_length=8192)], ...] = Field(max_length=100)


class AskUserResponse(BaseModel):
    model_config = ConfigDict(extra="forbid", strict=True, frozen=True)
    type: Literal["ask_user_response"]
    id: Identifier
    answers: tuple[AskUserAnswer, ...] = Field(min_length=1, max_length=100)


HITLResponse = Annotated[
    ToolApprovalResponse | AskUserResponse, Field(discriminator="type")
]


class HITLMessageMetadata(WireModel):
    """Preserve unknown extensions without interpreting them as permission."""

    metadata: dict[str, JsonValue] = Field(default_factory=dict)
    extensions: list[Identifier] = Field(default_factory=list, max_length=100)

    def request(self) -> HITLRequest:
        if HITL_EXTENSION not in self.extensions:
            raise ValueError("HITL extension was not declared")
        return REQUEST_ADAPTER.validate_python(self.metadata.get(HITL_EXTENSION))


class Snapshot(BaseModel):
    model_config = ConfigDict(extra="forbid", frozen=True)


class TaskIdentity(Snapshot):
    gateway: Identifier
    endpoint: Identifier
    runtime_session_id: Identifier
    context_id: Identifier
    task_id: Identifier


class ContinuationIdentity(TaskIdentity):
    status_message_id: Identifier
    request_hash: Digest


class LeafIdentity(TaskIdentity):
    owner_id: Identifier
    binding_id: Identifier | None = None
    pending_request_id: Identifier
    request_hash: Digest

    def key(self) -> str:
        # Task identity, not native call ID, scopes a response owner.
        return normalized_hash(
            [
                self.gateway,
                self.runtime_session_id,
                self.task_id,
                self.pending_request_id,
                self.request_hash,
            ]
        )


class VerifiedAssociation(Snapshot):
    owner_id: Identifier
    outer: TaskIdentity
    leaf: TaskIdentity
    evidence_source: Literal["control_plane_creation", "gateway_continuation"]
    evidence_reference: Identifier


class TrustedToolMapping(Snapshot):
    """One entry from a verified immutable prepared-revision configuration snapshot."""

    provider: Literal["claude", "codex"]
    prepared_revision: Identifier
    compiled_alias: Identifier
    remote_server_id: Identifier
    endpoint: Identifier
    tool: Literal["merge_pull_request_with_approval"]
    require_approval: Literal[True]
    operation: Literal["mainloop.merge_pull_request_with_approval.v1"] = MERGE_OPERATION

    def public_name(self) -> str:
        if self.provider == "claude":
            return f"mcp__{self.compiled_alias}__{self.tool}"
        return f"{self.compiled_alias}.{self.tool}"


class VerifiedLeafConfiguration(Snapshot):
    owner_id: Identifier
    binding_id: Identifier
    runtime_session_id: Identifier
    provider: Literal["claude", "codex"]
    prepared_revision: Identifier
    evidence_reference: Identifier
    mappings: tuple[TrustedToolMapping, ...] = Field(max_length=100)


class MergeInvocation(Snapshot):
    proposal_id: Identifier
    request_id: Identifier


class MergeReceiptKey(Snapshot):
    owner_id: Identifier
    leaf_binding_id: Identifier
    leaf_runtime_session_id: Identifier
    operation: Literal["mainloop.merge_pull_request_with_approval.v1"] = MERGE_OPERATION
    proposal_id: Identifier
    invocation_request_id: Identifier


class CallSnapshot(Snapshot):
    leaf: LeafIdentity
    call_id: Identifier | None = None
    tool_name: Identifier | None = None
    arguments_hash: Digest
    approved: bool | None = None
    configuration: VerifiedLeafConfiguration | None = None
    mapping: TrustedToolMapping | None = None
    merge_key: MergeReceiptKey | None = None


class DecisionReceipt(Snapshot):
    action_id: Identifier
    owner_id: Identifier
    request_hash: Digest
    request: HITLRequest
    response: HITLResponse
    outer: ContinuationIdentity
    calls: tuple[CallSnapshot, ...] = Field(min_length=1, max_length=100)
    associations: tuple[VerifiedAssociation, ...] = Field(default=(), max_length=100)
    outbound_message_id: Identifier

    @model_validator(mode="after")
    def bounded(self):
        normalized_hash(self.model_dump(mode="json"))
        return self


class ObservedSession(Snapshot):
    gateway: Identifier
    runtime_session_id: Identifier
    owner_id: Identifier
    verified_creator: Identifier
    agent_id: Identifier
    endpoint: Identifier
    context_id: Identifier
    prepared_revision: Identifier
    binding_id: Identifier | None = None
    lifecycle: Identifier


class ObserverCheckpoint(Snapshot):
    inventory_cursor: str | None = Field(default=None, max_length=8192)
    task_cursors: dict[str, str] = Field(default_factory=dict)
    next_session_id: str | None = Field(default=None, max_length=2048)


class HITLProjection(Snapshot):
    id: Identifier
    owner_id: Identifier
    outer: ContinuationIdentity
    payload: HITLRequest
    leaves: tuple[LeafIdentity, ...] = Field(default=(), max_length=100)
    associations: tuple[VerifiedAssociation, ...] = Field(default=(), max_length=100)
    availability: Literal["pending", "unavailable", "stale", "answered"]
    unavailable_reason: str | None = Field(default=None, max_length=4096)
