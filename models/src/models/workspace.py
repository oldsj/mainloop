"""Declarative workspace intent and durable lifecycle observations."""

import ipaddress
import re
from enum import StrEnum
from typing import Annotated, Literal
from urllib.parse import urlsplit

from pydantic import AwareDatetime, ConfigDict, Field, StrictStr, field_validator

from models.native_agent import ContractModel

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


class WorkspaceDesiredState(StrEnum):
    RUNNING = "running"
    SUSPENDED = "suspended"


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
    protocol: Literal["http"] = "http"


class WorkspaceService(WorkspaceContractModel):
    name: Annotated[StrictStr, Field(min_length=1, pattern=r"^[a-z][a-z0-9-]{0,31}$")]
    image: Annotated[StrictStr, Field(min_length=1)]
    env: dict[StrictStr, StrictStr] = Field(default_factory=dict)
    ports: tuple[Annotated[int, Field(ge=1, le=65535, strict=True)], ...] = ()

    @field_validator("ports", mode="before")
    @classmethod
    def parse_json_ports(cls, values):
        return tuple(values) if isinstance(values, list) else values

    @field_validator("ports")
    @classmethod
    def unique_ports(cls, values: tuple[int, ...]) -> tuple[int, ...]:
        if len(values) != len(set(values)):
            raise ValueError("service ports must be unique")
        return values


class WorkspaceDev(WorkspaceContractModel):
    image: Annotated[StrictStr, Field(min_length=1)] | None = None
    devcontainer_ref: Annotated[StrictStr, Field(min_length=1)] | None = None
    actor_template: (
        Annotated[StrictStr, Field(pattern=r"^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$")]
        | None
    ) = None
    services: tuple[WorkspaceService, ...] = ()
    ports: tuple[WorkspacePort, ...] = ()
    idle_timeout_minutes: Annotated[int, Field(ge=1, le=1440, strict=True)] = 30

    @field_validator("services", "ports", mode="before")
    @classmethod
    def parse_json_collections(cls, values):
        return tuple(values) if isinstance(values, list) else values

    @field_validator("image", "devcontainer_ref")
    @classmethod
    def validate_nonblank(cls, value: str | None) -> str | None:
        if value is not None and not value.strip():
            raise ValueError("image and devcontainer_ref must not be blank")
        return value

    @field_validator("services")
    @classmethod
    def unique_service_names(
        cls, values: tuple[WorkspaceService, ...]
    ) -> tuple[WorkspaceService, ...]:
        names = [service.name for service in values]
        if len(names) != len(set(names)):
            raise ValueError("service names must be unique")
        return values

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
    """Declarative workspace policy and dev environment settings."""

    repo_url: StrictStr | None = None
    branch: Annotated[StrictStr, Field(min_length=1)]
    agent_kinds: tuple[WorkspaceAgentKind, ...] = ()
    skills: tuple[Annotated[StrictStr, Field(min_length=1)], ...] = ()
    mcp_servers: tuple[Annotated[StrictStr, Field(min_length=1)], ...] = ()
    egress_allowlist: tuple[Annotated[StrictStr, Field(min_length=1)], ...] = ()
    resource_class: Annotated[StrictStr, Field(pattern=r"^[a-z][a-z0-9-]{0,31}$")]
    dev: WorkspaceDev | None = None

    @field_validator("dev")
    @classmethod
    def require_image_source(cls, value: WorkspaceDev | None) -> WorkspaceDev | None:
        if value is not None and (value.image is None) == (
            value.devcontainer_ref is None
        ):
            raise ValueError("dev requires exactly one of image or devcontainer_ref")
        return value

    @field_validator("repo_url")
    @classmethod
    def validate_repo_url(cls, value: str | None) -> str | None:
        if value is None:
            return None
        if re.fullmatch(r"[^@\s]+@[^:\s]+:.+", value):
            host = value.split("@", 1)[1].split(":", 1)[0]
            if _is_host_or_ip(host) and value.split(":", 1)[1]:
                return value
            raise ValueError("repo_url must identify a valid host and repository path")
        try:
            parsed = urlsplit(value)
            hostname = parsed.hostname
        except ValueError as exc:
            raise ValueError("repo_url must be a valid URL") from exc
        if (
            parsed.scheme not in {"https", "ssh", "git"}
            or not hostname
            or not _is_host_or_ip(hostname)
            or not parsed.path.strip("/")
            or parsed.query
            or parsed.fragment
            or (parsed.scheme == "https" and parsed.username)
            or parsed.password
        ):
            raise ValueError(
                "repo_url must be an HTTPS, SSH, or Git URL with a host and repository path"
            )
        return value

    @field_validator("branch")
    @classmethod
    def validate_branch(cls, value: str) -> str:
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
            raise ValueError("branch is not a valid Git branch name")
        return value

    @field_validator("skills", "mcp_servers", "egress_allowlist")
    @classmethod
    def validate_references(cls, values: tuple[str, ...]) -> tuple[str, ...]:
        if len(values) != len(set(values)):
            raise ValueError("manifest references must be unique")
        if any(
            any(char.isspace() or ord(char) < 32 for char in value) for value in values
        ):
            raise ValueError("manifest references must not contain whitespace")
        return values

    @field_validator("egress_allowlist")
    @classmethod
    def validate_egress_hosts(cls, values: tuple[str, ...]) -> tuple[str, ...]:
        for host in values:
            if host != host.lower() or host.endswith(".") or "*" in host:
                raise ValueError(
                    "egress entries must be exact lowercase hostnames or IP addresses"
                )
            try:
                ipaddress.ip_address(host)
                continue
            except ValueError:
                pass
            if not _is_host_or_ip(host):
                raise ValueError(
                    "egress entries must be exact lowercase hostnames or IP addresses"
                )
        return values


class WorkspaceConditionStatus(StrEnum):
    TRUE = "True"
    FALSE = "False"
    UNKNOWN = "Unknown"


class WorkspaceCondition(WorkspaceContractModel):
    type: Annotated[StrictStr, Field(min_length=1)]
    status: WorkspaceConditionStatus
    reason: Annotated[StrictStr, Field(min_length=1)]
    message: StrictStr
    last_transition_time: AwareDatetime


class WorkspaceTransition(WorkspaceContractModel):
    from_state: WorkspaceObservedState | None = None
    to_state: WorkspaceObservedState
    reason: Annotated[StrictStr, Field(min_length=1)]
    occurred_at: AwareDatetime


class WorkspaceLifecycle(WorkspaceContractModel):
    workspace_id: WorkspaceIdentifier
    session_id: WorkspaceIdentifier
    desired_state: WorkspaceDesiredState
    observed_state: WorkspaceObservedState
    manifest: WorkspaceManifest
    conditions: tuple[WorkspaceCondition, ...] = ()
    last_transition: WorkspaceTransition | None = None
    operation_id: WorkspaceIdentifier | None = None
    snapshot_ref: WorkspaceIdentifier | None = None
    last_activity_at: AwareDatetime | None = None
    ownership_generation: Annotated[int, Field(ge=1, strict=True)] = 1
    updated_at: AwareDatetime
