"""Fixture-only normalisation for sanitized native Codex observations.

This module deliberately has no Codex process, SDK, transport, clock, or file
I/O.  A caller supplies the source cursor and ingestion timestamp that belong
to an observed record.  The adapter maps the small set of native event shapes
covered by the fixtures into the provider-neutral runtime contract and keeps
unknown records as ``unknown`` events with their original evidence reference.
"""

import re
from collections.abc import Collection, Iterable, Mapping
from dataclasses import dataclass
from enum import StrEnum

from models.native_agent import (
    AttentionRequest,
    NativeBinding,
    NativeEvent,
    ProviderExtension,
)


class CodexAdapterError(ValueError):
    """The sanitized external record cannot be safely normalized."""


class CodexEvidenceKind(StrEnum):
    """The evidence meaning retained alongside a normalized event."""

    RECEIPT = "receipt"
    DELIVERY = "delivery"
    ACTIVITY = "activity"
    OUTPUT = "output"
    COMPLETION = "completion"
    INTERRUPTION = "interruption"
    QUIET = "quiet"
    ATTENTION = "attention"
    USAGE = "usage"
    CONTINUATION = "continuation"
    UNKNOWN = "unknown"


class CodexDeliverySignal(StrEnum):
    """A delivery-related observation, separate from native status."""

    RECEIPT = "receipt"
    DELIVERED = "delivered"
    COMPLETED = "completed"
    INTERRUPTED = "interrupted"


@dataclass(frozen=True, slots=True)
class CodexObservation:
    """A normalized contract event plus its Codex-specific evidence meaning."""

    event: NativeEvent
    evidence_kind: CodexEvidenceKind
    delivery_signal: CodexDeliverySignal | None = None

    @property
    def native_event(self) -> NativeEvent:
        """Use an explicit name when passing the event to the shared store."""
        return self.event


@dataclass(frozen=True, slots=True)
class _Classification:
    normalized_type: str
    evidence_kind: CodexEvidenceKind
    delivery_signal: CodexDeliverySignal | None = None


_RECEIPT_TYPES = {
    "input.received",
    "message.received",
    "request.received",
    "turn.received",
    "message.accepted",
    "turn.accepted",
}
_QUIET_TYPES = {
    "keepalive",
    "no.output",
    "session.idle",
    "stream.end",
    "stream.idle",
    "turn.idle",
}
_INTERRUPTED_TYPES = {
    "response.aborted",
    "response.cancelled",
    "response.canceled",
    "session.interrupted",
    "turn.aborted",
    "turn.cancelled",
    "turn.canceled",
    "turn.interrupted",
}
_TRANSPORT_LOST_TYPES = {
    "connection.closed",
    "connection.lost",
    "session.disconnected",
    "stream.disconnected",
    "transport.lost",
}
_CONTINUATION_TYPES = {
    "context.compacted",
    "context.compaction",
    "context.continued",
    "thread.compacted",
    "thread.resumed",
    "turn.continued",
}
_USAGE_TYPES = {
    "thread.tokenusage.updated",
    "thread.token_usage.updated",
    "turn.usage",
    "usage",
    "usage.updated",
}
_COMPLETION_TYPES = {
    "response.completed",
    "run.completed",
    "session.completed",
    "turn.completed",
}
_COMPLETED_STATUSES = {"completed"}
_FAILED_TYPES = {
    "response.failed",
    "run.failed",
    "session.failed",
    "turn.failed",
}
_REQUEST_ITEM_TYPES = {
    "approval",
    "approval_request",
    "request_approval",
    "request_user_input",
    "user_input_request",
}
_ACTIVITY_ITEM_TYPES = {
    "command_execution",
    "command_execution_output",
    "file_change",
    "file_change_output",
    "mcp_tool_call",
    "tool_call",
    "collab_tool_call",
    "reasoning",
    "web_search",
}
_MESSAGE_ITEM_TYPES = {"agent_message", "assistant_message", "message"}
_ITEM_TYPE_ALIASES = {
    "agentMessage": "agent_message",
    "commandExecution": "command_execution",
    "fileChange": "file_change",
    "mcpToolCall": "mcp_tool_call",
    "webSearch": "web_search",
}
# Installed native server requests, in ``_canonical`` spelling. The JSON-RPC
# request ``id`` is the correlation identity; ``serverRequest/resolved`` echoes
# it as ``params.requestId``.
_NATIVE_APPROVAL_METHODS = frozenset(
    {
        "item.command_execution.request_approval",
        "item.file_change.request_approval",
        "item.permissions.request_approval",
    }
)
_NATIVE_USER_INPUT_METHODS = frozenset({"item.tool.request_user_input"})
_NATIVE_REQUEST_METHODS = _NATIVE_APPROVAL_METHODS | _NATIVE_USER_INPUT_METHODS
_NATIVE_RESOLVED_METHOD = "server_request.resolved"


