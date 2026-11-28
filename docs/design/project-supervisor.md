# Design: a standing supervisor per project

Status: **proposed**. Nothing here is built yet; each section names the slice that builds it.
Current behaviour is in [delegation](../specs/delegation.md), [tasks](../specs/tasks.md),
[task handoff](../specs/task-handoff.md) and [loops](../specs/loops.md).

## Why

In the owner's herdr workflow each space (project) has one supervisor that spawns workers, steers
them and reads their output, and one orchestrator sits above every space. Mainloop has the top
(main) and the bottom (code tasks), but its middle is a coordination task made per job: a project
has none, one or three, each ending with its job. The web UI is being rebuilt so that a space is a
project whose first tab is its supervisor ([herdr layout](../specs/layout.md)), which needs one
supervisor a project can be addressed by.

Three layers, at most, enforced by the server:

| Layer | Who                                                                                                  | May create                                                                              | Reports to       |
| ----- | ---------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------- | ---------------- |
| 1     | **main**, one per owner (depth 0)                                                                    | project supervisors (by sending them work); ad-hoc supervisors for work with no project | the owner        |
| 2     | **supervisors** (depth 1): one **standing supervisor** per project, plus ad-hoc ones with no project | workers and reviewers, its own direct children                                          | main             |
| 3     | **workers and reviewers** (depth 2): code tasks, review tasks                                        | nothing                                                                                 | their supervisor |

Depths 0–2 and "a child cannot delegate" exist today. What is new is that a project has exactly
one layer-2 agent and that main no longer puts work into a project except through it.

## Identity

- The standing supervisor is a **task**: a root coordination task with `standing = true` and a
  `project_id`. Being a task gives it, unchanged, everything a supervisor already has: children
  whose reports route to it, `steer`/`follow_up`/`task_cancel`/`task_retry` over them, loops,
  transcripts, liveness, a task page and a session page.
- **One per project, until the owner retires it.** A partial unique index on
  `tasks (project_id) WHERE standing` and not ended makes a second one impossible
  (`POST /projects/{id}/supervisor/retire` cancels it with its children; the next first use starts a
  new one). The task id never changes; a
  restart or a crash replaces its _attempt_, not the task.
- **Created on first use**, not at registration (most registered repositories never need an idle
  session): the first `supervisor_send` from main, the owner's `POST /projects/{id}/supervisor`
  (the UI's "start" on a space's supervisor tab), or the first owner-created task in the project.
  Creation is an upsert on the index, so concurrent first uses make one task.
- **How main reaches it:** a new main-only MCP tool,
  `supervisor_send(project_id, request_id, text)`. It ensures the supervisor exists and sends
  `text` to its session like `follow_up` does (`[message from main]`, message id from the caller
  and `request_id`, so a retry never sends twice), waking it. The result names the supervisor's
  task and session and says `sent` or `queued`. Main can also use `steer`, `follow_up`,
  `task_get`, `task_transcript` on it by task id, as on any task.
- **Main's `delegate` with a `project_id` is refused** (409 `project_supervisor`, "send the work
  to the project's supervisor with supervisor_send"), for both modes. The refusal comes after the
  idempotency check, so a replay of a request made before the change still returns its stored
  operation. Main's `delegate` without a project still creates an ad-hoc root coordination task
  (the [ops agent](../specs/ops-agent.md), research), which stays layer 2: a code task needs a
  project, so its children can only be coordination tasks, and those cannot delegate.
- **The owner's own project tasks** (`POST /tasks` with a `project_id`) become children of the
  project's standing supervisor (created if needed), which is told
  `[the owner started <task link> under you]`. They count against its caps like any child; a
  refusal lists the held children as `parent_capacity` does today.

## Lifecycle

