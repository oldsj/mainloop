# How Mainloop works

Mainloop keeps project conversations, agent work, and workspace state together. The control
plane stores messages and delivery state; native Claude Code and Codex sessions run as kagent
Sessions. Branch workspace Sessions have their own git checkout. Mainloop uses attention as a compute
budget: active workspaces run, while kagent puts inactive ones to sleep until they are used again.

Production has run the Go/React stack since 2026-10-12, and read-only smoke run 11 passed. These
diagrams describe implemented code paths; they do not claim further live cluster proof. Known
verification gaps are listed below.

## System overview

```mermaid
flowchart LR
  person["Browser or phone"] --> app["React app"]
  app <--> api["Go server (serve-api)"]
  api --> data[("PostgreSQL<br/>messages, deliveries, workspace state")]
  api -->|"agent turns (A2A) and Sessions"| kagent["kagent gateway"]
  person -->|tailnet preview URL| preview["Mainloop preview proxy"]
  preview --> router["Substrate router"]

  subgraph workspace["One kagent Session actor per workspace"]
    runtime["Harness container<br/>checkout, native CLI, dev server"]
  end
  kagent --> workspace
  router --> workspace
```

Mainloop owns conversations, message delivery, workspace policy, and the mapping from native
sessions to workspaces. kagent runs the Sessions and their actors. Native agent turns go to kagent over A2A; agent sessions keep their
native session identity, history, and tools. The preview proxy checks the
owner and allowed ports before sending traffic through the router.

## Attention and parking

Think of workspace compute like foveated rendering in VR: what needs attention is loaded and
running; the rest is parked. Here, parking is kagent's work, not Mainloop's. kagent's control
API (`kagent.api.v1`, Substrate 1.0) has no suspend or resume call: kagent suspends an idle
Session's Actor itself, after its own quiesce delay, and wakes it on use. Mainloop never asks for
either.

```mermaid
stateDiagram-v2
  [*] --> Running: workspace created
  Running --> Asleep: kagent's quiesce delay passes with no use
  Asleep --> Running: message, workspace preparation, preview request or live-view attach
```

A sleeping Session stays `READY`. Mainloop reads asleep or awake from
`Session.runtime_association.current_active` on `GetSession` and shows a sleeping workspace as
`suspended`; an absent association (no Actor verified on that read) is not known, not asleep.
What wakes it is the next A2A
send, `PrepareSessionWorkspace`, or a connection through the Substrate router (a preview request
or a live-view attach). Opening a workspace page does not wake its actor. Processes do not survive
sleep, because snapshots are data-only: the checkout is kept, but a dev server must be started
again after a wake. The workspace's `dev.idle_timeout_minutes` is still stored and returned but is
no longer enforced.

**One open turn per session** is enforced in PostgreSQL, not in memory. Recording a message takes
a transaction-scoped PostgreSQL advisory lock (`pg_advisory_xact_lock`) on the session's key, then decides `recorded` or `queued` from
the open deliveries in the same transaction. The REST and MCP containers both write the ledger,
so the in-process lock only orders work inside one process.

No sleep or wake latency has been measured against kagent, and the behaviour above is implemented
and tested with fakes only on this API. The live checks that are still owed are listed in
`docs/specs/workspaces.md`.

## Sending a message

```mermaid
sequenceDiagram
  actor Owner
  participant UI as React app
  participant Mainloop as Go server (serve-api)
  participant DB as PostgreSQL
  participant Kagent as kagent gateway
  participant Agent as Native agent session

  Owner->>UI: Send a message
  UI->>Mainloop: Submit message
  Mainloop->>DB: Record message and delivery under workspace lock
  Mainloop->>Kagent: Create the Session on first use
  Mainloop->>Kagent: A2A SendStreamingMessage (messageId = delivery id)
  Kagent->>Agent: Run the turn in the native session
  Agent-->>Kagent: Task status and artifacts
  Kagent-->>Mainloop: Stream of task events
  Mainloop->>DB: Save delivery outcome and the mirrored reply
  UI->>Mainloop: Poll the conversation for the reply
```

Recording the delivery before connecting gives Mainloop a stable delivery to reconcile if a
connection drops. kagent keeps no event cursor, so after a drop or restart Mainloop reads the
current task (`GetTask`, or the first event of `SubscribeToTask`) and replaces its projection
with it. `KAGENT_SEND_NOT_ACCEPTED` is retried with the same `messageId`; any other uncertain
outcome is resolved by finding the task that holds that `messageId`, never by sending again.

