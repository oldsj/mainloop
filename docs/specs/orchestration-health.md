# Orchestration health

Status: **Claude friction and reported input/output implemented and fixture-qualified**,
2026-10-10. They have a durable collector, an owner API and a responsive page. This is not
live qualification; a kagent Claude harness rollout and separately authorized live task remain.
Complete friction/cache and re-orientation measures, Codex usage, model and subscription fit,
and scheduled reviews remain proposed.

History before `e776537` describes the retired Python backend. Where other text on this page differs from the Go status, the Go status is current.

Mainloop should show where orchestration encounters friction, spends context and tokens,
and needs design attention. Start with Claude tool failures and token counts on one page.
Keep delivery, task completion, approval, workspace health and publication state separate.
Model and subscription fit follow that first usable path; they do not block it.

## Go server status

The Claude driver emits allowlisted `turn`, `tool` and `usage` observations through the
shared `EventSink.Health` method. A2A data artifacts use schema/name
`kagent.dev/orchestration-health/v1`, stable `epoch:sequence` event/artifact IDs and ordered
sequences. A live process retains its producer epoch across parked approval resumes. A new
process starts a new epoch; multiple epochs under the same A2A receipt mark a producer gap.
EOF closes observed coverage; truncation or the post-result execution ceiling leaves it partial.
Tool grouping uses opaque tool-operation/call keys, action, safe path and category. Generic MCP
errors stay `mcp_error`; error text does not establish permission denial or an HTTP status.
Health delivery is best-effort: the driver queues observations without blocking, a sink error
or stall never fails, delays or blocks cancellation of the turn, and dropped observations leave
a sequence gap that Mainloop reports as partial coverage.

Result usage is a qualified single-result **turn total** (`basis=turn_total`), never added to
assistant-response counters. Input means Claude's reported `input_tokens`, not an inferred
cache-inclusive input total. Root totals are treated conservatively as potentially tree-inclusive:
separate native-child usage is not accepted or added. Repeated result UUIDs do not add another
sample. Multiple result iterations produce a replacement revision with unavailable counters;
their additive/cumulative semantics are not qualified. Missing counts also remain unavailable.
Cache, child attribution and context occupancy remain unknown.

The Go server validates each observation strictly on import (exactly v1, no unknown fields,
enumerations enforced; [event.go](../../server/internal/health/event.go)) and binds attribution
through sessions, attempts and delivery receipts. It collects only by reading kagent's `GetTask`;
the Go code has no stream importer. Two paths collect, and both write through the same idempotent
ingest. When a delivery ends, the sessions package ingests the health artifacts of the task it already
holds in the transaction that marks the delivery terminal, so the observations and the receipt commit
with the delivery. A failed ingest is rolled back to a savepoint and logged: the delivery still ends,
and the sweep collects it later (health data is derived, so it never blocks a session from finishing).
The reconcile sweep, run every five seconds in
[`server/internal/health`](../../server/internal/health/collect.go), is the backstop and the only
collector of running turns, turns with no artifacts when they ended, and deliveries whose ingest
failed. The owner read API writes nothing, and collection neither sends a prompt nor changes delivery
success.

- Discovery starts tracking up to 500 new receipts per pass: each delivery that carries a kagent
  task, running (`delivered`) or finished. A receipt is written once. A delivery whose receipt
  is written (by either path) is flagged `health_tracked` in the same transaction. This flag is
  authoritative for discovery; the partial index `deliveries_health` holds only unflagged candidates,
  and discovery does not scan retained receipts to choose its batch. It locks candidates in creation
  time and ID order, skips deliveries locked by terminal ingest, then inserts receipts from the
  marked deliveries. Skipped deliveries remain eligible for a later pass if terminal ingest did not
  track them. An existing receipt is preserved on conflict. Retention leaves the flag set; an
  intentional rebuild of the derived health tables must also clear the affected deliveries' flags.
- Each pass reads up to 50 due receipts. A running turn's receipt stays pending and is read again
  after 15 seconds, doubling per failed read up to one hour. A read of a finished delivery marks
  the receipt collected for good, and a collected receipt is never reopened.
