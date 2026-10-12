# Scheduled runs

A scheduled run is a cron schedule that kagent owns and fires. Mainloop proxies the schedule's
CRUD at `/schedules`, and turns each execution kagent starts into a Mainloop session (and, when it
fails, one inbox item). Labels: **implemented** is in `server/internal/schedules` and covered by
fake-backed tests against a scratch PostgreSQL; nothing here has been run against a live kagent.

## Which schedules are this deployment's (implemented)

kagent lists every schedule of its principal, and one principal is shared by every Mainloop
instance on that kagent. Only distinct Agent names keep instances apart, so a schedule belongs to
this deployment when both hold:

- its create went through this instance, so `scheduled_run_schedules` has a row for it, and
- its Agent is one this deployment owns: a configured provider-profile Agent, or one in
  `kagent_agents`.

Everything below applies that rule.

- **Create** on an Agent outside the owned set is refused with 403 before anything is written or
  sent.
- **List, get, update, delete, trigger and executions** treat any other schedule as not found (404).
  A list page can hold fewer items than its limit; the server reads further pages while a whole page
  is foreign, up to a bound.
- **Adoption** reads only the registry, never kagent's list. A failed or malformed list cannot stop
  it.

## Create and its idempotency key (implemented)

The registry row (request id, Agent, config, state `intent`) is written before `CreateScheduledRun`
is sent, and the request id goes to kagent unchanged. A request id reused with a different Agent or
config is 409, and so is a replay whose schedule has since been deleted.