Mainloop calls kagent as its own Kubernetes ServiceAccount: the pod mounts a projected token
for the audience `kagent-invocation`, which kagent verifies by TokenReview (its principal is
the ServiceAccount's UID, so recreating the ServiceAccount changes who owns the Sessions). With
`KAGENT_CONTROL_TOKEN_FILE` naming that file, the backend validates a bounded token file
at startup and rereads it for every lifecycle, discovery and A2A request/reconnect, so the
kubelet's rotation is picked up. It sends
only a bearer Authorization header, and requires `KAGENT_USER_ID=mainloop` as expected
principal configuration. Auth refusals are service configuration failures: they never mean
a Session was deleted, and cannot cause replacement or turn resend. Unknown delivery still
requires observation. With the token file unset, legacy insecure `x-user-id: KAGENT_USER_ID`
behavior is preserved; changing that legacy identity loses access to existing Sessions.

Server enforcement depends on enabling kagent's `service-token` mode and default-deny policy
in deployment, with a trusted HTTPS control endpoint and actor exclusion. This enforcement
is not yet deployed. The control bearer is backend-only, separate from provider authentication
and per-binding MCP grants; see [credentials](specs/credentials.md). A genuine NOT_FOUND or
deleted Session can still be replaced on the next message under the existing lifecycle rules.

## Development workspaces and previews

A workspace is a kagent Session created with a checkout request (repository, ref, branch, depth).
The request is stored and resent unchanged if the Session is replaced. A workspace declares its
preview ports and idle timeout. Archiving a session or deleting the workspace deletes its kagent
Session.

```mermaid
flowchart LR
  browser["Browser"] --> url["Preview URL<br/>port--workspace--preview.domain"]
  url --> proxy["Mainloop preview proxy<br/>owner and port checks"]
  proxy --> router["Substrate router<br/>CONNECT actor-upstream:port"]
  router --> actor["Session actor<br/>dev server"]
```

The preview URL is `https://<port>--<workspace>--preview.<domain>`. The preview proxy checks that
the workspace is the configured owner's and that the port is declared, then reaches the Session's actor
through the router. The tunnel target is the actor named in the Session's runtime association
when kagent reports one, then the actor its A2A authority names, and otherwise kagent's stable Actor name
(`session-<kagent session id, lower case>` in `KAGENT_ACTOR_ATESPACE`). The proxy asks kagent to
wake nothing: the `CONNECT` through the router is what wakes a sleeping Actor. A Session that is
not `READY` gets the "waking up" page (`503`, `Retry-After: 2`) and the router is not contacted.
A preview of a just-woken workspace can fail until the agent has started the dev server again.

## Isolation and known gaps

- Preview and live-view CONNECT traffic goes through the Substrate router over mutual TLS: the
  backend presents a pod certificate whose SPIFFE ID must be on the router's allow-list, and
  verifies the router against the servicedns trust bundle. The router knows nothing of owners or
  ports; those checks live in Mainloop.
- The backend reaches Substrate only through the router CONNECT path.
- Workspace lifecycle and preview have fake-backed and Postgres-backed tests. Sleep and wake on
  the current kagent API (Substrate 1.0) are tested with fakes only; a live check on a Kind stack
  is pending. Create, first and second turn and stopping a turn were verified live on a Kind
  cluster before the Go/React cutover, on the retired Python backend and the previous kagent API
  (Substrate 0.4); the suspend, resume and idle-out checks of that run describe calls that no
  longer exist. Still unverified live: the cold-clone first turn against the client's 30
  second timeout, the K1 negative cases, stopping a parked turn, WebSocket previews of a sleeping
  workspace, and the review repairs (they were not redeployed). `docs/specs/workspaces.md` lists
  both. No live write-path run of the Go server is recorded here yet.
- The Agent Harness's git origins are a kagent installation setting.

## Glossary

| Term          | Meaning                                                                              |
| ------------- | ------------------------------------------------------------------------------------ |
| **Session**   | A kagent Session: one native agent conversation and the actor it runs in.            |
| **Workspace** | The git checkout a Session is created with, plus its preview ports and idle timeout. |
| **Actor**     | The Substrate sandbox kagent runs a Session in.                                      |
| **Router**    | Substrate's control path for connecting Mainloop to an actor.                        |
| **Park**      | kagent puts an idle Session's Actor to sleep; the next use wakes it.                 |

## Agent tools and network isolation

Default-off Git enrollment stores an immutable original create plan, purpose hashes and current
field-20 runtime association in PostgreSQL. Native creation freezes MCP/read/optional-push refs
before bytes; durable owned non-turn warmup and fresh GetSession precede usable Git publication.
Native send confirms required publication. Injectable listeners use a PostgreSQL adapter
with one caller-owned connection through ordered dispatch/outcome locks. Each quarantine seed
read revalidates the same credential stamp. No transaction spans external I/O.

The existing publication ledger fences unresolved Git writes by owner/repository/branch across
rotation, restart and successors. Cancellation revokes both purposes while retaining source,
claim and operation state. Independent cleanup tombstones delete Git Secrets with UID
preconditions. Tracked source and GitOps manifests include the `git` container (`serve-git`) from the backend image
sidecar, `mainloop-git-read` / `mainloop-git-push` port-80 Services, and ingress restricted to
`ate-system` pods labelled `app=atenet-egress` on listener ports 8003/8004. The upstream uses
repository-scoped GitHub App installation tokens with `contents: read` for fetches and
`contents: write` for pushes. `GIT_TRANSPORT_ENABLED` and `PUSH_GATE_ENABLED` remain default off;
disabled listeners refuse traffic. This tracked wiring is not proof of a deployed image,
GitOps rollout or live Actor containment. Native handoff/retention/checkout adapters and
live/runtime/package gates remain separate.
See [Git authority](specs/push-gate.md).

Native agents use the `mainloop` MCP server, a dedicated stateless Streamable HTTP listener
on port 8002. Service `mainloop-mcp` serves port 80 at
`http://mainloop-mcp.mainloop.svc.cluster.local/mcp`; it exposes no REST API.
The Substrate egress gateway replaces the agent's literal `Authorization: Bearer
kagent-credential-injected` placeholder with its binding credential. Mainloop stores a token
hash on the binding and creates a runtime-owned `kagent/mainloop-mcp-<credential-identity>` Secret
with the full header in its `authorization` key, labeled `mainloop.dev/actor-egress=true`
and `mainloop.dev/purpose=mcp`. Terminal or archived bindings lose tool access; cleanup
deletes their Secret.
Ordinary bindings use the binding ID as their credential identity. An explicit idle
main-provider switch rotates that identity while retaining the product session, child
ownership and report routing. It leaves the old native Session as it is (kagent puts it to sleep), records it in
`main_native_history`, then provisions the persisted new native create contract. Main's
Agent reference is pinned with that contract. Chat is blocked and reports are held during
preparation; an owner repeats the same selection to recover an interrupted operation.
Recovery uses the pinned contract independently of current catalog admission. A confirmed
absent or settled failed preparation target can be explicitly abandoned for a fresh identity;
uncertain creation retains its original request. MCP intent commits share the retirement
authority lock, and delivery completion, reply and turn accounting commit atomically.
The current main-owner slot belongs to a valid coordination binding. Bootstrap releases
an inactive binding's slot and revokes its remaining bearer in the same transaction that
claims its replacement, retaining old native contracts, history and delivery receipts.
`native_bindings.main_user_id` persists historical main membership separately from the
unique `main_owner_id` reservation. It survives role/authority changes and migration replay.
Provider operations require a valid main when historical membership exists; only ordinary
bootstrap can establish a replacement. True first use can still create a main. Migration
backfills membership once from retained main role, reservation, pinned contract or native history,
without treating ordinary workers/children in the same main thread as main bindings.
See [main-provider selection](specs/chat.md#selecting-the-main-provider).

A **NetworkPolicy-enforcing CNI is required**. The REST API has no application authorization;
its port 8000 must admit only the frontend and the Tailscale gateway. MCP port 8002 admits
only `ate-system` egress pods labelled `app: atenet-egress`. Verify those gateway pod labels
and the Tailscale gateway selector against the installation, and prove blocked connections
fail before deployment. A default Kind CNI does not provide this enforcement. The cleartext
gateway-to-Mainloop hop depends on this isolation and the pinned stock agentgateway path;
never expose the MCP Service outside the cluster.

The base includes the dedicated MCP container, Service and ingress policy. Cross-namespace
integration resources are separately rendered with `k8s/integrations/kagent`: the
Role/RoleBinding and RemoteMCPServer. Binding Secrets are created by Mainloop at runtime.
The trusted backend Role grants namespace-wide Secret `get/create/delete` in `kagent`.
RBAC cannot constrain dynamic creates by name or label; admission policy can restrict writes,
while prefix-scoped reads require a separate broker or authorization boundary.
Main and child AgentTemplates must bind that RemoteMCPServer.
Mainloop uses three kinds of kagent Agent, named by settings. Agent templates and harnesses
remain owned by the kagent installation.

| Setting                                           | Runs                                      | Harness requirements                                       |
| ------------------------------------------------- | ----------------------------------------- | ---------------------------------------------------------- |
| `KAGENT_MAIN_AGENT` (`mainloop-main`)             | the Claude main thread                    | `sessionIdleTTL: 0s`                                       |
| `KAGENT_MAIN_CODEX_AGENT` (`mainloop-main-codex`) | the Codex main thread                     | `sessionIdleTTL: 0s`                                       |
| `KAGENT_WORKSPACE_CLAUDE_AGENT` / `_CODEX_AGENT`  | workspaces and sessions the owner starts  | `sessionIdleTTL: 0s`, `git.origins` (the repository hosts) |
| `KAGENT_CLAUDE_AGENT` / `KAGENT_CODEX_AGENT`      | child agents the main thread delegates to | default TTL and snapshot scope                             |

A workspace needs its own Agents because kagent measures idle time from the last A2A event
(previews do not count) and deletes the Session and its actor on expiry,
along with uncommitted and unpushed work. kagent puts idle compute to sleep by itself (Mainloop
makes no suspend or resume call); snapshots are data-only, so files survive sleep and processes
such as a dev server do not. Archiving or deleting ends the
Session. Main and child templates bind the `mainloop` RemoteMCPServer. Standalone workspace Agents
have no Mainloop tool identity and do not bind that server. Gateway port 8083 must admit only
Mainloop, and TaskStore must admit only actors, using installation-specific policies.

The supported gateway is stock Substrate v0.3.0-alpha3 using agentgateway revision
`50999825cb55904801f7fd6b18b865179d0d50c4`, image digest
`sha256:f1907a50b2e74a071da53fcd2008d585b6a63d31b4e1ba3ee46cf22b342cf04b`.
That dataplane injects complete header values on HTTP and HTTPS when a configured placeholder
header is present. The credential provider must authorize the actor's atespace to read the
per-binding Secrets in `kagent`, selected by `mainloop.dev/actor-egress=true`; Mainloop's
namespace-scoped publisher Role does not grant the provider that access. Keep this version
pinned and repeat the gateway regression proof on every Substrate/agentgateway upgrade: HTTP injection is not a portable guarantee of
other dataplanes. The proposed atenet cleartext allowlist patch is parked and is not required.

The hash-only live gateway proof established HTTP/HTTPS injection and placeholder
isolation on that pinned stock dataplane. The joint proof (a native agent calling the Mainloop MCP
server through the gateway, and blocked connections) passed on a Cilium cluster; the Kind
overlay's default CNI does not enforce NetworkPolicy, so the blocked-connection check needs a
policy-enforcing CNI. The Secret holds the complete `Bearer ml_…` header value; NetworkPolicy
remains required.

## Repository layout

The sections above describe the implemented code. The migration and its direction are in
[ROADMAP.md](../ROADMAP.md#implementation-direction-implemented).

| Path                                               | Role                                                                                                      |
| -------------------------------------------------- | --------------------------------------------------------------------------------------------------------- |
| `api/`                                             | Wire contract (`openapi.yaml`) and the MCP tool manifest. Go and TypeScript types are generated from it.  |
| `server/`                                          | Go server, one binary: `serve-api`, `serve-mcp`, `serve-git`, `migrate` and operator commands.            |
| `web/`                                             | React app (Vite) and its static server (`web/serve`).                                                     |
| `backend/`, `models/`, `frontend/`, `packages/ui/` | Retired Python and SvelteKit code, replaced by `server/` and `web/`. Git history before `e776537` has it. |

Durable work runs as a Postgres reconciler; DBOS is removed. Each side effect is an intent row with an
explicit state and an idempotency key, and no database lock is held across network I/O. Deployment
changes are image and commit pin bumps applied through GitOps.
