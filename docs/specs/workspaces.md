# Workspaces

Workspaces are runtime resources attached to sessions. Workspace lifecycle is separate from the
session's task status, native-agent activity, message delivery, user attention, and publication
state.

## Lifecycle

Mainloop records desired state (`running` or `suspended`), observed state, conditions, the last
observed transition, an operation ID, and the last known snapshot reference. The lifecycle is
owned by Mainloop; Substrate is the source of actor observations.

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
`/workspaces/{id}`. The workspace page shows the manifest, conditions, transition time and
snapshot reference, and offers suspend, resume, and status refresh controls. Session badges and
session status are not changed by workspace operations.

## API

- `GET /workspaces` lists the current user's workspace lifecycle records.
- `GET /workspaces/{id}` returns one workspace lifecycle and manifest.
- `POST /workspaces/{id}/suspend` records the desired state and requests suspension.
- `POST /workspaces/{id}/resume` records the desired state and requests resumption.
- `POST /workspaces/{id}/refresh` reads Substrate status without changing desired state.
- Lifecycle changes are published through the existing event stream as `workspace:updated`.

Suspend is refused while the native delivery ledger contains a recorded, queued, sending,
delivered-but-incomplete, or uncertain delivery. The API reports `409` with the reason. A
transport timeout is stored as `unknown`; the UI asks the owner to refresh status, and Mainloop
does not replay the operation without inspecting the actor first. The durable workspace
generation is advanced with a compare-and-swap before a lifecycle control call.

## Declarative manifest

Each workspace exposes repository URL and branch, allowed agent kinds (`claude` and `codex`),
skill and MCP references, an egress host allowlist, and a resource class. These values describe
intent only. This slice stores and displays them; it does not provision repositories, tools,
network policy, or resources. A missing repository URL is reported as undeclared rather than
inferred.

## Scope and evidence

These endpoints operate on existing Substrate workspace bindings. They do not provision an
actor. Runtime behavior is covered by fake-backed tests; this specification does not claim a
live cluster integration proof.
