# Provider profiles

Mainloop owns a Claude/Codex native provider registry. `GET /providers` returns configured
profiles and scoped capability evidence on the owner REST listener. There is no registry write
API. Tool/API callers select a profile ID, never AgentRefs, namespaces, credentials or harness wiring.

Profiles contain ID, display name, native provider, kagent adapter, configuration revision,
role-specific AgentRefs, configured profile aliases, enabled flag and capability evidence.
Roles are `main`, `supervisor`, `child`, and `agent` (owner workspace). A missing role is rejected;
a supervisor is never silently routed as main. Operator profile aliases are configurable ID
shortcuts, not legacy delegation payload aliases. Task schemas reject the old `kind` payload.

`PROVIDER_PROFILES` is a validated JSON list replacing matching explicit default IDs or adding
Claude/Codex profiles. IDs/aliases must be unique. Reserved `claude` and `codex` IDs cannot be
redirected to a different native provider or used as another profile's alias. Unsupported native
providers, adapters, malformed AgentRefs and unknown fields fail closed.

```json
[
  {
    "id": "codex-review",
    "display_name": "Codex review",
    "native_provider": "codex",
    "configuration_revision": "review-v1",
    "agents": {
      "supervisor": { "namespace": "kagent", "name": "codex-review" },
      "child": { "namespace": "kagent", "name": "codex-child" }
    },
    "enabled": true,
    "capabilities": [{ "capability": "native_create", "state": "unknown" }]
  }
]
```

Explicit default construction uses `KAGENT_NAMESPACE`, `KAGENT_MAIN_AGENT`,
`KAGENT_CLAUDE_AGENT`, `KAGENT_CODEX_AGENT`, workspace Agent settings and the separate
`KAGENT_SUPERVISOR_CLAUDE_AGENT`/`KAGENT_SUPERVISOR_CODEX_AGENT` settings. Default revision is
`native-v1`. All capability evidence starts unknown. Configured revisions are operator-owned;
no defaults fabricate qualification.

Task selection precedence is explicit choice, project default (`GET/PUT
/projects/{id}/default-provider` with expected preference version), installation default
(`TASK_DEFAULT_PROVIDER_PROFILE_ID`). Selection source is persisted. Explicit owner constraints
are inherited by supervisors. A task attempt pins its profile ID, native provider, revision and
immutable role AgentRef, so later registry changes cannot reroute its persisted identity.
Disabled profiles block new selection and do not prevent reading/reconciling a pinned attempt.
Existing session/workspace routing remains in its runtime adapter until S1 integrates attempt routing.

Task qualification requires proved live evidence for native create/identity, delivery
receipt/completion, cancellation reconciliation and Mainloop MCP credential injection. Code
also requires workspace Git origins and environment composition. Unknown/partial/unsupported or
fixture-only evidence cannot qualify production selection. Pure fake tests may explicitly opt
into fixture evidence. Snapshot/import capabilities are not a committed-handoff prerequisite.
Evidence states remain `proved`, `partial`, `unsupported`, `unknown`; proved/partial require
scope and evidence reference. Existing owner-session APIs retain their configured selection
behavior until S1; S0 task mutators remain unavailable and cannot create native sessions.
