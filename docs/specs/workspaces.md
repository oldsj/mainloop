# Workspaces

Workspaces are runtime resources attached to sessions. A project branch workspace has its own
Substrate actor and lifecycle. Workspace lifecycle is separate from the session's task status,
native-agent activity, message delivery, user attention, and publication state.

## Lifecycle

Mainloop records desired state (`running` or `suspended`), observed state, conditions, the last
observed transition, an operation ID, the last known snapshot reference, and the last activity
time. Mainloop owns desired state; Substrate is the source of actor observations.

| Observed state                           | User label                   | Meaning                                                                    |
| ---------------------------------------- | ---------------------------- | -------------------------------------------------------------------------- |
| `running`                                | RUNNING                      | Substrate reports the actor running.                                       |
| `suspending`                             | SUSPENDING                   | Substrate is suspending the actor.                                         |
| `suspended` with a snapshot reference    | PARKED                       | Substrate reports suspension and Mainloop has a snapshot reference.        |
| `suspended` without a snapshot reference | SUSPENDED · SNAPSHOT UNKNOWN | Actor suspension is observed, but parking is not confirmed.                |
| `resuming`                               | RESUMING                     | Substrate is restoring the actor.                                          |
| `failed`                                 | FAILED                       | Substrate reports a crashed/deleting actor, or the bound actor is missing. |
| `unknown`                                | UNKNOWN                      | A transport or unrecognized actor state prevents a reliable conclusion.    |

The UI shows workspace state wherever sessions are listed and links from the session detail to
`/workspaces/{id}`. The workspace page shows the manifest, conditions, transition time, snapshot
reference, idle timeout, and last activity, with suspend, resume, refresh, and delete controls.
Session badges and session status are not changed by workspace operations.

## API

- `GET /workspaces` lists the current user's workspace lifecycle records.
- `GET /workspaces/{id}` returns one workspace lifecycle and manifest.
- `POST /workspaces` accepts a project ID, branch, and strict dev manifest, then provisions one
  actor from its declared actor template or the configured default template.
- `POST /workspaces/{id}/suspend` records the desired state and requests suspension.
- `POST /workspaces/{id}/resume` records the desired state and requests resumption.
- `POST /workspaces/{id}/refresh` reads Substrate status without changing desired state.
- `POST /workspaces/{id}/touch?reason=preview` records activity and wakes a suspended workspace.
  The preview proxy can call the same `touch_workspace(workspace_id, reason)` service API.
- `DELETE /workspaces/{id}` deletes the actor and its shim token Secret. Open deliveries return
  `409`; an unconfirmed Substrate deletion keeps the durable workspace binding for reconciliation.
- Lifecycle changes are published through the existing event stream as `workspace:updated`.

Suspend is refused while the native delivery ledger contains a recorded, queued, sending,
delivered-but-incomplete, or uncertain delivery. The API reports `409` with the reason. A
transport timeout is stored as `unknown`; the UI asks the owner to refresh status, and Mainloop
does not replay the operation without inspecting the actor first. The durable workspace
generation is advanced with a compare-and-swap before a lifecycle control call.

## Declarative manifest

Each workspace exposes repository URL and branch, allowed agent kinds (`claude` and `codex`),
skill and MCP references, an egress host allowlist, a resource class, and an optional `dev`
section. `dev` requires exactly one of `image` or `devcontainer_ref`; it can declare an actor
template, sibling services (`name`, `image`, `env`, and numeric ports), HTTP preview ports
(`name`, `number`, `protocol`), and an idle timeout from 1 to 1440 minutes. Unknown fields,
duplicate service or port names, duplicate ports, and invalid timeouts are rejected by the
shared strict model.

The project page creates a workspace per branch and lists each workspace independently. Turn,
delivery, and preview activity use the durable last-activity timestamp. Mainloop's existing
reconcile loop checks idle workspaces once a minute and suspends expired workspaces through the
fenced lifecycle operation; open deliveries still block suspension. Services and image values
are declared by the project manifest and must match its configured actor template.

## Scope and evidence

Runtime behavior is covered by fake-backed tests; this specification does not claim a live
cluster integration proof. The sample under `examples/devenv-sample/` documents the intended
Node plus Postgres project manifest shape.
