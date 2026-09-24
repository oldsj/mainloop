"""Fixture-only normalization for the native Claude Code stream boundary.

It accepts validated, JSON-shaped observations from a native Claude
session and maps the observable parts to the provider-neutral runtime contract.
The fixture envelope supplies the source cursor and raw-evidence reference;
neither is synthesized from a process identity or a transcript message.
"""

from __future__ import annotations

from collections.abc import Iterable, Mapping
from datetime import datetime
from typing import Any, Literal

from pydantic import AwareDatetime, BaseModel, ConfigDict, Field

from models.native_agent import (
    AttentionRequest,
    CapabilityResult,
    CapabilityState,
    NativeBinding,
    NativeEvent,
    ProviderExtension,
)

CLAUDE_PROVIDER = "claude"


class ClaudeRawEvent(BaseModel):
    """The known fields of a native Claude stream record.

    ``extra=allow`` is intentional: an unrecognized native event is retained as
    an ``unknown`` contract event instead of being silently discarded.  Known
    fields remain strict so malformed records fail before they reach the shared
    event store.
    """

    model_config = ConfigDict(extra="allow", frozen=True)

    type: str = Field(min_length=1, strict=True)
    subtype: str | None = Field(default=None, min_length=1, strict=True)
    uuid: str | None = Field(default=None, min_length=1, strict=True)
    session_id: str | None = Field(default=None, min_length=1, strict=True)
    parent_tool_use_id: str | None = Field(default=None, min_length=1, strict=True)
    request_id: str | None = Field(default=None, min_length=1, strict=True)
    request: dict[str, Any] | None = None
    response: dict[str, Any] | None = None
    message: dict[str, Any] | None = None
    event: dict[str, Any] | None = None
    model: str | None = Field(default=None, min_length=1, strict=True)
    version: str | None = Field(default=None, min_length=1, strict=True)
    effort: str | None = Field(default=None, min_length=1, strict=True)
    is_error: bool | None = Field(default=None, strict=True)
    error: str | None = Field(default=None, min_length=1, strict=True)
    result: str | None = Field(default=None, strict=True)
    usage: dict[str, Any] | None = None
    timestamp: AwareDatetime | None = None
    logical_message_id: str | None = Field(default=None, min_length=1, strict=True)
    exit_code: int | None = Field(default=None, strict=True)


class ClaudeFixtureRecord(BaseModel):
    """Sanitized source metadata wrapped around one raw Claude observation."""

    model_config = ConfigDict(extra="forbid", frozen=True)

    source_cursor: int = Field(ge=1, strict=True)
    raw_evidence_ref: str = Field(min_length=1, strict=True)
    source_at: AwareDatetime | None = None
    logical_message_id: str | None = Field(default=None, min_length=1, strict=True)
    event: ClaudeRawEvent


class ClaudeRuntimeObservation(BaseModel):
    """A process observation that is not a native stream event.

    Quiet is an absence of new native evidence, and process exit is a workspace
    observation.  Neither can prove native completion, so neither is converted
    to a ``completed`` event.  The source cursor is the last cursor observed by
    the fixture harness; these records do not advance the native event journal.
    """

    model_config = ConfigDict(extra="forbid", frozen=True, strict=True)

    kind: Literal["process_exit", "quiet"]
    source_cursor: int = Field(ge=1, strict=True)
    raw_evidence_ref: str = Field(min_length=1, strict=True)
    source_at: AwareDatetime | None = None
    exit_code: int | None = Field(default=None, strict=True)


def _record(raw: ClaudeFixtureRecord | Mapping[str, Any]) -> ClaudeFixtureRecord:
    if isinstance(raw, ClaudeFixtureRecord):
        return raw
    return ClaudeFixtureRecord.model_validate(raw)


def _mapping(value: Any, *, label: str) -> Mapping[str, Any] | None:
    if value is None:
        return None
    if not isinstance(value, Mapping):
        raise ValueError(f"{label} must be an object")
    return value


