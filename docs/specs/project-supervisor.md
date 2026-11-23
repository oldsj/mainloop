# Project supervisor

A project has one **standing supervisor**: the agent that takes the project's work from main and
the owner, hands it to workers and reviewers, and reports back to main. It is the middle of three
layers: main (one per owner), a standing supervisor per project, and its workers and reviewers,
which cannot delegate. The design, and the slices still to come, are in
[docs/design/project-supervisor.md](../design/project-supervisor.md).

Status: **identity, rest, liveness, main's routing, restart and recovery implemented** (server,
contract and MCP tools), not yet proven live. Not built yet: owner tasks under it, per-project
model settings, and retiring it.

## What it is

- A **task**: a root coordination task of the project with `standing: true`. It has what any
  supervisor task has: children whose reports route to it, the task tools over them, loops,
  transcripts, a task page and a session page. Its title is `Supervisor of <owner/repo>`.
- **One per project, until the owner retires it** ([below](#retire)). The database refuses a second
  standing task for a project while one has not ended (a partial unique index), and a standing task
  that is not a root coordination task of a project.
- **Created on first use.** `POST /projects/{project_id}/supervisor` answers the project's standing
  supervisor as a `TaskView`, creating it the first time: a create operation provisions its first
  attempt as for any task. Concurrent first uses make one task. A project of another owner, or none,
  is `404 project_not_found`. It belongs to the project, not to whoever started it: its
  `creator_binding_id` is null.

## At rest

- It never completes. A `completed` report from it means "this batch is done": the task goes to
  `waiting` with reason **`idle`**, its attempt stays active, nothing is settled, and main hears
  the report, as it hears any root task's terminal report. Its children keep running. A later
  `progress` report, or a follow-up, moves it back to `running`.
- The liveness sweep keeps its state true to its session. Running, with no turn open, nothing waiting
  on the owner and no message on its way to its session, it moves to `waiting`/`idle` instead of
  being blocked `stalled`; at rest, once its session has a turn again (a child's report, a loop
  firing, a message from main or the owner), it moves back to `running`, within a sweep
  (the sweep runs every minute). Each move publishes `task:updated`.
- A turn that is open but records nothing for `TASK_STALL_TIMEOUT` is still `stalled`. A session that
  is gone (not one kagent will revert on the next send) is recovered, at rest or not (see Recovery).
- `TASK_MAX_RUNTIME` bounds a **wake**, not its life: every move from `waiting`/`idle` to `running`,
  whatever makes it, restarts the bound on its current attempt (a database trigger).
- A `failed` report from it is refused, `422 standing_outcome`: it reports `blocked` instead.
- Only a standing task can be `waiting`/`idle` (a database check).
- Cancel and reassign are refused, `409 standing_supervisor`, for the owner, main and any other
  caller, and its view says so: `actions.cancel` and `reassign` are unavailable with reason
  `standing_supervisor`. `retry` is a restart (below). `follow_up` is available as on any open task.
  The owner ends it with a retire ([below](#retire)).

## How main reaches it

- `supervisor_send(project_id, request_id, text)`, an MCP tool only main has. It starts the
  project's standing supervisor if it has none (as `POST /projects/{id}/supervisor` does) and sends
  `text` to its session as `follow_up` does: labelled `[message from main]`, waking it from rest,
  queued behind a turn that is open. The result names its task, attempt and session and says
  `sent` or `queued`. The message id derives from main and `request_id`, so a retry never sends
  twice.
- A supervisor that is starting is waited on, up to 30 seconds, until its first brief is recorded,
  so main's message never runs before the brief. Past that, the call is refused
  `409 supervisor_starting` and main sends again under the same `request_id`.
- Main's `delegate` with a `project_id` is refused `409 project_supervisor` (see
  [delegation](delegation.md)); without one it still makes a root coordination task.
- The line naming main's role in its first message says this (guidance v7): a project's work goes to
  its supervisor with `supervisor_send`, `delegate` is for work outside any project. Main's existing
  sessions get it with their next message.
- Main uses `follow_up`, `steer`, `task_get` and `task_transcript` on the supervisor's task id as on
  any task, and `task_list` shows it with `standing: true`.

## The owner's project tasks

<a id="owner-tasks"></a>The owner's `POST /tasks` with a `project_id` (both modes) makes a child of
the project's standing supervisor, not a root task:

- The supervisor is started if the project has none, in the same transaction, as a first use does.
  A project of another owner, or none, is `404 project_not_found`, and nothing is started.
- The task is the supervisor's child (`parent_task_id` and `root_task_id` are the supervisor's;
  `creator_binding_id` is null). It counts against the project's worker limit and the global limit
  as the supervisor's own workers do. Over either, the create is refused
  (`409 project_capacity` or `global_capacity`) and the supervisor is not told.
- The supervisor is told in a message:
  `[the owner started child task <id> under you]`, the task's title, mode and `/tasks/<id>`, and
  that it reports to the supervisor and is managed as one it delegated. The message goes through
  the same outbox as its children's reports: batched with them, sent when its session has no turn
  open, and after its first brief when the create started it. A retry of the same `request_id` makes
  no second task and no second message.
- Its reports go to the supervisor, as any child's do. The owner sees and manages it as any task.
- A task with no `project_id` is a root task, as before. Main's `delegate` with a `project_id` is still
  refused (above).

## How many workers it runs

Its workers are bounded by the project's [worker limit](tasks.md#project-worker-limit)
(`max_workers`, default 6), not by the per-parent limit, and by the owner-wide global limit. A
delegate past it is refused `409 project_capacity`; the supervisor waits for a worker to end. The
owner sets the limit on the project (`PUT /projects/{id}/worker-limit`), or main does for the owner
with `project_settings`. The supervisor cannot change it.

## Restart and reset

A restart replaces its attempt with a new one, on the same task, in a new native session. It runs
the [handoff](task-handoff.md) operation (the old attempt is drained, its stop confirmed and fenced
before the successor exists, so there is never a second writer), with these differences:

- **Its workers are kept.** Its open children are not cancelled and keep their slots. A child's own
  calls (report, task reads) keep working while the attempt is replaced: for a standing parent, a
  child's authority asks that the parent task is open, not that its attempt is active. A child's
  report or a loop firing made meanwhile waits and is delivered to the successor.
- **What the successor is told** (its brief's Continuation, read from the database, no model call):
  that it was restarted and is still the project's standing supervisor; its last report (any
  attempt's, at most 4000 characters); its open workers, each with its link, status, pull request,
  CI and last report line; its armed loops; and the newest three messages its old session received
  after that report (at most 1000 characters each), so a message that woke a dying session is not
  lost.
- **Reset** (`fresh: true`) is a restart whose successor is not given the last report or those
  messages: it starts from the facts (workers, loops) only.
- **Limits.** No `TASK_MAX_HANDOFFS` cap. Main may restart it three times in a rolling hour; the
  fourth is `409 handoff_limit` (detail `handoffs`, `limit`, `window`). The owner is not limited.

## Retire

<a id="retire"></a>`POST /projects/{project_id}/supervisor/retire` (`{request_id}`) ends the project's
standing supervisor, for the owner only: there is no MCP tool for it, so main and supervisors cannot.
It is for a project that is done with (archived, or a dev project being cleaned up).

- Its open children are cancelled at once, in the same transaction, as a regular supervisor's are when
  it stops. Their attempts are stopped by the settle sweep.
- The answer is `202` with a `cancel` task operation. The supervisor is then cancelled as any task is:
  its session is stopped, and the task becomes `cancelled` once the stop is confirmed. A child created
  meanwhile is cancelled with it.
- A repeat of the same `request_id` answers the same operation. With no standing supervisor that
  has not ended, the answer is `404 task_not_found`; a project of another owner, or none, is
  `404 project_not_found`.
- Afterwards the project has no supervisor: `Project.supervisor` is null, and restart and retire are
  `404 task_not_found`. Its next first use (the owner's `POST /projects/{project_id}/supervisor`, or
  main's `supervisor_send`) starts a new standing supervisor with a new task, no continuation and no
  children. The retired task stays, `cancelled`, with its history.

## Recovery

When its native session is gone (kagent has no such Session, or it is terminal and not one kagent
reverts on the next send), Mainloop restarts it, as a restart above, instead of blocking it
`session_lost`:

- The liveness sweep starts it within a minute, running or at rest. A `supervisor_send` that finds
  the session gone starts it too and answers `409 supervisor_starting`: main sends again under the
  same `request_id`, and the message goes to the new session once it has its brief (a send while a
  restart is under way waits for it, up to the start wait).
- The recovery is keyed by the gone attempt (request id `standing-recover:<attempt id>`), so the
  sweep and a send that both notice start one operation, and one that finds a restart already under
  way joins it.
- It counts against the same three restarts per rolling hour as main's (every restart in the hour
  counts, the owner's too). Past that, it is not restarted: it is blocked `session_lost` and the
  owner restarts it.
- `follow_up` to it with its session gone is still refused `409 session_gone`, as for any task; use
  `supervisor_send` or a restart.

How to ask for a restart:

- The owner: `POST /projects/{project_id}/supervisor/restart` with `{request_id, fresh}` answers the
  handoff's `TaskOperation` (202). A retry of the same request id and body answers the same
  operation, even after the restart moved the task. No standing supervisor yet is
  `404 task_not_found`; a foreign or missing project `404 project_not_found`; one already in a
  handoff `409 operation_pending`. `POST /tasks/{id}/retry` on its task is a restart too.
- Main: `task_retry` on its task id.

## Its brief

Every attempt's first message is a coordination task's brief, with a **Standing supervisor**
section after the stored brief: it is the project's standing supervisor and the middle of three
layers; it never completes, reports `completed` and ends its turn when a batch is done, and reports
`blocked`, never `failed`; it hands code to workers with `delegate` and arms a loop or ends its turn
rather than polling. The section is rendered, not stored, so a change of wording reaches the next
attempt.

## API

- `Task.standing` (boolean, on every task) marks it; the web keys the space's supervisor tab on it.
- `Project.supervisor` on `GET /projects`, `GET /projects/{id}` and the project detail: null until
  first use, then `{task_id, session_id, status, reason, started_at}`. `session_id` is the session
  of the task's current active attempt, null while there is none (before its first attempt is
  made).
- `POST /projects/{project_id}/supervisor`: see above.
- `POST /projects/{project_id}/supervisor/retire`: see [Retire](#retire).
- Task reason `idle` joins the `TaskReason` enum.
- Owner questions from it (`report` `blocked` with `owner_action`) reach Needs you as any task's do.
