# Credentials

## Go server status (G7)

Status: **implemented** in `server/internal/{agentauth,k8ssecrets,mcpserver,topics}` and wired into the session and workspace services; tests use a scratch PostgreSQL and a fake Kubernetes clientset. **Not measured** against a live cluster or a real kagent, so whether kagent accepts a replayed `CreateSession` that carries credentials is unconfirmed.

History before `e776537` describes the retired Python backend. Where other text on this page differs from the Go status, the Go status is current.

- **One credentials table** holds the MCP, git-read and git-push bearers: `planned` (the intent), `published` (the Secret exists and its UID is recorded), `revoked` (the Secret is owed a delete) and `deleted`. Only the SHA-256 of a bearer is stored. The bearer is `ml_` plus the HMAC-SHA256 of the credential id under `AGENT_TOKEN_KEY`, which is required; there is no fallback to another secret. A binding that is revoked and enrolled again gets a new credential id and so a new bearer.
- **Publishing** writes the `planned` row in a short transaction, records the create start on the row (only while it is still `planned`, so a row revoked first never gets a Secret), creates the Secret with no transaction open, then records the UID. A retry returns the published credential, or resumes a `planned` one; an existing Secret is accepted only if it is immutable with the same labels and bytes, and is never overwritten. A new credential is refused while the binding's earlier Secret is still owed a delete. If the credential is revoked while its Secret is being created, `Issue` deletes that Secret (with its UID) and returns `ErrRevoked`, because cleanup may already have finished with the row. The API process builds one credential service and passes it to the services that issue; outside a cluster `Issue` fails with "Kubernetes not configured" before writing anything, and inside a cluster a Kubernetes client that cannot be built stops startup. `Issue` refuses a binding whose session is archived or completed, failed or cancelled (`ErrBindingEnded`): it locks the session row `FOR SHARE` while planning, so an archive in flight either waits for the plan and then revokes it, or has committed first and is seen. The Secret create runs under a 30-second deadline.
- **Secrets** are immutable, in the kagent namespace, named `<MAINLOOP_NAMESPACE>-mcp-<binding>` (and `-git-read-`, `-git-push-`), with the single key `authorization` and the labels `mainloop.dev/actor-egress=true` and `mainloop.dev/purpose`. Mainloop needs only `get`, `create` and `delete` on Secrets.
- **Revoking** one binding changes only that binding's rows. The `credential-cleanup` reconcile loop deletes each revoked credential's own Secret with a UID precondition, so a newer Secret under the same name survives and an already missing Secret counts as done. A Secret whose UID was never recorded is looked up by name first. If the lookup finds nothing, cleanup keeps looking until 45 seconds after the Secret create started (the create deadline plus a margin; `credentials.create_started_at` is written just before the call, and a row whose create never started is not waited on), because a create still in flight may yet land the Secret. A publish that finds its row neither published nor revoked, or cannot read it, leaves the Secret alone. A sweep revokes `planned` rows older than ten minutes, live rows whose binding is gone, and live rows whose session is archived or completed, failed or cancelled (superseded, idle and waiting-on-user sessions keep theirs). Callers that end a binding revoke in the same transaction as the state change (`agentauth.RevokeInTx` with the transaction's queries); the Secret delete follows after commit. The session service does this on cancel and archive, and the workspace service on delete and when it drops a refused create.
- **Who gets a credential.** The main thread (role `main`, grant `coordination`) and every workspace (role `agent`, grant `workspace`) get an MCP credential, published before the kagent Session is created and sent as one `SessionCredential`: origin `http://mainloop-mcp.<MAINLOOP_NAMESPACE>.svc.cluster.local`, header `Authorization`, a reference to the binding's Secret and its `authorization` key. Plain native sessions (grant `none`) get none. Every replay of a create (a workspace delete, and the session service's resolve sweep for the main thread and native sessions) sends the same reference whenever the store is configured and the role/grant pair is valid, even if the credential has since been revoked, and issues nothing: kagent compares credentials on a replay and refuses a mismatch. When a send finds the old Secret still owed a delete (or a revoke lands mid-publish) it waits and retries without using an attempt. Outside a cluster the store is unconfigured: sessions and workspaces are created without a credential, with a warning in the log, so agents there cannot reach MCP. Cleanup runs in the API process and only inside a cluster.
- **Authentication** takes the bearer, finds the published MCP credential by hash, then checks the binding is fresh (the session is not archived and not completed, failed or cancelled), then checks the role and grant pair. Supervisor and child bindings are refused unless the process supplies a task-principal check (`Authenticator.Depth`). Any failure is a 401 with no reason given.
- **Tools** follow one per-actor table, shared by discovery and invocation and recorded in [`api/mcp-tools.json`](../../api/mcp-tools.json) (generated from the Go tool definitions and policy table by `go generate ./internal/mcpserver`; a Go test and the Go CI workflow check that the file is current). `/mcp` serves every tool except `merge_pull_request_with_approval`, which is alone on `/mcp/merge-approval`; the four merge tools exist only when `MAINLOOP_MERGE_TOOLS_ENABLED` is exactly `true`. An actor with no tool on the surface gets 403. The Go server implements all of them. `task_retry` and `task_reassign` are dropped.
- **Transport** is stateless Streamable HTTP with JSON responses on `:8002`. Only POST is served. The `Host` must be in `MAINLOOP_MCP_HOSTS` (default `mainloop-mcp.<MAINLOOP_NAMESPACE>.svc.cluster.local`); in a cluster startup fails if the list does not include the host agents are given as the MCP origin; `/health` is exempt. A request carrying an `Origin` header is refused with 403 on both MCP paths. Invalid arguments come back as an error result `[validation] invalid tool arguments`.

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
reference selects the `mainloop` MCP origin and Authorization header in the binding
Secret `mainloop-mcp-<binding-id>` in the kagent namespace; it contains no bearer. Each Secret is Opaque, has only the `authorization` key
containing the full `Bearer …` header,
and carries `mainloop.dev/actor-egress=true` and `mainloop.dev/purpose=mcp`. The publisher
creates it before CreateSession and verifies an existing Secret on retry without overwriting
conflicts. The Secret publisher derives that binding's bearer, while
PostgreSQL stores only its hash. MCP authentication checks the current binding and hash on every
request, so revocation denies a cached bearer immediately even if Secret cleanup is delayed.