def _required_text(value: Any, *, label: str) -> str:
    if type(value) is not str or not value:
        raise ValueError(f"{label} must be a non-empty string")
    return value


def _optional_text(value: Any, *, label: str) -> str | None:
    if value is None:
        return None
    return _required_text(value, label=label)


def _optional_nonnegative_int(value: Any, *, label: str) -> int | None:
    if value is None:
        return None
    if type(value) is not int or value < 0:
        raise ValueError(f"{label} must be a non-negative integer")
    return value


def _first_value(sources: Iterable[Mapping[str, Any]], key: str) -> Any:
    for source in sources:
        if key in source:
            return source[key]
    return None


def _first_text(values: Iterable[Any], *, label: str) -> str | None:
    for value in values:
        if value is not None:
            return _optional_text(value, label=label)
    return None


def _message_sources(raw: ClaudeRawEvent) -> tuple[Mapping[str, Any], ...]:
    sources: list[Mapping[str, Any]] = []
    message = _mapping(raw.message, label="message")
    if message is not None:
        sources.append(message)
    event = _mapping(raw.event, label="event")
    if event is not None:
        nested_message = _mapping(event.get("message"), label="event.message")
        if nested_message is not None:
            sources.append(nested_message)
        sources.append(event)
    return tuple(sources)


def _usage_sources(raw: ClaudeRawEvent) -> tuple[Mapping[str, Any], ...]:
    sources: list[Mapping[str, Any]] = []
    usage = _mapping(raw.usage, label="usage")
    if usage is not None:
        sources.append(usage)
    for source in _message_sources(raw):
        nested_usage = _mapping(source.get("usage"), label="usage")
        if nested_usage is not None:
            sources.append(nested_usage)
    return tuple(sources)


def _extension(raw: ClaudeRawEvent) -> ProviderExtension:
    sources = _message_sources(raw)
    extras = raw.model_extra or {}
    native_event_id = raw.uuid or _first_text(
        (source.get("id") for source in sources),
        label="native identifier",
    )
    model = _first_text(
        (
            raw.model,
            *(source.get("model") for source in sources),
        ),
        label="model",
    )
    effort = _first_text(
        (
            raw.effort,
            *(source.get("effort") for source in sources),
        ),
        label="effort",
    )
    runtime_version = _first_text(
        (raw.version, extras.get("claude_code_version")),
        label="runtime_version",
    )
    usage = _usage_sources(raw)
    return ProviderExtension(
        provider=CLAUDE_PROVIDER,
        runtime_version=runtime_version,
        native_event_id=native_event_id,
        model=model,
        effort=effort,
        input_tokens=_optional_nonnegative_int(
            _first_value(usage, "input_tokens"), label="usage.input_tokens"
        ),
        output_tokens=_optional_nonnegative_int(
            _first_value(usage, "output_tokens"), label="usage.output_tokens"
        ),
    )


def _content_kinds(raw: ClaudeRawEvent) -> tuple[str, ...]:
    message = _mapping(raw.message, label="message")
    if message is None or "content" not in message:
        return ()
    content = message["content"]
    if isinstance(content, str):
        return ("text",)
    if not isinstance(content, list):
        raise ValueError("message.content must be text or a list")
    kinds: list[str] = []
    for index, block in enumerate(content):
        block_mapping = _mapping(block, label=f"message.content[{index}]")
        if block_mapping is None:
            raise ValueError(f"message.content[{index}] must be an object")
        kinds.append(_required_text(block_mapping.get("type"), label="content.type"))
    return tuple(kinds)


def _stream_event_type(raw: ClaudeRawEvent) -> str | None:
    event = _mapping(raw.event, label="event")
    if event is None:
        raise ValueError("stream_event requires an event object")
    return _optional_text(event.get("type"), label="event.type")


