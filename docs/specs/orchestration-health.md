# Orchestration health

Status: **slice 2 implemented and fixture-qualified**, 2026-10-10. Claude friction and
reported input/output have a durable collector, owner API and responsive page. This is not
live qualification; a kagent Claude harness rollout and separately authorized live task remain.
Slices 3–5, scheduled reviews and model/subscription routing remain proposed.

Mainloop should show where orchestration encounters friction, spends context and tokens,
and needs design attention. Start with Claude tool failures and token counts on one page.
Keep delivery, task completion, approval, workspace health and publication state separate.
Model and subscription fit follow that first usable path; they do not block it.

## Source inventory at slice 1

This inventory examines Mainloop `f3b672054c913ebd6837eab14d09c983ed7e27a1` and
the kagent fork branch `fork/mainloop-substrate-0-4` at
`798607072a204b47332bd2e26c5f7f229e733358`. These are source observations,
not a claim about the currently deployed image. Evidence keys below cite exact file lines.
Existing synthetic fixtures establish wire examples, not live provider qualification.

**Available now Y** means Mainloop durably stores the named observation and can query it.
**N** includes information visible transiently in A2A but absent from Mainloop storage.
Partial signals are named explicitly rather than presented as complete coverage.

| Signal                                            | Native source and harness behavior                                                                                                                                                                                                                                                            | Available now Y/N | Mainloop persistence and gap                                                                                                                                                                                                                                             |
| ------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Native turn failure and delivery trouble          | Both harnesses return terminal outcomes through A2A. Claude retains a safe failure diagnostic; Codex reduces a failed turn to a generic failure. C1, X1, K1.                                                                                                                                  | Y                 | `native_deliveries` stores state, A2A task ID, evidence reference and bounded detail. It does not retain each transition or establish a permission-specific cause. M1, M2.                                                                                               |
| Tool and MCP failures; permission denials         | Claude `tool_use` and `tool_result.is_error` become paired calls/results. Codex translates command, file and MCP items; its result error flag tests `status == failed`, so other statuses/error payloads need separate qualification. Raw arguments/results reach A2A data parts. C2, X2, K2. | N                 | The client can decode data parts and holds artifacts in memory, but mirrors only text and delivery state. No durable tool counts, denial categories or call timeline. M2, M3.                                                                                            |
| Approval requests and owner decisions             | Claude approval broker and Codex MCP elicitation park the native turn; shared executor emits structured `input-required` metadata. This is protected-tool approval, not comprehensive OS/sandbox denial telemetry. K3, X3.                                                                    | Y                 | HITL request projections, verified aliases, immutable decision receipts and transport states persist. Exact wait duration is unavailable: request `observed_at` is overwritten on refresh; a recorded decision is distinct from native resumption. M4, M5.               |
| Blocked and failed reports                        | Mainloop MCP `report` records an explicit attempt-scoped claim and transactionally updates the task.                                                                                                                                                                                          | Y                 | `task_reports`, `task_events` and recipient intents persist. Count report claims separately from verified runtime failure; reasons are not a typed friction taxonomy. M6.                                                                                                |
| Retry loops and MCP HTTP 500 loops                | Distinct native tool call IDs can reveal repeated attempts. MCP maps known application errors to `isError`; unexpected failures may escape that mapping. C2, X2, M7.                                                                                                                          | N                 | No general durable MCP call/error log is installed. Selected merge request/invocation receipts and task operations are business records, not a complete call log. Process/access logs cannot supply reliable historical counts. M7, M8.                                  |
| Repeated file reads and reads before first edit   | Claude forwards tool names and inputs such as `Read.file_path`; edit/write tools use the same generic path. Codex forwards file changes and shell commands but drops structured `commandActions`, including best-effort read paths. C2, X2, P2.                                               | N                 | No stored read/edit events. Arbitrary shell commands, MCP tools and native children make file coverage incomplete; do not infer reads by parsing commands or file contents.                                                                                              |
| Compactions and rework after compaction/new turn  | Claude parser handles only `system/init`; other system subtypes are dropped. Historical spike documented `system/compact_boundary`, outside the current transport. Codex has `contextCompaction` items and deprecated `thread/compacted`; translator ignores both. C1, X1, X2, P2, M9.        | N                 | No current compaction/context event table. Repeated reads/edits after a boundary could be measured; whether work was redone unnecessarily remains judgment.                                                                                                              |
| Claude input/output and cache tokens              | Synthetic `result.usage` fixture has input/output counts. Parser omits usage; assistant usage is also omitted. Cache-read/write fields have documented API semantics but need fixtures qualified against the pinned CLI. C1, C2, F1, P3.                                                      | N                 | No token ledger. Turn totals do not establish per-model-call context size or exclusive per-native-child usage.                                                                                                                                                           |
| Codex tokens, cache and context                   | Native `token_count` contains usage/rate-limit information; App Server exposes `thread/tokenUsage/updated` with `total`, `last` and optional context-window capacity. Translator has no usage branch. X1, P1, P4.                                                                             | N                 | No usage persistence. Source schema permits cached and cache-write input counts; support/presence still needs pinned synthetic fixtures and a bounded live proof. Capacity is not occupied context.                                                                      |
| Existing telemetry export                         | Claude can enable native OTLP exporters; Codex renders OTEL configuration. Compiler identity and content-capture policy are separate from the A2A event contract. K4.                                                                                                                         | N                 | No Mainloop OTLP ingestion/aggregation path is present. Export configuration does not prove deployment, delivery or complete usage; sampled tracing cannot be the accounting ledger.                                                                                     |
| Design-cohesion findings                          | Existing delegation and `report` can carry a reviewer's judgment about merged changes. M6.                                                                                                                                                                                                    | N                 | Generic reports exist; periodic diff selection, schedule, finding schema and cohesion view do not.                                                                                                                                                                       |
| Model/effort versus task difficulty and outcome   | Harness configuration selects a model; current Claude/Codex configs have no explicit effort field. Parsers do not export actual model/effort. Mainloop stores provider/profile revision and attempts; binding `model` is nullable. K5, M10.                                                   | N                 | No complete observed execution identity, difficulty label, repair count or structured review verdict. Attempts, reports and delivery counts provide partial outcome inputs, not model-fit conclusions. A generic `ProviderExtension` type is not an installed collector. |
| ChatGPT/Codex subscription utilization and resets | App Server defines `account/rateLimits/read` and `account/rateLimits/updated`, with named buckets and primary/secondary windows. Native token-count metadata also has optional limits. Harness neither queries nor translates these. X1, P4, P5.                                              | N                 | No subscription identity/window snapshots or headroom. Provider/account-wide consumption includes activity outside Mainloop; token totals cannot be converted to subscription percent.                                                                                   |
| Claude subscription utilization and resets        | Claude documents plan usage bars in `/usage`. Current harness exposes neither a quota query nor a normalized limit event; unknown stream event types are dropped. Interactive availability is not a supported headless contract. C1, P6.                                                      | N                 | No subscription limit/reset snapshots. API rate limits and session cost estimates are different quantities. Use owner-entered snapshots initially; qualify a supported native limit event/read interface before automating collection.                                   |