def _mapping(value: object, label: str) -> Mapping[str, object]:
    if not isinstance(value, Mapping):
        raise CodexAdapterError(f"{label} must be an object")
    return value


def _codex_binding(value: NativeBinding | Mapping[str, object]) -> NativeBinding:
    binding = NativeBinding.model_validate(value)
    if binding.provider != "codex":
        raise CodexAdapterError("Codex adapter requires a Codex native binding")
    return binding


def _record_and_event(
    raw: Mapping[str, object],
) -> tuple[Mapping[str, object], Mapping[str, object]]:
    """Return the fixture envelope and its native event object."""
    event_value = raw.get("event")
    if event_value is None:
        return raw, raw
    return raw, _mapping(event_value, "event")


def _native_type(event: Mapping[str, object]) -> str:
    for key in ("type", "method", "event", "kind"):
        value = event.get(key)
        if value is not None:
            if not isinstance(value, str) or not value.strip():
                raise CodexAdapterError(f"event {key} must be a non-empty string")
            return value
    raise CodexAdapterError("event is missing its native type")


def _canonical(value: str) -> str:
    canonical = value.strip().replace("/", ".").replace("-", "_")
    canonical = re.sub(r"(?<=[a-z0-9])(?=[A-Z])", "_", canonical)
    return canonical.lower()


def _params(event: Mapping[str, object]) -> Mapping[str, object]:
    value = event.get("params")
    if value is None:
        return {}
    return _mapping(value, "params")


def _nested_objects(
    record: Mapping[str, object],
    event: Mapping[str, object],
    params: Mapping[str, object],
) -> tuple[Mapping[str, object], ...]:
    """Collect known Codex payload objects without recursively guessing fields."""
    values: list[Mapping[str, object]] = [record, event, params]
    for source in (record, event, params):
        for key in (
            "data",
            "error",
            "item",
            "result",
            "thread",
            "tokenUsage",
            "token_usage",
            "turn",
            "usage",
        ):
            value = source.get(key)
            if isinstance(value, Mapping):
                values.append(value)
                if key in {"tokenUsage", "token_usage"}:
                    for usage_key in ("last", "total"):
                        nested = value.get(usage_key)
                        if isinstance(nested, Mapping):
                            values.append(nested)
    return tuple(values)


def _first_value(
    objects: Iterable[Mapping[str, object]], keys: tuple[str, ...]
) -> object | None:
    for source in objects:
        for key in keys:
            if key in source:
                return source[key]
    return None


def _optional_text(value: object | None, label: str) -> str | None:
    if value is None:
        return None
    if not isinstance(value, str) or not value:
        raise CodexAdapterError(f"{label} must be a non-empty string when present")
    return value


def _required_field(record: Mapping[str, object], key: str) -> object:
    value = record.get(key)
    if value is None:
        raise CodexAdapterError(f"fixture record is missing {key}")
    return value


def _item(
    record: Mapping[str, object],
    event: Mapping[str, object],
    params: Mapping[str, object],
) -> Mapping[str, object] | None:
    for source in (record, event, params):
        value = source.get("item")
        if value is not None:
            return _mapping(value, "item")
    return None


def _item_type(item: Mapping[str, object] | None) -> str | None:
    if item is None or "type" not in item:
        return None
    value = item["type"]
    if not isinstance(value, str) or not value.strip():
        raise CodexAdapterError("item type must be a non-empty string")
    return _ITEM_TYPE_ALIASES.get(value, _canonical(value))