def _attention_request(raw: ClaudeRawEvent) -> AttentionRequest:
    request_id = _required_text(raw.request_id, label="request_id")
    request = _mapping(raw.request, label="request")
    if request is None:
        raise ValueError("can_use_tool requires a request object")
    subtype = _required_text(request.get("subtype"), label="request.subtype")
    if subtype != "can_use_tool":
        raise ValueError("not a can_use_tool request")
    _required_text(request.get("tool_name"), label="request.tool_name")
    if _mapping(request.get("input"), label="request.input") is None:
        raise ValueError("request.input must be an object")
    return AttentionRequest(
        deduplication_key=request_id,
        request_type="approval",
        answer_shape="boolean",
    )


def _classify(
    raw: ClaudeRawEvent,
) -> tuple[
    Literal[
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
    ],
    AttentionRequest | None,
    str | None,
]:
    if raw.type == "system":
        if raw.subtype is None:
            raise ValueError("system event requires subtype")
        if raw.subtype == "compact_boundary":
            return "continuation", None, None
        if raw.subtype == "init":
            return "activity", None, None
        return "unknown", None, None

    if raw.type == "assistant":
        message = _mapping(raw.message, label="message")
        if message is None or "content" not in message:
            raise ValueError("assistant event requires message.content")
        message_error = message.get("error")
        if raw.error is not None or message_error is not None:
            _optional_text(
                raw.error if raw.error is not None else message_error,
                label="assistant.error",
            )
            return "interrupted", None, None
        kinds = _content_kinds(raw)
        if "text" in kinds:
            return "output", None, None
        if kinds:
            return "activity", None, None
        return "unknown", None, None

    if raw.type == "user":
        message = _mapping(raw.message, label="message")
        if message is None or "content" not in message:
            raise ValueError("user event requires message.content")
        return "activity", None, None

    if raw.type == "stream_event":
        event_type = _stream_event_type(raw)
        if event_type == "content_block_delta":
            event = _mapping(raw.event, label="event") or {}
            delta = _mapping(event.get("delta"), label="event.delta")
            if delta is not None and delta.get("type") == "text_delta":
                return "output", None, None
            return "activity", None, None
        if event_type in {
            "message_start",
            "message_delta",
            "message_stop",
            "content_block_start",
            "content_block_stop",
        }:
            return "activity", None, None
        return "unknown", None, None

    if raw.type == "result":
        if raw.subtype == "success" and raw.is_error is False:
            return "completed", None, None
        if raw.is_error is True or raw.subtype in {
            "error",
            "error_during_execution",
        }:
            return "interrupted", None, None
        return "unknown", None, None

    if raw.type == "control_request":
        request = _mapping(raw.request, label="request")
        if request is None:
            raise ValueError("control_request requires a request object")
        subtype = _required_text(request.get("subtype"), label="request.subtype")
        if subtype == "can_use_tool":
            return "attention", _attention_request(raw), None
        return "unknown", None, None

    if raw.type == "control_response":
        response = _mapping(raw.response, label="response")
        if response is None:
            raise ValueError("control_response requires a response object")
        response_subtype = _required_text(
            response.get("subtype"), label="response.subtype"
        )
        response_request_id = _required_text(
            response.get("request_id"), label="response.request_id"
        )
        if response_subtype == "success":
            permission_response = _mapping(
                response.get("response"), label="response.response"
            )
            behavior = (
                None
                if permission_response is None
                else permission_response.get("behavior")
            )
            if behavior is not None:
                _required_text(behavior, label="response.response.behavior")
            if behavior not in {"allow", "deny"}:
                return "unknown", None, None
            return (
                "attention_resolved",
                None,
                response_request_id,
            )
        if response_subtype == "error":
            _required_text(response.get("error"), label="response.error")
        return "unknown", None, None

    if raw.type == "transport" and raw.subtype == "lost":
        return "transport_lost", None, None

    if raw.type == "usage":
        return "usage", None, None

    if raw.type in {"process_exit", "quiet"}:
        raise ValueError(
            f"{raw.type} is a runtime observation; call observe_runtime instead"
        )

    return "unknown", None, None


