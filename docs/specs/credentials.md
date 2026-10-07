# Agent credentials

Native agents authenticate to their provider through the kagent installation: the Agent's
ModelConfig holds the provider credential, and Mainloop neither stores nor injects it. Mainloop
has no credential broker, seeding, or sign-in flow.

## Attention and recovery

The native turn path does not check credentials or raise a sign-in attention item: turns go to
kagent, and a provider authentication failure surfaces as a failed task. A failed turn is not
replayed automatically. Surfacing credential health from the kagent ModelConfig condition is a
later change.

## Tool access

Native sessions receive a per-binding credential reference in kagent `CreateSession`. The
reference selects the `mainloop` MCP origin, Authorization header and one key in the runtime
Secret; it contains no bearer. The Secret publisher derives that binding's bearer, while
PostgreSQL stores only its hash. MCP authentication checks the current binding and hash on every
request, so revocation denies a cached bearer immediately even if Secret cleanup is delayed.

Bindings persist an `mcp_grant_kind`: `coordination` for main and child bindings, `workspace` for
new owner workspaces created through `POST /workspaces`, and `none` for ordinary standalone or
retained legacy workspace bindings. Possessing a credential reference does not select tools;
discovery and invocation use the binding's role and grant together. Workspace grants expose
`whoami` and scoped PR creation. Merge tools still require the existing default-off enablement
gate and the same server-side workspace scope checks.

Creation stores the selected grant, hash, non-secret credential reference and checkout before its
first kagent create call. Retries reuse the persisted request and reference. A publish/revoke
lock is shared through PostgreSQL across Mainloop processes. Revocation clears the hash before
best-effort Secret removal; a cleanup record independent of the binding keeps retries durable
after workspace deletion removes the session rows. Suspending or resuming a workspace preserves
its grant. Existing `agent` bindings are not enrolled by migration.
