# Loops

A loop is a durable wake-up that main or a supervisor arms instead of polling: when its trigger
fires, Mainloop sends one message to the session that armed it, so that session can end its turn
and still hear when something is worth looking at. Issue #314.

Status: **implemented** for task, pull request and time triggers, armed and managed by agents
through MCP. The quiet trigger with its no-progress stop, and the owner's view and controls, are
**proposed** (see [Not built](#not-built)).

## Why

Supervisors and main otherwise babysit work by hand: waiting for a child to finish, re-checking CI,
looking at production again later. A turn that sleeps or polls for that holds a session, and its
`/tmp` and processes do not survive a workspace suspend (see [delegation](delegation.md), "Shared
agent guidance"). An ended turn with an armed loop does: the loop is a row, and its firing is a
message through the same sessions ledger that delivers child reports.

## Who arms a loop, and who it wakes

- **Callers.** Main and supervisors, through the MCP tools `loop_arm`, `loop_list` and `loop_stop`.
  Children do not arm loops (they have `watch_checks` for their own pull request), and a completed
  task's attempt cannot. Refused: 403 `task_management_denied`.
- **Recipient.** The loop belongs to its recipient, the session that armed it, named by the same key
  report deliveries use: `main`, or `parent-task:<task id>` for a supervisor. A supervisor's loops
  follow its task across a [retry or reassign](task-handoff.md), and end when the task ends.
- **Subject.** A task or pull request trigger watches a task the caller manages: any of the owner's
  tasks for main, a supervisor's own children. Another task is 403 `loop_subject_denied`; one that
  does not exist, 404 `task_not_found`; a pull request trigger on a coordination task, 422
  `loop_requires_code_task`. A time trigger may name a subject too, and its messages then say how
  that task stands.
- **Idempotency.** The caller's `request_id` (per binding) returns the loop it armed; the same id
  with a different request is 409 `request_payload_conflict`.

No [goal](goals.md) is needed: a loop on a plain task is the first slice.

## Triggers

- **`task`.** Fires when the subject's status or reason changes. With `statuses`, only when the
  change lands on one of them (for example `completed`, `failed`, `blocked`). A loop armed on a
  status the subject is already in fires at once, so arming just after the change still hears of
  it. Without `statuses` the loop starts from the subject as it is and fires on the next change.
- **`pull_request`.** Fires when what the [task projection](tasks.md) saw of a code task's pull
  request changes: a pull request appears, its number or state (`open`, `closed`, `merged`)
  changes, or CI's result or the commit it is for changes. With `pr_conditions`, only when one of
  them holds after the change: `ci_success` and `ci_failure` (CI's result for the pull request's
  head commit, so a result for an older commit never counts), `merged`, `closed`. As for `task`, a
  condition that already holds when the loop is armed fires at once. The message says how the pull
  request stands, for example `pull request #9 is open, CI success at the head <sha>`. It reads the
  projection the task projection sweep keeps fresh, so a loop adds no GitHub reads and fires within a
  sweep of the change; a pull request that is not a task's stays with [`watch_checks`](ci.md).
- **`time`.** Fires after `after_minutes`, then every `every_minutes` when given; with only
  `every_minutes`, the first firing is one interval away. A one-time timer is `done` once it fires.
  A timer that was asleep (the server down) fires once on waking, not once per missed interval.

**How a trigger is watched.** A loop compares stored state, not events, so nothing is lost when a
notification is missed or the event log is pruned. A task loop keeps a baseline, the subject's
`status` or `status/reason`; a pull request loop, the pull request's number, state, CI result and
CI commit. Either looks again on every such change of its subject (a database trigger makes the
subject's loops due and notifies `mainloop_task_loops`) and at least every 15 seconds otherwise. The evaluation holds the loop row while it reads, and the subject's change waits
on that row, so a change is either seen by the evaluation or makes the next one due. Time loops are
due at their next fire time. Every armed loop is looked at again at least every five minutes, which
is when an expiry or an ended recipient is noticed.

## One firing is one message

A firing is a `task_loop_firings` row with its own `task_event_deliveries` row, delivered by the
report delivery loop exactly as a child's report is (see [delegation](delegation.md), "Notifications"):

- It waits while the recipient has an open turn and for the report batch window, then every report
  and firing pending for that recipient goes out as **one** message, in one turn. A message that
  carries both starts `[N updates]`. A firing is never steered into a running turn. A suspended
  session is reached through the sessions ledger like any report.
- It also waits for the loop's minimum interval after the firing before it.
- **Coalescing.** A loop has at most one firing waiting to be sent. Further occurrences fold into
  it: its facts are kept in order, at most 10, with the latest always last and a count of the ones
  folded in beyond that. Once the firing has a message id its facts are frozen, so a resend after a
  crash is the same text; a later occurrence starts the next firing.
- The message names the loop, its firing number and limit, the caller's own `note` ("You asked:
  …"), and the facts, for example `task <id> ("<title>") is blocked/owner_decision, was running`. It
  says so when it is the loop's last firing or a one-time timer's.
- Delivery follows the report rules: idempotent message ids derived from the delivery, the ledger
  read before any resend, up to three sends. A firing the ledger could not deliver ends
  `uncertain`; there is no task to block for it. A firing to a recipient whose task ended is
  cancelled.

## States and limits

`armed` is the only live state. The final states: `done` (a one-time timer fired), `exhausted` (its
last firing made), `expired`, `stopped` (by `loop_stop`) and `ended` (its supervisor's task ended).
A firing already made when a loop stops or ends is still delivered, unless its recipient ended.

| Limit                  | Default | Range                    |
| ---------------------- | ------- | ------------------------ |
| `max_firings`          | 20      | 1 to 100                 |
| `min_interval_seconds` | 60      | 0 to 86400               |
| `expires_minutes`      | 1440    | 1 to 10080               |
| armed loops per caller | 20      | 409 `loop_limit` past it |

Each firing counts once against `max_firings`; occurrences folded into a firing do not count. The
loop becomes `exhausted` when it makes its last firing.

**Liveness.** A supervisor with an armed loop, or a firing to it on its way, ended its turn to wait:
the liveness sweep does not block it as `stalled` (as for [`watch_checks`](ci.md)). The loop's
expiry bounds that wait, and the runtime bound still holds.

## Usage

Mainloop does not record turn usage yet ([orchestration-health](orchestration-health.md)), so no
token budget is enforced; a loop's cost is bounded by its firings, minimum interval and expiry. Each
firing's delivery row names the ledger message that opened its turn, so once usage is recorded per
turn (#72), a loop's usage is the sum over its firings' turns, labelled _attributed_.

## Relation to other wake-ups

- **`watch_checks`** ([ci](ci.md)) stays as it is: a worker's own one-shot wake on live CI and
  reviews for its pull request. Loops follow its pattern (an intent row on `internal/reconcile`, a
  message id that dedups a resend) and its liveness exemption.
- **Holds** ([tasks](tasks.md#holds)) are main's way to set a task aside from the owner's views
  until a time; a hold's end message is not a loop.
- **[Scheduled runs](scheduled-runs.md)** are kagent cron schedules that each start a **new**
  session. A loop wakes the **existing** session that armed it, with its identity and history, and
  never creates a kagent Session.

## Not built

- **`quiet` trigger and the no-progress stop** (proposed): fire when the subject has not changed for
  N minutes. Arming takes `expect_change` (default false); only a loop armed with it, a repair-style
  loop, stops as `exhausted` after three delivered firings in a row with an unchanged subject, and
  raises an `owner_decision` attention item. A plain periodic check-in, such as "check prod every
  30 minutes", is never stopped for seeing no change.
- **Owner view and controls** (proposed): the task page lists a task's loops, armed ones first, with
  the next fire time or what each waits on and its last firings, and offers pause, resume, fire now
  and delete through REST routes. A paused loop is not evaluated; fire now takes the same firing path
  once, ignoring the minimum interval and counting against `max_firings`. A `loop:updated` event
  frame lands with that view if the task page needs one.
- **Other sources**: delivery stalls, attention items, and deploy or production health (which
  Mainloop does not observe yet).
- **Goals** ([goals](goals.md)): a goal's supervisor arms loops like any supervisor; a goal-level
  budget is not built.