def _validate_session(raw: ClaudeRawEvent, binding: NativeBinding) -> None:
    if raw.session_id is not None and raw.session_id != binding.native_session_id:
        raise ValueError("native event belongs to another Claude session")


def _native_type(raw: ClaudeRawEvent) -> str:
    if raw.subtype is None:
        return f"claude.{raw.type}"
    return f"claude.{raw.type}.{raw.subtype}"


def binding_from_init(
    raw: ClaudeFixtureRecord | Mapping[str, Any],
    *,
    binding_id: str,
    workspace_id: str,
    creation_mode: Literal["created", "attached", "discovered"] = "created",
    ownership_generation: int = 1,
) -> NativeBinding:
    """Build a provider-neutral binding from an observed Claude init record."""

    record = _record(raw)
    event = record.event
    if event.type != "system" or event.subtype != "init":
        raise ValueError("a Claude binding requires a system.init record")
    native_session_id = _required_text(event.session_id, label="session_id")
    return NativeBinding.model_validate(
        {
            "binding_id": binding_id,
            "workspace_id": workspace_id,
            "provider": CLAUDE_PROVIDER,
            "runtime_type": "claude-native-cli",
            "native_session_id": native_session_id,
            "creation_mode": creation_mode,
            "ownership_generation": ownership_generation,
            "observed": _extension(event),
        }
    )


def claude_fixture_capabilities() -> tuple[CapabilityResult, ...]:
    """Return claims limited to the sanitized fixture boundary."""

    return (
        CapabilityResult(
            capability="session_identity",
            state=CapabilityState.PROVED,
            scope="fixture",
            evidence_ref="fixture://claude/stream.json#cursor-1",
            detail="system.init preserves the native session identifier",
        ),
        CapabilityResult(
            capability="ordered_events",
            state=CapabilityState.PROVED,
            scope="fixture",
            evidence_ref="fixture://claude/stream.json#cursor-2",
            detail="fixture source cursors are carried into NativeEvent",
        ),
        CapabilityResult(
            capability="cursor_reconnect",
            state=CapabilityState.PROVED,
            scope="fixture",
            evidence_ref="fixture://claude/stream.json#cursor-2",
            detail="duplicate source events remain idempotent through ContractStore",
        ),
        CapabilityResult(
            capability="native_completion",
            state=CapabilityState.PROVED,
            scope="fixture",
            evidence_ref="fixture://claude/stream.json#cursor-7",
            detail=(
                "only an explicit successful result with is_error=false is completed"
            ),
        ),
        CapabilityResult(
            capability="interruption",
            state=CapabilityState.PROVED,
            scope="fixture",
            evidence_ref="fixture://claude/interruption.json#cursor-3",
            detail="an explicit error result projects to interrupted, not completed",
        ),
        CapabilityResult(
            capability="attention_request",
            state=CapabilityState.PARTIAL,
            scope="fixture",
            evidence_ref="fixture://claude/stream.json#cursor-4",
            detail=(
                "can_use_tool is normalized as pending approval; "
                "only an explicit allow/deny response resolves it"
            ),
        ),
        CapabilityResult(
            capability="usage",
            state=CapabilityState.PARTIAL,
            scope="fixture",
            evidence_ref="fixture://claude/stream.json#cursor-2",
            detail=(
                "present token fields are preserved; absent values remain unavailable"
            ),
        ),
        CapabilityResult(
            capability="continuation_observation",
            state=CapabilityState.PARTIAL,
            scope="fixture",
            evidence_ref="fixture://claude/stream.json#cursor-6",
            detail="compact_boundary is observed; native resume semantics are unproved",
        ),
        CapabilityResult(
            capability="delivery_receipt",
            state=CapabilityState.UNSUPPORTED,
            scope="fixture",
            detail="the fixture stream has no native receipt for a logical message",
        ),
        CapabilityResult(
            capability="steering",
            state=CapabilityState.UNSUPPORTED,
            scope="fixture",
            detail="this normalizer has no send or steering operation",
        ),
        CapabilityResult(
            capability="history",
            state=CapabilityState.UNSUPPORTED,
            scope="fixture",
            detail="a stream observation is not a native history export",
        ),
        CapabilityResult(
            capability="live_native_behavior",
            state=CapabilityState.UNKNOWN,
            detail="no subscription-backed Claude process was started",
        ),
    )


