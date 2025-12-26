"""Configuration management."""

import os
from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    """Application settings."""

    # Google Cloud
    google_cloud_project: str = ""
    bigquery_dataset: str = "mainloop"

    # Agent Controller
    agent_controller_url: str = "http://mainloop-agent-controller:8001"

    # Server
    host: str = "0.0.0.0"
    port: int = 8000

    model_config = SettingsConfigDict(
        env_file=".env",
        env_file_encoding="utf-8",
        case_sensitive=False,
    )


# Global settings instance
settings = Settings()
