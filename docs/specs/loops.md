# Loops

Status: **Proposed.** Nothing on this page is implemented. It is issue #314, built on
[goals](goals.md) (issue #263), [AGENTS.md](../../AGENTS.md)'s reconciler rule, and the existing
event and delivery machinery this page names throughout rather than duplicates.

## Why (Proposed)

The owner today babysits running work by hand: watching CI, confirming production stayed healthy,
noticing a task gone silent, waiting out a checkpoint window. Each of those checks needs a person
or an outside session to wake up, look, and act or go back to waiting, even though Mainloop already
owns all of that state. A loop is a durable, owner-visible wake-up rule that fires a single bounded
turn of the owning supervisor's own session instead.

## Loop definition (Proposed)

A loop has:

- **Owner.** A goal or a task — any task a supervisor could otherwise poll by hand, not only a
  goal's root. The loops issue's own example, "merge #312 when green, then confirm prod," is a loop
  on a single task with no goal involved.
- **Trigger.** One event trigger or one time trigger (see "Triggers" below). A loop has exactly one
  trigger; a supervisor that wants to wake on several conditions arms several loops.
- **State.** `armed` (watching), `paused` (owner-suspended, not watching), `firing` (a firing is in
  flight), `exhausted` (a guardrail stopped it; see "Guardrails"), `deleted` (terminal).
- **Limits.** Maximum firings, minimum interval between firings, and a budget. A loop owned by a
  goal inherits the goal's own quota/wall-clock limits as an upper bound in addition to its own; a
  loop on a plain task sets its own.

## Triggers (Proposed)

**Event triggers**, from sources Mainloop already observes — a loop adds no new observation, only a
new reader of an existing one:

- a check run finished, from the existing task-projection CI observation sweep (see
  [tasks](tasks.md), "Go status (G8b)" and [pull-requests](pull-requests.md));
- a pull request changed state or merged, from the same task-projection sweep's PR observation;
- a task or attempt changed state, from the existing `task:updated` event (see [events](events.md));
- a delivery stalled, from the existing stalled-turn signal (see
  [orchestration-health](orchestration-health.md), "Stalled turns");
- an attention item opened, from the existing `attention_items` / `inbox:updated` projection (see
  [inbox](inbox.md)).

A deploy or production health-check change, named in the issue as a future source, is not yet
observed by Mainloop at all (see [devloop](devloop.md), which only reaches a shared dev staging
target). A loop cannot fire on it until that observation exists; see [goals](goals.md)'s first-slice
proposal and "Open questions" below.

**Time triggers**: fire after a fixed delay, or on "no change for N minutes" — a stall-style
trigger in the same spirit as task liveness's own stall clock
(`progress_marker` / `progress_at`, see [tasks](tasks.md), "Liveness bounds"), but scoped to
whatever the loop's own owner considers a change: a task's version/report, or a goal's progress
rollup.

## One firing (Proposed)

A firing delivers exactly one bounded message to the owning supervisor's native session: what
fired (the trigger's kind and its minimal identifying facts — a check run's name and conclusion, a
PR's number and new state, a task's id and new status) plus the current state, read fresh at firing
time rather than cached when the loop was armed. The supervisor acts on it, then re-arms a next
trigger or lets the loop end, the same way a task already ends a turn after reporting rather than
polling for its next instruction. A firing is never a replay: it carries no prior turn's history,
the same way a task notification today carries one new fact rather than a transcript (see
[tasks](tasks.md), "Notifications and recovery").

## Durability under the reconciler rule (Proposed)

A loop firing follows [AGENTS.md](../../AGENTS.md)'s reconciler rule exactly: a durable intent row
per loop, in an explicit state (`pending`, `delivering`, `delivered`, `failed`), with an idempotency
key per firing — the loop's id plus the trigger occurrence's own identity (the event's id for an
event trigger, the scheduled fire time for a time trigger). The intent row commits before any
native send is attempted, the same ordering [tasks](tasks.md) already uses for a report's delivery
intent. An uncertain firing is reconciled by checking the existing delivery ledger (the same way a
task report's delivery is checked, see [tasks](tasks.md), "Go status (G8b)") before any retry, never
by resending blindly. A firing must never happen twice: once its intent row reaches `delivered`, no
later pass can re-fire the same occurrence key.

## Busy or suspended supervisor (Proposed)

**Recommended: coalesce.** Queuing risks piling up several "what fired" messages that each claimed
to be the current state at the time they were queued, which stops being true the moment a later one
supersedes it — exactly what "never a replay" rules out. Skipping loses a real signal outright,
which defeats the point of a loop that exists so the owner does not have to notice something
happened. Coalescing keeps the single-bounded-message property: while a turn is already in flight,
or the session is suspended, further firings for the same loop update one pending firing in place
rather than queuing a second message, and the pending firing is delivered once, carrying the latest
state, as soon as the session is ready. This reuses the exact mechanism tasks already use for a busy
session: a report delivered to a session with one open turn is admitted at once as a queued
sessions-ledger delivery behind whatever else is already queued there (see [tasks](tasks.md), "Go
status (G8b)"), and a HITL response wakes a suspended session through the same
`sessions.Service.ReadySession` path before sending (see [inbox](inbox.md)). A loop needs no new
delivery primitive, only a new source feeding the existing one, collapsed to at most one pending
message per loop.

## Guardrails (Proposed)

- **Max firings.** A lifetime count per loop; reaching it moves the loop to `exhausted`.
- **Minimum interval.** A debounce so a burst of the same event (several check runs finishing
  within seconds) produces one firing, not several, by folding firings that would otherwise occur
  inside the interval into the same coalesced pending firing described above.
- **Budget.** Inherited from the owning goal's own quota limit when the owner is a goal; a
  standalone budget on a loop that owns a plain task.
- **No-progress stop.** If a run of consecutive firings produces no change in the owner's observed
  state (the same fingerprint idea task liveness already uses to detect a wedged turn, see
  [tasks](tasks.md), "Liveness bounds"), the loop stops itself (`exhausted`) and raises an attention
  item — the same `owner_decision` item [goals](goals.md) raises when a goal's repairs stop
  converging, reused here for a loop on a plain task too, since the condition is identical: nothing
  automatic is left to try.

## Owner controls (Proposed)

- **Pause.** Moves the loop to `paused`; no firing is evaluated or delivered while paused. A
  firing already in flight when pause is requested still settles.
- **Fire now.** A manual firing, taking the same durable intent/idempotency path as any other
  firing (a synthetic occurrence key), bypassing the minimum interval once but still counting
  against max firings.
- **Delete.** Terminal (`deleted`); no further firing is evaluated. An in-flight firing still
  settles through the reconciler; it is simply never retried or re-armed afterward.

## Agents end turns instead of polling (Proposed)

A native turn that would otherwise sleep or poll for a condition should instead end its turn — after
reporting progress or taking no action — and arm the loop that will wake it when the condition is
worth looking at again, rather than looping inside the sandbox. This is the same discipline the
shared agent guidance already gives every task today: committed work and `/data` survive a pause,
but other writes, `/tmp` and processes may be reset (see [delegation](delegation.md), "Shared agent
guidance"). An ended turn with an armed loop survives a workspace suspend; a sleeping turn does not.

## Relation to scheduled runs (Proposed)

[Scheduled runs](scheduled-runs.md) are cron schedules kagent owns and fires: each execution starts
a **new** kagent Session, with its own conversation identity, turned into a new Mainloop session (or
one inbox item on failure). A loop does the opposite: it wakes an **existing** supervisor's current
native session and attempt, with the same identity, history and tools it already has. A loop never
creates a kagent Session, never calls `CreateScheduledRun`, and is not a row in the schedules
registry.

What a loop does reuse from the existing machinery:

- the sessions-ledger delivery and wake path (`sessions.Service.ReadySession`, queued admission
  behind an open turn) that reports and HITL responses already use to reach a busy or suspended
  session (see [tasks](tasks.md) and [inbox](inbox.md));
- the `server/internal/reconcile` intent-row/lease/idempotency framework already used by the HITL
  dispatcher and the event retention sweep (see [events](events.md), "Background work");
- the existing event sources named under "Triggers" above — no new observation code, only a new
  reader.

What it does not reuse: kagent's cron registry, its own idempotency key shape, or its
adoption/executions-cursor machinery (see [scheduled-runs](scheduled-runs.md)) — none of that is
about waking a session that already exists.

## Proposed API shape in prose

- **`Loop`** fields: `id`; `owner_type` (`goal` or `task`); `owner_task_id`; `trigger`, a typed
  union — `{kind: event, source, filter}` (naming one of the event sources above and any narrowing
  filter, such as one PR number), `{kind: time, after_seconds}`, or `{kind: stall,
no_change_minutes}`; `state` (`armed`, `paused`, `firing`, `exhausted`, `deleted`); `limits`
  (`max_firings`, `min_interval_seconds`, `budget`); `next_fire_at` (time triggers) or
  `watched_event` (event triggers); and a bounded list of recent firings, each with its trigger
  facts, delivered-at time, and one-line outcome.
- **`POST /tasks/{id}/loops`** (or `/goals/{id}/loops`): creates a loop on that owner, idempotent by
  `request_id` the same way task creation already is.
- **`GET /tasks/{id}/loops`** / **`GET /goals/{id}/loops`**: the owner's armed, paused and recently
  exhausted loops.
- **`GET /loops/{id}`**: one loop's full detail and firing history.
- **`POST /loops/{id}/pause`**, **`/resume`**, **`/fire-now`**, **`DELETE /loops/{id}`**: owner
  actions, each carrying the loop's expected version the way a task action already carries the
  task's (see [tasks](tasks.md), "Admission and integration ports").

The OpenAPI change implementing this shape is a later PR; this page fixes the shape in prose first.

## Open questions

- Which event source comes first to build: check runs, PR state and task state are already
  observed and are the cheapest to wire; delivery-stall and attention-opened need their existing
  sweeps to be read by a new consumer; deploy/production health has no observation at all yet (see
  [goals](goals.md), "Open questions").
- Can a loop fire on a plain task with no goal at all, with the same guardrails as a goal-owned
  loop, or does a standalone loop need its own, simpler guardrail set?
- Is coalescing into one pending firing always the right default, or should some event sources (for
  example, a merge outcome) always get their own firing even while one is already pending?
- Does a coalesced or skipped occurrence count against `max_firings`, or only a firing that actually
  reaches the session?
- Usage accounting per firing (issue #314's own open question, #72): how is a firing's own token/compute
  cost attributed, given the same usage-accounting gaps [orchestration-health](orchestration-health.md)
  already documents for a turn?
- Does the event stream need a new `loop:updated` frame (see [events](events.md)), or can a loop's
  state changes ride the existing `task:updated` / `schedule:updated` frames since a loop is always
  read through its owning task or goal?
- Can the owning supervisor itself arm or re-arm a loop through an MCP tool, or is arming an
  owner-only REST action for the first slice?
