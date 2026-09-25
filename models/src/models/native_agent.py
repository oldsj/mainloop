"""Portable native-runtime records; no provider calls or persistence side effects."""

from enum import StrEnum
from typing import Annotated, Literal

from pydantic import AwareDatetime, BaseModel, ConfigDict, Field, model_validator

Identifier = Annotated[str, Field(min_length=1, strict=True)]
Generation = Annotated[int, Field(ge=1, strict=True)]


class ContractModel(BaseModel):
    model_config = ConfigDict(
        extra="forbid", frozen=True, revalidate_instances="always"
    )


class CapabilityState(StrEnum):
    PROVED = "proved"
    PARTIAL = "partial"
    UNSUPPORTED = "unsupported"
    UNKNOWN = "unknown"


class CapabilityResult(ContractModel):
    capability: Identifier
    state: CapabilityState = CapabilityState.UNKNOWN
    scope: Literal["fixture", "live", "unverified"] = "unverified"
    evidence_ref: Identifier | None = None
    detail: str | None = None

    @model_validator(mode="after")
    def evidence_required(self):
        if self.state in (CapabilityState.PROVED, CapabilityState.PARTIAL):
            if self.evidence_ref is None or self.scope == "unverified":
                raise ValueError("proved/partial capabilities require scoped evidence")
        return self


class ProviderExtension(ContractModel):
    """Typed observed metadata, never a bag of provider-defined durable states."""

    provider: Identifier
    runtime_version: Identifier | None = None
    native_event_id: Identifier | None = None
    model: Identifier | None = None
    effort: Identifier | None = None
    input_tokens: Annotated[int, Field(ge=0, strict=True)] | None = None
    output_tokens: Annotated[int, Field(ge=0, strict=True)] | None = None


class WorkspaceBinding(ContractModel):
    workspace_id: Identifier
    runtime_endpoint: Identifier
    observed_at: AwareDatetime
    observed_state: Literal["ready", "unavailable", "unknown"] = "unknown"
    capabilities: tuple[CapabilityResult, ...] = ()


class NativeBinding(ContractModel):
    binding_id: Identifier
    workspace_id: Identifier
    provider: Identifier
    runtime_type: Identifier
    native_session_id: Identifier
    creation_mode: Literal["created", "attached", "discovered"]
    ownership_generation: Generation
    observed: ProviderExtension | None = None


class MessageEnvelope(ContractModel):
    logical_message_id: Identifier
    source_task_id: Identifier
    source_topic_id: Identifier | None = None
    payload_ref: Identifier
    authority_ref: Identifier
    created_at: AwareDatetime
    desired_binding_id: Identifier


class DeliveryState(StrEnum):
    RECORDED = "recorded"
    QUEUED = "queued"
    SENDING = "sending"
    DELIVERED = "delivered"
    COMPLETED = "completed"
    FAILED = "failed"
    UNCERTAIN = "uncertain"


class DeliveryAttempt(ContractModel):
    attempt_id: Identifier
    logical_message_id: Identifier
    binding_id: Identifier
    ownership_generation: Generation
    created_at: AwareDatetime
    updated_at: AwareDatetime
    state: DeliveryState = DeliveryState.RECORDED
    evidence_ref: Identifier | None = None
    result: str | None = None

    @model_validator(mode="after")
    def chronological(self):
        if self.updated_at < self.created_at:
            raise ValueError("attempt timestamps must not regress")
        return self


class ReconciliationEvidence(ContractModel):
    attempt_id: Identifier
    binding_id: Identifier
    evidence_ref: Identifier
    observed_at: AwareDatetime
    outcome: Literal["not_delivered", "delivered", "completed"]


class NativeStatus(StrEnum):
    UNKNOWN = "unknown"
    ACTIVE = "active"
    COMPLETED = "completed"
    INTERRUPTED = "interrupted"
    WAITING = "waiting"


class AttentionRequest(ContractModel):
    deduplication_key: Identifier
    request_type: Literal["question", "approval", "error"]
    answer_shape: Literal["text", "boolean", "choice"]
    choices: tuple[Identifier, ...] = ()

    @model_validator(mode="after")
    def valid_choices(self):
        if (self.answer_shape == "choice") != bool(self.choices):
            raise ValueError("only choice answers require choices")
        return self


class NativeEvent(ContractModel):
    binding_id: Identifier
    ownership_generation: Generation
    source_cursor: Annotated[int, Field(ge=1, strict=True)]
    native_type: Identifier
    normalized_type: Literal[
        "activity",
        "output",
        "completed",
        "interrupted",
        "attention",
        "attention_resolved",
        "transport_lost",
        "usage",
        "continuation",
        "unknown",
    ]
    source_at: AwareDatetime | None = None
    ingested_at: AwareDatetime
    raw_evidence_ref: Identifier
    logical_message_id: Identifier | None = None
    attention: AttentionRequest | None = None
    attention_key: Identifier | None = None
    extension: ProviderExtension | None = None

    @model_validator(mode="after")
    def attention_payload(self):
        if (self.normalized_type == "attention") != (self.attention is not None):
            raise ValueError("attention events require an attention request")
        if (self.normalized_type == "attention_resolved") != (
            self.attention_key is not None
        ):
            raise ValueError("attention resolution requires its correlation key")
        return self


class AttentionItem(ContractModel):
    binding_id: Identifier
    source_cursor: Annotated[int, Field(ge=1, strict=True)]
    logical_message_id: Identifier | None = None
    request: AttentionRequest
    state: Literal["pending", "resolved"] = "pending"


class Checkpoint(ContractModel):
    binding_id: Identifier
    ownership_generation: Generation
    evidence_cursor: Annotated[int, Field(ge=0, strict=True)] = 0
    native_status: NativeStatus = NativeStatus.UNKNOWN
    verified_at: AwareDatetime | None = None
    pending_delivery: tuple[DeliveryAttempt, ...] = ()
    attention: tuple[AttentionItem, ...] = ()
    repository_ref: Identifier | None = None
    candidate_ref: Identifier | None = None
