# Loops

A loop is a durable wake-up that main or a supervisor arms instead of polling: when its trigger
fires, Mainloop sends one message to the session that armed it, so that session can end its turn
and still hear when something is worth looking at. Issue #314.

Status: **implemented** for task, pull request, quiet and time triggers and the no-progress stop,
armed and managed by agents through MCP, and for the owner's REST routes to list and control a
task's loops. The task page section that uses those routes is **proposed** (see
[Not built](#not-built)).

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
- **`quiet`.** Fires when the subject has gone `quiet_minutes` without changing, then again after
  each further quiet period while it stays unchanged. A change is a new version of the task (its
  status, a report, an operation, its pull request) or new activity in its current attempt's turn,
  as the [liveness sweep](tasks.md) records it. A change starts the period again. The message says
  how long the task has been unchanged and how it stands. A task that finishes ends the loop.
- **`time`.** Fires after `after_minutes`, then every `every_minutes` when given; with only
  `every_minutes`, the first firing is one interval away. A one-time timer is `done` once it fires.
  A timer that was asleep (the server down) fires once on waking, not once per missed interval.
  With `task_id`, each message says how that task stands.

**How a trigger is watched.** A loop compares stored state, not events, so nothing is lost when a
notification is missed or the event log is pruned. A task loop keeps a baseline, the subject's
`status` or `status/reason`; a pull request loop, the pull request's number, state, CI result and
CI commit. Either looks again on every such change of its subject (a database trigger makes the
subject's loops due and notifies `mainloop_task_loops`) and at least every 15 seconds otherwise. A
quiet loop keeps the subject's version and its attempt's activity marker, with when it last saw
them change, and looks every 15 seconds. The evaluation holds the loop row while it reads, and the
subject's change waits on that row, so a change is either seen by the evaluation or makes the next
one due. Time loops are
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

## The no-progress stop

A loop can wait for progress that may never come: a supervisor checking every 30 minutes whether a
child's repair landed, or a quiet loop on a child that has stopped moving. Arming with
`expect_change: true` says the loop expects its subject to change between firings. Each firing
compares the subject (its version and activity marker, as for `quiet`) with what the previous firing
found, or the arm for the first. After **three firings in a row** that find it unchanged, the third
firing's message says the loop stopped, and the loop ends `exhausted` with `stop_reason =
no_progress`. The attention sweep then asks the owner to decide about the task (see
[inbox](inbox.md)). Any change between firings resets the count. An occurrence folded into a waiting
firing is not a firing and does not count.

`expect_change` defaults to false, and only a `quiet` loop or a `time` loop with `task_id` takes
it (otherwise 422 `invalid_request`). A plain periodic check-in, such as "check prod every 30
minutes", leaves it false and is never stopped for seeing no change; `max_firings` and the expiry
still bound it.

## The owner's view and controls

Main and supervisors arm loops; the owner sees and steers them from the task they concern.

- **`GET /tasks/{task_id}/loops`** lists the loops watching the task and the loops its supervisor
  armed, live ones first, then the latest that ended (at most 50). Each carries its trigger and
  settings, who it wakes (`main`, or `supervisor` with the supervisor's task), its firing count and
  limit, its next fire time (a live time loop's timer, or when a live quiet loop fires if its task
  stays unchanged; null for a loop that waits on a change), its `stop_reason`, and its latest five
  firings with their facts and delivery state.
- **`POST /loops/{loop_id}/pause`** pauses an armed loop: its trigger is not evaluated until it is
  resumed. Its expiry and an ended recipient still end it, and a supervisor waiting on a paused loop
  is still waiting, not stalled.
- **`POST /loops/{loop_id}/resume`** resumes a paused loop, which looks at once: a change while it
  was paused fires as a task or pull request loop would, a timer that passed fires once, and a
  quiet loop's period starts again.
- **`POST /loops/{loop_id}/fire`** (`request_id`) fires a live loop now with the fact `fired now by
the owner …`: one firing, without waiting for the minimum interval, counting against
  `max_firings`, or folded into its firing still waiting to be sent. The same `request_id` again
  fires nothing more.
- **`DELETE /loops/{loop_id}`** stops a live loop for good. A firing it already made is still
  delivered, and the loop stays in the task's history as `stopped`.

A control the loop's state does not allow (pausing a loop that is not armed, resuming one that is
not paused, firing or deleting one that ended) is 409 `loop_state_conflict`; another owner's or an
unknown loop is 404 `loop_not_found`.

## States and limits

`armed` and `paused` (by the owner) are the live states. The final states: `done` (a one-time timer fired), `exhausted` (its
last firing made, or stopped for no progress), `expired`, `stopped` (by `loop_stop`) and `ended`
(its supervisor's task ended, or a quiet loop's task finished).
A firing already made when a loop stops or ends is still delivered, unless its recipient ended.

| Limit                  | Default | Range                    |
| ---------------------- | ------- | ------------------------ |
| `max_firings`          | 20      | 1 to 100                 |
| `min_interval_seconds` | 60      | 0 to 86400               |
| `expires_minutes`      | 1440    | 1 to 10080               |
| live loops per caller  | 20      | 409 `loop_limit` past it |

Each firing counts once against `max_firings`; occurrences folded into a firing do not count. The
loop becomes `exhausted` when it makes its last firing.

**Liveness.** A supervisor with a live loop (armed or paused), or a firing to it on its way, ended its turn to wait:
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

- **The task page section** (proposed, next): the task page lists the task's loops with the routes
  above, with pause, resume, fire now and delete. It refetches on a timer and after each control;
  no `loop:updated` event frame is planned unless the page needs one.
- **Main's loops without a task** (a time loop main armed with no `task_id`) appear on no task
  page; `loop_list` is their only listing.
- **Other sources**: delivery stalls, attention items, and deploy or production health (which
  Mainloop does not observe yet).
- **Goals** ([goals](goals.md)): a goal's supervisor arms loops like any supervisor; a goal-level
  budget is not built.
