# Owner memory

Status: **implemented** (server: `server/internal/ownermemory`, the `remember`, `forget` and
`owner_memory` tools, `/owner/memory`; web: Settings → Memory). Live use is not measured. It extends [project memory](project-context.md#project-memory), which is
implemented, with an owner scope; where this spec is silent, owner memory behaves as project memory
does.

An owner says some things once that hold everywhere: how they like reports written, which
decisions they want to make themselves, that they review on a phone, that they prefer one pull
request per change. Today each project's memory, or the owner's hand-written dotfiles, is the only
place for that, so it is either repeated per project or lost when the main session compacts. Owner
memory holds those few lines and loads them into every session the owner has, in every project and
in the main thread.

It is deliberately small. Memory goes stale, and memory that is wrong costs more than memory that
is missing, so this spec makes the store tiny, curated by the owner, and aged on purpose rather
than trying to remember more.

## What goes where

- **Owner memory** (this spec) holds the owner's own intent that is true across projects:
  preferences, standing decisions, how they like to work. Learned in chat, curated in the app.
- **Project memory** ([project-context](project-context.md#project-memory)) holds the same kind of
  line when it is about one project. If an entry names a project, a repository or its code, it is
  project memory.
- **Agent dotfiles** ([agent-dotfiles](agent-dotfiles.md)) hold what the owner writes by hand in
  git: the owner-level `AGENTS.md`, skills and subagents. An owner memory entry that has proved
  itself can be moved there (see [moving an entry to dotfiles](#moving-an-entry-to-dotfiles)).
- **Mainloop's own records** hold facts: tasks and their state, reports, pull requests and CI,
  transcripts, projects, schedules, cluster and workspace state. These are **never** stored as
  memory. An agent that needs one looks it up when it needs it, with the tools that already exist
  (`task_get`, `task_history`, `transcript_search`, `project_context` and the rest). Mainloop
  already sees all of this, and a copy in memory would only be a stale second answer. Memory holds
  intent; the records hold facts.

What is not built: no memory framework, no vector store or embedding search, no automatic
extraction from transcripts, no summarising model call. An entry exists because an agent called
`remember` with words the owner said, and stays active because the owner keeps it.

## Entries

An owner memory entry is a project memory entry without a project.

- **Bound:** an entry is at most 500 bytes of UTF-8 without NUL, folded onto one line, as in
  project memory. Active owner entries hold at most **3072 bytes** together. This is smaller than
  project memory's 4096 because owner memory rides in every brief of every project, next to that
  project's instructions and memory. The owner holds at most **10 open proposals**.
- **Who records what:**
  - The **main session** records the owner's own words, and its entries are `active` at once. It
    does not record facts it verified, even true ones: those are Mainloop's records (above).
  - **Supervisors** (tasks without a parent) may only propose (`proposed`), because they read
    untrusted input. The owner accepts or rejects a proposal. A typical proposal is a supervisor
    passing on something the owner told it through `supervisor_send`.
  - **Workers** (child tasks) and **workspace agents** cannot record owner memory at all: they
    rarely hear the owner directly, and most of what they read is untrusted. A `remember` or
    `forget` with `scope: owner` from one is `forbidden`. They still read owner memory, and may
    say in a report that an entry looks wrong.
- **Source:** as in project memory, the server records the recording session, its task and
  attempt for a task, and the session's latest delivery (the turn it was in). The owner view links
  to that turn, so the owner can see the words an entry was taken from.
- **Replacing:** an entry may name an active owner entry it `replaces`. When the owner contradicts
  an active entry in chat ("actually, I want draft PRs now"), main records the new wording with
  `replaces` rather than adding a second entry. The old one ends as `replaced`.
- **Repeating:** recording text that is already open answers that entry, as in project memory.
  When main records text that is already active, it also confirms it (`last_confirmed_at` moves to
  now): the owner said it again. A proposal of active text from any other session confirms nothing.
- **Full:** a `remember` by main, or an accept, that would take active owner memory past 3072 bytes
  is refused with `memory_full` (409 on the API) and changes nothing. The fix is consolidation:
  forget an entry, or replace two with one shorter line. Main is told this in the refusal and asks
  the owner which to drop rather than choosing silently.

### States

`proposed` and `active` as in project memory, plus one new open state:

| State       | Loaded into sessions | How it is reached                                                      | Final |
| ----------- | -------------------- | ---------------------------------------------------------------------- | ----- |
| `proposed`  | no                   | `remember` by a non-main session                                       | no    |
| `active`    | yes                  | `remember` by main; owner accepts a proposal; owner keeps a lapsed one | no    |
| `lapsed`    | no                   | the staleness sweep, at the end of the grace period (below)            | no    |
| `rejected`  | no                   | owner rejects a proposal                                               | yes   |
| `withdrawn` | no                   | the proposing session withdraws its own proposal                       | yes   |
| `replaced`  | no                   | a replacing entry becomes active                                       | yes   |
| `forgotten` | no                   | main forgets an active entry; owner forgets an active or lapsed one    | yes   |
| `moved`     | no                   | owner moves an active or lapsed entry to dotfiles or to a project      | yes   |

`lapsed` is not final: the owner can keep it (back to `active`, confirmed now) or forget it.
Nothing is deleted without a decision by the owner or main. Every change into or out of `active`
takes the owner's next memory sequence number, so a pinned brief renders the same entries for
ever, as project memory's do.

## Staleness

Each entry carries `created_at` and `last_confirmed_at`. `last_confirmed_at` starts when the entry
becomes active and moves only when the owner confirms it: **Keep** in the owner view, keeping a
lapsed entry, or main recording the same text again from the owner's words. Nothing else
confirms an entry: an agent using it, citing it or following it without complaint does not, so
silence never keeps a line alive.

- **Review window:** one owner setting, `review_after_days`, **14 days by default**, from 1 to
  365, for every entry; an entry has no window of its own. Changing it applies to every entry at
  once, from its `last_confirmed_at`; it never confirms anything.
- **Due for review:** an active entry not confirmed within the review window is due. It is still
  loaded, and it appears in the owner view's review queue as "Still true?" with **Keep** and
  **Forget**.
- **Lapsed:** a due entry not kept within a further **7 days** is moved to `lapsed` by the
  attention sweep. It stops being loaded into new briefs and running sessions (they are told it was
  removed, below), and it stays in the owner view until the owner keeps or forgets it. The 7-day
  grace is fixed, not a setting.
- **Age is always shown.** Rendered entries carry the date they were last confirmed, so an agent
  can weigh a line confirmed yesterday against one confirmed three weeks ago, and the owner sees
  each entry's age in the view.
- **Contradiction:** an agent that finds an entry wrong does not act on its own judgement against
  it. Main asks the owner, or replaces the entry when the owner has just said otherwise. A task
  says so in its report, and may propose a replacement.

With the default, a real standing preference costs the owner one tap every two weeks, and a line
nobody has looked at stops steering agents within three weeks. An owner whose preferences are
steadier can lengthen the window; the cost of a short one is review taps, the cost of a long one
is agents following a line that stopped being true.

## Tools

The existing `remember` and `forget` tools take a scope. `project_id` stays required for project
memory, so a forgotten argument is a validation error rather than a silent owner-level write.

- `remember(text, scope?, project_id?, replaces?)`: `scope` is `project` (the default) or
  `owner`. With `project`, `project_id` is required, as today. With `owner`, `project_id` must be
  absent and `replaces` names an owner entry. The answer adds `scope` to today's fields.
- `forget(entry_id, scope?, project_id?)`: the same rule. Main forgets an active owner entry; a
  supervisor withdraws its own owner proposal.

`scope: owner` on either tool is for main and supervisors only; from a worker or workspace agent it
is `forbidden`. The tools' descriptions for those roles leave the owner scope out.

- `owner_memory()`: a new read-only tool answering the active owner entries (with `entry_id`,
  `text`, `last_confirmed_at`), `used_bytes`, `max_bytes` (3072), how many proposals and due
  entries wait for the owner, and a `text` rendering. Listed for every live role, like
  `project_context`. Lapsed entries are not returned to agents.

Refusals are project memory's (`not_found`, `conflict`, `forbidden`, `memory_full`,
`too_many_proposals`, `validation`). `validation` covers `scope: owner` with a `project_id`, and
`scope: project` without one.

The tool descriptions say what belongs: the owner's own words about how they work, true across
projects; never a fact Mainloop records, never a repository fact, never a project-specific line.
The shared guidance's memory bullet gains one clause naming the owner scope (a guidance version
bump).

## API

In `api/openapi.yaml` (tag `owner-memory`):

- `GET /owner/memory`: the active entries, then the lapsed ones, then the proposals, each oldest
  first, with `used_bytes`, `max_bytes` (3072), `max_entry_bytes` (500), `review_after_days` (the owner's
  setting, default 14) and `grace_days` (7). Each entry carries `author`, `source` (session, task, attempt and delivery
  ids), `replaces_id`, `state`, `created_at`, `last_confirmed_at`, `decided_at`, and `due_at` (when
  it becomes due; past for a due entry).
- `POST /owner/memory/{entry_id}/decision` with `{decision}`: `accept` or `reject` (a proposal),
  `keep` (an active entry, which confirms it, or a lapsed one, which makes it active again),
  `forget` (an active or lapsed entry), `moved` (an active or lapsed entry, moved to dotfiles) and
  `move_to_project` with `project_id` (below). A decision that
  finds the entry already where it would leave it answers it again; any other state is 409, as is
  an accept or keep past the bound. 404 for an entry that is not the owner's.
- `PUT /owner/memory/settings` with `{review_after_days}` (integer, 1 to 365): the saved setting.
  422 outside the range. It is an owner setting, not policy, so it is not gated on
  `MAINLOOP_OWNER_POLICY_WRITES_ENABLED`.

Writes take an owner-level row lock, so concurrent accepts and keeps never pass the bound.

## Where it loads

Every session gets the active owner entries: the main session, supervisors, workers and workspace
agents, Claude and Codex alike.

- **Task briefs:** a `## Owner memory` section after `## Assigned work` and before
  `## Project instructions`, so the brief reads from owner-wide to project-specific and the more
  specific section comes later. The shared guidance stays where it is, at the end after project
  memory; putting owner memory after it would separate owner memory from the other preferences
  and place it after the project sections that override it. The owner memory sequence is pinned
  on the attempt
  (`task_attempts.owner_memory_seq`) before the brief is first sent, like `memory_seq`.
- **The main session:** in the context block of its first message, after the role line and the
  shared guidance ([chat](chat.md)). The delivery that carries the block records the owner memory
  sequence it gave (`deliveries.given_owner_memory_seq`); the binding has no column of its own.
- **Running sessions:** a session given an older sequence gets the changes in front of its next
  Mainloop-delivered message, never mid-turn and only once, as project memory's changes are
  ([running sessions](project-context.md#running-sessions)). For a task it is an `## Owner memory
changes` part of the `[mainloop:context]` block (formerly `[mainloop:project-context]`); main
  gets the same block in front of a later message, and the whole section inside its context block
  when that is rendered again. A session that was given no owner memory gets the whole
  `## Owner memory` section in the block instead of the changes. What a session has is recorded on the delivery
  (`deliveries.given_owner_memory_seq`). The block's 2 KiB cap is shared: owner memory gets what the
  project part leaves, lists removals before new entries, and brings the session only to the newest
  sequence whose changes fit whole, so the recorded sequence is always one the session was told all
  of. The rest goes with the next message, and the block ends with the cut note. Each change to the
  set (a lapse included, one entry at a time) takes its own sequence, so one change always fits.

Rendered as:

```text
## Owner memory
The owner's own standing preferences, across every project. Follow them unless the project's
instructions or memory below, or the assigned work, say otherwise. If one looks wrong or out of
date, say so (main: ask the owner); do not quietly ignore it.

- <entry> (owner memory <entry id>, confirmed <YYYY-MM-DD>)
```

The date is absolute, not "N days ago", so a resend of a pinned brief is the same bytes. With no
active entries the section is left out.

**Precedence:** the assigned work, then project instructions, then project memory, then owner
memory, then dotfiles `AGENTS.md`. More specific and more deliberate wins. Mainloop renders this
order in text only; nothing enforces it, and server-side policy applies whatever memory says.

## The owner view

Settings gets a **Memory** section (`/settings#memory`):

- **Waiting for you (N):** proposals, with text, the entry each would replace, who proposed it and
  when (linked to the task or session turn), and **Accept** / **Reject**.
- **Review window:** "Ask me if still true after N days" (default 14), saved as it changes.
- **Still true? (N):** due and lapsed entries, oldest confirmation first. A lapsed one is marked
  "Not loaded since <date>". Each has **Keep** and **Forget**.
- **Active:** every loaded entry with its age ("confirmed 12 days ago"), its source link ("By main,
  from this turn"), "N of 3,072 bytes used", and **Forget**. Each entry has **Move to project** and
  **Move to dotfiles** actions (below). An entry whose text is already in the dotfiles is flagged
  "Already in your dotfiles" with **Forget** (below).
- A refused decision shows the server's reason and re-reads the list, as the project Memory section
  does.

**Needs you:** one item for the owner (`source_kind` `owner_memory`, kind `review`), opened while
any proposal waits, or any entry is due, lapsed or already in the dotfiles, with a line such as "2 owner memory proposals and
1 entry to review." It is low priority like project memory's: counted, listed last, never pushed.
It links to `/settings#memory`.

## Moving an entry to a project

Main decides whether a line is owner-wide or one project's, and can guess wrong. **Move to
project** on an active or lapsed entry asks for one of the owner's projects and, in one
transaction under the owner's and the project's memory locks, adds the text to that project's
memory as an active entry and ends the owner entry as `moved`. The new project entry's source
names the owner entry it came from, and the owner entry records the project entry it became. If
the project already has an open entry with the same text, the move answers it instead of adding
a second copy: an active one as it is, a proposal accepted as the moved entry. If the project's active memory would pass its 4096 bytes, the move is refused (409 `memory_full`) and
changes nothing. Both memories take a new sequence number, so running sessions are told of the
removal and, in that project, of the new entry.

## Moving an entry to dotfiles

An entry that has held for months is a candidate for the owner's dotfiles `AGENTS.md`, where it is
versioned and visible outside Mainloop. **Move to dotfiles** opens the entry's text ready to copy,
names the configured dotfiles repository, directory and file (`AGENTS.md`), and offers **I've moved
it**, which sets the entry `moved` and stops loading it. Mainloop never writes to the dotfiles
repository. With no dotfiles source configured, the action links to the dotfiles setup instead.

**Already in the dotfiles.** Each time Mainloop exports the owner's dotfiles at a new commit
([agent dotfiles](agent-dotfiles.md)), it checks every active and lapsed owner entry against the
exported `AGENTS.md`. An entry whose text, folded the same way an entry is (one line, runs of
whitespace as one space) and compared case-insensitively, appears in that file is flagged with the
commit it was found at. The match is exact on purpose; a reworded line is not found, and the owner
uses **Move to dotfiles** for it. A flagged entry still loads, since the duplicate does no harm
beyond its bytes, and the owner view offers **Forget** for it ("Already in your dotfiles at
<commit>; forget it here?"). The flag is cleared if a later export no longer contains the text.
`GET /owner/memory` carries it as `in_dotfiles_commit` (null when not found).

## Not stored, and why

The agent-facing description lists these as refusals of intent (the server cannot tell them
apart from preferences, so they are guidance, not checks): task or PR state, CI results, what a
transcript said, project lists, cluster or workspace state, credentials or tokens, anything about
a person other than the owner's own wishes. A secret-looking entry (the dotfiles secret scanner's
patterns) is refused with `validation`.

## Test plan

Server, with Postgres and fakes, no live agents:

- `remember` scope validation: owner with `project_id`, project without it, default project
  behaviour unchanged for existing callers.
- Main's owner entry is active at once; a supervisor's and a worker's are proposed; the source is
  the server's, not the caller's.
- Bounds: 500 B per entry, 3072 B active (concurrent accepts and keeps under the lock never pass
  it), 10 open proposals.
- `replaces` ends the old entry on activation; repeating active text by main confirms it, by a
  worker does not.
- Staleness with an injected clock: due at 14 days by default, still rendered; lapsed at 21 days
  with a new sequence number; a changed `review_after_days` moves every entry's due date and confirms
  none; an agent reading or citing an entry does not confirm it; keep returns it to active and resets the clock; forget and moved are final.
- Rendering: section placement in code and coordination briefs, absolute dates, a pinned resend is
  byte-identical, empty set adds nothing, the brief size test updated for the new budget.
- Running sessions: a task and the main session each get the change once on their next message,
  including a lapse, and nothing mid-turn.
- Needs you: one item, opened by a proposal or a due entry, rewritten as counts change, resolved
  when none is left.

- Workers and workspace agents: `scope: owner` on `remember` and `forget` is `forbidden`; they
  still get owner memory in their briefs. A supervisor's owner entry is a proposal.
- Move to project: the project entry is active with its source, the owner entry is `moved`, both
  sequences move; a full project memory refuses the move and changes nothing.
- Already in dotfiles: a dotfiles export whose `AGENTS.md` holds an entry's text (whitespace and
  case aside) flags it with the commit, a reworded line is not flagged, a later export without it
  clears the flag, and a flagged entry opens the Needs you item.

Web, with mocked API responses: the three lists, each decision, a refused decision, the age and
source rendering, Move to project (including a refused move), the "Already in your dotfiles" flag,
and Move to dotfiles with and without a dotfiles source.

Not measured by any of this: whether agents follow owner memory, and whether 14 days is the right
default. Both need use.

## Decided

Answers the owner gave to this spec's first open questions:

- **One review window** for every entry, the owner's `review_after_days`; no per-entry window and
  no exemption.
- **The 7-day grace is fixed**, not a setting.
- **Only main and supervisors** record owner memory; workers and workspace agents cannot propose.
- **Move to project** is offered for a line main put in the wrong scope.
- **An entry already in the dotfiles `AGENTS.md` is flagged**, with an offer to forget it.
- **3072 bytes** of active owner memory is the bound.

## Open questions

- **A full memory.** If the owner keeps hitting `memory_full` with entries they all want, should
  the owner view suggest moving the oldest-confirmed ones to dotfiles?
- **Move to owner.** The reverse of Move to project: should a project memory entry that turns out
  to hold everywhere be movable to owner memory from the project page?