- Each task is stored in its own short transaction after kagent returns. No transaction or row
  lock is held during kagent I/O.
- A failed read leaves the receipt pending with the `source_unavailable` gap. After eight failed
  reads it becomes unavailable.
- A pending receipt older than the event window is closed as unavailable. A receipt whose session,
  agent binding or delivery is gone is abandoned the same way.
- Retention runs as its own sweep every ten minutes.

Coverage expects a receipt for every running or finished delivery that discovery tracks, so a
running turn with no observations yet keeps its row and session partial when other turns have been
observed; when no turn in the row or session has any observations, coverage is `unknown`. An open receipt does the
same even when the observations read so far look complete. Import allows 2,048 health events per
receipt and 8 KiB per event; overflow and integrity failures mark gaps.

The health ledger enforces event/sequence uniqueness,
retains source timestamp precision, rebuilds path-free turn summaries and serializes retention
against imports. Same identity/same payload is a no-op; conflicts record bounded categories.
Deleting workspace rows cannot erase persisted attribution. Project deletion cascades health
rows; the owner-removal operation deletes health only. Retention freezes a turn before removing
its detail. Expired replay cannot restore detail or increment frozen totals. Retention settings
are `HEALTH_EVENT_RETENTION_DAYS` (30) and `HEALTH_SUMMARY_RETENTION_DAYS` (365), with summaries
required to outlive detail. Independent report/decision/conversation retention is unchanged.

`GET /orchestration-health` and `GET /orchestration-health/events` are scoped to the configured
owner, including filter authorization. The `/health` page defaults to 24 hours; filters cover
project, product task, attempt, session and provider. It shows distinct failures, groups of three or more failed calls,
reported input/output, coverage, existing blocked/failed report IDs and approval state. Main and
owner sessions without product tasks appear as **Unassigned**. Product child tasks remain
separate rows, so their totals are not added to the parent's reported root total. Unknown metrics
show **Unavailable**; partial sources and supported-versus-observed turn coverage stay explicit.
Collection gaps count only for receipts or turns inside the selected window. Task, session and
attempt choices list the whole project, provider and window scope, so a scoped entry link does
not restrict later choices.

A sanitized A2A data fixture is round-tripped by kagent's gateway PostgreSQL test;
Claude driver/A2A and parked-resume fixtures check the producer boundary. No live provider,
production rollout or subscription proof is claimed.

## React status (migration slice F8)

The React health page (`web/src/features/health`, route `/health`) is tested with MSW against the
owner API contract. The Go handlers for `/orchestration-health` are implemented in
`server/internal/app` and covered by PostgreSQL route tests on fixture data, so the page has not
been run against live data. Labels follow the Go section's: **implemented** is in the code and
covered by those tests.

Implemented in the React client:

- The filters are the page's URL: window, project, task, session, attempt and provider. An
  unreadable value is dropped to its default and the page still loads. Apply writes the URL; Clear
  filters returns to the default 24-hour view.
- The filter panel opens by default at 640 px and wider, and starts closed below that. Task,
  session and attempt choices come from the wider project, provider and window scope, so narrowing
  one filter does not remove the others.
- Changing the project, provider or window clears the task, session and attempt in the draft, since
  each belongs to one scope. Nothing is written until Apply.
- Rows sort by failures, most first, with unknown counts last. An unknown count shows
  "Unavailable". A row with no task or title shows "Unassigned".
- Each row is checked on its own. A malformed row is dropped and counted; the others render. A row
  whose id repeats an earlier row is dropped and counted separately, with its own notice.
- Each report and approval is checked on its own. A malformed one is left out of its row, and the
  row says how many were left out. Its totals and other claims still show.
- A session's source observations load only when asked for. A reload clears them.
- Each approval shows its HITL card.
- A failed refresh keeps the rows already shown, with an inline error and a retry.

Not proven:

