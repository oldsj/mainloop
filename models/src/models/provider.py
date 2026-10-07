"""Operator-owned native provider profiles; callers select only an ID."""

from typing import Annotated, Literal

from pydantic import Field, model_validator

from models.native_agent import CapabilityResult, ContractModel

ProviderProfileId = Annotated[
    str, Field(strict=True, min_length=1, max_length=80, pattern=r"^[a-z][a-z0-9_-]*$")
]
KagentName = Annotated[
    str,
    Field(
        strict=True,
        min_length=1,
        max_length=63,
        pattern=r"^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$",
    ),
]
ProviderRole = Literal["main", "supervisor", "child", "agent"]


class ProviderAgentRef(ContractModel):
    namespace: KagentName
    name: KagentName


class ProviderProfile(ContractModel):
    id: ProviderProfileId
    display_name: Annotated[str, Field(strict=True, min_length=1, max_length=200)]
    runtime_adapter: Literal["kagent"] = "kagent"
    native_provider: Literal["claude", "codex"]
    configuration_revision: Annotated[str, Field(strict=True, min_length=1)]
    agents: dict[ProviderRole, ProviderAgentRef]
    aliases: tuple[ProviderProfileId, ...] = ()
    enabled: Annotated[bool, Field(strict=True)] = True
    capabilities: tuple[CapabilityResult, ...] = ()

    @model_validator(mode="after")
    def unique_capabilities(self):
        if not self.agents:
            raise ValueError("at least one role AgentRef is required")
        names = [result.capability for result in self.capabilities]
        if len(names) != len(set(names)):
            raise ValueError("capability evidence must be unique by capability")
        if len(self.aliases) != len(set(self.aliases)):
            raise ValueError("aliases must be unique")
        return self