class ClaudeSessionNormalizer:
    """Normalize one bound native Claude session without owning its process."""

    def __init__(self, binding: NativeBinding | Mapping[str, Any]):
        self.binding = NativeBinding.model_validate(binding)
        if self.binding.provider != CLAUDE_PROVIDER:
            raise ValueError("Claude normalizer requires a Claude binding")

    @classmethod
    def from_init(
        cls,
        raw: ClaudeFixtureRecord | Mapping[str, Any],
        **binding_kwargs: Any,
    ) -> "ClaudeSessionNormalizer":
        return cls(binding_from_init(raw, **binding_kwargs))

    @property
    def capabilities(self) -> tuple[CapabilityResult, ...]:
        return claude_fixture_capabilities()

    def normalize(
        self,
        raw: ClaudeFixtureRecord | Mapping[str, Any],
        *,
        ingested_at: datetime,
        ownership_generation: int | None = None,
    ) -> NativeEvent:
        """Map one source record to the shared event contract.

        The caller supplies ingestion time and ownership generation so replay
        observations remain distinguishable without changing source identity.
        ``ContractStore`` remains responsible for fencing, deduplication, and
        contiguous checkpoint projection.
        """

        record = _record(raw)
        event = record.event
        _validate_session(event, self.binding)
        normalized_type, attention, attention_key = _classify(event)
        generation = (
            self.binding.ownership_generation
            if ownership_generation is None
            else ownership_generation
        )
        if type(generation) is not int or generation < 1:
            raise ValueError("ownership_generation must be a positive integer")
        if (
            record.logical_message_id is not None
            and event.logical_message_id is not None
            and record.logical_message_id != event.logical_message_id
        ):
            raise ValueError("logical message IDs disagree between envelope and event")
        return NativeEvent.model_validate(
            {
                "binding_id": self.binding.binding_id,
                "ownership_generation": generation,
                "source_cursor": record.source_cursor,
                "native_type": _native_type(event),
                "normalized_type": normalized_type,
                "source_at": record.source_at or event.timestamp,
                "ingested_at": ingested_at,
                "raw_evidence_ref": record.raw_evidence_ref,
                "logical_message_id": record.logical_message_id
                or event.logical_message_id,
                "attention": attention,
                "attention_key": attention_key,
                "extension": _extension(event),
            }
        )

    def normalize_many(
        self,
        records: Iterable[ClaudeFixtureRecord | Mapping[str, Any]],
        *,
        ingested_at: datetime,
        ownership_generation: int | None = None,
    ) -> tuple[NativeEvent, ...]:
        return tuple(
            self.normalize(
                record,
                ingested_at=ingested_at,
                ownership_generation=ownership_generation,
            )
            for record in records
        )

    def observe_runtime(
        self, raw: ClaudeFixtureRecord | Mapping[str, Any]
    ) -> ClaudeRuntimeObservation:
        """Preserve process/quiet observations without calling them completion."""

        record = _record(raw)
        event = record.event
        _validate_session(event, self.binding)
        if event.type == "process_exit":
            if event.exit_code is None:
                raise ValueError("process_exit requires an exit_code")
            return ClaudeRuntimeObservation(
                kind="process_exit",
                source_cursor=record.source_cursor,
                raw_evidence_ref=record.raw_evidence_ref,
                source_at=record.source_at or event.timestamp,
                exit_code=event.exit_code,
            )
        if event.type == "quiet":
            return ClaudeRuntimeObservation(
                kind="quiet",
                source_cursor=record.source_cursor,
                raw_evidence_ref=record.raw_evidence_ref,
                source_at=record.source_at or event.timestamp,
            )
        raise ValueError("observe_runtime accepts only process_exit or quiet")