- The live health path. The Go collector and handlers are tested against scratch PostgreSQL and a
  fake kagent only: no live kagent run and no deployed page has been checked.
- Live event refresh. The page does not yet react to health events.

Native session identity polls only while a turn is in flight.

## Proposed views

One owner-scoped **Orchestration health** page, reachable from the project and session views,
defaults to the last 24 hours. Filter by project, product task, attempt, agent session and
provider. A session without a product task stays visible as **Unassigned**. Mobile starts
with totals and task rows; selecting a row reveals its observations and coverage.

1. **Friction:** failed tool attempts by category, permission denials when explicitly known,
   open approval waits, recorded decisions, blocked report claims and repeated failed calls.
   Show source and evidence identity. Three distinct failures of the same normalized operation
   in a turn mark **Repeated failures**, a possible retry loop. Retransmission of one call is
   counted once. A delivery failure is not counted again as a tool failure.
2. **Re-orientation:** successful reads per repository-relative path; repeated reads within a
   turn and across turns; reads before the first successful edit; overlap of paths read before
   and after a compaction. Show **No edit observed** when appropriate. These are proxies for
   context work: normal investigation, review and changing files can justify repeated reads.
   No automatic wasted-token total or claim of duplicated work follows from them.
3. **Cost and cache:** reported input, output, cache-read and cache-write tokens, input cache
   share, compaction counts and context-input samples over time. Unknown counters display
   **Unavailable**, with supported/observed turn coverage. Initially this is token activity;
   display currency only with an explicit priced API source, never subscription token prices.
4. **Design cohesion:** dated review reports against exact merged commit ranges, with paths,
   evidence references, duplicated behavior, a suggested shared helper and confidence.
   Findings are reviewer judgment, not a duplicate-code score or authority to refactor.
5. **Model and subscription fit:** configured versus observed model/effort, difficulty/stage,
   outcome, explicitly linked repairs/reviews, retries, native turns and tokens. Separately
   show each subscription's reported limits, reset windows, freshness and potential unused
   headroom. Keep suggestions advisory; no automatic provider switch or new work dispatch.

No view changes native compaction, approvals, admission policy or task status. Observation
gaps appear alongside measurements; zero means supported and observed with no occurrences.

## Smallest durable event contract

Add one versioned, typed health event to kagent's shared runtime sink. Provider adapters
extract allowlisted fields while native events are in memory, and emit A2A **data** artifacts
under a proposed `kagent.dev/orchestration-health/v1` schema. Telemetry must never become
assistant reply text, a prompt, an MCP delegation command or an approval response.

Mainloop validates the schema in `server/internal/health` and persists append-only
`orchestration_health_events`. Common fields are:

- `schema_version`, `event_id`, `producer_epoch`, ordered `sequence`, `kind`, `occurred_at`
  and server `received_at`; timestamps include their source/precision.
- Gateway/runtime-session ID, A2A task ID, native turn key and optional call/request key.
  Native identity can use an opaque scoped key; do not copy native histories.
- Native provider/runtime version, measurement source and coverage status.
- Mainloop assigns `owner_id`, project, product task, attempt, binding/session and profile
  revision through verified bindings and delivery/attempt records at ingestion. It never
  trusts owner/project/task IDs from the event payload. A2A task IDs are not product task IDs.

| Kind         | Allowlisted payload                                                                                                                                                                       | Purpose                                                                       |
| ------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------- |
| `turn`       | `started/finished/failed/interrupted`, optional configured/observed model and effort with provenance                                                                                      | Native turn boundaries, not each approval-resume invocation or model response |
| `tool`       | `started/finished`, call key, bounded tool class/name, `read/edit/search/other`, optional safe path, `success/error/denied/unknown`, safe error code, HTTP status, exit code, duration    | Paired attempts, friction and file activity                                   |
| `usage`      | Scope and accounting basis, sample key/revision, nullable input-total, uncached-input, output, cache-read, cache-write and reasoning-output counts; optional context-input/window samples | Correct accounting without mixing cumulative and incremental counters         |
| `compaction` | Boundary key, `started/completed`, trigger, nullable pre/post context-input counts                                                                                                        | Boundaries and read overlap, with no summary content                          |
| `approval`   | Verified request/leaf key, `waiting/decision_recorded/resumed/expired`, allowlisted decision category                                                                                     | First-seen waits and separate response/transport/resume times                 |
| `coverage`   | Per-signal `supported/unsupported/unknown`, `complete/partial`, last sequence and bounded gap reason                                                                                      | Detect missing boundaries, truncated collection and schema/version gaps       |