def _text_value(
    record: Mapping[str, object],
    event: Mapping[str, object],
    params: Mapping[str, object],
    item: Mapping[str, object] | None,
) -> str | None:
    sources: list[Mapping[str, object]] = []
    if item is not None:
        sources.append(item)
    sources.extend((record, event, params))
    value = _first_value(sources, ("text", "message", "output", "content"))
    if value is None:
        return None
    if not isinstance(value, str):
        # Structured content is not silently turned into user-visible output.
        return None
    return value


def _attention_request(
    record: Mapping[str, object],
    event: Mapping[str, object],
    params: Mapping[str, object],
    item: Mapping[str, object] | None,
) -> AttentionRequest | None:
    sources: list[Mapping[str, object]] = []
    if item is not None:
        sources.append(item)
    sources.extend((record, event, params))
    value = _first_value(sources, ("attention", "request"))
    if value is None:
        return None
    attention = _mapping(value, "attention")
    required = {
        "deduplication_key": attention.get("deduplication_key"),
        "request_type": attention.get("request_type"),
        "answer_shape": attention.get("answer_shape"),
    }
    if any(value is None for value in required.values()):
        # The native record signals a request but does not expose enough data
        # for the shared attention contract. Keep it as unsupported evidence.
        return None
    choices = attention.get("choices", ())
    if not isinstance(choices, (tuple, list)):
        raise CodexAdapterError("attention choices must be an array")
    return AttentionRequest(
        deduplication_key=required["deduplication_key"],
        request_type=required["request_type"],
        answer_shape=required["answer_shape"],
        choices=tuple(choices),
    )


def _attention_key(
    record: Mapping[str, object],
    event: Mapping[str, object],
    params: Mapping[str, object],
    item: Mapping[str, object] | None,
) -> str | None:
    sources: list[Mapping[str, object]] = []
    if item is not None:
        sources.append(item)
    sources.extend((record, event, params))
    value = _first_value(
        sources, ("attention_key", "attentionKey", "deduplication_key")
    )
    return _optional_text(value, "attention key")


@dataclass(frozen=True, slots=True)
class _NativeAttention:
    """Attention facts from a recognised native method; empty when incomplete."""

    request: AttentionRequest | None = None
    key: str | None = None


def _native_text(source: Mapping[str, object], key: str) -> str | None:
    value = source.get(key)
    return value if isinstance(value, str) and value else None


def _native_request_id(value: object) -> str | None:
    if isinstance(value, str) and value:
        return value
    if type(value) is int:
        return str(value)
    return None


def _native_user_input_request(
    params: Mapping[str, object], key: str
) -> AttentionRequest | None:
    """Map exactly one plain question; anything else stays unsupported."""
    questions = params.get("questions")
    if not isinstance(questions, (list, tuple)) or len(questions) != 1:
        return None
    question = questions[0]
    if not isinstance(question, Mapping):
        return None
    if _native_text(question, "id") is None:
        return None
    if _native_text(question, "question") is None:
        return None
    # The shared contract has no secret answer shape.
    secret = question.get("isSecret")
    if secret is not None and secret is not False:
        return None
    free_form = question.get("isOther")
    if free_form is not None and not isinstance(free_form, bool):
        return None
    options = question.get("options")
    if options is None:
        return AttentionRequest(
            deduplication_key=key, request_type="question", answer_shape="text"
        )
    if not isinstance(options, (list, tuple)) or not options or free_form:
        # Empty options are ambiguous, and choices cannot also allow free text.
        return None
    labels: list[str] = []
    for option in options:
        label = _native_text(option, "label") if isinstance(option, Mapping) else None
        if label is None:
            return None
        labels.append(label)
    if len(set(labels)) != len(labels):
        return None
    return AttentionRequest(
        deduplication_key=key,
        request_type="question",
        answer_shape="choice",
        choices=tuple(labels),
    )


