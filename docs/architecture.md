# How Mainloop works

Mainloop keeps project conversations, agent work, and workspace state together. The control
plane stores messages and delivery state; native Claude Code and Codex sessions run as kagent
Sessions. Branch workspace Sessions have their own git checkout. Mainloop uses attention as a compute
budget: active workspaces run, while inactive workspaces are suspended until needed.

These diagrams describe implemented code paths; they do not claim a live cluster proof. Known
verification gaps are listed below.

## System overview

```mermaid
flowchart LR
  person["Browser or phone"] --> app["SvelteKit app"]
  app <--> api["FastAPI + DBOS"]
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
running; the rest is parked. Here, parking means that Mainloop asks kagent to suspend the Session.
An `idle` workspace is still present in Mainloop; `idle` is the waiting period before suspension,
not a separate kagent state.

```mermaid
stateDiagram-v2
  [*] --> Running: workspace created
  Running --> Idle: no turn or preview activity
  Idle --> Running: new turn or preview activity
  Idle --> Parked: configured idle timeout expires
  Parked --> Waking: message, preview request, or resume action
  Waking --> Running: actor restored and healthy
  Waking --> Parked: actor confirmed still suspended
  Waking --> Unknown: wake result is not confirmed
```

Each workspace sets its idle timeout (`dev.idle_timeout_minutes`); the default is 30 minutes.
Mainloop checks for expired workspaces about once a minute. Turns, resumes and preview requests
count as activity. Opening a workspace page does not wake its actor. Deliveries in `recorded`,
`queued` (unless held after stop), `sending` or `delivered` states block parking. The check runs under the per-session
lock of the REST process, which orders it with that process's own deliveries. That lock does not
reach the MCP container, a second writer in the same pod, so a message it records during a
suspend can still arrive. Delivery handles that case: a turn that finds the Session suspended
resumes it before sending. A new message resumes a suspended Session, and so does a preview
request (below).

**One open turn per session** is enforced in PostgreSQL, not in memory. Recording a message takes
`pg_advisory_xact_lock(hashtext(...))` for the session, then decides `recorded` or `queued` from
the open deliveries in the same transaction. The REST and MCP containers both write the ledger,
so the in-process `asyncio` lock only orders work inside one process.

No wake or suspend latency has been measured against kagent. The live checks that are still
owed are listed in `docs/specs/workspaces.md`.

## Sending a message

```mermaid
sequenceDiagram
  actor Owner
  participant UI as SvelteKit
  participant Mainloop as FastAPI + DBOS
  participant DB as PostgreSQL
  participant Kagent as kagent gateway
  participant Agent as Native agent session

  Owner->>UI: Send a message
  UI->>Mainloop: Submit message
  Mainloop->>DB: Record message and delivery under workspace lock
  Mainloop->>Kagent: Create or resume the Session
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

Mainloop calls kagent as one fixed service identity, `KAGENT_USER_ID`, and kagent scopes Sessions
to the identity that created them. A Session kagent reports as not found is treated as deleted and
replaced by a new one on the next message. Changing `KAGENT_USER_ID` is therefore a migration:
every existing kagent Session becomes not found and is replaced, losing its native context.

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
through the router. With `onQuiesce: Full` on the harness, the router would wake a suspended
actor without telling kagent, leaving the Session `suspended` and invisible to idle-out. So before
connecting, the proxy resumes a suspended Session through kagent (`ResumeSession`, the same path as
the resume endpoint), once however many previews arrive together. If kagent cannot be asked, the
preview fails (`502`, WebSocket close `1013`) without touching the router. Idle-out then suspends
the workspace again after its idle timeout.

## Isolation and known gaps

- Preview CONNECT traffic goes through the Substrate router, which has no authentication of its
  own; the owner and port checks live in Mainloop.
- The backend reaches Substrate only through the router CONNECT path.
- Workspace lifecycle, preview and idle-out have fake-backed and Postgres-backed tests. They were
  also verified live on a Kind cluster: create, first and second turn, idle-out, a message to a
  suspended workspace, a suspend racing a message, wake-on-preview from a suspended workspace
  (one `ResumeSession` for concurrent previews) and stopping a turn. Still unverified live: the
  cold-clone first turn against the client's 30 second timeout, the K1 negative cases, stopping a
  parked turn, WebSocket previews of a suspended workspace, and the review repairs (they were not
  redeployed). `docs/specs/workspaces.md` lists both.
- The Agent Harness's git origins and `onQuiesce: Full` are kagent installation settings.

## Glossary