Blocked reports project directly from existing `task_reports` IDs; they need no duplicate
event ledger. Mainloop's own MCP boundary may emit the same typed `tool` vocabulary with
source `mainloop_mcp`. It records known policy/application codes and unexpected HTTP status,
not exception text, request bodies or responses. If native and server observations cannot
be correlated by a verified call/request key, display their source counts separately rather
than summing them as independent failures.

### Persistence, replay and attribution

Allocate the producer epoch before starting native work. Sequence and event identities stay
stable across a parked turn's resume segments. Each event has a stable artifact ID and is
stored by the existing gateway before it is considered published. Task snapshots must retain
these artifacts so `GetTask`/subscribe recovery can import them without replaying a prompt.
The health extension requires fixture proof of this gateway round trip, not just driver output.

Use a uniqueness constraint on `(owner_id, gateway, event_id)`. Same ID/same typed payload is
a no-op; same ID/different payload becomes a bounded integrity diagnostic. Imported/forked
history preserves original event identity and is excluded from new activity. A restarted
producer starts a new epoch and records an observation gap; it does not pretend to recover
native stdout that was never persisted. Missing starts, finishes or sequences make the relevant
rollup partial. Closing coverage markers require all preceding events to have persisted.

Reconnect ingests both stream artifacts and `GetTask` snapshots using the same function,
including already completed deliveries. Health storage never changes delivery success or
starts another writer. Failed health ingestion marks collection partial and reconciles the
existing task; loss of a gateway/native source remains an explicit gap. Bound ingestion
per task and payload size; overflow records a gap instead of silently sampling counts.

Persist attribution at ingestion so deleting workspace rows cannot erase history. Main and
owner workspace sessions may have no product task. Native subagent usage has its own scope:
tree-inclusive totals cannot also be summed with their children's exclusive totals. When a
runtime reports only a tree total, show that total on the parent and mark child attribution
unavailable. Do not infer a subscription/account from the provider name alone.

### Privacy and retention

Health payloads contain counts, identifiers, categories and permitted paths. Exclude prompts,
messages, file contents, patches, shell commands, search queries, full tool arguments/results,
error text, URLs with queries, credentials and account email addresses. Reject unknown payload
fields. Extract typed paths before discarding provider inputs; never parse shell text for reads.
Health collection does not require the native OTLP content-capture option to be enabled.
Normalize paths lexically relative to the known checkout; omit outside-workspace paths and
mask sensitive paths such as credential files using a project-scoped opaque key. No filesystem
read or symlink traversal is needed to collect a path. Paths remain owner-private metadata.

Implemented defaults: events and path-level detail **30 days**; path-free turn summaries
**365 days**. Task/session totals derive from those summaries. Retention is configurable and project/owner deletion also removes health
data and associated opaque keys. Do not alter the independent retention of conversations,
reports, immutable owner decision receipts or publication audit. Rebuild rollups while their
inputs exist; older aggregates are frozen with an aggregation-version and coverage marker.
Reject expired event replays so they cannot resurrect deleted detail or increment frozen totals.
Future subscription snapshots use the same 30-day detail/365-day path-free trend policy.

## Aggregation rules

Start with indexed queries and a rebuildable turn summary; add daily materialization only
when needed. Keys include owner, project, product task, attempt, agent binding, native provider,
model and day. Per-task totals sum attempts once; parent rows separate **Own** and **Descendants**
so a task tree is never double-counted. Per-agent means a stable session binding, not a pod,
provider display name or model. Show unlabeled and unattributed work explicitly.

