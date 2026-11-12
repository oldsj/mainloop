# Ops agent

Status: slice 1 (read-only) is implemented in configuration. Its kagent and infrastructure parts
are in oldsj/infrastructure; this spec covers how Mainloop reaches it. Not yet proven live.

The ops agent answers questions about the owner's home cluster and homelab: pods, logs, events,
alerts, Argo CD applications and node health. It follows the infrastructure repository's ops-agent
pattern (`k8s/components/ops-agent/README.md`): the model decides, and a tool server that holds
the credentials says no. Its Session holds no credentials and reaches nothing but its MCP servers.

## How main reaches it

No dedicated API or task kind exists. The ops agent is an operator provider profile, `ops`, in
`PROVIDER_PROFILES` (see [Provider profiles](providers.md)), with one role:

```json
{
  "id": "ops",
  "display_name": "Ops (home cluster, read-only)",
  "native_provider": "claude",
  "configuration_revision": "ops-readonly-v1",
  "agents": { "supervisor": { "namespace": "kagent", "name": "ops" } }
}
```

Main hands over an ops request with `delegate`, `mode: "coordination"` and
`provider_profile_id: "ops"` (see [Task delegation](delegation.md)). The task is an ordinary
top-level coordination task:

- Its attempt runs the kagent Agent `ops` as a supervisor at depth one, with a coordination MCP
  grant. It has no repository authority, and it reports through `report` like any supervisor.
- The profile has no `child`, `agent` or `main` role. The ops agent therefore cannot delegate
  children, cannot run as a code task, and cannot become the main thread. Each of those resolves
  to "provider has no Agent" and is refused.
- Its Sessions use the Agent SDK driver. The `ops` Agent is listed in `MAINLOOP_LIVE_AGENTS`, so
  the live view serves them like any other live session.

## What it can do (slice 1)

The agent's tool server, `ops-tools`, is read-only. It reads Kubernetes objects except Secrets and
ConfigMaps (never pod or workload specs), events, pod logs, VictoriaLogs and VictoriaMetrics
queries, firing alerts, Argo CD application status and which resources are out of sync, and node
health. It refuses logs and content-bearing metrics from the private namespaces, and it redacts
secret-looking strings. It has no exec, port-forward, apply, sync, Talos or 1Password access.
Kubernetes RBAC (a read-only ClusterRole without Secrets) is the boundary, and the server enforces
the rest.

The tool server authenticates the ops Agent's Sessions with a bearer that kagent's egress gateway
injects from a Secret. The native agent sees only a placeholder. Mainloop names no credential for
it: Mainloop's own MCP credential is the only Session credential it sends, as for any
coordination task (see [Credentials](credentials.md)).

Permission mode: the SDK driver starts these Sessions in `bypassPermissions`, and a Harness cannot
start them in `auto` yet. The live view can switch the mode for each Session. Starting in `auto`
needs a small kagent change; slice 1 doesn't depend on it, because nothing in the Session can
write.

## Not yet built (slice 2, designed)

- Writes (`kubectl apply`, Argo sync, Talos actions, 1Password reads) as tools bound with
  `requireApproval`. The harness pauses before each call, and the pause surfaces as an approval
  request on the task, the same human-in-the-loop path as other approval cards. The tool server
  holds the write credentials. Merges to the infrastructure repository stay human.
- Home Assistant and OpenCozy MCP servers bound to the ops agent (or main), with their tokens
  injected by the egress gateway rather than held in the Session.