| Term          | Meaning                                                                              |
| ------------- | ------------------------------------------------------------------------------------ |
| **Session**   | A kagent Session: one native agent conversation and the actor it runs in.            |
| **Workspace** | The git checkout a Session is created with, plus its preview ports and idle timeout. |
| **Actor**     | The Substrate sandbox kagent runs a Session in.                                      |
| **Router**    | Substrate's control path for connecting Mainloop to an actor.                        |
| **Park**      | Suspend an idle Session so its compute is not running until needed.                  |

## Agent tools and network isolation

Native agents use the `mainloop` MCP server, a dedicated stateless Streamable HTTP listener
on port 8002. Service `mainloop-mcp` serves port 80 at
`http://mainloop-mcp.mainloop.svc.cluster.local/mcp`; it exposes no REST API.
The Substrate egress gateway replaces the agent's literal `Authorization: Bearer
kagent-credential-injected` placeholder with its binding credential. Mainloop stores a token
hash on the binding and publishes the gateway credential under that binding id in
`kagent/mainloop-agent-tokens`. Terminal or archived bindings lose tool access.

A **NetworkPolicy-enforcing CNI is required**. The REST API has no application authorization;
its port 8000 must admit only the frontend and the Tailscale gateway. MCP port 8002 admits
only `ate-system` egress pods labelled `app: atenet-egress`. Verify those gateway pod labels
and the Tailscale gateway selector against the installation, and prove blocked connections
fail before deployment. A default Kind CNI does not provide this enforcement. The cleartext
gateway-to-Mainloop hop depends on this isolation and the pinned stock agentgateway path;
never expose the MCP Service outside the cluster.

The base includes the dedicated MCP container, Service and ingress policy. Cross-namespace
bootstrap resources are separately rendered with `k8s/integrations/kagent`: the empty token
Secret, name-scoped Role/RoleBinding, and RemoteMCPServer. Configure GitOps to preserve the
Secret's runtime-managed data. Main and child AgentTemplates must bind that RemoteMCPServer.
Mainloop uses three kinds of kagent Agent, named by settings. Agent templates and harnesses
remain owned by the kagent installation.

| Setting                                          | Runs                                      | Harness requirements                                                                         |
| ------------------------------------------------ | ----------------------------------------- | -------------------------------------------------------------------------------------------- |
| `KAGENT_MAIN_AGENT` (`mainloop-main`)            | the main thread                           | `sessionIdleTTL: 0s`                                                                         |
| `KAGENT_WORKSPACE_CLAUDE_AGENT` / `_CODEX_AGENT` | workspaces and sessions the owner starts  | `sessionIdleTTL: 0s`, `git.origins` (the repository hosts), `snapshotPolicy.onQuiesce: Full` |
| `KAGENT_CLAUDE_AGENT` / `KAGENT_CODEX_AGENT`     | child agents the main thread delegates to | default TTL and snapshot scope                                                               |

A workspace needs its own Agents because kagent measures idle time from the last A2A event
(previews and `ResumeSession` do not count) and deletes the Session and its actor on expiry,
along with uncommitted and unpushed work. `onQuiesce: Full` keeps files and the dev server across
a suspend, which previews need, but costs a full snapshot at every quiesce, which children should
not pay. The main thread is exempt from Mainloop idle-out. Mainloop parks idle workspace compute itself (`SuspendSession`); archiving or deleting ends the
Session. Main and child templates bind the `mainloop` RemoteMCPServer. Standalone workspace Agents
have no Mainloop tool identity and do not bind that server. Gateway port 8083 must admit only
Mainloop, and TaskStore must admit only actors, using installation-specific policies.

The supported gateway is stock Substrate v0.3.0-alpha3 using agentgateway revision
`50999825cb55904801f7fd6b18b865179d0d50c4`, image digest
`sha256:f1907a50b2e74a071da53fcd2008d585b6a63d31b4e1ba3ee46cf22b342cf04b`.
That dataplane injects complete header values on HTTP and HTTPS when a configured placeholder
header is present. The credential provider must authorize the actor's atespace to read the
`kagent` namespace containing `mainloop-agent-tokens`; Mainloop's name-scoped writer Role does
not grant the provider that access. Keep this version pinned and repeat the gateway regression
proof on every Substrate/agentgateway upgrade: HTTP injection is not a portable guarantee of
other dataplanes. The proposed atenet cleartext allowlist patch is parked and is not required.

The companion's hash-only live gateway proof established HTTP/HTTPS injection and placeholder
isolation on that pinned stock dataplane. The joint proof (a native agent calling the Mainloop MCP
server through the gateway, and blocked connections) passed on a Cilium cluster; the Kind
overlay's default CNI does not enforce NetworkPolicy, so the blocked-connection check needs a
policy-enforcing CNI. The Secret holds the complete `Bearer ml_…` header value; NetworkPolicy
remains required.
