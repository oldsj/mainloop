# Provider profiles

Mainloop owns a Claude/Codex native provider registry. `GET /providers` returns configured
profiles and scoped capability evidence on the owner REST listener. There is no registry write
API. Tool/API callers select a profile ID, never AgentRefs, namespaces, credentials or harness wiring.

Profiles contain ID, display name, native provider, kagent adapter, configuration revision,
role-specific AgentRefs, configured profile aliases, enabled flag, capability evidence, and the
models and reasoning-effort levels a session on the profile offers through `/model` and `/effort`
(see [chat](chat.md#the-model-and-effort-commands)). Roles are `main`, `supervisor`, `child`, and
`agent` (owner workspace). A missing role is rejected; a supervisor is never silently routed as
main. Operator profile aliases are configurable ID shortcuts, not legacy delegation payload
aliases. Task schemas reject the old `kind` payload.

`models` is a list of `{id, label}`; `default_model` names one of them (or is unset when `models`
is empty). `effort_levels` is a subset of `minimal`, `low`, `medium`, `high`, `xhigh`, `max`;
`default_effort` names one of them (or is unset when `effort_levels` is empty). Mainloop's built-in
Claude profile lists Sonnet 5, Opus 5, Haiku 4.5 and Fable 5.1, defaulting to Sonnet 5, and effort
`low`/`medium`/`high`/`xhigh`/`max`, defaulting to `medium`. Its built-in Codex profile lists two
models defaulting to the first, and effort `minimal`/`low`/`medium`/`high`, defaulting to `medium`.
An operator profile sets its own `models` and `effort_levels` in `PROVIDER_PROFILES`, or leaves them
empty: `/model` still accepts a model by name against an empty list (kagent is the real gate on
whether it exists), but `/effort` has nothing to offer or accept. The one exception is a profile
that replaces a built-in `claude` or `codex` profile without changing its `native_provider`: if it
leaves `models`/`default_model` or `effort_levels`/`default_effort` empty, it inherits that pair
from the built-in profile it replaces, so an operator who overrides only `agents` and
`capabilities` (the common case: naming a different kagent Agent) does not lose `/model` and
`/effort`. Setting either pair explicitly still wins over the built-in. A profile that changes
`native_provider`, or that is not replacing a built-in ID, never inherits.

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

Explicit default construction uses `KAGENT_NAMESPACE`, `KAGENT_MAIN_AGENT` (the Claude profile's `main` role),
`KAGENT_MAIN_CODEX_AGENT` (the Codex profile's `main` role, default `mainloop-main-codex`),
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
Verified native HITL requests on a live local binding remain readable and answerable after
its pinned Agent leaves current configuration. Discovery checks the local binding and the
attempt's pinned AgentRef before applying the current Agent allowlist to unbound Sessions.
Creator, owner, archive/deletion, replacement and stored endpoint/identity checks still apply;
an unbound Session on an unconfigured Agent remains unavailable.
Existing session/workspace routing remains in its runtime adapter until S1 integrates attempt routing.

Task qualification requires proved live evidence for native create/identity, delivery
receipt/completion, cancellation reconciliation and Mainloop MCP credential injection. Code
also requires workspace Git origins and environment composition. Unknown/partial/unsupported or
fixture-only evidence cannot qualify production selection. Pure fake tests may explicitly opt
into fixture evidence. Snapshot/import capabilities are not a committed-handoff prerequisite.
Evidence states remain `proved`, `partial`, `unsupported`, `unknown`; proved/partial require
scope and evidence reference. Existing owner-session APIs retain their configured selection
behavior until S1; S0 task mutators remain unavailable and cannot create native sessions.

The owner selects Claude or Codex for the main thread with `GET/PUT /main-thread/provider`; see [main provider selection](chat.md#selecting-the-main-provider). That selection uses the persisted main contract and is independent of task profile preferences. The `main` role resolves per provider: a disabled profile cannot be selected for a new main identity, while a pending or finished selection keeps the Agent pinned on its binding.

## Go server status (migration slice G4)

Implemented in `server/` and covered by offline tests with a fake registry: `GET /providers`, the
`PROVIDER_PROFILES` parser, and `GET/PUT /projects/{id}/default-provider`. Startup fails with the
validation error when `PROVIDER_PROFILES` or a `KAGENT_*` agent name used by the built-in profiles
is malformed. The default-provider write is a compare-and-swap on the preference version:
`expected_version: 0` creates the row, a lost race returns 409 `stale_provider_preference`, and a
missing or other owner's project returns 404 `project_not_found`. A profile must be enabled and
have a `supervisor` Agent, otherwise the write returns 422.

`models`, `default_model`, `effort_levels` and `default_effort` are validated the same way as the
rest of a profile (a malformed `PROVIDER_PROFILES` entry fails startup) and are covered by the same
offline registry tests. A session's own `/model` and `/effort` override — the durable per-session
state these choices feed, and the A2A metadata it produces — is a separate slice in
`server/internal/sessions` and `server/internal/kagent`; see
[chat](chat.md#the-model-and-effort-commands).

History before `e776537` describes the retired Python backend. Where other text on this page differs from the Go status, the Go status is current.

Not yet in the Go server (proposed, later slices): task selection precedence, attempt pinning,
qualification and `TASK_DEFAULT_PROVIDER_PROFILE_ID`.
