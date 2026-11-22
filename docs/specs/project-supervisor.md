# Project supervisor

A project has one **standing supervisor**: the agent that takes the project's work from main and
the owner, hands it to workers and reviewers, and reports back to main. It is the middle of three
layers: main (one per owner), a standing supervisor per project, and its workers and reviewers,
which cannot delegate. The design, and the slices still to come, are in
[docs/design/project-supervisor.md](../design/project-supervisor.md).

Status: **identity, rest and liveness implemented** (server and contract), not yet proven live.
Not built yet: main routing work through it (`supervisor_send`), restart and recovery that
keep its children, owner tasks under it, and per-project model settings.

## What it is

- A **task**: a root coordination task of the project with `standing: true`. It has what any
  supervisor task has: children whose reports route to it, the task tools over them, loops,
  transcripts, a task page and a session page. Its title is `Supervisor of <owner/repo>`.
- **One per project, for the life of the project.** The database refuses a second standing task
  for a project (a partial unique index), and a standing task that is not a root coordination task
  of a project.
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
- A turn that is open but records nothing for `TASK_STALL_TIMEOUT` is still `stalled`, and a session
  that is gone (not one kagent will revert on the next send) blocks it `session_lost`, at rest or not.
- `TASK_MAX_RUNTIME` bounds a **wake**, not its life: every move from `waiting`/`idle` to `running`,
  whatever makes it, restarts the bound on its current attempt (a database trigger).
- A `failed` report from it is refused, `422 standing_outcome`: it reports `blocked` instead.
- Only a standing task can be `waiting`/`idle` (a database check).
- Cancel, retry and reassign are refused, `409 standing_supervisor`, for the owner, main and any
  other caller, and its view says so: `actions.cancel`, `retry` and `reassign` are unavailable with
  reason `standing_supervisor`. `follow_up` is available as on any open task.

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
- Task reason `idle` joins the `TaskReason` enum.
- Owner questions from it (`report` `blocked` with `owner_action`) reach Needs you as any task's do.
