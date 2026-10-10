"""Content-free v1 observations shared with the kagent harness."""

from datetime import datetime
from pathlib import PurePosixPath
from typing import Annotated, Literal

from pydantic import (
    BaseModel,
    ConfigDict,
    Field,
    TypeAdapter,
    field_validator,
    model_validator,
)

SCHEMA = "kagent.dev/orchestration-health/v1"


def is_health_part(name: str | None, data: object) -> bool:
    prefix = "kagent.dev/orchestration-health/"
    schema = data.get("schema_version") if isinstance(data, dict) else None
    return bool(name and name.startswith(prefix)) or (
        isinstance(schema, str) and schema.startswith(prefix)
    )


Key = Annotated[str, Field(min_length=1, max_length=128, pattern=r"^[a-zA-Z0-9_.:-]+$")]
Count = Annotated[int, Field(strict=True, ge=0, le=2**53 - 1)]


class Snapshot(BaseModel):
    model_config = ConfigDict(extra="forbid", strict=True)


class Turn(Snapshot):
    state: Literal["started", "finished", "failed"]
    coverage: Literal["complete", "partial"]


class Tool(Snapshot):
    call_key: Key
    phase: Literal["started", "finished"]
    tool_class: Literal["builtin", "shell", "mcp", "other"]
    operation_key: Key
    action: Literal["read", "edit", "search", "other"]
    path: Annotated[str, Field(max_length=256)] | None
    outcome: Literal["success", "error", "denied", "unknown"]
    category: Literal["none", "tool_error", "mcp_error", "permission_denied"]

    @field_validator("path")
    @classmethod
    def safe_path(cls, value):
        if value is None:
            return value
        path = PurePosixPath(value)
        if (
            not value
            or path.is_absolute()
            or str(path) != value
            or ".." in path.parts
            or any(c in value for c in "\n\r\x00?@\\")
            or any(
                part.startswith(".env")
                or "credential" in part
                or "secret" in part
                or part in {".ssh", ".aws", ".kube"}
                or part.endswith((".pem", ".key"))
                for part in value.lower().split("/")
            )
        ):
            raise ValueError("unsafe health path")
        return value


class Usage(Snapshot):
    scope: Literal["turn"]
    basis: Literal["turn_total"]
    inclusion: Literal["tree_inclusive"]
    sample_key: Key
    revision: Annotated[int, Field(strict=True, ge=1)]
    input_tokens: Count | None
    output_tokens: Count | None
    coverage: Literal["complete", "partial"]


class Event(Snapshot):
    schema_version: Literal["kagent.dev/orchestration-health/v1"]
    event_id: Key
    producer_epoch: Key
    sequence: Annotated[int, Field(strict=True, ge=1, le=1000000)]
    occurred_at: datetime
    time_source: Literal["adapter_observed"]
    time_precision: Literal["nanosecond"]
    provider: Literal["claude"]
    runtime_version: Annotated[str, Field(max_length=32, pattern=r"^[a-zA-Z0-9_.-]*$")]
    source: Literal["claude_result_stream"]
    turn_key: Key
    runtime_session_id: Key
    a2a_task_id: Key

    @model_validator(mode="after")
    def identity(self):
        if (
            self.turn_key != self.producer_epoch
            or self.event_id != f"{self.producer_epoch}:{self.sequence}"
        ):
            raise ValueError("invalid producer identity")
        return self

    @field_validator("occurred_at")
    @classmethod
    def aware(cls, value):
        if value.utcoffset() is None:
            raise ValueError("timestamp requires timezone")
        return value


class TurnEvent(Event):
    kind: Literal["turn"]
    turn: Turn


class ToolEvent(Event):
    kind: Literal["tool"]
    tool: Tool


class UsageEvent(Event):
    kind: Literal["usage"]
    usage: Usage

    @model_validator(mode="after")
    def sample(self):
        if self.usage.sample_key != self.turn_key:
            raise ValueError("sample must belong to its turn")
        return self


HealthEvent = Annotated[TurnEvent | ToolEvent | UsageEvent, Field(discriminator="kind")]
event_adapter = TypeAdapter(HealthEvent)