Each send of the create is an **attempt** (the route, a client retry, the sweep's replay). An
attempt takes a **lease** on the intent, expiring `Limits.CreateAttemptBound` later (2 minutes by
default), and its call to kagent runs under a deadline `Limits.CreateAttemptMargin` (15 seconds by
default) before the lease expires. No attempt can outlive its lease. Expiry is compared with the
database clock.

- **A schedule back wins.** Any attempt that gets one confirms the row (`intent` to `created`, with
  `schedule:updated`, in one transaction) and drops the leases. A confirmation that finds no row, or
  another schedule under the request id, is an error the client retries; it is never answered 201
  without the row.
- **A route attempt's refusal never deletes the intent.** It returns its own refusal and ends its own
  lease. Another attempt may still make the schedule: kagent checks the Agent and inserts as separate
  steps, so one call can be refused while another commits.
- **An uncertain answer** (timeout, 5xx, lost reply) leaves the lease to run out, since the create may
  still land.
- **Only the sweep's recovery decides a refusal is final.** It replays an intent only when it has no
  unexpired lease, on an Agent this deployment owns now, and only one recovery runs at a time (the
  sweep lease). kagent dedupes on the request id, so the replay is also the read: a schedule back
  confirms the intent; a definite refusal with no other live lease deletes it, because nothing can
  still succeed (the intent is locked while the leases are counted); an uncertain answer leaves it for
  a later pass.
- An intent on an Agent the deployment no longer owns is not replayed; it waits for the Agent to
  return.

Residual risk: a kagent server that commits a create more than the margin after the client's deadline
(its handler outliving the cancelled call) could land after the recovery's refusal deleted the
intent. The margin bounds that: 15 seconds by default, and in practice more, because the kagent
client's own request timeout (30 seconds by default) ends the call well before the lease's deadline.

One Mainloop instance per kagent principal is an invariant (a second environment gets its own
kagent). The registry and the owned-Agent filter keep the routes and the sweep to this instance's own
schedules; they are not a defence against another instance sharing the principal, and kagent has no
read-only lookup by request id for resolving an intent on a departed Agent.

## Executions become sessions (implemented)

Each pass reads a registry schedule's executions after its cursor. An execution that has reserved a
kagent Session becomes one Mainloop session bound to it; one that failed or timed out opens one
inbox item. `scheduled_run_links.execution_id` is the idempotency key, and the link, the session,
the binding, the item and `schedule:updated` are written in one transaction.

- The cursor is held back from executions newer than `Limits.CursorSafetyWindow` (5 minutes by
  default), so an execution that becomes visible late with a lower id is still found within that
  window. Execution IDs are UUIDv7 values minted on kagent replicas. The window assumes replica
  clock skew plus commit/visibility delay stays below it; a later lower ID can otherwise be missed
  permanently. Keep replica clocks synchronized and raise the window if that bound is too small.
- A repeated page token ends the pass with an error instead of looping.
- The link keeps `session_id` after the session is deleted, and the session is never made again.
  The executions list still shows the id.

## Native session control (implemented)

- The binding stores the schedule's Agent namespace and name independently of provider profiles.
  Session control, task stop observation, HITL observation and follow-up delivery prefer that reference, including for an
  Agent no profile names. Adoption writes no Session-create marker and does not add an Agent to
  `kagent_agents` or an unresolved create to `CountUnresolvedBindings`.
- Cancelling an adopted session commits its cancelled status and an `unknown` agent outcome before
  calling kagent. It lists all native tasks in the session's bound and previously used contexts and
  cancels non-final ones, including a scheduled turn with no Mainloop delivery. No database lock or
  transaction spans those calls.
- `agent` is `stopped` after a confirmed native cancel, `not_running` when no native task remains
  running, or `unknown` when a listing, cancel or in-flight send cannot be confirmed. An empty task
  list does not settle a reserved or in-flight scheduled dispatch. A fresh `GetScheduledRunExecution`
  must show a final execution, or identify its task and that task must be observed final; then the
  native contexts are listed again and any remaining task is cancelled. Cancel replies
  must identify the requested task and context and report a terminal state. The outcome is stored;
  repeated cancellation returns it without repeating the remote action.
- A scheduled-cancel sweep revisits adopted sessions whose outcome is `unknown`. Each pass handles
  at most 50 sessions within one minute, with each session's native calls bounded to 20 seconds.
  Retry times are committed before native I/O and back off from 30 seconds to five minutes, surviving
  restart. No age or retry count turns uncertainty into a confirmed stop. Only positive evidence
  moves the stored outcome from `unknown` to `stopped` or `not_running`. The sweep also preserves
  uncertainty about any Mainloop sends cancellation closed; it never replays a scheduled send.

These paths have fake-kagent and scratch PostgreSQL regression evidence, including HITL adoption
and a follow-up through the delivery loop. Live kagent behavior has not been measured.

## Web UI (implemented in `web/`, tested with MSW only)

Reached from the project page's "Scheduled runs" link, or directly at `/schedules`. The list covers
the whole deployment: the contract gives schedules no project scope.

- **List** (`/schedules`): each schedule shows its name, a plain-words reading of its cron line, the
  raw expression, Active or Paused, its agent, time zone and next run. Pages load with "Load more",
  in the order the server returns them (by schedule id).
- **Create** (`/schedules/new`): the agent picker offers enabled providers only. The agent cannot
  change later. "Start paused" is available here. Resending an unchanged form reuses its request id,
  so a dropped reply cannot make a second schedule; a changed form gets a new id.
- **Edit** (`/schedules/$id`): name, five-field schedule, time zone, prompt and execution timeout.
  Every write sends the etag of the version the form was opened at, so a save never overwrites a newer
  version silently. A refetch does not change that etag, so a change made elsewhere shows as a
  conflict on the next save.
  - **412 (changed elsewhere):** the user's edits stay in the form, and the newer saved values show
    beside them. The user saves again to replace them, or discards their edits to take the saved
    version.
  - **409 or 404 (deleted elsewhere):** the form locks.
- **Pause and resume** send the version the page holds with only the paused flag changed, so unsaved
  edits are not written. They are off while a conflict notice shows. Per the contract, a change
  applies only to executions reserved after the update.
- **Run now** sends a request id and confirms "Run requested". The run then appears under Executions.
- **Delete** asks for confirmation. Future runs stop. Past executions and their sessions remain.
- **Cron line:** five fields, read the way robfig/cron v3 reads them: minute, hour, day of month,
  month, day of week (0 to 6, names allowed). Macros such as `@daily`, and `?`, `L`, `W` and `#`, are
  rejected. Common shapes read as plain words ("Every day at 02:00", "Every 15 minutes"). Other valid
  expressions read as "Custom:" with each field spelled out. The first problem, if any, is shown.
- **Time zone:** an IANA name, starting as the browser's zone. Blank means UTC. `Local` is rejected.
- **Prompt:** 1 to 32,768 bytes, with a live byte count.
- **Execution timeout:** seconds, default 900, minimum one microsecond. It includes queueing and
  provisioning.
- **Executions** (per schedule, newest first): each row says "Scheduled run" or "Manual run", gives
  its state and any failure reason, and links "Open session" once the run has a session. Otherwise
  it says "No session yet." Older runs load with "Load older executions".

Live updates: a `schedule:updated` event refreshes the schedule list, the open schedule and its
executions. The event names only the change, so each page re-reads the server. A reconnect also
refreshes every query. A real server event has not been observed; the tests send a fake one.

Evidence: vitest with MSW covers each behaviour above. The MSW handlers are hand-written and no test
checks them against `api/openapi.yaml`, so the contract match rests on review. No run has been made
against a live kagent or a deployed Mainloop.
