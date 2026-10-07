"""Purpose-separated publication contracts; these confer no MCP or owner authority."""

from enum import StrEnum
from typing import Annotated

from pydantic import BaseModel, ConfigDict, Field


class PushContract(BaseModel):
    model_config = ConfigDict(extra="forbid", frozen=True)


class ProtectedBranchPolicy(PushContract):
    project_id: Annotated[str, Field(min_length=1)]
    version: Annotated[int, Field(ge=1)]
    default_branch: Annotated[str, Field(min_length=1)]
    patterns: tuple[str, ...] = ()
    previous_defaults: tuple[str, ...] = ()


class PushGrant(PushContract):
    id: Annotated[str, Field(min_length=1)]
    owner_id: Annotated[str, Field(min_length=1)]
    project_id: Annotated[str, Field(min_length=1)]
    repository: Annotated[str, Field(min_length=1)]
    branch: Annotated[str, Field(min_length=1)]
    workspace_id: Annotated[str, Field(min_length=1)]
    session_id: Annotated[str, Field(min_length=1)]
    runtime_identity: Annotated[str, Field(min_length=1)]
    version: Annotated[int, Field(ge=1)] = 1
    role: str = "agent"
    grant_kind: str = "workspace"
    active: bool = True
    archived: bool = False
    terminal: bool = False


class RefUpdate(PushContract):
    ref: Annotated[str, Field(min_length=1)]
    old_oid: Annotated[str, Field(pattern=r"^[0-9a-f]{40}$")]
    new_oid: Annotated[str, Field(pattern=r"^[0-9a-f]{40}$")]


class PublicationState(StrEnum):
    PENDING = "pending"
    DISPATCHING = "dispatching"
    CONFIRMED = "confirmed"
    REJECTED = "rejected"
    UNKNOWN = "unknown"


class PublicationAttempt(PushContract):
    request_id: Annotated[str, Field(min_length=1)]
    grant_id: Annotated[str, Field(min_length=1)]
    repository: Annotated[str, Field(min_length=1)]
    update: RefUpdate
    grant_version: Annotated[int, Field(ge=1)]
    policy_version: Annotated[int, Field(ge=1)]
    state: PublicationState = PublicationState.PENDING
