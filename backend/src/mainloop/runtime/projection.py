"""Deterministic replay of a binding's contiguous evidence prefix."""

from collections.abc import Iterable

from models.native_agent import (
    AttentionItem,
    Checkpoint,
    DeliveryAttempt,
    DeliveryState,
    NativeBinding,
    NativeEvent,
    NativeStatus,
)


def project_checkpoint(
    binding: NativeBinding,
    events: Iterable[NativeEvent | dict],
    attempts: Iterable[DeliveryAttempt | dict] = (),
    *,
    repository_ref: str | None = None,
    candidate_ref: str | None = None,
) -> Checkpoint:
    """Replay without a model. Cursors start at 1 and never reset on takeover.

    Gapped events remain stored but cannot advance any projected state. Replayed
    historical generations are valid; live writes are fenced by ContractStore.
    Observation generations do not determine native source-event order.
    """
    binding = NativeBinding.model_validate(binding)
    ordered: dict[int, NativeEvent] = {}
    for raw in events:
        event = NativeEvent.model_validate(raw)
        if event.binding_id != binding.binding_id:
            raise ValueError("event belongs to another binding")
        if event.ownership_generation > binding.ownership_generation:
            raise ValueError("event belongs to a future owner")
        previous = ordered.get(event.source_cursor)
        if previous is not None and event.model_dump(
            exclude={"ownership_generation", "ingested_at"}
        ) != previous.model_dump(exclude={"ownership_generation", "ingested_at"}):
            raise ValueError("conflicting source cursor")
        # Reconnect observations are not new source events. Choose a canonical
        # observation independently of input order; source cursors order replay.
        if previous is None or (event.ownership_generation, event.ingested_at) < (
            previous.ownership_generation,
            previous.ingested_at,
        ):
            ordered[event.source_cursor] = event

    cursor = 0
    status = NativeStatus.UNKNOWN
    verified_at = None
    attention: dict[str, AttentionItem] = {}
    while cursor + 1 in ordered:
        cursor += 1
        event = ordered[cursor]
        if event.normalized_type in {"activity", "output"}:
            status = NativeStatus.ACTIVE
        elif event.normalized_type == "completed":
            status = NativeStatus.COMPLETED
        elif event.normalized_type == "interrupted":
            status = NativeStatus.INTERRUPTED
        elif event.normalized_type == "transport_lost":
            status = NativeStatus.UNKNOWN
        elif event.attention is not None:
            key = event.attention.deduplication_key
            existing = attention.get(key)
            if existing is not None:
                if (
                    existing.request != event.attention
                    or existing.logical_message_id != event.logical_message_id
                ):
                    raise ValueError("conflicting attention correlation")
            else:
                attention[key] = AttentionItem(
                    binding_id=binding.binding_id,
                    source_cursor=cursor,
                    logical_message_id=event.logical_message_id,
                    request=event.attention,
                )
            if attention[key].state == "pending":
                status = NativeStatus.WAITING
        elif event.attention_key is not None:
            if event.attention_key not in attention:
                raise ValueError("attention resolution without request")
            old = attention[event.attention_key]
            attention[event.attention_key] = old.model_copy(
                update={"state": "resolved"}
            )
            status = (
                NativeStatus.WAITING
                if any(item.state == "pending" for item in attention.values())
                else NativeStatus.UNKNOWN
            )
        if event.normalized_type in {
            "activity",
            "output",
            "completed",
            "interrupted",
            "transport_lost",
            "attention",
            "attention_resolved",
        }:
            verified_at = event.source_at

    pending = []
    for raw in attempts:
        attempt = DeliveryAttempt.model_validate(raw)
        if (
            attempt.binding_id != binding.binding_id
            or attempt.ownership_generation > binding.ownership_generation
        ):
            raise ValueError("attempt outside binding history")
        if attempt.state not in {DeliveryState.COMPLETED, DeliveryState.FAILED}:
            pending.append(attempt)
    return Checkpoint(
        binding_id=binding.binding_id,
        ownership_generation=binding.ownership_generation,
        evidence_cursor=cursor,
        native_status=status,
        verified_at=verified_at,
        pending_delivery=tuple(sorted(pending, key=lambda item: item.attempt_id)),
        attention=tuple(attention[key] for key in sorted(attention)),
        repository_ref=repository_ref,
        candidate_ref=candidate_ref,
    )
