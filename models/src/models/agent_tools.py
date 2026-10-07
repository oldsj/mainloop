"""Validated inputs for the Mainloop MCP tools."""

from typing import Annotated, Literal

from pydantic import BaseModel, ConfigDict, Field, StringConstraints, field_validator

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


class OpenPullRequest(ToolInput):
    project_id: Annotated[str, Field(min_length=1, max_length=100)]
    branch: Annotated[str, Field(min_length=1, max_length=255)]
    expected_sha: Annotated[str, StringConstraints(pattern=r"^[0-9a-f]{40}$")]
    title: Annotated[str, Field(min_length=1, max_length=256)]
    body: Annotated[str, Field(max_length=60000)]
    request_id: Annotated[str, Field(min_length=1, max_length=100)]

    @field_validator("branch")
    @classmethod
    def feature_branch(cls, value: str) -> str:
        # git-check-ref-format branch rules; reject rev expressions and owner:branch heads.
        if (
            value.startswith(("-", "/", "refs/"))
            or value.endswith(("/", "."))
            or any(c.isspace() or ord(c) < 32 or ord(c) == 127 for c in value)
            or any(c in value for c in "~^:?*[\\")
            or any(s in value for s in ("..", "@{", "//"))
            or value == "@"
            or any(p.startswith(".") or p.endswith(".lock") for p in value.split("/"))
        ):
            raise ValueError("invalid feature branch")
        return value
