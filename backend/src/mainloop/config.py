"""Configuration management."""

from typing import Literal
from urllib.parse import quote_plus

from pydantic import BaseModel, ConfigDict, Field, computed_field
from pydantic_settings import BaseSettings, SettingsConfigDict


class SubstrateActorBinding(BaseModel):
    """Deployment-provided route and token Secret for one pre-created actor."""

    atespace: str
    actor: str
    shim_token_secret_name: str

    model_config = ConfigDict(extra="forbid", frozen=True)


class Settings(BaseSettings):
    """Application settings."""

    # Database (PostgreSQL) - constructed from parts
    db_host: str = "localhost"
    db_port: str = "5432"
    db_name: str = "mainloop"
    db_user: str = "mainloop"
    db_password: str = ""

    @computed_field
    @property
    def database_url(self) -> str:
        """Construct database URL from parts."""
        encoded_password = quote_plus(self.db_password)
        return f"postgresql://{self.db_user}:{encoded_password}@{self.db_host}:{self.db_port}/{self.db_name}"

    # Claude
    claude_code_oauth_token: str = ""  # OAuth token for Claude Code API
    claude_agent_url: str = "http://claude-agent:8001"
    claude_workspace: str = "/workspace"
    claude_model: str = "sonnet"  # Main thread model
    claude_worker_model: str = "opus"  # Worker model (for background tasks)

    # Native agents under Herdr (workspace pod reached over Kubernetes pod-exec)
    workspace_namespace: str = "herdr-spike"
    workspace_pod: str = "workspace-0"
    main_pod: str = (
        "main-0"  # pod that runs the native main thread (scratch cwd, no repo)
    )

    # Native-session workspace transport. Herdr remains the default; Substrate attaches to
    # pre-created actors through the CONNECT router and never creates or resumes actors itself.
    workspace_runtime: Literal["herdr", "substrate"] = "herdr"
    substrate_router_address: str = (
        "http://atenet-router.ate-system.svc.cluster.local:8081"
    )
    substrate_shim_secret_namespace: str = "mainloop-control"
    substrate_actor_bindings: dict[
        Literal["claude", "codex"], SubstrateActorBinding
    ] = Field(default_factory=dict)
    substrate_resume_timeout_seconds: float = 120.0

    # Substrate workspace-runtime adapter (bounded integration spike; see
    # docs/architecture/native-agent-inventory.md and .tasknotes/plan.md). Empty
    # kubeconfig/context falls back to the ambient kubeconfig. One actor per session
    # replaces the fixed workspace_namespace/workspace_pod pair above for Substrate-backed
    # sessions; Herdr pod-exec keeps working unchanged for sessions that are not.
    substrate_kubeconfig: str = ""
    substrate_context: str = ""
    substrate_atespace: str = "mainloop-workspaces"
    substrate_actor_template: str = "mainloop-workspace"
    substrate_cli: str = "kubectl-ate"
    substrate_preview_base_url: str = ""

    # Native main thread (context model). MAIN_THREAD_MODE=native replaces the SDK chat path.
    main_thread_mode: str = "sdk"  # sdk | native
    main_thread_model: str = "sonnet"
    main_thread_effort: str = "medium"
    # Rotation: cut to a fresh native session when the context grew by this many tokens above
    # the lineage's first-turn baseline, or after this many completed turns (whichever first).
    main_rotate_tokens: int = 20000
    main_rotate_turns: int = 12
    main_carry_over_messages: int = 6
    native_child_kinds: str = "claude,codex"
    agent_token_key: str = (
        ""  # HMAC key for per-binding agent tokens (falls back to DB password)
    )

    # GitHub
    github_token: str = ""

    # Server
    host: str = "0.0.0.0"
    port: int = 8000
    frontend_domain: str = "mainloop.example.com"  # Frontend domain for CORS

    @computed_field
    @property
    def frontend_origin(self) -> str:
        """Construct frontend origin URL from domain."""
        return f"https://{self.frontend_domain}"

    # K8s Job callback URL (internal service URL for Jobs to call back)
    backend_internal_url: str = (
        "http://mainloop-backend.mainloop.svc.cluster.local:8000"
    )

    # Worker image for K8s Jobs (use local image for dev)
    worker_image: str = "ghcr.io/oldsj/mainloop-agent-controller:latest"
    worker_image_pull_policy: str = "IfNotPresent"  # Use "Never" for local dev

    # Test environment flag (enables test-only endpoints)
    is_test_env: bool = False

    # Mock GitHub for testing without real GitHub API
    use_mock_github: bool = False

    model_config = SettingsConfigDict(
        env_file=".env",
        env_file_encoding="utf-8",
        case_sensitive=False,
    )


# Global settings instance
settings = Settings()
