"""Single-process contract reference implementation, with no delivery side effects.

A future database implementation must make each operation atomic and persist the
SENDING intent before touching a transport. This object is not a scheduler.
"""

from datetime import datetime

from mainloop.runtime.projection import project_checkpoint

from models.native_agent import (
    CapabilityResult,
    Checkpoint,
    DeliveryAttempt,
    DeliveryState,
    MessageEnvelope,
    NativeBinding,
    NativeEvent,
    ReconciliationEvidence,
    WorkspaceBinding,
)


class ContractError(ValueError):
    """Rejected operation; the store remains unchanged."""


class StaleOwnership(ContractError):
    """Caller does not own the current binding generation."""


def capability_result(
    workspace: WorkspaceBinding | dict, name: str
) -> CapabilityResult:
    """Return the declared result, including unsupported, with no fallback action."""
    workspace = WorkspaceBinding.model_validate(workspace)
    matches = [item for item in workspace.capabilities if item.capability == name]
    if len(matches) > 1:
        raise ContractError("duplicate capability declarations")
    return matches[0] if matches else CapabilityResult(capability=name)


class ContractStore:
    """Test-local records for one binding. No I/O, clocks, UUIDs, or model calls."""

    def __init__(self, binding: NativeBinding | dict):
        self._binding = NativeBinding.model_validate(binding)
        self._messages: dict[str, MessageEnvelope] = {}
        self._attempts: dict[str, DeliveryAttempt] = {}
        self._events: dict[int, NativeEvent] = {}

    @property
    def binding(self) -> NativeBinding:
        return self._binding

    @property
    def messages(self) -> tuple[MessageEnvelope, ...]:
        return tuple(self._messages.values())

    @property
    def attempts(self) -> tuple[DeliveryAttempt, ...]:
        return tuple(self._attempts.values())

    @property
    def events(self) -> tuple[NativeEvent, ...]:
        return tuple(self._events[key] for key in sorted(self._events))

    def _fence(self, generation: int) -> None:
        if (
            type(generation) is not int
            or generation != self.binding.ownership_generation
        ):
            raise StaleOwnership("ownership generation is not current")

    def take_ownership(self, expected_generation: int) -> NativeBinding:
        """Compare-and-swap generation. Never releases uncertain work for retry."""
        self._fence(expected_generation)
        self._binding = NativeBinding.model_validate(
            {
                **self.binding.model_dump(),
                "ownership_generation": expected_generation + 1,
            }
        )
        for key, attempt in self._attempts.items():
            if attempt.state == DeliveryState.SENDING:
                self._attempts[key] = DeliveryAttempt.model_validate(
                    {
                        **attempt.model_dump(),
                        "state": DeliveryState.UNCERTAIN,
                    }
                )
        return self.binding

    def record_message(
        self, raw: MessageEnvelope | dict, generation: int
    ) -> MessageEnvelope:
        self._fence(generation)
        message = MessageEnvelope.model_validate(raw)
        if message.desired_binding_id != self.binding.binding_id:
            raise ContractError("message targets another binding")
        old = self._messages.get(message.logical_message_id)
        if old is not None and old != message:
            raise ContractError("logical message ID reused with different content")
        self._messages.setdefault(message.logical_message_id, message)
        return self._messages[message.logical_message_id]

    def create_attempt(
        self, raw: DeliveryAttempt | dict, generation: int
    ) -> DeliveryAttempt:
        self._fence(generation)
        attempt = DeliveryAttempt.model_validate(raw)
        if (
            attempt.binding_id != self.binding.binding_id
            or attempt.ownership_generation != generation
        ):
            raise StaleOwnership("attempt binding/generation mismatch")
        if attempt.logical_message_id not in self._messages:
            raise ContractError("record logical message before delivery")
        if (
            attempt.state != DeliveryState.RECORDED
            or attempt.evidence_ref is not None
            or attempt.result is not None
        ):
            raise ContractError("new attempts must start recorded without a result")
        old = self._attempts.get(attempt.attempt_id)
        if old is not None:
            identity = {
                "attempt_id",
                "logical_message_id",
                "binding_id",
                "ownership_generation",
                "created_at",
            }
            if old.model_dump(include=identity) != attempt.model_dump(include=identity):
                raise ContractError("attempt ID reused with different identity")
            return old
        prior = [
            item
            for item in self.attempts
            if item.logical_message_id == attempt.logical_message_id
        ]
        if any(item.state != DeliveryState.FAILED for item in prior):
            raise ContractError("existing attempt prevents duplicate delivery")
        if prior and attempt.created_at < max(item.updated_at for item in prior):
            raise ContractError("retry predates prior attempt")
        self._attempts[attempt.attempt_id] = attempt
        return attempt

    def transition(
        self,
        attempt_id: str,
        generation: int,
        state: DeliveryState | str,
        at: datetime,
        *,
        evidence_ref: str | None = None,
    ) -> DeliveryAttempt:
        self._fence(generation)
        old = self._attempts[attempt_id]
        if old.ownership_generation != generation:
            raise StaleOwnership("historical attempt requires reconciliation")
        state = DeliveryState(state)
        allowed = {
            DeliveryState.RECORDED: {DeliveryState.QUEUED, DeliveryState.FAILED},
            DeliveryState.QUEUED: {DeliveryState.SENDING, DeliveryState.FAILED},
            DeliveryState.SENDING: {DeliveryState.DELIVERED, DeliveryState.UNCERTAIN},
            DeliveryState.DELIVERED: {DeliveryState.COMPLETED},
        }
        if state not in allowed.get(old.state, set()):
            raise ContractError(f"invalid delivery transition: {old.state} -> {state}")
        if (
            state in {DeliveryState.DELIVERED, DeliveryState.COMPLETED}
            and not evidence_ref
        ):
            raise ContractError("delivery/completion requires evidence")
        updated = DeliveryAttempt.model_validate(
            {
                **old.model_dump(),
                "state": state,
                "updated_at": at,
                "evidence_ref": evidence_ref,
            }
        )
        if updated.updated_at < old.updated_at:
            raise ContractError("transition time regressed")
        self._attempts[attempt_id] = updated
        return updated

    def reconcile(
        self, raw: ReconciliationEvidence | dict, generation: int
    ) -> DeliveryAttempt:
        """Current owner may resolve historical uncertainty using correlated evidence."""
        self._fence(generation)
        evidence = ReconciliationEvidence.model_validate(raw)
        old = self._attempts[evidence.attempt_id]
        if evidence.binding_id != self.binding.binding_id:
            raise ContractError("reconciliation belongs to another binding")
        historical_unsent = old.ownership_generation < generation and old.state in {
            DeliveryState.RECORDED,
            DeliveryState.QUEUED,
        }
        if historical_unsent and evidence.outcome != "not_delivered":
            raise ContractError("historical unsent attempts can only be retired")
        if not historical_unsent and old.state not in {
            DeliveryState.UNCERTAIN,
            DeliveryState.DELIVERED,
        }:
            raise ContractError("attempt does not require reconciliation")
        if old.state == DeliveryState.DELIVERED and evidence.outcome != "completed":
            raise ContractError("acknowledged delivery cannot be undone")
        states = {
            "not_delivered": DeliveryState.FAILED,
            "delivered": DeliveryState.DELIVERED,
            "completed": DeliveryState.COMPLETED,
        }
        if evidence.observed_at < old.updated_at:
            raise ContractError("reconciliation evidence predates attempt state")
        updated = DeliveryAttempt.model_validate(
            {
                **old.model_dump(),
                "state": states[evidence.outcome],
                "updated_at": evidence.observed_at,
                "evidence_ref": evidence.evidence_ref,
                "result": evidence.outcome,
            }
        )
        self._attempts[old.attempt_id] = updated
        return updated

    def ingest(self, raw: NativeEvent | dict, generation: int) -> NativeEvent:
        self._fence(generation)
        event = NativeEvent.model_validate(raw)
        if (
            event.binding_id != self.binding.binding_id
            or event.ownership_generation != generation
        ):
            raise StaleOwnership("event binding/generation mismatch")
        if (
            event.logical_message_id is not None
            and event.logical_message_id not in self._messages
        ):
            raise ContractError("event references unknown logical message")
        old = self._events.get(event.source_cursor)
        if old is not None:
            if old.model_dump(
                exclude={"ownership_generation", "ingested_at"}
            ) != event.model_dump(exclude={"ownership_generation", "ingested_at"}):
                raise ContractError("source cursor reused with different event")
            return old
        # Validate the candidate projection before mutating the event journal.
        project_checkpoint(self.binding, (*self.events, event), self.attempts)
        self._events[event.source_cursor] = event
        return event

    def checkpoint(self, generation: int, **references: str | None) -> Checkpoint:
        self._fence(generation)
        return project_checkpoint(
            self.binding, self.events, self.attempts, **references
        )
