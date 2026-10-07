# Credentials

## Provider authentication

Native agents authenticate to their provider through the kagent installation: the Agent's
ModelConfig holds the provider credential, and Mainloop neither stores nor injects it. Mainloop
has no credential broker, seeding, or sign-in flow.

## Attention and recovery

The native turn path does not check credentials or raise a sign-in attention item: turns go to
kagent, and a provider authentication failure surfaces as a failed task. A failed turn is not
replayed automatically. Surfacing credential health from the kagent ModelConfig condition is a
later change.

## Per-binding MCP grant

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

## Mainloop control service credential

Provider authentication, Mainloop-to-kagent control authentication, and per-binding MCP
access are three separate credential classes. Provider credentials stay in kagent's
ModelConfig/Secret configuration and travel only through its provider egress integration.
Their rotation and revocation are operated in kagent, without a Mainloop login flow. Only
approved provider egress credentials may be available to actors; control credentials must
never be included in actor credential references, environment, snapshots or tools.

`KAGENT_CONTROL_TOKEN_FILE` optionally names a mounted Secret file in the Mainloop backend.
Settings and the client validate it at startup: unreadable, empty, malformed or over-4096-byte
files fail configuration. The file contains an ASCII bearer with an optional trailing newline.
Mainloop rereads it with the same size bound before every lifecycle, Agent discovery and A2A
request, including stream reconnects. Reload failure denies the call; there is no fallback.
The token is sent only as `Authorization: Bearer …` to `KAGENT_GATEWAY_URL`; `x-user-id` is
omitted. `KAGENT_USER_ID=mainloop` remains required as the expected principal. Secret bytes
are never included in settings, provider profiles, UI or API responses.

Operators must use HTTPS with server trust verification for bearer transport, mount the
whole Secret read-only without `subPath` so kubelet rotation reaches the process, and keep
both backend and controller copies outside actor-accessible Secret selectors. Enable the
kagent controller's `service-token` authentication and default-deny policy in deployment.
**Enforcement is not yet deployed:** this client support alone does not isolate actors or
turn on server enforcement. When the file setting is absent, the existing insecure mode
continues unchanged, sending `x-user-id: KAGENT_USER_ID`.

Rotate by configuring the controller to accept current and next tokens, updating the backend
file, then revoking the old controller token after new admissions succeed. New calls and
reconnects pick up rotation without restart; already admitted streams keep their admission.
Immediate stream revocation requires closing them at the trusted frontend. HTTP 401/403 and
gRPC UNAUTHENTICATED/PERMISSION_DENIED are service configuration failures, never missing
Sessions or provider sign-in requests. They do not replace Sessions or resend refused turns.
Previously uncertain delivery remains subject to observation, with no automatic resend.

Per-binding MCP credentials remain separate: their non-secret references are persisted with
CreateSession requests and their bearer is injected only for the approved Mainloop MCP
origin. They grant the binding's scoped tools, never kagent control access. Rotation requires
trusted publication of the binding credential; revocation clears its current hash immediately
and then removes its Secret best-effort. Publisher or reference configuration failure prevents
creation/delivery and does not expose credentials to the owner API. Neither MCP credentials
nor provider credentials may substitute for the control credential.
