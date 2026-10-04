# mainloop

A single conversation to manage all of your AI sessions - that keeps working while you're away.

Your main conversation thread is the closest digital mapping to your own internal thread of consciousness. Everything else flows into a unified inbox that surfaces only what needs your attention.

Inspired by [You Are The Main Thread](https://claudelog.com/mechanics/you-are-the-main-thread/) — you are the bottleneck, so spawn parallel AI sessions and let them handle the work while you stay in flow.

| Desktop                                                                  | Mobile                                                                                                                          |
| ------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------- |
| <img width="600" alt="mainloop desktop" src="docs/images/desktop.png" /> | <img width="150" alt="mainloop mobile" src="https://github.com/user-attachments/assets/3b263abe-9fee-45c5-9ba8-38edd5e5e4a8" /> |

## How It Works

```text
You (phone/laptop)
    │
    ▼
┌─────────────────────────────────────────────────────┐
│                   Main Thread                        │
│       Native Claude Code session via kagent          │
│                                                      │
│   user@mainloop$ research X      ← inline sessions  │
│   ├── [research X] thinking...   ← threaded reply   │
│   └── [research X] here's what   ← notification     │
└──────────────┬────────────────┬─────────────────────┘
               │                │
       ┌───────▼──────┐  ┌──────▼───────┐
       │   Session 1  │  │   Session 2  │  ...
       │   (Claude)   │  │   (Opus)     │
       │              │  │              │
       │   Research   │  │  Feature dev │
       └──────────────┘  └──────────────┘
```

- **Main thread**: One continuous native conversation; delegated sessions surface results back
- **Sessions**: Native Claude Code or Codex work with their own conversations; appear as colored threads in your timeline
- **Notifications**: Slack-style thread replies notify you when sessions need attention or complete
- **Persistence**: Mainloop stores conversations, delivery records, and workspace lifecycle state in PostgreSQL; native history remains with the provider CLI in its kagent Session
- **Runtime isolation**: Substrate workspaces use gVisor actors. The pinned fork honors the
  agent image's non-root UID `10001`; microVM isolation is deferred.

## Quick Start

```bash
# Copy example environment file and configure
cp .env.example .env
# Set the Substrate router, actor bindings, and shim Secret names.
# Keep provider credentials in Mainloop's configured control-plane Secrets; actors receive synthetic placeholders only.

# Start all services
make dev

# Frontend: http://localhost:3000
# Backend:  http://localhost:8000/docs
```

## Production Deployment

The Kubernetes manifests under `k8s/apps/mainloop/` provide reusable bases and example overlays. Supply environment-specific images, domains, credentials, and storage through your deployment configuration, and apply production changes through your GitOps workflow.

## Project Structure

```text
mainloop/
├── backend/       # Python FastAPI + DBOS workflows
├── frontend/      # SvelteKit + Tailwind v4 (mobile-first responsive)
├── models/        # Shared Pydantic models
├── packages/ui/   # Design tokens + theme.css
└── k8s/           # Kubernetes manifests
```

## UI

- **Mobile**: Bottom tab bar (Chat / Sessions)
- **Desktop**: Chat with always-visible Sessions sidebar

**Chat** — your main thread with inline sessions:

- Sessions spawn as colored thread blocks in your timeline
- Session messages appear as Slack-style thread notifications
- Click to expand inline or zoom to fullscreen view
- Terminal-style prompt: `user@context$` with content on new line

**Sessions** — all background work in one place:

1. Active sessions (with live status)
2. Sessions needing input (questions, plan reviews)
3. Completed and failed sessions
4. Each session has its own conversation you can zoom into

## Agent Workflow

Agents are native sessions spawned for development tasks. Mainloop records the session and its deliveries; the native CLI session runs in a kagent Agent (A2A) selected by agent kind.

```text
┌─────────────┐     ┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│   Spawn     │────►│    Work     │────►│     PR      │────►│    Close    │
│   (main)    │     │  (kagent)  │     │  (GitHub)   │     │  (summary)  │
└─────────────┘     └─────────────┘     └─────────────┘     └─────────────┘
       ▲                   │
       └───────────────────┘
         check in / spawn more
```

1. **Spawn** - Main thread creates agent for a task
2. **Work** - A native Claude Code or Codex session runs in its kagent Agent
3. **PR** - Agent creates and merges GitHub PR when ready
4. **Close** - Agent posts summary back to main thread

You stay in main thread, checking in on agents and spawning new ones as needed.

## Documentation

**Specs** (source of truth for app behavior):

- [Chat](docs/specs/chat.md) - Main thread conversation
- [Sessions](docs/specs/sessions.md) - Background work and status
- [Layout](docs/specs/layout.md) - Mobile and desktop views

**Guides**:

- [Architecture](docs/architecture.md) - Substrate workspaces, delivery, previews, and credentials
- [Contributing](CONTRIBUTING.md) - Local setup, development commands, and contribution guidance

## License

This project is licensed under the [Sustainable Use License v1.0](LICENSE.md) - a source-available license that allows free use for internal business, non-commercial, and personal purposes.

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
Secret's runtime-managed data. Each native AgentTemplate must bind that RemoteMCPServer.
Configure `KAGENT_MAIN_AGENT` (default `mainloop-main`) as a dedicated Claude Agent whose
Harness has `sessionIdleTTL: 0s`; child agents retain their own TTLs. Agent templates and
harnesses remain owned by the kagent installation. Gateway port 8083 must admit only Mainloop,
and TaskStore must admit only actors, using installation-specific policies.

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
isolation on that pinned stock dataplane. Joint Mainloop/native-agent MCP proof and blocked
connection/CNI verification remain pending. The Secret holds the complete `Bearer ml_…` header
value; NetworkPolicy remains required even though the gateway proof passed.

Slice a2 requires a **fresh database**. There is no supported upgrade from earlier slices or
credential-less kagent bindings. Reset dev/spike data before using this channel. Main and child
Sessions start with credential references; the main thread uses its dedicated main Agent.
Earlier native session history is not carried into a2. Mainloop does not migrate or reuse
pre-a2 sessions.
