# Native-agent boundary inventory

Status: contract implementation and synthetic test cases only. No native-provider
proof, production wiring, database migration, or workspace lifecycle change is
included. `ROADMAP.md` describes the intended architecture; the existing specs
continue to describe user-visible behavior.

## Existing implementation and replacement points

| Boundary | Current source and behavior | Later native-runtime boundary |
| --- | --- | --- |
| Main conversation | `backend/src/mainloop/api.py` `/chat` and `services/chat_handler.py` assemble a prompt from a PostgreSQL summary and recent messages. `get_claude_response` calls the Claude Agent SDK and exposes a Mainloop `spawn_session` MCP tool. | Persist logical intent, then deliver to a bound native session. Native history and tools remain authoritative; do not carry prompt reconstruction into the adapter. |
| Other SDK entrypoints | `backend/src/mainloop/claude_agent.py` contains a direct SDK wrapper. `services/claude_agent.py` calls the HTTP worker and parses text/result/error stream records. `claude-agent/server.py` exposes `/execute` (including a resume session ID and compaction observations) and `/execute/stream`. | Replace deliberately after native proof. HTTP transport errors do not establish that a native prompt was not delivered. These existing entrypoints are unchanged. |
| One-shot job execution | `services/k8s_jobs.py` creates session jobs with prompt/model/callback environment values. `claude-agent/job_runner.py` calls SDK `query`, collects output, native session ID and cost, then posts a result with bounded callback retries. | A stable workspace and native binding must outlive individual process/job identities. Native session IDs must not be confused with product session IDs. No new scheduler or terminal manager belongs in this contract. |
| Durable workflow | `workflows/session_worker.py` provisions a namespace, builds conversation prompts, starts jobs, waits on DBOS result and user-message topics, and updates product session status. Result timeouts currently raise and lead to failure handling. `workflows/main_thread.py` manages user-thread/queue coordination. `workflows/dbos_config.py` configures DBOS queues and replay versioning. | Reuse appropriate durability and routing boundaries later, with explicit delivery uncertainty and fenced ownership. Changing workflow behavior would require a version bump; this slice changes none. |
| Persistence | `db/postgres.py` stores threads, conversations, messages, projects, sessions, queue items and notifications. `workflows/transactions.py` supplies DBOS transaction helpers. `models/session.py` combines product execution and attention-related statuses; `models/workflow.py` carries queue and workflow records. | Add durable bindings, logical messages, attempts, raw-evidence cursors and projections later. Do not treat existing session status or a stored transcript message as a native delivery receipt. |
| Message submission and callbacks | `api.py` `/sessions/{id}/message` saves a user message, then wakes a waiting worker via DBOS. `/internal/sessions/{id}/complete` forwards a job result to the workflow. | Stable logical-message IDs and attempt identities must span submission, delivery and reconciliation. Callback receipt is distinct from native completion evidence. |
| Compaction | `services/compaction.py` invokes SDK summarization and stores derived conversation summaries. `chat_handler.py` and `claude-agent/server.py` also observe SDK `compact_boundary` events. | Keep product summaries separate from native context management. The runtime reports continuation/compaction observations; it does not implement native compaction. Deterministic checkpoints require no summarizer. |
| SSE | `backend/src/mainloop/sse.py` has an in-process per-user event bus with random notification IDs and heartbeat events. `api.py` exposes `/events`. There is no persisted source-cursor replay in this bus. | Reuse notification transport later, backed by durable projections and explicit replay semantics. Browser reconnect alone cannot guarantee missing events are recovered. |
| Frontend | `frontend/src/lib/api.ts`, `sse.ts`, and stores for sessions, session messages, inbox and notifications consume current HTTP/SSE records. `docs/specs/chat.md` and `sessions.md` describe current behavior. | Keep attention, delivery, activity, workspace health and publication distinct in later UI changes. This foundation changes neither HTTP/SSE shapes nor frontend behavior. |

## Implemented local contract

`models/src/models/native_agent.py` owns shared Pydantic records. Runtime code
imports these records rather than redefining durable backend models.
`backend/src/mainloop/runtime/contracts.py` implements a single-binding,
single-process reference store; `projection.py` rebuilds a checkpoint from
serialized events and delivery-attempt snapshots. Neither module imports an SDK,
DBOS, application configuration, database client or transport.

- External dictionaries are validated on entry. Unknown fields, malformed dates,
  naive timestamps, invalid event variants and non-integer cursors/generations
  are rejected. Records are frozen and collection fields are tuples.