- **Never completes.** It is `running` while a turn is open, and otherwise `waiting` with a new
  reason, **`idle`**. Its `report`:
  - `progress`: recorded on the task, as today.
  - `completed`: means "this batch is done". The task goes to `waiting`/`idle`, the attempt stays
    active, nothing is settled or drained. The report goes to main, as a root task's terminal
    report does today.
  - `blocked`: as today (to main and the owner; `owner_action` makes it a Needs-you decision).
  - `failed`: refused (422 `standing_outcome`): a standing supervisor reports `blocked` instead.
- **Sleeps when idle.** Ending a turn needs nothing from Mainloop: kagent parks an idle Session's
  Actor and wakes it on the next send ([workspaces](../specs/workspaces.md)). The liveness sweep
  moves a standing supervisor with no turn running (no kagent task submitted or working, nothing
  queued for it) to `waiting`/`idle` instead of `stalled`; `stalled` applies to it only while a
  turn is open and records nothing new for `TASK_STALL_TIMEOUT` (a wedged turn).
- **Wakes** on a message: main's `supervisor_send`/`follow_up`/`steer`, the owner's composer or
  follow-up, a child's report, a loop firing, a check watch. Each is an existing delivery to its
  session; the delivery that starts a turn moves the task to `running` and sets the attempt's
  `reopened_at`, so `TASK_MAX_RUNTIME` bounds a **wake**, not the supervisor's whole life.
- **Cancel is refused** (409 `standing_supervisor`) for main, supervisors and the owner: the
  owner's levers are restart and reset (below), and retire, which cancels it with its children.

## Rolling handoff

A standing supervisor's attempt is replaced, on the same task, in three cases: the owner or main
restarts it, the owner resets it, or its session is gone. Each runs the existing handoff operation
([task handoff](../specs/task-handoff.md)) with two differences:

1. **Its children are kept.** A regular supervisor handoff cancels the supervisor's open children.
   A standing one's children stay running and holding their slots; the successor adopts them
   (children belong to the task, not the attempt). Reports a child makes meanwhile queue for the
   task's current attempt and are delivered to the successor, which the report outbox already
   does for a superseded recipient. A child's own MCP calls keep working: for a standing parent the
   child's authority check asks that the parent _task_ is open, not that its attempt is active.
2. **No `TASK_MAX_HANDOFFS` cap.** Mainloop's own recoveries and main's `task_retry` are limited to
   3 per rolling hour instead, so a crash loop stops: the fourth leaves the task `blocked`/
   `session_lost` for the owner. The owner is never limited.

The successor's **Continuation** carries, without a model call (the old session may be dead):

- the supervisor's last report (outcome and summary, at most 4000 characters). Its brief tells it to
  keep that report a current summary of what it is doing, its decisions and open threads, since it
  is what survives;
- a rendering of its tree from the database: each open child (title link, status/reason, pull
  request, CI, last report line) and its armed loops;
- the messages it received after that report (newest three, 1000 characters each), so a message that
  woke a dying session is not lost;
- project instructions and memory, as every brief carries.

**Restart** (`POST /projects/{id}/supervisor/restart`, `{request_id, fresh: false}`, and main's
`task_retry`) is a retry with that continuation. **Reset** (`fresh: true`) leaves out the last
report and messages and keeps the tree: the new session starts from facts, not from the old one's
account of them. Both are on the supervisor's tab and task page.

## Failure

