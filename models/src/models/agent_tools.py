"""Validated inputs for the Mainloop MCP tools."""

from typing import Annotated, Literal

from pydantic import BaseModel, ConfigDict, Field, StringConstraints

Text = Annotated[
    str, StringConstraints(strip_whitespace=True, min_length=1, max_length=2000)
]
Name = Annotated[
    str, StringConstraints(strip_whitespace=True, min_length=1, max_length=80)
]
SessionId = Annotated[str, StringConstraints(strip_whitespace=True, min_length=1)]


class ToolInput(BaseModel):
    model_config = ConfigDict(extra="forbid", str_strip_whitespace=True)


class TopicOpen(ToolInput):
    name: Name
    status: Annotated[str, Field(max_length=2000)] | None = None


class Record(ToolInput):
    text: Text
    topic: Name | None = None


class PendingDone(ToolInput):
    id: Annotated[str, Field(min_length=8)]


class Delegate(ToolInput):
    topic: Name = "inbox"
    kind: Literal["claude", "codex"]
    title: Annotated[str, Field(max_length=80)] = ""
    brief: Annotated[str, Field(min_length=1)]


class Report(ToolInput):
    summary: Annotated[str, Field(min_length=1, max_length=4000)]


class OptionalSession(ToolInput):
    session: SessionId | None = None


class RequiredSession(ToolInput):
    session: SessionId


class Read(RequiredSession):
    since: Annotated[int, Field(ge=0)] = 0