- A logical ID names one immutable message envelope, including authority and
  payload references. A duplicate identical request returns the existing record;
  changing content under that ID is a conflict. References must identify immutable
  content in a future persistence layer; this store does not dereference them.
- Attempts are separately identified and correlated to a recorded message. One
  unresolved or successful attempt prevents another send attempt. A retry is
  explicit and gets a new attempt ID only after proven non-delivery or failure
  before entering `sending`. Attempt identity retries return current state.
- Delivery follows `recorded -> queued -> sending -> delivered -> completed`.
  Failure is allowed before sending. Persisting `sending` must precede transport
  activity in a future implementation. Disconnect during sending becomes
  `uncertain`; timeouts cannot turn it into failure or trigger a replay.
  An acknowledged delivery remains delivered across transport loss.
- Resolving uncertainty requires typed evidence tied to the attempt and binding,
  with a non-regressing observation time. `not_delivered` allows an explicit new
  attempt; `delivered` and `completed` prohibit replay. Evidence references are
  assertions supplied by an adapter, not independently authenticated proof here.
- Every store mutation and checkpoint read checks the current ownership
  generation. Takeover is a compare-and-swap increment and conservatively marks
  in-flight `sending` attempts uncertain. Current owners reconcile historical
  uncertain attempts without rewriting their original generation. Production
  generation allocation, authorization and durable fencing remain future work.
- Source cursors are positive, contiguous integer positions starting at one,
  scoped to a binding and never reset by ownership changes. Adapters must map
  their source order to this contract and preserve raw evidence references; they
  must not assign a fresh cursor on replay. Opaque provider tokens belong in
  adapter-side source mapping, not guessed numeric ordering.
  Ownership generations fence ingestion, not source-event order: a new owner can
  fill an earlier cursor gap before an event observed by a previous owner.
  Projection permits decreasing observation generations across source cursors,
  while still rejecting another binding or a future ownership generation.
- The same cursor and semantic payload is one event even if ingestion time or
  ownership generation differs. Live ingestion still requires the current owner
  and retains the original stored event on reconnect. Batch replay selects the
  earliest ownership generation (then ingestion time) as the canonical observation
  regardless of input order. Reusing a cursor for different evidence is a conflict.
  Out-of-order events are retained, but projection stops at the first missing
  cursor. Filling a gap replays the contiguous prefix in source order. Unknown
  events retain raw evidence and advance that prefix without guessing completion.
- Attention keys are scoped to the binding. Repeated requests correlate to one
  item and its first source cursor; conflicting request/message correlations are
  rejected. Resolution explicitly names the key. A new question requires a new
  key; a replay cannot reopen a resolved item. Resolving one request preserves
  waiting status while another request remains pending; resolving the last leaves
  native status unknown until new evidence arrives. Attention state is separate
  from native activity and delivery state.
- Capability results distinguish `proved`, `partial`, `unsupported`, and
  `unknown`; absent capabilities stay unknown. Proved/partial results require
  an evidence reference and fixture/live scope. Unsupported results are returned
  as typed records with no fallback operation. Optional provider metadata is a
  typed extension; absent model, effort and usage remain absent.
- Checkpoints contain the contiguous evidence cursor, latest observed native
  status, source timestamp when available, attention, pending attempts, and
  caller-supplied repository/candidate references. Those references do not prove
  publication or review acceptance. Replay uses historical events plus attempt
  snapshots and the binding's current generation, with no model call.

## Evidence and limits

`backend/tests/runtime/test_contracts.py` contains synthetic unittest cases for
logical and attempt ID conflicts, disconnect uncertainty, evidence-gated retry,
stale ownership, duplicate and out-of-order events, attention correlation,
unsupported capabilities, malformed input, and checkpoint reconstruction.
These are contract examples, not recordings of real Codex or Claude interfaces.
The implementation handoff does not claim the suite has executed.

The controller should run from the repository root:

```sh
uv run --project backend python -m unittest discover -s backend/tests/runtime -p test_contracts.py
```

Require a nonzero test count and successful assertions; empty discovery is not
acceptance evidence. Controller lint and later integrated adapter checks remain
separate gates. No live-provider capability is established by this slice.

This reference store is not thread-safe or durable and does not survive process
loss by itself. A production store needs transactions, uniqueness constraints,
authenticated evidence, append-only attempt history and ownership authorization.
There is no network delivery, automatic retry, provider adapter, process owner,
Kubernetes operation, subscription use, or change to existing call paths here.