- **Friction:** count distinct completed failed/denied calls and unresolved starts separately.
  Allowlisted structured denial codes establish permission friction; generic `is_error`
  establishes only a tool error. Retry-pattern keys use tool/action/safe path/category, never
  raw argument hashes. Intentional owner rejection and infrastructure denial remain distinct.
- **Approvals:** use the first verified waiting event and native resume/end event for observed
  wall-clock wait, including suspended time. Display decision latency and response-transport
  uncertainty separately. Existing overwritten HITL observation timestamps cannot backfill
  exact historic waits. Unknown ends stay open/partial, rather than ending at last refresh.
- **Reads:** count successful explicit reads once per call/path. Repeated reads are subsequent
  reads of a path within the selected scope; track cross-turn overlap separately. First edit
  means first successful explicit edit by sequence. Missing boundary or shell-read coverage
  makes reads-before-edit partial. Post-compaction overlap indicates re-orientation, not motive.
- **Usage:** adapters label `response`, `turn` or `session` scope and `incremental` or
  `cumulative` basis. Sum exclusive response samples once, or use the qualified turn total,
  never both. Cumulative samples replace the earlier revision; derive differences only
  between ordered samples in the same counter epoch with a known baseline. Counter reset,
  rollback or absent baseline is a gap, not a negative delta or a full total assigned to a turn.
  Multiple Claude result iterations and approval resumes need explicit accounting fixtures.
- **Cache:** Claude input-total is uncached + cache-read + cache-write under the documented
  API semantics. Codex input already includes cached input; do not add caches again.
  Reasoning output is a component, not an addition to output. Cache share is
  `sum(cache_read) / sum(input_total)` over compatible samples with both counters known.
  A zero denominator is unavailable; never average per-turn percentages or hide excluded turns.
  Cache-write absence stays unknown unless the qualified provider contract explicitly supplies
  zero. Invalid subsets produce diagnostics, not clamped plausible-looking percentages.
- **Context:** graph only a provider-reported context-input sample or a clearly labelled
  per-model-call input proxy. Turn/session totals are cumulative work, not context occupancy;
  context-window capacity is a separate line. Compaction pre/post measurements stay nullable.

## Model and subscription fit after the MVP

### Model fit

Store execution metadata on turn/usage observations: configured and observed model, effort,
provider/profile revision and provenance. Leave defaults unknown unless the runtime reports
their effective value. Record model changes/reroutes instead of attributing the whole task to
its final model. Operator configuration identity is useful but does not prove execution identity.

Add owner/supervisor-authored difficulty and stage labels (`simple`, `hard`, `live_debugging`,
`unknown`; implementation/review/repair) with a version and author. No classification from
prompt contents is required. Extend report evidence with candidate-bound review findings and
explicit `repair_of` links; a progress report is not a repair and a task retry is not necessarily
a quality failure. Use existing verified publication outcomes separately from report claims.

Compare like difficulty/stage groups across model/effort: accepted outcomes, linked repair
cycles, finding severity, task retries, native turns, tokens and friction coverage. Flag
**Possible over-provisioning** for a configured heavy profile on labeled simple work, and
**Possible under-provisioning** for a configured light profile with linked correctness repairs.
Show the underlying rows, sample size and unknowns. Missing authority, environment failures
and incomplete context are competing explanations, not evidence that a model is inadequate.
Owner-configured tier mappings avoid guessing capability from model names. No automatic
model downgrade, provider fallback or subscription-spending decision follows from a flag.

### Subscription windows

Add a separate typed `subscription_snapshot` event after core usage collection. A trusted
operator mapping links runtime authentication configuration to an opaque owner subscription ID;
never derive identity from tokens, account email or a runtime-supplied account claim. Store
provider, bucket/limit ID, auth mode, source (`native_reported`, `owner_entered`, `estimated`),
observed time, nullable used percent/absolute units/limit, window duration/reset time and
freshness. API quota, subscription quota and extra-credit balances remain distinct.

