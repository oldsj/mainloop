"""Server-owned provider registry. No discovery, inference loop or caller-owned wiring."""

from mainloop.config import settings

from models import CapabilityResult
from models.provider import ProviderAgentRef, ProviderProfile, ProviderRole

CAPABILITIES = (
    "native_create",
    "native_resume",
    "native_identity",
    "message_receipt",
    "message_completion",
    "cancel_reconciliation",
    "workspace_git_origins",
    "workspace_environment_composition",
    "workspace_snapshot",
    "workspace_freeze",
    "workspace_import",
    "mainloop_mcp_credentials",
    "hitl",
    "retention",
)


class ProviderRegistry:
    def __init__(self, profiles: list[ProviderProfile]):
        self.profiles = tuple(profiles)
        self._by_id: dict[str, ProviderProfile] = {}
        for profile in profiles:
            for name in (profile.id, *profile.aliases):
                if name in self._by_id:
                    raise ValueError(f"duplicate provider ID or alias: {name}")
                self._by_id[name] = profile

    def resolve(
        self, identifier: str, role: ProviderRole, *, selecting: bool = False
    ) -> ProviderProfile:
        profile = self._by_id.get(identifier)
        if profile is None:
            raise ValueError(f"no provider profile is configured for {identifier!r}")
        if role not in profile.agents:
            raise ValueError(f"provider {identifier!r} has no {role!r} Agent")
        if selecting and not profile.enabled:
            raise ValueError(f"provider {identifier!r} is disabled")
        return profile


def registry() -> ProviderRegistry:
    """Explicit Claude/Codex defaults; operator profiles replace matching IDs or add IDs."""
    defaults = [
        ProviderProfile(
            id=kind,
            display_name="Claude Code" if kind == "claude" else "Codex",
            native_provider=kind,
            configuration_revision="native-v1",
            agents={
                "main": ProviderAgentRef(
                    namespace=settings.kagent_namespace, name=settings.kagent_main_agent
                ),
                "supervisor": ProviderAgentRef(
                    namespace=settings.kagent_namespace,
                    name=getattr(settings, f"kagent_supervisor_{kind}_agent"),
                ),
                "child": ProviderAgentRef(
                    namespace=settings.kagent_namespace,
                    name=getattr(settings, f"kagent_{kind}_agent"),
                ),
                "agent": ProviderAgentRef(
                    namespace=settings.kagent_namespace,
                    name=getattr(settings, f"kagent_workspace_{kind}_agent"),
                ),
            },
            capabilities=tuple(
                CapabilityResult(capability=name) for name in CAPABILITIES
            ),
        )
        for kind in ("claude", "codex")
    ]
    configured = settings.provider_profiles
    profiles = {profile.id: profile for profile in defaults}
    for profile in configured:
        profiles[profile.id] = profile
    return ProviderRegistry(list(profiles.values()))


TASK_REQUIRED_CAPABILITIES = frozenset(
    {
        "native_create",
        "native_identity",
        "message_receipt",
        "message_completion",
        "cancel_reconciliation",
        "mainloop_mcp_credentials",
    }
)
TASK_CODE_CAPABILITIES = frozenset(
    {"workspace_git_origins", "workspace_environment_composition"}
)


def qualify_task_profile(profile, role, mode, *, allow_fixture=False):
    """Only explicitly proved evidence qualifies; tests must opt into fixture scope."""
    if not profile.enabled or role not in profile.agents:
        raise ValueError("provider_unavailable")
    required = TASK_REQUIRED_CAPABILITIES | (
        TASK_CODE_CAPABILITIES if mode == "code" else frozenset()
    )
    evidence = {c.capability: c for c in profile.capabilities}
    for name in required:
        value = evidence.get(name)
        if (
            value is None
            or value.state != "proved"
            or (
                value.scope != "live"
                and not (allow_fixture and value.scope == "fixture")
            )
        ):
            raise ValueError(f"provider_unqualified:{name}")
    return profile