def _native_attention(
    canonical: str,
    event: Mapping[str, object],
    params: Mapping[str, object],
    binding: NativeBinding,
) -> _NativeAttention | None:
    """Translate installed native request/resolution methods.

    Returns ``None`` when the method is not one of them. For a recognised
    method, an incomplete payload, or one for another thread, yields an empty
    result so the caller keeps the record as unknown evidence.
    """
    if (
        canonical != _NATIVE_RESOLVED_METHOD
        and canonical not in _NATIVE_REQUEST_METHODS
    ):
        return None
    thread_id = _native_text(params, "threadId")
    if thread_id != binding.native_session_id:
        return _NativeAttention()
    if canonical == _NATIVE_RESOLVED_METHOD:
        request_id = _native_request_id(params.get("requestId"))
    else:
        request_id = _native_request_id(event.get("id"))
        if _native_text(params, "turnId") is None:
            return _NativeAttention()
        if _native_text(params, "itemId") is None:
            return _NativeAttention()
    if request_id is None:
        return _NativeAttention()
    key = f"codex-request:{thread_id}:{request_id}"
    if canonical == _NATIVE_RESOLVED_METHOD:
        return _NativeAttention(key=key)
    if canonical in _NATIVE_APPROVAL_METHODS:
        request = AttentionRequest(
            deduplication_key=key, request_type="approval", answer_shape="boolean"
        )
    else:
        request = _native_user_input_request(params, key)
    return _NativeAttention(request=request, key=key if request else None)


def _status(
    record: Mapping[str, object],
    event: Mapping[str, object],
    params: Mapping[str, object],
) -> str | None:
    # Turn/response status is a string terminal state. Thread status is a
    # structured ThreadStatus object (for example {"type": "idle"}) and is
    # deliberately not interpreted as a turn state.
    for source in (params, event, record):
        for key in ("response", "run", "session", "turn"):
            value = source.get(key)
            if not isinstance(value, Mapping):
                continue
            for status_key in ("status", "state"):
                if status_key not in value or value[status_key] is None:
                    continue
                status = value[status_key]
                if not isinstance(status, str) or not status:
                    raise CodexAdapterError(
                        "status must be a non-empty string when present"
                    )
                return _canonical(status)

    # Fixture envelopes may carry a direct status. Keep the same precedence
    # after nested terminal objects, while leaving params.thread.status alone.
    for source in (params, event, record):
        for status_key in ("status", "state"):
            if status_key not in source or source[status_key] is None:
                continue
            status = source[status_key]
            if not isinstance(status, str) or not status:
                raise CodexAdapterError(
                    "status must be a non-empty string when present"
                )
            return _canonical(status)
    return None


def _native_thread_matches_binding(
    params: Mapping[str, object], binding: NativeBinding
) -> bool:
    """Return false when any explicit native thread identity is foreign."""
    identities: list[object] = []
    for key in ("threadId", "thread_id"):
        if key in params:
            identities.append(params[key])

    thread = params.get("thread")
    if thread is not None:
        thread_object = _mapping(thread, "thread")
        for key in ("id", "threadId", "thread_id"):
            if key in thread_object:
                identities.append(thread_object[key])

    return all(
        isinstance(identity, str)
        and bool(identity)
        and identity == binding.native_session_id
        for identity in identities
    )


def _native_event_id(
    record: Mapping[str, object],
    event: Mapping[str, object],
    params: Mapping[str, object],
    item: Mapping[str, object] | None,
) -> str | None:
    direct = _first_value(
        (record, event, params), ("native_event_id", "event_id", "eventId")
    )
    if direct is not None:
        return _optional_text(direct, "native event ID")
    # A direct event object may use id as its native event identity. Do not
    # treat JSON-RPC method ids as event ids; they correlate requests.
    if "method" not in event and event.get("id") is not None:
        return _optional_text(event["id"], "event ID")
    # Only one identifier fits the extension, so keep the most specific one:
    # item, then turn, then thread; an object's ``id`` before its flat alias.
    if item is not None:
        item_id = item.get("id")
        if item_id is not None:
            return _optional_text(item_id, "item ID")
    if params.get("itemId") is not None:
        return _optional_text(params["itemId"], "item ID")
    for key in ("turn", "thread"):
        value = params.get(key)
        if isinstance(value, Mapping) and value.get("id") is not None:
            return _optional_text(value["id"], f"{key} ID")
        if params.get(f"{key}Id") is not None:
            return _optional_text(params[f"{key}Id"], f"{key} ID")
    return None


def _usage_value(
    objects: Iterable[Mapping[str, object]], keys: tuple[str, ...]
) -> int | None:
    value = _first_value(objects, keys)
    if value is None:
        return None
    if type(value) is not int or value < 0:
        raise CodexAdapterError("usage values must be non-negative integers")
    return value


