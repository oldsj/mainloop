# Task handoff

A retry or reassign replaces a task's attempt with a new one. A retry keeps the task's provider;
a reassign moves the task to the provider it names. Either way the successor is a fresh native
session with a fresh binding and credential, on the same task: same id, brief, project, topic and
parent. To continue with the agent's context instead, send a [follow-up](tasks.md#follow-up), which
goes to the task's own session.

Status: **implemented** for coordination and code tasks. A code task's successor continues on the
task branch from its head on origin. That needs a GitHub App; without one a code task's retry and
reassign are recorded `blocked` with reason `handoff_unavailable` and the view reports both
unavailable. No provider fallback is inferred: Mainloop never retries or reassigns on its own
initiative.

## Who may ask, and when

- **Callers.** The owner (`POST /tasks/{id}/retry`, `POST /tasks/{id}/reassign`, the task page's
  **Retry** and **Reassign**), main and a supervisor for the tasks they manage (MCP `task_retry`,
  `task_reassign`). Each request carries the task version and current attempt the caller saw, and a
  request id; the same request id with the same payload returns the stored operation.
- **Tasks.** A task with a current attempt whose status is `running`, `waiting`, `blocked` or
  `failed`, and that is not already in a handoff (a code task only with a GitHub App). The task view offers `retry` and
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
     the request returns. A supervisor's open children are cancelled with it, as for a cancel (not a project's standing
     supervisor's: its restart keeps them, see [project supervisor](project-supervisor.md)).
     The task moves to `waiting` / `handoff`. The operation is recorded `draining`.
   - A failed task whose attempt has already ended reopens to `waiting` / `handoff`. The operation
     is recorded `requested`.
2. **Stop** (`draining`). The old session is ended `cancelled`, and the operation waits for the stop
   observer to confirm the runtime stopped, exactly as a [cancel](tasks.md) does, with the same
   forced suspend and delete graces and the same `unresolved` bound. Once it is confirmed, an
   attempt with no workspace (a supervisor, a project's standing supervisor, a coordination child)
   has its kagent Session deleted (its finished turns are copied to Mainloop's transcripts first),
   so its actor gives up its place on the worker before the successor's is woken: otherwise it
   holds it until kagent suspends it idle (300 s), and on a full worker the successor's first send
   finds no room. Nothing runs in it and nothing is sent to it again, so there is still one writer.
   This is best effort: if kagent refuses, it is logged and the handoff goes on, and the successor's
   send waits for room ([sessions](sessions.md#runtime-capacity)). A code attempt keeps its actor,
   whose workspace may still be needed. Then `source_fencing`.
3. **Fence and successor** (`source_fencing`, or `requested` for a failed task), in one transaction:
   the old attempt's credentials are revoked and its capacity slot released (`fenced`); the
   successor attempt is inserted with `predecessor_id` set, taking the slot back; the old attempt
   becomes `superseded` with `successor_id` and `superseded_at` (an attempt that had already
   ended keeps its final state and gets the same links); the task's current attempt becomes the
   successor. A code task's branch writer claim is released by the fence and taken by the
   successor in the same transaction. A reassign first sets the task's `assigned_profile_id` to the
   target (`selection_source` `explicit`) and clears its model and effort, which were checked
   against the old provider. Then `target_creating`, or `checkpoint_required` for a code task. A
   failed child whose parent has no free slot waits for one within
   `TASK_PROVISION_WINDOW_SECONDS`, then the task fails again.
4. **Branch head** (code tasks, `checkpoint_required`). Mainloop reads the task branch on origin
   through the GitHub App. Nothing can move it now: pushgate refuses to release a writer claim
   while one of its pushes is unresolved, so the fence settled every push of the old attempt, and
   the successor holds the claim but has no workspace yet. When the branch is there, the
   successor starts at its head (`checkpoint_ref` `refs/heads/<branch>`). When it was never pushed,
   the successor starts from the task's base ref, or the default branch (`HEAD`) when it has none,
   resolved to its commit. The commit is recorded as the successor's `initial_ref` and the ref
   read as the operation's `checkpoint_ref`, in one transaction, before any workspace call. A read
   that fails is tried again; once `TASK_PROVISION_WINDOW_SECONDS` have passed since the successor
   was made, the successor is aborted and the task fails (`provisioning_unavailable`). A branch
   that cannot be found is never taken as never pushed unless the base can be read: lost access
   to the repository fails the task rather than starting it over from the base. A cancel recorded
   during the read wins before any workspace is made. Then `target_creating`.
5. **Provision** (`target_creating`, `target_ready`). As a create: the successor gets its session
   and binding (for a code task, a workspace on the task branch at `initial_ref`), becomes
   `active`, the task `running`, and its first brief is delivered once. The operation
   `completed`.

A superseded attempt never reopens: a later follow-up goes to the successor. A code task's
projection keeps the pull request an earlier attempt of the chain opened through Mainloop, since
the successor works on the same branch.

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
last report (outcome and summary, at most 4000 characters) or says it made none. A code task's note
also says where the workspace starts: at the branch head, with what the old attempt pushed and
nothing it did not (and to keep working on the branch's pull request), or at the base commit
because the branch was never pushed. It reads only rows that do not change once the successor's
workspace is made, so a resend of the brief is the same text.

Not carried: the old session's transcript, pending approvals, its holds, and a code attempt's
unpushed work (its workspace is not read).

## Not built

- **Automatic recovery.** Mainloop does not retry or reassign a stuck task on its own before asking
  the owner; see [inbox](inbox.md#reconciliation-and-merge-attention-a1) for the Problems
  classification a stuck task gets meanwhile.
- **Retention.** Superseded sessions are not archived or deleted on a schedule.
