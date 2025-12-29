"""Configuration management."""

import os
from pydantic_settings import BaseSettings, SettingsConfigDict
from pydantic import computed_field


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
        return f"postgresql://{self.db_user}:{self.db_password}@{self.db_host}:{self.db_port}/{self.db_name}"

    # Google Cloud (legacy - being migrated to PostgreSQL)
    google_cloud_project: str = ""
    bigquery_dataset: str = "mainloop"

    # Claude
    claude_oauth_token: str = ""  # OAuth token for Claude API
    claude_agent_url: str = "http://claude-agent:8001"
    claude_workspace: str = "/workspace"
    claude_model: str = "sonnet"  # Main thread model
    claude_worker_model: str = "opus"  # Worker model (for background tasks)

    # GitHub
    github_token: str = ""

    # Server
    host: str = "0.0.0.0"
    port: int = 8000

    # K8s Job callback URL (internal service URL for Jobs to call back)
    backend_internal_url: str = "http://mainloop-backend.mainloop.svc.cluster.local:8000"

    model_config = SettingsConfigDict(
        env_file=".env",
        env_file_encoding="utf-8",
        case_sensitive=False,
    )


# Global settings instance
settings = Settings()
