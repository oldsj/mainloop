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
is sent, and the request id goes to kagent unchanged. A lost reply is recovered by replaying the
create from the row: the retry from the client does it, and so does the sweep once the intent is
older than `Limits.IntentGrace`. kagent returns the schedule the first send made, and the row becomes
`created`. A request id reused with a different Agent or config is 409, and so is a replay whose
schedule has since been deleted.

Attempts can overlap (a client retry while the first call is still running, or the sweep). A definite
refusal from kagent deletes the intent only when no other attempt began within `Limits.IntentGrace`
before or after it, because another attempt may have made the schedule. A contested intent stays and
the sweep replays it after the grace; a lone refusal then ends it. A success whose registry row is
gone, or is registered to another schedule, is an error the client retries; it is never answered 201
without the row. The sweep reads the owned Agents before it replays and sends no create for an intent on an Agent
this deployment no longer owns; that intent waits for the Agent to return.

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
  default), so an execution that becomes visible late with a lower id is still found.
- A repeated page token ends the pass with an error instead of looping.
- The link keeps `session_id` after the session is deleted, and the session is never made again.
  The executions list still shows the id.

## Known gaps

- Cancelling an adopted session in Mainloop does not cancel the kagent run behind it.
- The binding of an adopted session carries no Agent reference, so `ResolveAgent` cannot resolve a
  scheduled session's Agent.

Both are queued to the sessions work.
