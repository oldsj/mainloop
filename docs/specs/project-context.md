# Project context

Status: **project instructions are implemented**: the API, task briefs, the `project_context` tool
and the Instructions section of the project page. **Project memory is implemented on the server**:
the `remember` and `forget` tools, the owner's API, task briefs, the Memory section of the project
page and its Needs you item. **Running task sessions** get changes on their next message. The
server is tested with Postgres and fakes, and the page with mocked API responses; none of it has
been measured against a live agent.

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
- **Project memory** holds short lessons agents record for later agents on the project: an owner
  preference said in chat, or a fact an agent verified that is not about the code.
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

### On the project page

The Instructions section sits first under the project page's Settings. It shows the saved version
and when it was saved, or "No instructions yet.", and a text box with the current text.

- The byte count under the box is of the trimmed text in UTF-8, as the server measures it. Save is
  offered only for a draft that differs from the saved text after trimming and is within the bound.
- A draft remembers the version it was started from. If the saved version moves under it, from
  another tab or a save refused with 409, the draft is kept as typed, the section shows the current
  version's text, and Save becomes "Save over version N", which sends that version. Discard draft
  returns the box to the saved text.
- Any other refused save shows the server's message and keeps the draft.
- Earlier versions lists the stored versions, read only when the owner opens it.

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
the attempt records which instructions it was given. Project memory follows the instructions
section and is pinned the same way (see [project memory](#project-memory)).

### Edits and sessions

An edit reaches every attempt briefed after it. An attempt already briefed keeps the version it
was given: nothing is steered into a running turn. A running agent can read the current text with
`project_context`, and its next message carries the change (see
[running sessions](#running-sessions)).

The main session is not bound to one project and its standing context carries no project's
instructions or memory; it reads them with `project_context` when it needs them, for example before
writing a brief.

### `project_context`

`project_context(project_id)` answers the project's current instructions and memory: `project_id`,
`version`, `instructions`, `max_bytes`, `updated_at` (when set), `memory` (the active entries),
`memory_used_bytes`, `memory_max_bytes`, `proposed` (how many proposals wait for the owner), and a
`text` line rendering them. It only reads. The main session may read any project of its owner; a task or workspace agent only
the project its session is bound to (`forbidden` otherwise); a project that is not the owner's is
`not_found`. A completed task that may only look at merge status does not have it.

## Project memory

Short entries agents record for every later agent on the project. They are for what the owner
said once and should not have to repeat, or a fact an agent verified that is not about the code; a
repository fact goes in an `AGENTS.md` change instead.

- **Bound:** an entry is at most 500 bytes of UTF-8 without NUL, folded onto one line (runs of
  whitespace become one space). Active entries hold at most 4096 bytes together.
- **Who records what:** the main session's entries are `active` at once; it records only the
  owner's own words or facts it verified. A task's or workspace agent's entries are `proposed`,
  since a worker reads untrusted input (repository content, pull request comments, CI logs), and
  only the owner accepts them. Recording text that is already active or proposed answers that
  entry, so a retried call adds nothing.
- **Source:** the server records where an entry came from, not the agent: the recording session,
  its task and attempt for a task, and the session's latest delivery (the turn it was in).
- **Replacing:** an entry may name an active entry it `replaces`. When the new entry becomes active
  (at once for main, on accept for a proposal) the old one ends as `replaced`. A proposal whose
  target has ended since is accepted on its own.
- **Ending:** the main session `forget`s an active entry (`forgotten`); a session withdraws a
  proposal it made itself (`withdrawn`). The owner accepts or rejects a proposal (`rejected`) and
  revokes an active entry (`forgotten`). No one else can end an entry, and every state but
  `proposed` and `active` is final.
- **Full:** a `remember` by main, or an accept, that would take active memory past 4096 bytes is
  refused (`memory_full`, or 409 on the API) and changes nothing; the fix is to forget an entry or
  replace one with a shorter consolidation. A project holds at most 20 open proposals
  (`too_many_proposals`).

Writes take the project row lock, so concurrent accepts never pass the bound.

### Memory tools

- `remember(project_id, text, replaces?)` answers the entry: `entry_id`, `state`, `author`,
  `entry` (its text), `replaces` when set, and a `text` line saying whether it is active or waits
  for the owner.
- `forget(project_id, entry_id)` answers the ended entry.

Both are listed for every live role, as `project_context` is. The main session may use any project
of its owner; a task or workspace agent only the project its session is bound to (`forbidden`
otherwise). Refusals: `not_found` (project or entry), `conflict` (the entry's state does not allow
it), `forbidden` (another session's proposal, or an active entry for a non-main session),
`memory_full`, `too_many_proposals`, `validation`.

### Memory API

- `GET /projects/{project_id}/memory`: the active entries, then the proposals, each oldest first,
  with `used_bytes`, `max_bytes` (4096) and `max_entry_bytes` (500). Each entry carries its
  `author` (`main`, `task` or `agent`), `source` (session, task, attempt and delivery ids),
  `replaces_id`, `created_at` and `decided_at`.
- `POST /projects/{project_id}/memory/{entry_id}/decision` with `{decision}` (`accept`, `reject` or
  `revoke`): the entry after the decision. A decision that finds the entry already where it would
  leave it answers it again; any other state is 409, as is an accept past the bound. 404 for a
  project or entry that is not the owner's.

### Memory in task briefs

Active entries follow the instructions section, in the order they became active:

```text
## Project memory
Lessons recorded for every task in this project, from the owner's own words, facts the main session
verified, or proposals the owner accepted. Follow them like the instructions; if one is wrong or out
of date, say so in your report.

- <entry> (memory <entry id>)
```

Proposals are never carried. Each change to the active set takes the project's next memory
sequence number, and before an attempt's brief is first sent the current number is pinned on the
attempt (`task_attempts.memory_seq`, 0 for none). Every send of that attempt's brief renders the
entries active at that number, so a resend carries the same entries even after one is forgotten.

### Memory on the project page

A **Memory** section follows Tasks, outside the folded Settings, so waiting proposals are in view.

- **Waiting for you (N)** lists the proposals, oldest first, each with its text, the active entry it
  would replace, who recorded it and when ("From a task" or "From an agent", linked to the task or
  session), and **Accept** and **Reject**.
- **Active** lists the entries every new brief carries, with "N of 4,096 bytes used". Each says who
  recorded it ("By main", linked to the main thread, or "From a task, …, accepted …") and offers
  **Revoke**.
- A decision re-reads the memory and the Needs you snapshot. A refused one (the entry was decided
  elsewhere, or accepting would pass 4096 bytes) shows the server's reason, and the list shows the
  entries as they are now. An entry that does not match the contract is left out; the rest show.

### Memory in Needs you

A project with proposals waiting has one Needs you item, not one per proposal (`source_kind`
`project_memory`, kind `review`). The attention sweep opens it, rewrites its line as the count
changes ("2 project memory proposals wait for you. Accept or reject them."), resolves it once none is
left and reopens the same item, unread, for a later proposal. `review` is a low-priority owner
decision: it counts toward the Needs you badge, is listed after the other items and sends no push
notification. Its row links to the project page's Memory section ("Review on the project page").

## Running sessions

A task session briefed before its project's instructions or memory changed gets the change in
front of its next Mainloop-delivered message, whoever sent it (the owner, main, or Mainloop). It
never interrupts a turn, and it is given once:

```text
[mainloop:project-context] This project's context changed since your brief. Where they differ, this replaces what your brief said.
## Project instructions (owner, v4, were v3)
<the new instructions>
## Project memory changes
- New: <entry> (memory <entry id>)
- Removed: <entry> (memory <entry id>)
[mainloop:new-message]
<the message>
```

- Only what changed goes: instructions up to 1 KiB are sent whole, longer ones as the lines removed
  (`- `) and added (`+ `); cleared instructions say so. Memory lists the entries that became active
  and the ones that stopped being active (forgotten, revoked or replaced), not the whole set.
- The block is at most 2 KiB. Past that it is cut at a whole line and ends with "… more changed:
  call `project_context` for the current instructions and memory."
- What a session has is recorded on the delivery that gave it (`deliveries.given_instructions_version`
  and `given_memory_seq`), in the transaction that freezes the sent text, so a resend sends the same
  bytes. A session has what its newest such delivery gave, or with none, what its attempt's brief
  pinned. A delivery kagent never saw (failed or cancelled with no native task) does not count, so
  the change goes with the next message instead.
- The mirrored message in the conversation stays what was written. The main session and sessions
  that are not a task attempt's get nothing: main reads project context with `project_context`.
