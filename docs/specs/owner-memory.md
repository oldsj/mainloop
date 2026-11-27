# Owner memory

Status: **proposed**, not implemented. Nothing here is in `api/openapi.yaml`, `api/mcp-tools.json`
or the server yet. It extends [project memory](project-context.md#project-memory), which is
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
  - **Supervisors, workers and workspace agents** may only propose (`proposed`), because they read
    untrusted input. The owner accepts or rejects a proposal. A typical proposal is a supervisor
    passing on something the owner told it through `supervisor_send`.
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
| `moved`     | no                   | owner marks an active or lapsed entry as moved to dotfiles             | yes   |

`lapsed` is not final: the owner can keep it (back to `active`, confirmed now) or forget it.
Nothing is deleted without a decision by the owner or main. Every change into or out of `active`
takes the owner's next memory sequence number, so a pinned brief renders the same entries for
ever, as project memory's do.

## Staleness

Each entry carries `created_at` and `last_confirmed_at`. `last_confirmed_at` starts when the entry
becomes active and moves only when the owner confirms it: **Keep** in the owner view, keeping a
lapsed entry, or main recording the same text again from the owner's words.

- **Due for review:** an active entry not confirmed in **60 days** is due. It is still loaded, and
  it appears in the owner view's review queue as "Still true?" with **Keep** and **Forget**.
- **Lapsed:** a due entry not kept within a further **14 days** is moved to `lapsed` by the
  attention sweep. It stops being loaded into new briefs and running sessions (they are told it was
  removed, below), and it stays in the owner view until the owner keeps or forgets it.
- **Age is always shown.** Rendered entries carry the date they were last confirmed, so an agent
  can weigh a line confirmed last week against one confirmed two months ago, and the owner sees
  each entry's age in the view.
- **Contradiction:** an agent that finds an entry wrong does not act on its own judgement against
  it. Main asks the owner, or replaces the entry when the owner has just said otherwise. A task
  says so in its report, and may propose a replacement.

The review window is long enough that a real standing preference costs the owner one tap every two
months, and short enough that a line nobody looked at since a different way of working stops
steering agents within about ten weeks.

## Tools

The existing `remember` and `forget` tools take a scope. `project_id` stays required for project
memory, so a forgotten argument is a validation error rather than a silent owner-level write.

- `remember(text, scope?, project_id?, replaces?)`: `scope` is `project` (the default) or
  `owner`. With `project`, `project_id` is required, as today. With `owner`, `project_id` must be
  absent and `replaces` names an owner entry. The answer adds `scope` to today's fields.
- `forget(entry_id, scope?, project_id?)`: the same rule. Main forgets an active owner entry; any
  other session withdraws its own owner proposal.
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

Described here, to be added to `api/openapi.yaml` first when this is built:

- `GET /owner/memory`: the active entries, then the lapsed ones, then the proposals, each oldest
  first, with `used_bytes`, `max_bytes` (3072), `max_entry_bytes` (500), `review_after_days` (60)
  and `grace_days` (14). Each entry carries `author`, `source` (session, task, attempt and delivery
  ids), `replaces_id`, `state`, `created_at`, `last_confirmed_at`, `decided_at`, and `due_at` (when
  it becomes due; past for a due entry).
- `POST /owner/memory/{entry_id}/decision` with `{decision}`: `accept` or `reject` (a proposal),
  `keep` (an active entry, which confirms it, or a lapsed one, which makes it active again),
  `forget` (an active or lapsed entry) and `moved` (an active or lapsed entry). A decision that
  finds the entry already where it would leave it answers it again; any other state is 409, as is
  an accept or keep past the bound. 404 for an entry that is not the owner's.

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
  shared guidance ([chat](chat.md)). The main binding records the owner memory sequence it was
  given, as it records the guidance version.
- **Running sessions:** a session given an older sequence gets the changes in front of its next
  Mainloop-delivered message, never mid-turn and only once, as project memory's changes are
  ([running sessions](project-context.md#running-sessions)). For a task it is an `## Owner memory
changes` part of the same `[mainloop:project-context]` block (renamed `[mainloop:context]`
  when built); for main it rides the context block. What a session has is recorded on the delivery
  (`deliveries.given_owner_memory_seq`). The block's 2 KiB cap is shared.

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
- **Still true? (N):** due and lapsed entries, oldest confirmation first. A lapsed one is marked
  "Not loaded since <date>". Each has **Keep** and **Forget**.
- **Active:** every loaded entry with its age ("confirmed 12 days ago"), its source link ("By main,
  from this turn"), "N of 3,072 bytes used", and **Forget**. Each entry has a **Move to dotfiles**
  action (below).
- A refused decision shows the server's reason and re-reads the list, as the project Memory section
  does.

**Needs you:** one item for the owner (`source_kind` `owner_memory`, kind `review`), opened while
any proposal waits or any entry is due or lapsed, with a line such as "2 owner memory proposals and
1 entry to review." It is low priority like project memory's: counted, listed last, never pushed.
It links to `/settings#memory`.

## Moving an entry to dotfiles

An entry that has held for months is a candidate for the owner's dotfiles `AGENTS.md`, where it is
versioned and visible outside Mainloop. **Move to dotfiles** opens the entry's text ready to copy,
names the configured dotfiles repository, directory and file (`AGENTS.md`), and offers **I've moved
it**, which sets the entry `moved` and stops loading it. Mainloop never writes to the dotfiles
repository. With no dotfiles source configured, the action links to the dotfiles setup instead.

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
- Staleness with an injected clock: due at 60 days, still rendered; lapsed at 74 days with a new
  sequence number; keep returns it to active and resets the clock; forget and moved are final.
- Rendering: section placement in code and coordination briefs, absolute dates, a pinned resend is
  byte-identical, empty set adds nothing, the brief size test updated for the new budget.
- Running sessions: a task and the main session each get the change once on their next message,
  including a lapse, and nothing mid-turn.
- Needs you: one item, opened by a proposal or a due entry, rewritten as counts change, resolved
  when none is left.

Web, with mocked API responses: the three lists, each decision, a refused decision, the age and
source rendering, and Move to dotfiles with and without a dotfiles source.

Not measured by any of this: whether agents follow owner memory, and whether 60 days is the right
window. Both need use.

## Open questions

- **The review window.** 60 days plus 14 is a guess. Should the owner set it, or should an entry
  carry its own ("until the end of the quarter")?
- **Workers proposing.** Should workers have the owner scope at all, or only main and supervisors?
  Workers rarely hear the owner directly; dropping it would cut a source of noisy proposals.
- **Main's judgement of scope.** Main decides whether a line is owner-wide or one project's. A
  wrong guess is cheap to fix in the view, but the view could offer **Move to project** too.
- **Confirmation by use.** Should an entry that agents cite and the owner does not dispute count as
  confirmed? This spec says no: only the owner confirms, so silence never keeps a line alive.
- **Detecting a move.** Mainloop could notice an entry's text in the exported dotfiles `AGENTS.md`
  and suggest **I've moved it**. Useful, but a fuzzy match; left out for now.
- **The 3072-byte bound.** Tight on purpose. If the owner keeps hitting `memory_full` with entries
  they all want, the answer may be moving the oldest to dotfiles rather than a larger bound.
