# Agent dotfiles

Status: proposed. Nothing in this document is implemented; every section describes intended
behavior for [#313](https://github.com/oldsj/mainloop/issues/313). See
[ROADMAP.md](../../ROADMAP.md#agent-context) for how this fits the "Agent context" direction, and
[delegation.md](delegation.md#shared-agent-guidance) for the shared guidance text this proposal
sits next to without replacing.

Agents in Mainloop start without the owner's standing context: no skills, no subagents, no
owner-level instructions beyond a repository's own `AGENTS.md`. This proposes carrying that
context in as native files from a git repository — "dotfiles" — materialized read-only into each
agent's home directory by kagent session preparation, the same way a repository's own `AGENTS.md`
already reaches a task workspace today. Mainloop adds no new editor, prompt layer or version
store: edits are commits to the dotfiles repository, and both Claude and Codex load the result
themselves.

## Proposed: configuration

An owner-wide dotfiles source — a repository URL and a ref (branch, tag or explicit SHA) — applies
to every project unless a project sets its own. A project-level override has the same shape
(`repository`, `ref`) and, while set, replaces the owner-wide source for that project's sessions
only; clearing it reverts to the owner-wide source. There is no per-task, per-session or
per-workspace override: the same two levels used for environment selection
([environments.md](environments.md)) and merge policy apply here.

Only the owner can set either level. Mainloop has one owner per installation and no separate
admin or member role, so this needs no new authorization model beyond the one every other
owner-level setting already uses.

An empty ref resolves to the dotfiles repository's default branch, the same convention workspace
checkout already uses for an empty `ref` ([workspaces.md](workspaces.md)).

Example (portable, not a real repository): owner-wide source `example/agent-dotfiles` at ref
`main`, with one project overriding to `example/agent-dotfiles` at ref `project-x-stable`.

## Proposed: resolution

At session creation — every workspace and the main thread's session — Mainloop resolves the
effective dotfiles source (the project's override if set, else the owner-wide source) to a full
commit SHA, through the same repository-scoped GitHub App client and commits-endpoint lookup that
already resolves a workspace's checkout ref to a SHA before creation ([workspaces.md](workspaces.md)).
The resolved commit is recorded on the session or workspace next to its checkout commit, not
re-resolved for the life of that session. A later change to the owner's or project's configured
source or ref never moves an existing session; only its next session picks up the new value.

## Proposed: manifest

The workspace manifest gains a `dotfiles` field recording the resolved source: repository, the
configured ref, and the resolved commit SHA. This follows the existing precedent of the manifest
already recording the selected agent kind ([workspaces.md](workspaces.md)), and is shown wherever
the manifest is shown today.

## Proposed: mapping table

`AGENTS.md` is the single source for owner-level instructions. Claude and Codex read it through
each CLI's own native project-instruction mechanism, with no Mainloop-authored prompt layer:

| Source path (dotfiles repository) | Claude Code                                                                                                                           | Codex                                                                                                |
| --------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------- |
| `AGENTS.md`                       | `~/.claude/CLAUDE.md`, written as a one-line `@AGENTS.md` import (the pattern this repository's own `CLAUDE.md` uses for `AGENTS.md`) | `~/.codex/AGENTS.md`, materialized directly — Codex reads `AGENTS.md` natively at its home directory |
| `skills/<name>/...`               | `~/.claude/skills/<name>/...`                                                                                                         | Unresolved — see "Codex user-level skills" below                                                     |
| `agents/<name>.md`                | `~/.claude/agents/<name>.md` (Claude subagents)                                                                                       | No equivalent found; not mapped                                                                      |
| `commands/<name>.md`              | `~/.claude/commands/<name>.md`                                                                                                        | `~/.codex/prompts/<name>.md` (see caveat below)                                                      |

Both files and directories are materialized only into the agent's home directory, never into the
repository checkout, so a repository's own `AGENTS.md`, `.claude/`, `.codex/` or `.agents/` content
is untouched (see "Precedence" below).

### Codex user-level skills

Checked against public Codex CLI documentation and sources reachable from this task (web search;
direct fetches of `developers.openai.com` pages were blocked by this workspace's network
allowlist, so none of the following is independently verified against the primary source):

- Older Codex documentation and third-party guides describe a personal skills directory at
  `~/.codex/skills/`, scanned alongside a project's own `.codex/skills/` or `.agents/skills/`.
- Search results citing an August 2026 documentation update describe skills moving to
  `~/.agents/skills` (personal) and `.agents/skills` (project), with `~/.codex/skills/` still
  honored for existing installations, plus an admin-level `/etc/codex/skills` tier.
- Custom prompts (`~/.codex/prompts/<name>.md`, flat `.md` files, no subdirectories) are
  documented at `developers.openai.com/codex/custom-prompts` per search results, but search
  results also describe them as deprecated in favor of skills.

Because these sources disagree and could not be checked against the primary documentation from
this task, **the exact current path for Codex's user-level skills directory is an open question**,
carried into "Open questions" below rather than asserted here. Codex's custom-prompts mechanism is
mapped above as the closest analogue to Claude's `commands/`, with the same caveat that it may be
superseded by Codex's skills mechanism before this spec is implemented.

Codex has no documented equivalent to Claude's subagents (`agents/`); that row is left unmapped
rather than guessed.

## Proposed: read-only materialization

kagent session preparation lays the resolved dotfiles commit into the agent's home directory,
read-only, for every session kagent prepares — including the main thread's session, which has no
repository checkout. This runs once per session, at the same preparation step that already
provisions the native harness, before the agent's first turn. Materialized files are not
re-fetched or re-resolved later in the session's life (see "Resolution" above): a session keeps
the commit it was given.

## Proposed: limits

- **Size and file count.** Preparation enforces a maximum total size and a maximum file count for
  the materialized tree, both bounded well below the existing workspace resource limits. Exact
  numbers are not chosen in this proposal (see "Open questions").
- **Allowed paths.** Only `AGENTS.md` and the `skills/`, `agents/` and `commands/` directories (as
  mapped above) are read from the dotfiles commit; anything else in the repository is ignored.
  Symlinks and paths that escape the dotfiles checkout are refused rather than followed.
- **Nothing executed.** Preparation only copies files into place. It does not run an install
  script, a hook, or any file from the dotfiles repository as code, the same boundary the existing
  Git read transport already keeps around repository content it serves ([git-transport.md](git-transport.md)).
- **Secret scan, fails closed.** The resolved commit is scanned for likely secrets before
  materialization. A detected match, or a scan that cannot complete, is treated the same way: that
  session's dotfiles are not materialized. The scan never fails open to "materialize anyway."

## Proposed: forks and snapshots

A fork inherits the exact dotfiles commit recorded on its source session, not a fresh resolution
against the currently configured source or ref — the same way a fork already carries over its
source session's project, provider profile and Agent rather than re-deriving them
([warm-state.md](warm-state.md)). A forked session's manifest records that inherited commit
unchanged; nothing re-runs preparation's resolution step for it.

How this interacts with kagent's `$HOME`/Data snapshot mechanics in practice is not established
here: the fork operation itself remains largely unproven as of this writing (see
[warm-state.md](warm-state.md)), so this is carried into "Open questions" rather than asserted as
settled.

## Proposed: precedence

Within one agent's home directory, a repository's own project-level files win over the
materialized user-level dotfiles, following each native CLI's own existing precedence — Mainloop
adds no precedence logic of its own. Claude Code and Codex both already resolve project-level
instructions, skills and commands ahead of user-level ones; dotfiles materialization only ever
writes into the user-level location (`~/.claude/...`, `~/.codex/...`) and never into the
repository checkout, so it cannot shadow or be shadowed by anything the repository itself commits
except through each CLI's own existing merge/override behavior.

## Proposed: UI

The workspace page (and anywhere a session's workspace is shown) shows the dotfiles commit as a
short SHA, linked to that commit in the dotfiles repository (`https`/`http` only, the same
link-rendering rule already applied to the repository link — [workspaces.md](workspaces.md)).
There is no in-app editor for dotfiles content; editing means committing to the dotfiles
repository.

## Proposed: failures

| Case                                          | Behavior                                                                                                                                                       |
| --------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Dotfiles repository unreachable at resolution | Session creation proceeds without materialized dotfiles; the reason is recorded on the manifest so it is visible where the commit link would otherwise appear. |
| Configured ref does not exist                 | Same as unreachable: session creation proceeds without dotfiles, with the reason recorded.                                                                     |
| Secret scan fails or finds a match            | Materialization for that session is skipped (see "Limits" above); the reason is recorded the same way.                                                         |

Dotfiles are supplementary context, not a precondition for an agent to run, so none of these cases
blocks session creation itself the way an unresolvable _checkout_ ref already does
([workspaces.md](workspaces.md)). Whether that is the right default, versus failing the session
the same way a bad checkout ref does, is open to revisiting once this is implemented.

## Proposed API contract changes

Described here, not yet in `api/openapi.yaml`; the contract change comes in a later
implementation PR. Shapes follow the existing owner-settings and project-selection pattern used by
environments ([environments.md](environments.md)):

- `GET /dotfiles-source` / `PUT /dotfiles-source` — read and set the owner-wide source,
  `{repository, ref}` (`ref` omittable for the repository's default branch).
- `GET /projects/{id}/dotfiles-source` — the project's override, or `204` when absent (same
  convention as `GET /projects/{id}/environment`).
- `PUT /projects/{id}/dotfiles-source` — set the project's override, same body shape.
- `DELETE /projects/{id}/dotfiles-source` — clear the override, reverting to the owner-wide source.
- A `dotfiles` object — `{repository, ref, resolved_commit, status}` — added to the existing
  workspace/session read models (`GET /workspaces/{id}`, and the session read it is shown
  alongside), next to the existing checkout fields. `status` carries the "no dotfiles this
  session" reasons from "Failures" above when materialization did not happen.

## Open questions

Carried over from #313, plus one found while writing this:

- Whether Codex reads skills from a user-level directory, and which one — see "Codex user-level
  skills" above; not verified against primary documentation from this task.
- How `$HOME` interacts with kagent's Data snapshots, and specifically whether a fork should keep
  its source's dotfiles commit or re-resolve to the latest configured ref — this proposal chooses
  "keep the source's commit" (see "Forks and snapshots"), but the underlying snapshot mechanics
  are not yet proven.
- Exact size and file-count limits, and the precise allowed-path list.
- Precedence when a repository's own `AGENTS.md` or `.claude/` conflicts with dotfiles — this
  proposal defers entirely to each native CLI's existing precedence (see "Precedence"), but that
  has not been checked against live CLI behavior.
- Whether this replaces the ROADMAP's proposed owner-level instruction document
  ([ROADMAP.md](../../ROADMAP.md#agent-context)), or that document becomes one file inside
  dotfiles.
- Whether an unreachable repository, a missing ref, or a failed scan should block session creation
  outright (as an unresolvable checkout ref already does) instead of the soft-skip this proposal
  defaults to (see "Failures").
