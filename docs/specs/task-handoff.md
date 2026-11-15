# Task handoff

A retry or reassign replaces a task's attempt with a new one. A retry keeps the task's provider;
a reassign moves the task to the provider it names. Either way the successor is a fresh native
session with a fresh binding and credential, on the same task: same id, brief, project, topic and
parent. To continue with the agent's context instead, send a [follow-up](tasks.md#follow-up), which
goes to the task's own session.

Status: **implemented for coordination tasks.** A code task's retry and reassign are still recorded
`blocked` with reason `handoff_unavailable` (see [Not built](#not-built)). No provider fallback is
inferred: Mainloop never retries or reassigns on its own initiative.

## Who may ask, and when

- **Callers.** The owner (`POST /tasks/{id}/retry`, `POST /tasks/{id}/reassign`, the task page's
  **Retry** and **Reassign**), main and a supervisor for the tasks they manage (MCP `task_retry`,
  `task_reassign`). Each request carries the task version and current attempt the caller saw, and a
  request id; the same request id with the same payload returns the stored operation.
- **Tasks.** A coordination task with a current attempt whose status is `running`, `waiting`,
  `blocked` or `failed`, and that is not already in a handoff. The task view offers `retry` and
  `reassign` exactly then. Refusals (409): `task_terminal` (cancelled or completed: a completed task
  takes a follow-up instead), `task_not_running` (no attempt yet), `operation_pending` (a cancel,
  retry or reassign is in progress), `task_settling` (a failed task whose attempt is still being
  stopped), `parent_not_active` and `child_capacity`/`global_capacity` (a failed child whose parent
  ended or has no free slot), `stale_task_version`/`stale_task_attempt`. A reassign target that can
  never run the task is 422 `provider_unknown`, `provider_role_unavailable` or `provider_disabled`.
- **Limit.** Main and supervisors may retry or reassign one task `TASK_MAX_HANDOFFS` times
  (default 3, at least 1), counted as successor attempts the task has had. Past that they get 409
  `handoff_limit` (detail: `handoffs`, `limit`), so an agent cannot loop a failing task forever;
  the owner decides what happens next, and the owner's own retries are never limited.

## What happens

The request and every later step are one task operation (`kind` `retry` or `reassign`), driven by
the `task-operations` loop. Every step is idempotent on stored state, so a crash anywhere resumes.

1. **Request.** Under the task lock, in the request's transaction:
   - A running attempt is moved to `draining` (stop requested), so the old agent loses MCP before
     the request returns. A supervisor's open children are cancelled with it, as for a cancel.
     The task moves to `waiting` / `handoff`. The operation is recorded `draining`.
   - A failed task whose attempt has already ended reopens to `waiting` / `handoff`. The operation
     is recorded `requested`.
2. **Stop** (`draining`). The old session is ended `cancelled`, and the operation waits for the stop
   observer to confirm the runtime stopped, exactly as a [cancel](tasks.md) does, with the same
   forced suspend and delete graces and the same `unresolved` bound. Then `source_fencing`.
3. **Fence and successor** (`source_fencing`, or `requested` for a failed task), in one transaction:
   the old attempt's credentials are revoked and its capacity slot released (`fenced`); the
   successor attempt is inserted with `predecessor_id` set, taking the slot back; the old attempt
   becomes `superseded` with `successor_id` and `superseded_at` (an attempt that had already
   ended keeps its final state and gets the same links); the task's current attempt becomes the
   successor. A reassign first sets the task's `assigned_profile_id` to the target
   (`selection_source` `explicit`) and clears its model and effort, which were checked against the
   old provider. Then `target_creating`. A failed child whose parent has no free slot waits for
   one within `TASK_PROVISION_WINDOW_SECONDS`, then the task fails again.
4. **Provision** (`target_creating`, `target_ready`). As a create: the successor gets its session
   and binding, becomes `active`, the task `running`, and its first brief is delivered once. The
   operation `completed`.

A superseded attempt never reopens: a later follow-up goes to the successor.

**One writer.** Concurrent retries and reassigns of a task carry the same version; the first takes
the task lock and moves the version, and the rest are refused. A cancel during a handoff wins
whatever step the handoff reached: before the successor exists, the handoff finds the task ended and
blocks; after, the cancel stops the successor. Either way the task ends `cancelled` with no attempt
left running.

## The successor's brief

The successor's first message is the task's brief as a create's would be (role guidance, the
brief, project instructions and memory, shared guidance), with a **Continuation** section after the
brief. It says whether the task was retried or reassigned (and from which provider), which attempt
number was stopped, that the new session does not have its context and the old attempt may have done
part of the work, that a supervisor's open children were cancelled, and quotes the old attempt's
last report (outcome and summary, at most 4000 characters) or says it made none. It reads only rows
that do not change once the old attempt is fenced, so a resend of the brief is the same text.

Not carried: the old session's transcript, pending approvals, and its holds.

## Not built

- **Code tasks.** Their successor must start from the task branch's pushed head, read from the
  remote before the workspace is created, or from the base with the continuation note when the
  branch was never pushed. Until then a code task's retry and reassign record a `blocked`
  operation with reason `handoff_unavailable`, the view reports both unavailable, and the task page
  does not show them.
- **Automatic recovery.** Mainloop does not retry or reassign a stuck task on its own before asking
  the owner; see [inbox](inbox.md#reconciliation-and-merge-attention-a1) for the Problems
  classification a stuck task gets meanwhile.
- **Retention.** Superseded sessions are not archived or deleted on a schedule.