def _extension(
    binding: NativeBinding,
    record: Mapping[str, object],
    event: Mapping[str, object],
    params: Mapping[str, object],
    item: Mapping[str, object] | None,
    objects: tuple[Mapping[str, object], ...],
) -> ProviderExtension:
    provider = (
        _optional_text(
            _first_value(
                objects,
                (
                    "provider",
                    "provider_name",
                    "providerName",
                    "model_provider",
                    "modelProvider",
                ),
            ),
            "provider",
        )
        or binding.provider
    )
    runtime_version = _optional_text(
        _first_value(objects, ("runtime_version", "runtimeVersion")),
        "runtime version",
    )
    model = _optional_text(
        _first_value(objects, ("model", "model_slug", "modelSlug")), "model"
    )
    effort = _optional_text(
        _first_value(objects, ("effort", "reasoning_effort", "reasoningEffort")),
        "effort",
    )
    usage_objects = objects
    input_tokens = _usage_value(
        usage_objects,
        ("input_tokens", "inputTokens", "input_token_count"),
    )
    output_tokens = _usage_value(
        usage_objects,
        ("output_tokens", "outputTokens", "output_token_count"),
    )
    return ProviderExtension(
        provider=provider,
        runtime_version=runtime_version,
        native_event_id=_native_event_id(record, event, params, item),
        model=model,
        effort=effort,
        input_tokens=input_tokens,
        output_tokens=output_tokens,
    )


def _is_request_event(
    canonical: str, item_type: str | None, request: AttentionRequest | None
) -> bool:
    return (
        request is not None
        or item_type in _REQUEST_ITEM_TYPES
        or canonical.endswith((".approval.requested", ".approval_request"))
        or canonical.endswith((".input.requested", ".user_input.requested"))
        or canonical in {"approval.requested", "request_user_input"}
    )


def _classify(
    canonical: str,
    status: str | None,
    item_type: str | None,
    text: str | None,
    request: AttentionRequest | None,
    attention_key: str | None,
) -> _Classification:
    if canonical in _TRANSPORT_LOST_TYPES:
        return _Classification("transport_lost", CodexEvidenceKind.UNKNOWN)
    if (canonical in _NATIVE_REQUEST_METHODS and request is None) or (
        canonical == _NATIVE_RESOLVED_METHOD and attention_key is None
    ):
        # Incomplete or unsupported native request shapes are evidence only.
        return _Classification("unknown", CodexEvidenceKind.UNKNOWN)
    if canonical in _CONTINUATION_TYPES or "compaction" in canonical:
        return _Classification("continuation", CodexEvidenceKind.CONTINUATION)
    if canonical in _USAGE_TYPES or canonical.endswith(".usage.updated"):
        return _Classification("usage", CodexEvidenceKind.USAGE)

    resolved = "resolved" in canonical or canonical.endswith((".answered", ".closed"))
    if resolved and attention_key is not None:
        return _Classification("attention_resolved", CodexEvidenceKind.ATTENTION)
    if _is_request_event(canonical, item_type, request):
        if request is not None:
            return _Classification("attention", CodexEvidenceKind.ATTENTION)
        return _Classification("unknown", CodexEvidenceKind.UNKNOWN)

    if canonical in _INTERRUPTED_TYPES:
        return _Classification(
            "interrupted",
            CodexEvidenceKind.INTERRUPTION,
            CodexDeliverySignal.INTERRUPTED,
        )
    if canonical in _COMPLETION_TYPES:
        if status in {"interrupted", "cancelled", "canceled", "aborted"}:
            return _Classification(
                "interrupted",
                CodexEvidenceKind.INTERRUPTION,
                CodexDeliverySignal.INTERRUPTED,
            )
        if status in {"failed", "error", "errored"}:
            return _Classification("unknown", CodexEvidenceKind.UNKNOWN)
        if status is None or status in _COMPLETED_STATUSES:
            return _Classification(
                "completed",
                CodexEvidenceKind.COMPLETION,
                CodexDeliverySignal.COMPLETED,
            )
        return _Classification("unknown", CodexEvidenceKind.UNKNOWN)
    if canonical in _FAILED_TYPES:
        if status in {"interrupted", "cancelled", "canceled", "aborted"}:
            return _Classification(
                "interrupted",
                CodexEvidenceKind.INTERRUPTION,
                CodexDeliverySignal.INTERRUPTED,
            )
        return _Classification("unknown", CodexEvidenceKind.UNKNOWN)
    if canonical in _RECEIPT_TYPES:
        return _Classification(
            "unknown", CodexEvidenceKind.RECEIPT, CodexDeliverySignal.RECEIPT
        )
    if canonical == "turn.started":
        return _Classification(
            "activity", CodexEvidenceKind.DELIVERY, CodexDeliverySignal.DELIVERED
        )
    if canonical in _QUIET_TYPES:
        return _Classification("unknown", CodexEvidenceKind.QUIET)

    item_is_message = item_type in _MESSAGE_ITEM_TYPES
    if canonical in {"item.started", "item.completed"} or item_type is not None:
        if item_is_message or canonical in {
            "agent.message",
            "assistant.message",
        }:
            if text:
                return _Classification("output", CodexEvidenceKind.OUTPUT)
            return _Classification("unknown", CodexEvidenceKind.QUIET)
        if item_type in _ACTIVITY_ITEM_TYPES:
            return _Classification("activity", CodexEvidenceKind.ACTIVITY)

    if canonical in {
        "agent.message",
        "assistant.message",
        "message.delta",
        "message.created",
        "output",
        "stdout",
        "text.delta",
    }:
        if text:
            return _Classification("output", CodexEvidenceKind.OUTPUT)
        return _Classification("unknown", CodexEvidenceKind.QUIET)
    return _Classification("unknown", CodexEvidenceKind.UNKNOWN)


