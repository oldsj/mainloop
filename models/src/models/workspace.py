"""Branch workspace intent and the lifecycle observed from its kagent Session."""

import ipaddress
import re
from enum import StrEnum
from typing import Annotated
from urllib.parse import urlsplit

from pydantic import AwareDatetime, ConfigDict, Field, StrictStr, field_validator

from models.native_agent import ContractModel
from models.provider import ProviderProfileId

WorkspaceIdentifier = Annotated[StrictStr, Field(min_length=1)]


class WorkspaceContractModel(ContractModel):
    model_config = ConfigDict(
        extra="forbid", frozen=True, strict=True, revalidate_instances="always"
    )


def _is_host_or_ip(host: str) -> bool:
    try:
        ipaddress.ip_address(host)
        return True
    except ValueError:
        labels = host.lower().split(".")
        return len(host) <= 253 and all(
            label
            and len(label) <= 63
            and re.fullmatch(r"[a-z0-9](?:[a-z0-9-]*[a-z0-9])?", label)
            for label in labels
        )


class WorkspaceAgentKind(StrEnum):
    CLAUDE = "claude"
    CODEX = "codex"


class WorkspaceObservedState(StrEnum):
    RUNNING = "running"
    SUSPENDING = "suspending"
    SUSPENDED = "suspended"
    RESUMING = "resuming"
    FAILED = "failed"
    UNKNOWN = "unknown"


class WorkspacePort(WorkspaceContractModel):
    name: Annotated[StrictStr, Field(min_length=1, pattern=r"^[a-z][a-z0-9-]{0,31}$")]
    number: Annotated[int, Field(ge=1, le=65535, strict=True)]


class WorkspaceDev(WorkspaceContractModel):
    """Dev server settings: which ports the preview URL may reach, and when to idle out."""

    ports: tuple[WorkspacePort, ...] = ()
    idle_timeout_minutes: Annotated[int, Field(ge=1, le=1440, strict=True)] = 30

    @field_validator("ports", mode="before")
    @classmethod
    def parse_json_ports(cls, values):
        return tuple(values) if isinstance(values, list) else values

    @field_validator("ports")
    @classmethod
    def unique_port_names_and_numbers(
        cls, values: tuple[WorkspacePort, ...]
    ) -> tuple[WorkspacePort, ...]:
        names = [port.name for port in values]
        numbers = [port.number for port in values]
        if len(names) != len(set(names)) or len(numbers) != len(set(numbers)):
            raise ValueError("workspace port names and numbers must be unique")
        return values


class WorkspaceManifest(WorkspaceContractModel):
    """What kagent clones into the harness (``repo_url``/``ref``/``branch``/``depth``), which
    agent runs there, and the dev server settings.

    The repository fields are the Session's ``workspace``. They are stored once and resent
    unchanged if the Session is replaced.
    """

    repo_url: Annotated[StrictStr, Field(min_length=1, max_length=2048)]
    # Branch, tag or commit to check out; empty means the repository default.
    ref: Annotated[StrictStr, Field(max_length=255)] = ""
    # Local branch to create or switch to.
    branch: Annotated[StrictStr, Field(min_length=1, max_length=255)]
    # Clone depth; 0 means the kagent default.
    depth: Annotated[int, Field(ge=0, le=1000, strict=True)] = 0
    agent_kind: ProviderProfileId = "claude"
    dev: WorkspaceDev = WorkspaceDev()

    @field_validator("repo_url")
    @classmethod
    def validate_repo_url(cls, value: str) -> str:
        try:
            parsed = urlsplit(value)
            hostname = parsed.hostname
        except ValueError as exc:
            raise ValueError("repo_url must be a valid URL") from exc
        if (
            parsed.scheme != "https"
            or not hostname
            or not _is_host_or_ip(hostname)
            or not parsed.path.strip("/")
            or parsed.query
            or parsed.fragment
            or parsed.username
            or parsed.password
        ):
            raise ValueError(
                "repo_url must be an HTTPS URL with a host and repository path and no credentials"
            )
        return value

    @field_validator("branch", "ref")
    @classmethod
    def validate_git_name(cls, value: str) -> str:
        if not value:
            return value
        invalid = set(" ~^:?*[\\")
        components = value.split("/")
        if (
            any(char in invalid or ord(char) < 32 for char in value)
            or value.startswith("-")
            or value.startswith("/")
            or value.endswith(("/", ".", ".lock"))
            or ".." in value
            or "@{" in value
            or value == "@"
            or "//" in value
            or any(
                component in ("", ".", "..")
                or component.startswith(".")
                or component.endswith((".", ".lock"))
                for component in components
            )
        ):
            raise ValueError("not a valid Git branch or ref name")
        return value


class WorkspaceLifecycle(WorkspaceContractModel):
    """A workspace as last observed from kagent. Nothing here is stored except the manifest."""

    workspace_id: WorkspaceIdentifier
    session_id: WorkspaceIdentifier
    observed_state: WorkspaceObservedState
    # Why the state is what it is, when kagent said (a failure, an operation in progress).
    detail: StrictStr | None = None
    manifest: WorkspaceManifest
    last_activity_at: AwareDatetime | None = None
    updated_at: AwareDatetime