Bindings persist an `mcp_grant_kind`: `coordination` for main and coordination supervisor/child
bindings, `workspace` for new owner and coding supervisor/child workspaces, and `none` for ordinary
standalone sessions. Existing sessions are not migrated or backfilled. Possessing a credential reference does not select tools;
discovery and invocation use the binding's role and grant together. Workspace grants expose
`whoami` and scoped PR creation. Merge tools still require the existing default-off enablement
gate and the same server-side workspace scope checks.

Creation stores the selected grant, hash, non-secret credential reference and checkout before its
first kagent create call. Retries reuse the persisted request and reference. A publish/revoke
lock is shared through PostgreSQL across Mainloop processes. Revocation clears the hash before
best-effort Secret removal; a cleanup record independent of the binding keeps retries durable
after workspace deletion removes the session rows. Cleanup deletes the whole binding Secret;
a missing Secret is already clean. Publication and deletion reject references that do not
match the binding. Shared-Secret bindings are not migrated, and there is no dual-write. Suspending or resuming a workspace preserves
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

## Delegated task grants

Supervisor/coordination, supervisor/workspace and child/workspace are enrolled pairs. A delegated
child/coordination binding is also task-backed; session-centric children remain available until
the task tool slice replaces that interface. MCP authentication rechecks durable current attempt,
role/depth, owner/project, exact task ancestry and, for code, the held writer generation. New
supervisor and coding-child pairs without an attempt are rejected. Parent revocation denies
child task authority. Credential publication requires a live current attempt and its code claim.

Coordination task discovery currently exposes identity only; task-scoped delegation/report tools
arrive in the next slice. It exposes no repository tools or legacy session-completion report.
Coding supervisor/child discovery exposes identity and scoped PR tools, with merge still behind
its existing flag. Push credentials remain separate from MCP credentials.

## Git capabilities (source support, default off)

`GIT_TRANSPORT_ENABLED` freezes MCP/read/eligible-push references before original CreateSession.
It requires the existing `AGENT_TOKEN_KEY` and exact sanctioned Mainloop service origins.
Domain-separated HMAC derives independent `gread_` and `push_` values from immutable issuance,
binding and original create identities. PostgreSQL stores hashes, references, current runtime
association and cleanup evidence, never capability or PAT bytes. Key recovery mismatch is a hold.

Git values publish after owned non-turn warmup and fresh GetSession proves the frozen contract.
Default/protected workspaces have read only; delegated creating targets can receive read only;
push requires active/current admission. Native send confirms required publications before model
bytes. Lost replies recover the same tuple/value. Revoked unknown creates recover that tuple
without publishing or restoring authority. Historical/dispatched tuples cannot gain new refs.

Git Secrets are immutable Opaque, named `mainloop-git-read-<issuance-id>` and optional
`mainloop-git-push-<issuance-id>`, with the full Authorization value in `authorization` and
purpose/binding/issuance labels. Conflicts are compared, never overwritten. Terminal, archive,
cancellation and replacement revoke both purposes even with flags off. Git cleanup uses recorded
UID preconditions and independent durable tombstones, retaining outages and replacement conflicts.
MCP publication and cleanup keep their own purpose and behavior.

Trusted outbound clients use the same owner's existing PAT. Actors receive only Mainloop
capabilities. No production listener, actor route, PAT mount or enforcement is installed here;
see the [push gate specification](push-gate.md) for authority and remaining release gates.

## Backend GitHub App credentials

Backend GitHub REST calls use `GITHUB_APP_ID` and `GITHUB_APP_PRIVATE_KEY`, with no
PAT or anonymous fallback. The private key is single-line standard base64 of an
unencrypted RSA PEM, validated on first use without exposing the input. Leading and
trailing ASCII whitespace is stripped from both values before validation; interior
whitespace and wrapped base64 remain invalid. The App
must be installed on each repository; installation IDs are discovered rather than
configured. Tokens stay in backend memory and are narrowed to one repository and
the endpoint's permissions. Native agents receive none of these App credentials.
See [Pull requests](pull-requests.md) for registration permissions, expiry, caching,
and the legacy issue-helper permission limitation. The default-off Git
transport described above is separate from this REST authentication.