### End-to-end boundary at slice 1

At the inventory revision, the path is native CLI → provider driver → shared `EventSink` → A2A artifacts/status
→ kagent gateway storage → Mainloop client projection → delivery ledger and conversation.
`EventSink` exposes session start, text, tool call and tool result, with no health/usage method
(K0). The gateway transactionally stores task projections and events, including artifact data
(K6). Mainloop retains those artifacts only in its in-memory projection; its database keeps
reply text and terminal delivery evidence (M1–M3). `ListTasks` omits artifacts, so reconciliation
must use `GetTask` for each known task (M11).

The old journal's `native_events`, lineage and context-token columns were explicitly removed
(M1). Extend the current kagent path; do not resurrect that adapter or add a session owner.

## Implemented slice 2

The Claude driver now emits allowlisted `turn`, `tool` and `usage` observations through the
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

Mainloop validates the strict shared [event model](../../models/src/models/orchestration_health.py)
and binds attribution through sessions, attempts and delivery receipts. Stream artifacts and
`GetTask` snapshots share the same transactional importer. Delivery handling only queues an
import; one background worker imports from a bounded queue with time-limited database and lock
waits, so health work never delays terminal delivery. A full queue or failed import is left to
recovery. The queue lives in process memory, so imports still queued when Mainloop exits are
lost until recovery re-imports them. The existing reconciliation sweep and the owner read API
recover completed receipts too, and the sweep also applies health retention; neither path sends
a prompt or changes delivery success. Receipt recovery is bounded to the 100 newest receipts and
five seconds per pass, covering every lookup, import and gap write, so an older lost receipt
stays partial; limits are visible, with a session filter available for narrower recovery. Import allows 2,048
health events per receipt and 8 KiB per payload; overflow and integrity failures mark gaps.