def _source_at(
    record: Mapping[str, object],
    event: Mapping[str, object],
    params: Mapping[str, object],
) -> object | None:
    return _first_value(
        (record, event, params),
        ("source_at", "sourceAt", "timestamp", "created_at", "createdAt"),
    )


def observe_codex_event(
    raw: Mapping[str, object],
    binding: NativeBinding | Mapping[str, object],
    *,
    source_cursor: int | None = None,
    ownership_generation: int | None = None,
    ingested_at: object | None = None,
    attention_keys: Collection[str] | None = None,
) -> CodexObservation:
    """Normalize one fixture record while retaining its source identity.

    ``source_cursor`` and ``raw_evidence_ref`` are required source facts.  A
    caller may provide the cursor/generation/ingestion timestamp separately
    when those values are maintained by a transport envelope, but this
    function never allocates or derives them.

    ``attention_keys`` optionally lists the attention requests the caller has
    already accepted. When supplied, a resolution for any other key stays
    ``unknown`` evidence, because the shared projection rejects a resolution
    that has no request and would stall the cursor. When omitted, the adapter
    is stateless and resolves any complete key.
    """
    record = _mapping(raw, "fixture record")
    native_binding = _codex_binding(binding)
    record, event = _record_and_event(record)
    params = _params(event)
    objects = _nested_objects(record, event, params)
    native_type = _native_type(event)
    cursor = (
        source_cursor
        if source_cursor is not None
        else _required_field(record, "source_cursor")
    )
    generation = (
        ownership_generation
        if ownership_generation is not None
        else record.get("ownership_generation", native_binding.ownership_generation)
    )
    received_at = (
        ingested_at
        if ingested_at is not None
        else _required_field(record, "ingested_at")
    )
    raw_evidence_ref = _required_field(record, "raw_evidence_ref")
    if "binding_id" in record and record["binding_id"] != native_binding.binding_id:
        raise CodexAdapterError("fixture record belongs to another binding")

    item = _item(record, event, params)
    item_type = _item_type(item)
    text = _text_value(record, event, params, item)
    canonical = _canonical(native_type)
    thread_matches_binding = _native_thread_matches_binding(params, native_binding)
    if not thread_matches_binding:
        request = None
        attention_key = None
        classification = _Classification("unknown", CodexEvidenceKind.UNKNOWN)
    else:
        native_attention = _native_attention(canonical, event, params, native_binding)
        if native_attention is None:
            request = _attention_request(record, event, params, item)
            attention_key = _attention_key(record, event, params, item)
        else:
            request = native_attention.request
            attention_key = native_attention.key
        classification = _classify(
            canonical,
            _status(record, event, params),
            item_type,
            text,
            request,
            attention_key,
        )
    if (
        classification.normalized_type == "attention_resolved"
        and attention_keys is not None
        and attention_key not in attention_keys
    ):
        classification = _Classification("unknown", CodexEvidenceKind.UNKNOWN)
    logical_message_id = _optional_text(
        _first_value(
            (record, event, params), ("logical_message_id", "logicalMessageId")
        ),
        "logical message ID",
    )
    extension = _extension(native_binding, record, event, params, item, objects)
    normalized = NativeEvent(
        binding_id=native_binding.binding_id,
        ownership_generation=generation,
        source_cursor=cursor,
        native_type=native_type,
        normalized_type=classification.normalized_type,
        source_at=_source_at(record, event, params),
        ingested_at=received_at,
        raw_evidence_ref=raw_evidence_ref,
        logical_message_id=logical_message_id,
        attention=request if classification.normalized_type == "attention" else None,
        attention_key=(
            attention_key
            if classification.normalized_type == "attention_resolved"
            else None
        ),
        extension=extension,
    )
    return CodexObservation(
        event=normalized,
        evidence_kind=classification.evidence_kind,
        delivery_signal=classification.delivery_signal,
    )