- **Crashed, recoverable runtime** (kagent's crash recovery, #459 / kagent #113): nothing to do;
  the next send reverts the Session.
- **Session gone** (kagent has no such Session, or it is terminal and not recoverable): the liveness
  sweep, or a send that finds it gone, starts a recovery handoff instead of blocking `session_lost`.
  A message that found it gone is recorded queued on the task and delivered to the successor after
  its continuation, instead of the `session_gone` refusal `follow_up` gives a regular task.
- **Never two writers.** The handoff fences the old attempt (credential revoked, runtime confirmed
  stopped) before the successor exists, in one transaction with the successor's insert. The
  recovery's request id is derived from the gone attempt (`standing-recover:<attempt id>`), so the
  sweep and a send that both notice start one operation; a recovery that finds a handoff already
  pending joins it.

## Workers and caps

The supervisor spawns workers and reviewers with `delegate` (its project is inherited; code tasks
need the project's environment as today), and steers, follows up, cancels, retries and reads them
with the tools it already has. `TASK_MAX_CHILDREN_PER_PARENT` and `TASK_MAX_CHILDREN_GLOBAL` (and
the overflow tier) apply unchanged; supervisors are root tasks, so they hold no worker slot. One
consequence: every code task becomes a child, so the global cap now counts work main used to start
as uncounted root tasks.

## Profile, permissions, model

- It runs as a task supervisor today does: binding role `supervisor`, depth 1, coordination grant
  (no checkout, no repository or merge tools), on its provider profile's supervisor Agent. Its tools
  are a supervisor's (task reads and management over its children, loops, `report`, issue writes,
  `project_context`, `remember`/`forget`), not main's (`task_hold`, topics).
- Provider, model and effort per project: `PUT /projects/{id}/supervisor/settings`
  `{provider_profile_id, model, effort}` (implemented; see the
  [spec](../specs/project-supervisor.md#settings)), validated as `delegate` validates them. Unset, it uses the
  project's default provider and the owner's `supervisor` [session default](../specs/chat.md#session-defaults)
  (else the `worker` one). A model or effort change
  applies to the running session at once (the session's own `/model`/`/effort` override); a
  provider change is a reassign handoff.
- Its brief says what it is: the project's standing supervisor; it never completes; it receives
  work from main and the owner as messages; it hands code to workers and does not write code; it
  ends each turn (arming a loop or `watch_checks` rather than polling) and reports `completed` when
  a batch is done; its last report is its handoff note.

## Existing coordination tasks

No data migration. Open coordination tasks keep running to their end as they are, and main's
existing root code tasks too; in the UI they sit in their project's space after the standing
supervisor. From the main-routing slice on, main cannot make new project-scoped ones. Coordination
children at depth 2 (reviewers) stay as they are: they cannot delegate today.

## API for the UI

- `Project.supervisor`: `null` until first use, then
  `{task_id, session_id, status, reason, started_at}`, where `session_id` is the current attempt's
  session (null while a successor is provisioning). `GET /projects` and `GET /projects/{id}` carry it,
  so the space list needs no extra read.
- `Task.standing: boolean`, so `GET /tasks` lets the agent list put the supervisor first in its space.
- `POST /projects/{id}/supervisor` (ensure; 200 with the `ProjectSupervisor`), `.../restart`,
  `GET`/`PUT /projects/{id}/supervisor/settings`. Messages use the existing session composer and
  `POST /tasks/{id}/follow-up`.
- Status-dot mapping: `waiting`/`idle` is hollow (at rest), `running` amber, `blocked` red.

## Slices

Each is one pull request with its spec under `docs/specs/` and Given/When/Then tests on fakes.

1. **Identity.** Migration (`tasks.standing`, the unique index, reason `idle`), contract
   (`Project.supervisor`, `Task.standing`, ensure endpoint), the standing brief, cancel refused.
2. **Lifecycle.** `completed` → idle, `failed` refused, idle instead of stalled, runtime per wake.
3. **Main routes through it.** `supervisor_send`; main's project `delegate` refused; main's
   guidance and standing context name each project's supervisor.
4. **Rolling handoff.** Children kept, child authority across a rollover, continuation, restart and
   reset endpoints, recovery from a gone session, the hourly limit.
5. **Owner tasks and settings.** Owner project tasks under the supervisor and per-project
   provider/model/effort (implemented).
6. **Proof on mainloop-dev.** A project's supervisor started, sent work, spawning a worker, idling,
   killed and recovered with its child intact; everything labelled `[proof project-supervisor]` and
   cleaned up.

## Not in scope

- Supervisors for more than one project, or a project with two standing supervisors.
- Automatic summarising by a model at handoff time.
- Main delegating to a supervisor of another owner.
