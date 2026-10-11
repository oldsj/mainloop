"""Owner selection and truthful main native provisioning state."""

from typing import Literal

from pydantic import BaseModel, ConfigDict

MainProvider = Literal["claude", "codex"]


class MainProviderSelection(BaseModel):
    model_config = ConfigDict(extra="forbid")
    provider: MainProvider
    restart_failed: bool = False


class MainProviderHistory(BaseModel):
    provider: str
    kagent_session_id: str | None


class MainProviderInfo(BaseModel):
    session_id: str
    desired_provider: MainProvider
    effective_provider: MainProvider | None
    status: Literal["ready", "not_started", "pending", "failed"]
    kagent_session_id: str | None
    error: str | None = None
    recoverable: bool = False
    history: list[MainProviderHistory]
