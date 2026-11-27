# Home tools

Status: **proposed, partly proven.** The tool servers and their kagent RemoteMCPServers are in the
private deployment repository. The read tools are bound to mainloop-dev's main and proven there
(2026-11-27); the writes and production's main follow. Mainloop's code doesn't change: like the
[ops agent](ops-agent.md), this is kagent and infrastructure configuration. This spec covers what
main can do with it.

Main handles the house and the kids from the owner's phone directly, without delegating: are the
doors locked, did the doorbell ring, what's drawing power, turn off the lights, lock the back door,
which TVs are on, how much screen time a child has left, close the TVs, log a chore. Most changes
run at once. A few wait for the owner's approval card first:

| Area           | Needs an approval card                                                                                                                                                                                  |
| -------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Home Assistant | Unlocking or opening a lock; opening or moving a cover (garage, gate, blinds); disarming an alarm; opener switches and buttons; scenes that set any of those; scripts and other calls it can't see into |
| OpenCozy       | Every call, reads included, for now (see [what enforces what](#what-enforces-what)). The aim is only approving or rejecting; granting screen time; opening or allowing a TV, and answering "Who's watching?" |

Everything else runs directly. Main doesn't ask in chat first: the card is the question.

## The tools

Three MCP servers are bound in the main thread's AgentTemplate beside `mainloop`, and a fourth is
defined but unbound. Two of the bound ones are approval-gated (`requireApproval: true`):

- **`home-tools`**, for Home Assistant: `find_entities`, `get_states`, `home_status` (locks, open
  doors and windows, doorbells, TVs and media that are on, the biggest power draws, alarms),
  `history` (up to 72 hours), `logbook` (up to 48 hours) and `control`, which calls a service on
  named entities at once. It's a tool server in the infrastructure repository's ops-agent pattern.
  Camera images, `entity_picture` URLs, GPS coordinates and secret-looking attributes are left
  out.
- **`home-tools-approval`**: the same server on another path, with only `secure_control`, which
  takes just the calls in the table above. Gated.
- **`opencozy-approval`**: OpenCozy's own MCP (`/api/mcp`) on the production instance, with every
  OpenCozy tool. Gated.
- **`opencozy`**: the same endpoint and credential, meant for the reads and the writes that run at
  once (closing or extending TVs, ending a TV session or iPad time, logging chores and routine
  steps, adding, changing or removing TVs). It's **unbound** until something enforces that split
  (next section).

Workers, supervisors and reviewers get none of these.

## What enforces what

- **Home Assistant: the tool server.** Home Assistant can't scope a token (even a non-admin user
  may call every device service), so home-tools is the boundary. `control` refuses every call the
  table gates; `secure_control` refuses every call it doesn't, so it can't be a way around a rule.
  The ungated path neither lists nor runs `secure_control`. Both refuse Home Assistant's own
  administration (restarts, reloads, backups, add-ons, people and zones) and targets the server
  can't see (areas, devices, floors, labels): every call names its entities. The token belongs to
  a non-admin user.
- **OpenCozy: its scopes, and the one gated binding.** The credential holds every assistant scope,
  and OpenCozy enforces them. OpenCozy has one endpoint, and the kagent Claude harness drops a
  binding's `tools:` list (its MCP server config has no tool filter). So an ungated `opencozy`
  binding would expose `approve` and `grant_screen_time` with no card. The dev proof on
  2026-11-27 ran `approve` that way, on an approval that didn't exist. Until OpenCozy serves the
  card-free tools on their own endpoint, as home-tools does, or the harness enforces `tools:`,
  every OpenCozy call goes through `opencozy-approval` and waits for a card.
- **The approval: the harness.** `requireApproval` pauses the turn before the call (`tool_deferred`),
  Mainloop shows it as an approval card, and the call runs only on the owner's approval
  (`POST /hitl/{request_id}/respond`), as in [agents and permissions](../architecture/agents-and-permissions.md).
  The tool servers can't verify the approval; they trust that only a gated binding reaches the gated
  tools.
- **Known gaps:**
  - Home Assistant opener switches are recognised by name, so a garage relay named "Relay 2"
    runs at once.
  - A Home Assistant automation that reacts to a direct change runs without approval.
  - While a card is pending, main's delivery stays open and new messages queue behind it (see
    [privileged actions](../design/privileged-actions.md#why-not-kagent-requireapproval)). Privileged actions,
    once built, avoid that; these calls can move onto them.

## Credentials and egress

No credential is in the Session. kagent's egress gateway adds each server's `Authorization`
header from a Secret in the `kagent` namespace, the same way as the ops agent's tool server (see
[ops agent](ops-agent.md#what-it-can-do-slice-1)). Mainloop names no credential, and its own MCP
credential stays the only Session credential it sends (see [credentials](credentials.md)). The
Session reaches the servers only through the gateway, and their origins are on kagent's cleartext
in-cluster MCP allowlist, as is the main thread's Agent on kagent's session allowlist.

## Picking them up

A kagent Session pins the Agent revision it was created with, so new tool bindings reach only
**new** Sessions. Resuming a suspended Session doesn't move it, and a fork keeps its source's
revision. The running main thread gets the tools only after a new main Session (a restart, which
loses the native context). A main identity also keeps the Agent it was bound to: moving main to
another Agent takes a provider switch (see [chat](chat.md)), not a restart. The
[personal tools](personal-tools.md) come in their own change, with their own restart.