The [health ledger](../../backend/src/mainloop/db/health.py) enforces event/sequence uniqueness,
retains source timestamp precision, rebuilds path-free turn summaries and serializes retention
against imports. Same identity/same payload is a no-op; conflicts record bounded categories.
Deleting workspace rows cannot erase persisted attribution. Project deletion cascades health
rows; the owner-removal operation deletes health only. Retention freezes a turn before removing
its detail. Expired replay cannot restore detail or increment frozen totals. Retention settings
are `HEALTH_EVENT_RETENTION_DAYS` (30) and `HEALTH_SUMMARY_RETENTION_DAYS` (365), with summaries
required to outlive detail. Independent report/decision/conversation retention is unchanged.

`GET /orchestration-health` and `GET /orchestration-health/events` are scoped to the configured
owner, including filter authorization. The [/health page](../../frontend/src/routes/health/+page.svelte)
is linked from project and session pages and uses `frontend/src/lib/api.ts`, the existing theme,
HITLCard and NativeIdentityStrip. Its default is 24 hours; filters cover project, product task,
attempt, session and provider. It shows distinct failures, groups of three or more failed calls,
reported input/output, coverage, existing blocked/failed report IDs and approval state. Main and
owner sessions without product tasks appear as **Unassigned**. Product child tasks remain
separate rows, so their totals are not added to the parent's reported root total. Unknown metrics
show **Unavailable**; partial sources and supported-versus-observed turn coverage stay explicit.
Collection gaps count only for receipts or turns inside the selected window. Task, session and
attempt choices list the whole project, provider and window scope, so a scoped entry link does
not restrict later choices.

