# Project context

Status: **project instructions are implemented** on the server (API, task briefs and the
`project_context` tool), tested with Postgres and fakes and not measured against a live agent. The
project page section, project memory and updates to running sessions are **proposed**: see
[Proposed](#proposed) below.

A project's owner can say something once and have every later agent working on the project follow
it: "UI demos use fictional data", "merge with --squash", "ask before touching billing". Mainloop
holds this per project, next to the repository, not in it.

## What goes where

- **The repository's `AGENTS.md`** (and a `CLAUDE.md` that imports it) holds repository facts: how
  to build and test, layout, code style and contribution rules. Anyone working on the repository
  needs them, with or without Mainloop, and they change through pull requests.
- **Project instructions** in Mainloop hold the owner's standing direction for agents on this
  project that does not belong in the repository: it is private, it is about how the owner runs the
  work rather than the code, or it changes faster than a pull request.
- **The shared guidance** ([delegation](delegation.md#shared-agent-guidance)) is the same for every
  project: how a Mainloop agent works at all.
- **Owner-wide** instructions and skills across every project are the proposed
  [agent dotfiles](agent-dotfiles.md), not this.

## Project instructions

One Markdown text per project, written only by the owner. Agents read it and cannot change it.

- **Bound:** at most 8192 bytes of UTF-8, without NUL, after trimming surrounding whitespace. The
  server refuses a longer text with 422; it is not cut. The bound is half the 16 KiB a brief may
  hold, since the instructions are carried next to it in every brief.
- **Versions:** every change is a new version (1, 2, …) and earlier versions are kept. Version 0
  with empty text means none were ever set. Saving empty text clears the instructions as a new
  version. Saving the current text again changes nothing and keeps the version.
- **Conflicts:** a save names the version it was made from (`expected_version`). If the
  instructions changed since then the save is refused with 409 and changes nothing, so an edit made
  from a stale read is never lost silently. Two saves from one read leave exactly one new version.
- **Not gated:** saving does not need `MAINLOOP_OWNER_POLICY_WRITES_ENABLED`. Instructions are
  guidance to agents, not policy; merge policy, protected paths and every other server-side check
  apply whatever the instructions say.

### API

- `GET /projects/{project_id}/instructions`: `project_id`, `version`, `text`, `max_bytes` (8192)
  and `updated_at` (null at version 0).
- `PUT /projects/{project_id}/instructions` with `{text, expected_version}`: the saved
  instructions, 409 when `expected_version` is not current, 422 for text over the bound or not
  valid text, 404 for a project that is not the owner's.
- `GET /projects/{project_id}/instructions/versions`: up to the 50 newest versions, newest first.

### In task briefs

Every task with a project, code or coordination, supervisor or child, carries the instructions in
its first message, after `## Assigned work` and before the shared guidance:

```text
## Project instructions (owner, v3)
The project owner's standing instructions for every task in this project. Follow them unless the
assigned work explicitly overrides one, and say so in your report when it does.

<the owner's text>
```

A project without instructions (version 0, or cleared) adds no section. The stored brief is not
changed; the section is rendered when the brief is sent, like the shared guidance.

Before an attempt's brief is first sent, the project's current version is pinned on the attempt
(`task_attempts.instructions_version`, 0 for none). Every send of that attempt's brief, including a
resend after a failed delivery, renders the pinned version, so one attempt never gets two texts and
the attempt records which instructions it was given.

### Edits and sessions

An edit reaches every attempt briefed after it. An attempt already briefed keeps the version it
was given: nothing is steered into a running turn. A running agent can read the current text with
`project_context`. Telling running sessions about an edit on their next message is proposed below.

The main session is not bound to one project and its standing context carries no project's
instructions; it reads them with `project_context` when it needs them, for example before writing
a brief.

### `project_context`

`project_context(project_id)` answers the project's current instructions: `project_id`,
`version`, `instructions`, `max_bytes`, `updated_at` (when set), and a `text` line rendering them.
It only reads. The main session may read any project of its owner; a task or workspace agent only
the project its session is bound to (`forbidden` otherwise); a project that is not the owner's is
`not_found`. A completed task that may only look at merge status does not have it.

## Proposed

Not implemented; planned as the next slices.

- **Project page:** an Instructions section to read, edit (with a byte counter and the 409 review
  the merge policy section already has) and see earlier versions.
- **Project memory:** short, source-linked entries recorded by agents (`remember` / `forget`),
  each at most 500 bytes, with the session, task, attempt and message it came from resolved by the
  server. The main session records entries that take effect at once, only from the owner's own
  words or facts it verified; they show "by main" with their source and the owner can revoke them.
  A task's entries are proposals that only the owner accepts, since a worker reads untrusted input
  (repository content, pull request comments, CI logs). Accepted entries are rendered into briefs
  after the instructions within a 4 KiB total; a `remember` past that bound is refused until an
  entry is forgotten or replaced. Pending proposals show on the project page and as one
  low-priority Needs you item per project, not one per proposal. A repository fact is proposed as
  an `AGENTS.md` change instead.
- **Running sessions:** a task session whose project context changed after its brief gets the
  changed context in front of its next Mainloop-delivered message, the way a guidance version bump
  reaches the main session ([chat](chat.md)). It never interrupts a turn.