def normalize_codex_event(
    raw: Mapping[str, object],
    binding: NativeBinding | Mapping[str, object],
    *,
    source_cursor: int | None = None,
    ownership_generation: int | None = None,
    ingested_at: object | None = None,
    attention_keys: Collection[str] | None = None,
) -> NativeEvent:
    """Return the provider-neutral event for one sanitized Codex record."""
    return observe_codex_event(
        raw,
        binding,
        source_cursor=source_cursor,
        ownership_generation=ownership_generation,
        ingested_at=ingested_at,
        attention_keys=attention_keys,
    ).event


def observe_codex_events(
    records: Iterable[Mapping[str, object]],
    binding: NativeBinding | Mapping[str, object],
    *,
    ownership_generation: int | None = None,
    ingested_at: object | None = None,
) -> tuple[CodexObservation, ...]:
    """Normalize records in supplied order; no cursor is assigned or sorted.

    Batches carry no attention state; use ``observe_codex_event`` with
    ``attention_keys`` when unmatched resolutions must stay unknown.
    """
    return tuple(
        observe_codex_event(
            record,
            binding,
            ownership_generation=ownership_generation,
            ingested_at=ingested_at,
        )
        for record in records
    )


def normalize_codex_events(
    records: Iterable[Mapping[str, object]],
    binding: NativeBinding | Mapping[str, object],
    *,
    ownership_generation: int | None = None,
    ingested_at: object | None = None,
) -> tuple[NativeEvent, ...]:
    """Normalize records in supplied order and discard no raw evidence."""
    return tuple(
        observation.event
        for observation in observe_codex_events(
            records,
            binding,
            ownership_generation=ownership_generation,
            ingested_at=ingested_at,
        )
    )


class CodexFixtureAdapter:
    """Small, side-effect-free adapter facade used by fixture tests."""

    def __init__(self, binding: NativeBinding | Mapping[str, object]):
        self.binding = _codex_binding(binding)

    def observe(
        self,
        raw: Mapping[str, object],
        *,
        source_cursor: int | None = None,
        ownership_generation: int | None = None,
        ingested_at: object | None = None,
        attention_keys: Collection[str] | None = None,
    ) -> CodexObservation:
        return observe_codex_event(
            raw,
            self.binding,
            source_cursor=source_cursor,
            ownership_generation=ownership_generation,
            ingested_at=ingested_at,
            attention_keys=attention_keys,
        )

    def normalize(
        self,
        raw: Mapping[str, object],
        *,
        source_cursor: int | None = None,
        ownership_generation: int | None = None,
        ingested_at: object | None = None,
        attention_keys: Collection[str] | None = None,
    ) -> NativeEvent:
        return self.observe(
            raw,
            source_cursor=source_cursor,
            ownership_generation=ownership_generation,
            ingested_at=ingested_at,
            attention_keys=attention_keys,
        ).event

    def normalize_many(
        self,
        records: Iterable[Mapping[str, object]],
        *,
        ownership_generation: int | None = None,
        ingested_at: object | None = None,
    ) -> tuple[NativeEvent, ...]:
        return normalize_codex_events(
            records,
            self.binding,
            ownership_generation=ownership_generation,
            ingested_at=ingested_at,
        )
