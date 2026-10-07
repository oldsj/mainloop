# Provider profiles

Mainloop owns the provider registry. `GET /providers` returns configured profiles and their
capability evidence through the same owner-only listener and `current_user` dependency as the
session and workspace APIs. The MCP listener does not expose this endpoint. Caller identity
headers do not change the configured owner. There is no registry write API.

A profile contains `id`, `display_name`, `native_provider` (Claude or Codex),
`runtime_adapter` (`kagent`), `configuration_revision`, role-specific `agents` (namespace and
name), `aliases`, `enabled`, and `capabilities`. Roles use the existing binding names:
`agent` for owner sessions and workspaces, `child` for delegation, and `main` for the home
thread. A profile can omit unsupported roles; selecting an omitted role is rejected.

## Operator configuration

`PROVIDER_PROFILES` is a JSON list, loaded through application settings. Entries replace
matching profile IDs or add new IDs. Unknown fields, unsupported adapters/providers, invalid
AgentRefs, duplicate IDs/aliases, and aliases that collide with other IDs are rejected. The
legacy IDs `claude` and `codex` are reserved and cannot be assigned as another profile's alias
or changed to a different native provider.

For example, an additional child-only Codex profile:

```json
[
  {
    "id": "codex-review",
    "display_name": "Codex review",
    "native_provider": "codex",
    "runtime_adapter": "kagent",
    "configuration_revision": "review-v1",
    "agents": {
      "child": { "namespace": "kagent", "name": "codex-review-child" }
    },
    "aliases": ["review"],
    "enabled": true,
    "capabilities": [{ "capability": "native_create", "state": "unknown" }]
  }
]
```

Without configuration, `claude` and `codex` retain the existing `KAGENT_NAMESPACE`,
`KAGENT_MAIN_AGENT`, `KAGENT_CLAUDE_AGENT`, `KAGENT_CODEX_AGENT`,
`KAGENT_WORKSPACE_CLAUDE_AGENT`, and `KAGENT_WORKSPACE_CODEX_AGENT` settings exactly.
Both defaults route `main` to the same existing main Agent. No sessions are migrated or rebound.
Operator configuration must retain profile IDs and AgentRefs while their sessions exist;
configuration revisions are descriptive, not per-binding routing snapshots in this slice.
Use a new ID for different runtime wiring.

## Selection and evidence

The existing `agent_kind` fields on session/workspace APIs and `kind` on delegate accept a
profile ID or configured alias. `claude` and `codex` remain accepted and omission still defaults
to Claude. New bindings store the canonical profile ID. Unknown, disabled, or role-ineligible
selections fail before creating session/workspace records. Delegation still requires inclusion
in `NATIVE_CHILD_KINDS` (IDs or aliases) and retains existing role and concurrency caps.
Callers cannot supply AgentRefs, namespaces, harness settings, or credentials to select a runtime.
The frontend picker remains Claude/Codex; this slice adds registry types without changing UI consumers.

Disabled profiles remain in `GET /providers` with `enabled: false`, so owners can distinguish
configuration from availability. Disabling a profile blocks new selection; existing bindings
continue to route, observe, and reconcile using it.

Capability results reuse the shared native-agent states: `proved`, `partial`, `unsupported`,
and `unknown`. Proved/partial results require an evidence reference and `fixture` or `live`
scope. Fixture evidence is not live qualification. Default evidence is unknown; omitted
capabilities are also unknown. The defaults describe native create/resume/identity, delivery
receipt/completion/cancel reconciliation, workspace Git origins/snapshot/freeze/import, MCP
credential injection, HITL, and retention. Existing creation does not acquire a new evidence
gate; future task requirements must explicitly require proved capabilities rather than treating
unknown as supported. This slice provides no handoff, snapshot transfer, task/attempt records,
or automatic provider selection. Provider-specific HITL/merge qualification remains governed
by its existing runtime/template contracts.
