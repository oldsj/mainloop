# Home tools (read-only)

Status: **proposed, partly configured.** The tool servers and their kagent RemoteMCPServers are in
the private deployment repository. They are not bound to the main thread's Agent yet. That binding comes
after a live proof on mainloop-dev. Nothing here is proven live. Mainloop's code doesn't change:
like the [ops agent](ops-agent.md), this is kagent and infrastructure configuration. This spec
covers what main can do with it.

Main answers household questions from the owner's phone directly, without delegating: are the
doors locked, did the doorbell ring, what's drawing power, which TVs are on, how much screen time
a child has left, what's waiting for approval. It can only read. A change (unlock a door, open the
TVs, approve a chore) isn't available yet. Main says exactly what the change would be and stops.
Writes come later as privileged actions the owner approves (oldsj/mainloop#435,
`docs/design/privileged-actions.md`).

## The tools

Two MCP servers, bound in the main thread's AgentTemplate (`mainloop-main`), beside `mainloop`:

- **`home-tools`**, for Home Assistant: `find_entities`, `get_states`, `home_status` (locks, open
  doors and windows, doorbells, TVs and media that are on, the biggest power draws, alarms),
  `history` (up to 72 hours) and `logbook` (up to 48 hours). It's a tool server in the
  infrastructure repository's ops-agent pattern. Home Assistant can't scope a token, so the server
  is the boundary: its only request to Home Assistant is a GET to the state, history or logbook
  API. Camera images, `entity_picture` URLs, GPS coordinates and secret-looking attributes are
  left out.
- **`opencozy`**, OpenCozy's own MCP (`/api/mcp`) on the production instance. Its credential is an
  assistant credential with only the `tv:read` and `approvals:read` scopes, plus the household
  reads every assistant credential has. OpenCozy lists and runs no tool outside the credential's
  scopes, so there are no write tools. The binding also allowlists the read tools.

## Credentials and egress

Neither credential is in the Session. kagent's egress gateway adds each server's `Authorization`
header from a Secret in the `kagent` namespace, the same way as the ops agent's tool server (see
[ops agent](ops-agent.md#what-it-can-do-slice-1)). Mainloop names neither credential, and its own
MCP credential stays the only Session credential it sends (see [credentials](credentials.md)). The
Session reaches both servers only through the gateway, and both origins are on kagent's cleartext
in-cluster MCP allowlist.

## Picking them up

A kagent Session pins the Agent revision it was created with, so new tool bindings reach only
**new** Sessions. Resuming a suspended Session doesn't move it, and a fork keeps its source's
revision. The running main thread gets the tools only after a new main Session (a restart, which
loses the native context). Until then it answers household questions as before.