Fixture acceptance is implemented in
[Mainloop's PostgreSQL tests](../../backend/tests/runtime/test_orchestration_health.py).
The same sanitized A2A data fixture is round-tripped by kagent's gateway PostgreSQL test;
Claude driver/A2A and parked-resume fixtures check the producer boundary. No live provider,
production rollout or subscription proof is claimed.

## React status (migration slice F8)

The React health page (`web/src/features/health`, route `/health`) is tested with MSW against the
owner API contract. The Go handlers for `/orchestration-health` are still unimplemented, so the
page has not been run against live data. Labels follow the Go section's: **implemented** is in the
code and covered by those tests.

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
  whose id repeats an earlier row is dropped and counted the same way.
- Each report and approval is checked on its own. A malformed one is left out of its row, and the
  row says how many were left out. Its totals and other claims still show.
- A session's source observations load only when asked for. A reload clears them.
- Each approval shows its HITL card.
- A failed refresh keeps the rows already shown, with an inline error and a retry.

Not proven:

- The live health path. The Go handlers are stubs, so the page has contract tests only.
- Live event refresh. The page does not yet react to health events.

Differences from the frozen page: native session identity polls only while a turn is in flight. The
frozen page polled every three seconds on each mount.

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

Mainloop validates the schema with shared Pydantic models and persists append-only
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
  API semantics (P3). Codex input already includes cached input (P4); do not add caches again.
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

Codex's supported App Server rate-limit read/notification is the preferred future source (P5).
Keep multiple bucket/windows separate; never sum snapshots from several agents sharing an
account. Reads use the already owned App Server connection, without starting a native turn
or another process merely to refresh quota. Claude's documented interactive usage display is
a possible owner snapshot source,
not proof of a harness-readable endpoint (P6). Qualify native structured subscription telemetry
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

## Vertical slices and acceptance

1. **Inventory/design (implemented):** this proposed spec and roadmap entry, immutable source
   references and synthetic fixture inventory. No product behavior or live qualification.
2. **Claude friction and input/output, one page (implemented, fixtures only):** extend driver/shared sink/A2A schema;
   prove gateway persistence; ingest validated health artifacts from streams and `GetTask`;
   add owner-scoped PostgreSQL turn summaries and one responsive page. Show distinct tool
   failures/repeated failures, existing blocked report claims/approval state, and input/output
   tokens per task and agent. Cache, exact approval durations and other providers show coverage
   gaps until qualified. Keep components shared with existing API/UI patterns.
3. **Complete friction/cache and re-orientation:** first-seen/resume approval events, Mainloop
   MCP status/codes with source correlation, Claude cache/assistant usage and compaction,
   explicit file reads/edits; then Codex usage/items/read actions and compaction. Expand the
   same page and aggregation contract; do not create another telemetry pipeline.
4. **Model and subscription fit:** capture effective model/effort and labeled difficulty/stage;
   link reviews/repairs; collect qualified Codex window snapshots and owner-entered Claude
   snapshots, with provenance, freshness and bounded estimates. This follows friction/cost.
5. **Scheduled design-cohesion review:** owner schedule, bounded merged-range selection,
   existing delegation and stored findings. Track proposed refactors as follow-up work.

Slice 2 acceptance uses sanitized fixtures/fakes and disposable PostgreSQL, not live agents:

- Three distinct failing MCP calls produce three failures and one repeated-failure group;
  replaying one artifact and importing the final task snapshot changes neither count.
- Two turns reporting input/output `3/2` and `4/1` produce `7/3` for their task, across a
  backend restart. Missing usage remains unavailable. Resume segments do not double-count.
- A gateway round trip retains typed data and event IDs. A dropped stream recovers through
  `GetTask`; an incomplete/truncated source stays visibly partial.
- Sensitive contents, unknown fields and forged attribution are rejected; permitted relative
  paths survive. Another owner's task/session IDs cannot be queried through health endpoints.
- Main/owner sessions without tasks remain visible. Task, attempt and agent totals reconcile
  without counting inclusive parent/native-child usage twice. Collection failures do not
  change delivery/task state. Retention/replay checks cannot resurrect expired detail.

Before claiming the slice live, perform one explicitly authorized, bounded Claude task that
produces a known tool failure and usage, restart the consumer, and verify the same totals on
the page. Bind evidence to exact deployed Mainloop/kagent revisions. Default checks remain
offline; no full suite, subscription-consuming run or deployment is authorized by this spec.

Later fixtures cover cache denominators/unknowns, compaction boundaries, cumulative reset,
declined Codex calls, counter inclusion, native children, model reroutes and concurrent agents
sharing one subscription. Window reset/staleness must not manufacture fresh headroom.

## Evidence references

Mainloop paths below refer to the inventory revision; kagent links pin the fork revision.
Codex source links pin `rust-v0.148.0` commit `3ba0f711642a888aec92a611a3f3b2211157ff89`,
the harness-pinned version. Provider documentation describes interfaces/semantics, not live
availability through this adapter.

| Key | File:line and observation                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| --- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| C1  | [go/harness/claude/internal/driver/parser.go:74](https://github.com/oldsj/kagent/blob/798607072a204b47332bd2e26c5f7f229e733358/go/harness/claude/internal/driver/parser.go#L74): envelope omits usage; system handles init only; result produces completion/failure.                                                                                                                                                                                                                                                                                                                                                                 |
| C2  | [go/harness/claude/internal/driver/parser.go:169](https://github.com/oldsj/kagent/blob/798607072a204b47332bd2e26c5f7f229e733358/go/harness/claude/internal/driver/parser.go#L169): assistant tool input; `parseUser` at line 232 preserves result/error. [process.go:515](https://github.com/oldsj/kagent/blob/798607072a204b47332bd2e26c5f7f229e733358/go/harness/claude/internal/driver/process.go#L515) forwards these into the shared sink.                                                                                                                                                                                      |
| X1  | [go/harness/codex/internal/driver/events.go:32](https://github.com/oldsj/kagent/blob/798607072a204b47332bd2e26c5f7f229e733358/go/harness/codex/internal/driver/events.go#L32): text/item/terminal switch; unknown notifications ignored.                                                                                                                                                                                                                                                                                                                                                                                             |
| X2  | [go/harness/codex/internal/driver/events.go:95](https://github.com/oldsj/kagent/blob/798607072a204b47332bd2e26c5f7f229e733358/go/harness/codex/internal/driver/events.go#L95): item projection excludes read actions/compaction and forwards command/file/MCP data; error flag at line 156.                                                                                                                                                                                                                                                                                                                                          |
| X3  | [go/harness/codex/internal/driver/process.go:184](https://github.com/oldsj/kagent/blob/798607072a204b47332bd2e26c5f7f229e733358/go/harness/codex/internal/driver/process.go#L184): MCP-only approval policy; elicitation routing at line 263.                                                                                                                                                                                                                                                                                                                                                                                        |
| K0  | [go/harness/runtime/runtime.go:35](https://github.com/oldsj/kagent/blob/798607072a204b47332bd2e26c5f7f229e733358/go/harness/runtime/runtime.go#L35): current sink vocabulary.                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| K1  | [go/harness/runtime/a2a/executor.go:350](https://github.com/oldsj/kagent/blob/798607072a204b47332bd2e26c5f7f229e733358/go/harness/runtime/a2a/executor.go#L350): terminal status.                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| K2  | [go/harness/runtime/a2a/executor.go:433](https://github.com/oldsj/kagent/blob/798607072a204b47332bd2e26c5f7f229e733358/go/harness/runtime/a2a/executor.go#L433): tool artifacts; typed tool result includes `isError` at line 483.                                                                                                                                                                                                                                                                                                                                                                                                   |
| K3  | [go/harness/runtime/a2a/executor.go:500](https://github.com/oldsj/kagent/blob/798607072a204b47332bd2e26c5f7f229e733358/go/harness/runtime/a2a/executor.go#L500): structured HITL encoding.                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| K4  | [go/harness/claude/internal/adapter/adapter.go:249](https://github.com/oldsj/kagent/blob/798607072a204b47332bd2e26c5f7f229e733358/go/harness/claude/internal/adapter/adapter.go#L249), [codex/internal/adapter/adapter.go:231](https://github.com/oldsj/kagent/blob/798607072a204b47332bd2e26c5f7f229e733358/go/harness/codex/internal/adapter/adapter.go#L231) and [go/pkg/tracing/runtime.go:151](https://github.com/oldsj/kagent/blob/798607072a204b47332bd2e26c5f7f229e733358/go/pkg/tracing/runtime.go#L151): optional OTLP/native identity and content policy.                                                                 |
| K5  | [go/harness/claude/config/config.go:71](https://github.com/oldsj/kagent/blob/798607072a204b47332bd2e26c5f7f229e733358/go/harness/claude/config/config.go#L71), [codex/config/config.go:27](https://github.com/oldsj/kagent/blob/798607072a204b47332bd2e26c5f7f229e733358/go/harness/codex/config/config.go#L27) and [codex/internal/driver/process.go:133](https://github.com/oldsj/kagent/blob/798607072a204b47332bd2e26c5f7f229e733358/go/harness/codex/internal/driver/process.go#L133): configured model, no effort field, thread start/resume selects model.                                                                    |
| K6  | [go/core/internal/database/session_tasks.go:327](https://github.com/oldsj/kagent/blob/798607072a204b47332bd2e26c5f7f229e733358/go/core/internal/database/session_tasks.go#L327) and [task_events.go:135](https://github.com/oldsj/kagent/blob/798607072a204b47332bd2e26c5f7f229e733358/go/core/internal/database/task_events.go#L135): transactional task/event storage and artifact projection.                                                                                                                                                                                                                                     |
| M1  | [backend/src/mainloop/db/postgres.py:178](../../backend/src/mainloop/db/postgres.py#L178): bindings and delivery schema; removed journal/context tables at line 244.                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| M2  | [backend/src/mainloop/runtime/native_sessions.py:1720](../../backend/src/mainloop/runtime/native_sessions.py#L1720) and [native_sessions.py:1930](../../backend/src/mainloop/runtime/native_sessions.py#L1930): stream fold persists text and delivery transitions, final reply and failure evidence.                                                                                                                                                                                                                                                                                                                                |
| M3  | [backend/src/mainloop/runtime/kagent_client.py:175](../../backend/src/mainloop/runtime/kagent_client.py#L175) and [kagent_client.py:302](../../backend/src/mainloop/runtime/kagent_client.py#L302): data parts supported, artifacts in memory, text-only reply projection.                                                                                                                                                                                                                                                                                                                                                           |
| M4  | [backend/src/mainloop/db/hitl_schema.py:39](../../backend/src/mainloop/db/hitl_schema.py#L39): request/response/member/transport storage and immutable receipts.                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| M5  | [backend/src/mainloop/db/hitl.py:134](../../backend/src/mainloop/db/hitl.py#L134): refresh overwrites observation time; [hitl.py:301](../../backend/src/mainloop/db/hitl.py#L301) records decisions before transport.                                                                                                                                                                                                                                                                                                                                                                                                                |
| M6  | [backend/src/mainloop/tasks/reports.py:24](../../backend/src/mainloop/tasks/reports.py#L24) and [db/task_schema.py:170](../../backend/src/mainloop/db/task_schema.py#L170): durable attempt-scoped report claims/outcomes/intents.                                                                                                                                                                                                                                                                                                                                                                                                   |
| M7  | [backend/src/mainloop/mcp_app.py:103](../../backend/src/mainloop/mcp_app.py#L103), [mcp_app.py:151](../../backend/src/mainloop/mcp_app.py#L151) and [mcp_app.py:214](../../backend/src/mainloop/mcp_app.py#L214): error mapping, authentication and invocation; no general call log hook.                                                                                                                                                                                                                                                                                                                                            |
| M8  | [backend/src/mainloop/db/merge_schema.py:44](../../backend/src/mainloop/db/merge_schema.py#L44) and [db/task_schema.py:56](../../backend/src/mainloop/db/task_schema.py#L56): selected business request receipts, not all MCP calls.                                                                                                                                                                                                                                                                                                                                                                                                 |
| M9  | [docs/spikes/native-main-thread-context.md:38](../spikes/native-main-thread-context.md#L38): historical journal/compaction proof; metadata description at line 51.                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| M10 | [models/src/models/provider.py:29](../../models/src/models/provider.py#L29), [db/task_schema.py:24](../../backend/src/mainloop/db/task_schema.py#L24), [models/task.py:136](../../models/src/models/task.py#L136), [models/native_agent.py:40](../../models/src/models/native_agent.py#L40): profile/attempt metadata, no task difficulty, generic optional provider extension.                                                                                                                                                                                                                                                      |
| M11 | [backend/src/mainloop/runtime/kagent_client.py:1525](../../backend/src/mainloop/runtime/kagent_client.py#L1525): task list needs follow-up `GetTask` for artifacts.                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| F1  | [go/harness/claude/testdata/stream-success.jsonl:7](https://github.com/oldsj/kagent/blob/798607072a204b47332bd2e26c5f7f229e733358/go/harness/claude/testdata/stream-success.jsonl#L7) and [stream-resume.jsonl:3](https://github.com/oldsj/kagent/blob/798607072a204b47332bd2e26c5f7f229e733358/go/harness/claude/testdata/stream-resume.jsonl#L3): synthetic usage `3/2` and `4/1`.                                                                                                                                                                                                                                                 |
| F2  | [go/harness/claude/testdata/stream-permission-idle-retry.jsonl:1](https://github.com/oldsj/kagent/blob/798607072a204b47332bd2e26c5f7f229e733358/go/harness/claude/testdata/stream-permission-idle-retry.jsonl#L1), [codex/testdata/app-server-tools.jsonl:1](https://github.com/oldsj/kagent/blob/798607072a204b47332bd2e26c5f7f229e733358/go/harness/codex/testdata/app-server-tools.jsonl#L1), [backend/tests/runtime/fixtures/kagent/turn-stream.sse:1](../../backend/tests/runtime/fixtures/kagent/turn-stream.sse#L1): existing sanitized call/retry and Mainloop text-stream examples. None proves current health persistence. |
| P1  | [codex-rs/app-server-protocol/src/protocol/common.rs:1770](https://github.com/openai/codex/blob/3ba0f711642a888aec92a611a3f3b2211157ff89/codex-rs/app-server-protocol/src/protocol/common.rs#L1770) and [v2/thread.rs:1732](https://github.com/openai/codex/blob/3ba0f711642a888aec92a611a3f3b2211157ff89/codex-rs/app-server-protocol/src/protocol/v2/thread.rs#L1732): token usage notification and counts/window fields.                                                                                                                                                                                                          |
| P2  | [codex-rs/app-server-protocol/src/protocol/v2/item.rs:124](https://github.com/openai/codex/blob/3ba0f711642a888aec92a611a3f3b2211157ff89/codex-rs/app-server-protocol/src/protocol/v2/item.rs#L124) and [item.rs:398](https://github.com/openai/codex/blob/3ba0f711642a888aec92a611a3f3b2211157ff89/codex-rs/app-server-protocol/src/protocol/v2/item.rs#L398): typed read actions and compaction item; [common.rs:1816](https://github.com/openai/codex/blob/3ba0f711642a888aec92a611a3f3b2211157ff89/codex-rs/app-server-protocol/src/protocol/common.rs#L1816): deprecated compacted notification.                                |
| P3  | [Claude prompt caching: token breakdown](https://platform.claude.com/docs/en/build-with-claude/prompt-caching): input/cache field semantics, not pinned harness fixture qualification.                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| P4  | [codex-rs/protocol/src/protocol.rs:2068](https://github.com/openai/codex/blob/3ba0f711642a888aec92a611a3f3b2211157ff89/codex-rs/protocol/src/protocol.rs#L2068), [protocol.rs:2157](https://github.com/openai/codex/blob/3ba0f711642a888aec92a611a3f3b2211157ff89/codex-rs/protocol/src/protocol.rs#L2157) and [protocol.rs:2241](https://github.com/openai/codex/blob/3ba0f711642a888aec92a611a3f3b2211157ff89/codex-rs/protocol/src/protocol.rs#L2241): native token-count/optional limits and cached input as a subset.                                                                                                           |
| P5  | [codex-rs/app-server-protocol/src/protocol/common.rs:1140](https://github.com/openai/codex/blob/3ba0f711642a888aec92a611a3f3b2211157ff89/codex-rs/app-server-protocol/src/protocol/common.rs#L1140), [common.rs:1806](https://github.com/openai/codex/blob/3ba0f711642a888aec92a611a3f3b2211157ff89/codex-rs/app-server-protocol/src/protocol/common.rs#L1806) and [v2/account.rs:531](https://github.com/openai/codex/blob/3ba0f711642a888aec92a611a3f3b2211157ff89/codex-rs/app-server-protocol/src/protocol/v2/account.rs#L531): account rate-limit read/notification and windows; no harness subscription query.                 |
| P6  | [Claude Code costs: usage command](https://code.claude.com/docs/en/costs#using-the-usage-command): interactive plan bars and distinction from local API-cost estimates.                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
