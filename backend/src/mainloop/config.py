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

    # Native sessions connect to pre-created Substrate actors through the CONNECT router.
    substrate_router_address: str = (
        "http://atenet-router.ate-system.svc.cluster.local:8081"
    )
    substrate_shim_secret_namespace: str = "mainloop-control"
    substrate_credential_secret_namespace: str = "mainloop-control"
    substrate_credential_secret_prefix: str = "mainloop-credential"
    substrate_credential_account: str = "owner"
    substrate_credential_owner_user_id: str = "local-dev-user"
    substrate_codex_auth_path: str = ""
    substrate_claude_token_path: str = ""
    substrate_shim_secret_prefix: str = "mainloop-shim"
    substrate_actor_bindings: dict[
        Literal["claude", "codex"], SubstrateActorBinding
    ] = Field(default_factory=dict)
    substrate_resume_timeout_seconds: float = 120.0

    # Substrate actor lifecycle control. Empty kubeconfig/context falls back to ambient config.
    substrate_kubeconfig: str = ""
    substrate_context: str = ""
    substrate_atespace: str = "mainloop-workspaces"
    substrate_actor_template: str = "mainloop-workspace"
    substrate_cli: str = "kubectl-ate"
    substrate_reauth_job_image: str = ""
    substrate_reauth_job_namespace: str = "mainloop-control"
    substrate_reauth_callback_url: str = "http://mainloop-backend:8000/internal/reauth"
    substrate_reauth_timeout_seconds: int = 1800

    # Native main thread (context model).
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