Codex's supported App Server rate-limit read/notification is the preferred future source.
Keep multiple bucket/windows separate; never sum snapshots from several agents sharing an
account. Reads use the already owned App Server connection, without starting a native turn
or another process merely to refresh quota. Claude's documented interactive usage display is
a possible owner snapshot source,
not proof of a harness-readable endpoint. Qualify native structured subscription telemetry
before implementing it; no private endpoint scraping or collection of provider credentials.

For a fresh reported window, headroom is `100 - used_percent`, labelled **As of <time>**.
Show reset time only when supplied. Account-wide deltas are observations across all clients,
not automatically attributable Mainloop consumption. After reset, bucket change or a stale
snapshot, require fresh data; a passed reset timestamp does not prove replenishment.
Proposed freshness threshold is five minutes, configurable; never poll more often solely
to make the dashboard look current.

If no automated quota data exists, allow an owner-entered used/limit/reset snapshot and
show Mainloop's tokens and successful calls beside it. If enough reported snapshots exist
within the same window, estimate consumption pace/headroom at reset from their observed
change, with sample interval, uncertainty and outside-Mainloop activity stated. This estimate
uses observed subscription units, not a fixed token-to-percent multiplier. With no baseline,
limit or reliable window, remaining headroom is **Unavailable**; show workload activity
instead. A token budget the owner sets is an estimated planning budget, not provider quota.

Show **Potential unused headroom** when a fresh snapshot reports capacity and no Mainloop
work is using that subscription, alongside eligible queued work and reset time. Activity in
other clients is unknown unless reported. This is an opportunity for owner steering, not a
claim of wasted money or permission to consume the subscription.

## Scheduled cohesion review

An owner enables a bounded review schedule for a project. The schedule creates a normal,
idempotent task intent keyed by project and exact merged commit range; existing delegation,
provider constraints and admission limits own its execution. It introduces no native cron,
second scheduler or independent writer. A skipped run for unavailable capacity remains visible.
The current code has no project review-schedule contract; that scheduling trigger is proposed.

Select verified merged revisions since the last reviewed checkpoint, with explicit commit/file
bounds. Use the existing main → supervisor → child roles: a coordination supervisor owns the
run and delegates a code review child with the repository checkout it needs. A coordination
task alone has no repository authority
([delegation](delegation.md)). The brief prohibits source edits/publication and requests a report;
a future read-only workspace capability must be qualified before claiming runtime enforcement.
Code-task review completion uses the report's completed review coverage, not fabricated merged
publication or a change to current code-task settlement rules. A completed code report currently
waits for publication, so it cannot by itself release review-task capacity. The enabled schedule
authorizes the owning supervisor to use existing `task_cancel` after consuming a valid review
report, and reconcile confirmed runtime disposal through the normal cancellation path. The
task remains visibly cancelled with a completed review report; do not call it a successfully
published code task. Unknown cleanup holds capacity and prevents an overlapping scheduled run.
Advance the reviewed-range checkpoint only after the bounded report is persisted, its range
is validated and the review runtime is confirmed disposed. A dedicated review completion
mode would be a separate lifecycle change, not an assumption in this plan. The supervisor
then reports coordination completion through the existing settlement path.

Findings stay in ordinary reports/evidence references: reviewed SHAs, paths, duplicated concern,
suggested helper, severity/confidence and explicit gaps. No raw diff or prompt is copied to the
health ledger. Empty findings with complete coverage differ from an incomplete review. Findings
can suggest follow-up tasks but do not authorize changes, post comments or resolve threads.

## Task liveness

Status: **implemented**, 2026-10-16. This is a separate concern from the friction/token
collection above: it answers whether a `running` task's native session is still there at all,
not how it is spending tokens or tools. See `docs/specs/tasks.md` ("Liveness bounds") for the
task-status contract; this section covers the mechanism.

