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
    """Defaults retain legacy settings; configured entries replace matching IDs or add IDs.

    Reserved legacy IDs cannot be redirected through aliases. Keep configured IDs and AgentRefs
    stable while sessions exist: bindings already use these IDs for runtime routing.
    """
    defaults = [
        ProviderProfile(
            id=kind,
            display_name="Claude Code" if kind == "claude" else "Codex",
            native_provider=kind,
            configuration_revision="legacy-v1",
            agents={
                "main": ProviderAgentRef(
                    namespace=settings.kagent_namespace, name=settings.kagent_main_agent
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
