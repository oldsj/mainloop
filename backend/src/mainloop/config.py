"""Configuration management."""

from urllib.parse import quote_plus, urlsplit

from pydantic import Field, computed_field, field_validator
from pydantic_settings import BaseSettings, SettingsConfigDict

from models.provider import ProviderProfile


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

    # The single owner: the identity of every request that reaches Mainloop. Mainloop is reached
    # only over the tailnet (and, in the cluster, through its NetworkPolicy), so there is no
    # per-request authentication. Workspaces and previews are scoped to this id.
    owner_id: str = Field("local-dev-user", validation_alias="MAINLOOP_OWNER_ID")

    @field_validator("owner_id")
    @classmethod
    def _owner_id_not_blank(cls, value: str) -> str:
        value = value.strip()
        if not value:
            raise ValueError("MAINLOOP_OWNER_ID must not be blank")
        return value

    # Workspace previews reach a dev server in the kagent harness through the Substrate router
    # (``CONNECT actor-upstream:<port>``, the target actor named by the ``ate-target-actor``
    # header). The router has no authentication; owner scoping happens in Mainloop.
    substrate_router_address: str = (
        "http://atenet-router.ate-system.svc.cluster.local:8081"
    )
    # The domain previews are served under: a preview URL is
    # ``<scheme>://<port>--<workspace>--preview.<this host>[:<this port>]``. It must be one DNS
    # label below a wildcard-certificate domain (``*.<this host>``).
    substrate_preview_base_url: str = "http://localhost:8001"
    substrate_preview_connect_timeout_seconds: float = 5.0
    # kagent runs each Session in an actor in this atespace, named ``session-<Session id>``.
    kagent_actor_atespace: str = "kagent"

    # kagent: native Claude and Codex sessions run as kagent Agents behind one gateway
    # (SessionService over grpc-web and A2A JSON-RPC). Mainloop acts as a fixed service identity.
    kagent_gateway_url: str = "http://kagent-controller.kagent.svc.cluster.local:8083"
    # kagent scopes Sessions to this identity. Changing it is a migration: every existing kagent
    # Session becomes not found and is replaced, losing its native context.
    kagent_user_id: str = "mainloop"
    kagent_namespace: str = "kagent"
    kagent_main_agent: str = "mainloop-main"
    # Child agents (delegated by the main thread) run on these.
    kagent_claude_agent: str = "claude-subscription"
    kagent_codex_agent: str = "codex-subscription-https"
    # Sessions the owner starts, and every workspace, run on these instead. A workspace holds
    # uncommitted and unpushed work, so its Harness must never expire the Session
    # (``sessionIdleTTL: 0s``), must allow ``git.origins`` and must snapshot with
    # ``snapshotPolicy.onQuiesce: Full`` (previews and wake). Children keep the default TTL and
    # snapshot scope. See docs/architecture.md.
    kagent_workspace_claude_agent: str = "claude-workspace"
    kagent_workspace_codex_agent: str = "codex-workspace"
    # JSON list of operator-owned profiles. Matching IDs override legacy defaults.
    provider_profiles: list[ProviderProfile] = Field(default_factory=list)

    @field_validator("provider_profiles")
    @classmethod
    def _unique_provider_ids(cls, profiles):
        names = [name for p in profiles for name in (p.id, *p.aliases)]
        if len(names) != len(set(names)):
            raise ValueError("duplicate provider ID or alias")
        for profile in profiles:
            if any(alias in ("claude", "codex") for alias in profile.aliases):
                raise ValueError("claude/codex are reserved legacy profile IDs")
            if (
                profile.id in ("claude", "codex")
                and profile.native_provider != profile.id
            ):
                raise ValueError("legacy profile native provider must not change")
        return profiles

    kagent_request_timeout_seconds: float = 30.0
    kagent_turn_timeout_seconds: float = 1800.0
    kagent_session_ready_timeout_seconds: float = 120.0
    # "Send not accepted" is retried with the identical message for at most this long.
    kagent_send_retry_budget_seconds: float = 30.0
    # A workspace with no preview traffic and no open turn for its idle timeout is suspended.
    # The check runs this often.
    workspace_idle_check_seconds: float = 60.0

    # Native main thread (context model).
    main_carry_over_messages: int = 6
    native_child_kinds: str = "claude,codex"
    # HMAC key for per-binding agent tokens. Required unless ``dev_mode`` (or ``is_test_env``) is
    # set, where it falls back to the DB password.
    agent_token_key: str = ""
    # Local development: relaxes startup requirements such as ``AGENT_TOKEN_KEY``.
    dev_mode: bool = False

    # GitHub
    github_token: str = ""

    # Server
    host: str = "0.0.0.0"
    port: int = 8000
    frontend_domain: str = "mainloop.example.com"  # Frontend domain for CORS
    # Scheme of the frontend origin (CORS and the cross-origin write guard). A Kind or local
    # deployment served over plain HTTP sets ``http``.
    frontend_scheme: str = "https"
    # The API's own domain, when it differs from the frontend's.
    api_domain: str = ""
    # The API answers only to these Host names, plus the frontend and API domains, loopback and
    # the preview hosts. Comma-separated, without ports (any port is accepted): the names callers
    # use to reach the API, for example its in-cluster Service names.
    api_hosts: str = Field("", validation_alias="MAINLOOP_API_HOSTS")

    @computed_field
    @property
    def frontend_origin(self) -> str:
        """Construct frontend origin URL from scheme and domain."""
        return f"{self.frontend_scheme}://{self.frontend_domain}"

    @property
    def allowed_api_hosts(self) -> frozenset[str]:
        """Return the lower-case host names the API serves (not preview hosts)."""
        names = [self.frontend_domain, self.api_domain, *self.api_hosts.split(",")]
        hosts = set()
        for name in names:
            if not name.strip():
                continue
            # FRONTEND_DOMAIN may include its development port; the Host guard compares
            # hostnames, while CORS retains the full frontend origin.
            try:
                hostname = urlsplit(f"//{name.strip()}").hostname
            except ValueError:
                continue
            if hostname:
                hosts.add(hostname.lower().rstrip("."))
        return frozenset(hosts)

    # Test environment flag (enables test-only endpoints)
    is_test_env: bool = False

    @property
    def is_dev(self) -> bool:
        """Return whether this is a development or test deployment."""
        return self.dev_mode or self.is_test_env

    # Mock GitHub for testing without real GitHub API
    use_mock_github: bool = False

    model_config = SettingsConfigDict(
        env_file=".env",
        env_file_encoding="utf-8",
        case_sensitive=False,
    )


# Global settings instance
settings = Settings()