A task attempt is agent activity Mainloop does not itself run; nothing before this slice bounded
how long one could claim to be `running` after its kagent session stopped existing or making
progress. The `task-liveness` sweep (`server/internal/tasks/liveness.go`) closes that gap without
adding a second session owner or scheduler: it only reads kagent's own session record and writes
the task's existing `status`/`reason` columns, the same durable fields every other task
transition uses.

Each pass lists the current, `active` attempt of every task still `status = 'running'` (a task
the sweep already blocked drops out of that list on its own; a look that finds nothing to do
still bumps the attempt's `updated_at` alone, so a backlog larger than one pass's 100-row cap
rotates through instead of the same oldest rows being read forever) and asks
`LivenessObserver.ObserveLiveness` for each attempt's own Mainloop session/binding id — never a
kagent id, which Mainloop's id is not. The production observer, `KagentLiveness`
(`server/internal/tasks/liveness.go`), resolves `agent_bindings.kagent_session_id` fresh from the
database on every call, outside any transaction; a binding with none yet (a coordination attempt
before its brief is delivered) answers unknown, and the sweep leaves it alone rather than judging
it lost. With a kagent session resolved, `kagent.Client.ObserveLiveness`, through `ObserveSession`,
answers whether it exists at all (`connect.CodeNotFound` reports it does not) and whether its
`RuntimeState` is terminal (failed, deleting or deleted). Activity is not kagent's own
`Session.updated_at`: that field only moves on a lifecycle operation (create, rename, resume,
suspend, delete), never a turn, so a long working turn would otherwise read as idle. Instead
`KagentLiveness` lists the A2A tasks of every native context the session has used (the same
contexts the stop observer lists); a task waiting on the owner counts as activity right
now, and otherwise the later of the newest task status timestamp, the sessions delivery ledger's
own last-activity record, and the time the sweep first saw the current fingerprint of the
session's submitted or working tasks (task id, state, status time and newest history message,
stored on the attempt as `progress_marker` / `progress_at`) stands in. A working status alone is
not activity: kagent keeps a turn working until it ends, and a turn whose native process has
wedged never ends. A read error
leaves the task alone for the next pass rather than guessing.

The sweep blocks a task at most once per cause, in this order: `session_lost` (the session is
gone or terminal — there is no point timing anything out once the runtime itself is gone),
`stalled` (no activity for `TASK_STALL_TIMEOUT_SECONDS`, default 2700/45m), then `runtime_limit`
(the attempt has run longer than `TASK_MAX_RUNTIME_SECONDS`, default 21600/6h, regardless of
activity, including a task that keeps recording progress — the safety net of last resort). None
of these cancels the native session or retries anything; the only effect is the existing `blocked`
status transition and the existing reconciliation-attention path
(`task_reconciliation_needs`), so the owner sees it the same way a `blocked` / `reconciliation`
task already surfaces, with a reason that says which of the three it was. Before writing, the
sweep re-reads the attempt and the task under the task's lock and requires the attempt to still be
`active` and the task to still be exactly `running` with no reason: a report that reached the task
first while the (uncached, network) observation was in flight owns it instead, and the stale
liveness signal is dropped. A later progress report from the task's current attempt recovers
`session_lost` and `stalled` back to `running` (the sweep can be wrong, and a report is proof it
was) but not `runtime_limit`, which stays blocked until the owner acts, since it fires regardless
of activity and a report alone would defeat the bound.

The same existence/terminal test fixes a related stuck state: cancelling a task whose kagent
session kagent has entirely forgotten (for instance across a kagent restart with no persistence)
used to never settle, because the runtime-stop resolver kept retrying `ListTasks`/
`FindTaskForMessage` against a session that would never reappear. `KagentStops.Stopped`, given a
`Liveness` observer, now checks it first and answers stopped at once when the session is gone or
terminal, before listing anything.

Not built: using this signal to automatically retry, reassign or cancel the stuck attempt; that
is an owner decision this slice deliberately leaves alone. Not proven: behavior against a live
kagent restart, only against fakes in `server/internal/tasks/liveness_test.go` and
`server/internal/kagent/liveness_test.go`.
