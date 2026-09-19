# Codex native-agent fixture boundary

Status: implemented normalizer and sanitized fixture evidence only. This
document does not claim a live Codex proof, production wiring, transport
ownership, or subscription-backed capability.

## Boundary

`backend/src/mainloop/runtime/codex.py` is a side-effect-free adapter. It
accepts a source envelope containing:

- `source_cursor`: a positive integer supplied by the source; the adapter does
  not allocate, renumber, or sort cursors;
- `raw_evidence_ref`: an immutable reference to the sanitized source record;
- `ingested_at` and optional `source_at` timestamps;
- optional ownership and logical-message identifiers; and
- one native event object using the fixture's `type`/`params` shape, or the
  native `method`/`params` shape with an optional JSON-RPC `id`.

The adapter validates the envelope with the shared native-agent models and
returns `NativeEvent` records. `observe_codex_event` additionally returns a
fixture-local evidence classification and, when present, a delivery signal.
`CodexFixtureAdapter` is only a convenience facade around those pure
functions. It does not start Codex, open a transport, write a database, or
advance a delivery attempt.

Native session identity remains on `NativeBinding.native_session_id`. Native
event, item, turn, and thread identifiers are retained in the typed provider
extension when the source exposes them. The extension holds one
`native_event_id`, so the most specific available identifier is kept in this
order: an explicit event ID (`native_event_id`, `event_id`, or `eventId`), a
plain `event.id` on an event without a JSON-RPC `method`, the item ID
(`item.id`, then `params.itemId`), the
turn ID (`params.turn.id`, then `params.turnId`), and the thread ID
(`params.thread.id`, then `params.threadId`). A missing identifier stays
`None`. The `id` of a JSON-RPC request (an event with a `method`) is never an
event ID; it is the attention correlation ID. Model, effort, runtime version, and
usage values are optional observations: an absent value stays `None`; no
default model, effort, zero usage, or synthetic source reference is created.
`NativeBinding.provider` identifies the bound adapter (`codex`). When the
native thread exposes `modelProvider`, `ProviderExtension.provider` preserves
that observed value (for example, `openai`); if it is absent, the required
extension provider field retains the binding identity without claiming that a
model provider was observed. A structured `params.thread.status` such as
`{"type":"idle"}` is thread state, not a turn terminal status.

## Normalization covered by the fixtures

The checked-in records under `backend/tests/runtime/fixtures/codex/` are
sanitized synthetic examples, not copied session logs. The table describes
what the fixture tests prove about this normalizer.

| Native evidence                                                                                                                                                                 | Shared event                                 | Fixture result                                                                                                                                      |
| ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------- |
| `thread/started`                                                                                                                                                                | `unknown`                                    | Preserves session/event identity and raw reference; existence is not completion.                                                                    |
| `thread/started` with structured `params.thread.status`                                                                                                                         | `unknown`                                    | Accepts native `ThreadStatus` objects such as `{"type":"idle"}` without interpreting them as turn completion.                                       |
| `turn/started`                                                                                                                                                                  | `activity`                                   | Records observed turn activity and exposes a separate `delivered` signal.                                                                           |
| `item/*` with `commandExecution`, `fileChange`, `mcpToolCall`, `webSearch`, or compatibility spellings                                                                          | `activity`                                   | Preserves tool activity without treating it as assistant output.                                                                                    |
| `item/*` with non-empty `agentMessage`/assistant message text or compatibility spellings                                                                                        | `output`                                     | Records output activity only.                                                                                                                       |
| `turn/completed` or an equivalent explicit completion event                                                                                                                     | `completed`                                  | Completion is emitted only from an explicit native completion event with no contradictory status or a supported `completed` status.                 |
| `turn/interrupted` or an explicit interrupted completion status                                                                                                                 | `interrupted`                                | Interruption remains distinct from completion.                                                                                                      |
| terminal event with `inProgress` or an unrecognized status                                                                                                                      | `unknown`                                    | Contradictory or unknown terminal status never emits completion or a completed delivery signal.                                                     |
| empty message/terminal output or idle/keepalive evidence                                                                                                                        | `unknown`                                    | Classified as `quiet`; it never claims completion.                                                                                                  |
| explicit request with a complete attention payload                                                                                                                              | `attention`                                  | Preserves the request and optional logical-message correlation.                                                                                     |
| explicit attention resolution with a key                                                                                                                                        | `attention_resolved`                         | Resolves only the named shared-contract attention key.                                                                                              |
| `item/commandExecution/requestApproval`, `item/fileChange/requestApproval`, `item/permissions/requestApproval` with the JSON-RPC `id` and `params.threadId`, `turnId`, `itemId` | `attention` (`approval`, `boolean`)          | Complete payloads only. The command, file, or permission details are not interpreted or required.                                                   |
| `item/tool/requestUserInput` with the ids above and exactly one plain question                                                                                                  | `attention` (`question`, `text` or `choice`) | Option labels become choices. Multiple questions, secret questions, empty or duplicate options, and options that also allow free text stay unknown. |
| `serverRequest/resolved` with `params.threadId` and `params.requestId`                                                                                                          | `attention_resolved`                         | Resolves the request with the same thread and request ID.                                                                                           |
| native request or resolution that is incomplete, for another thread, or of an unsupported method                                                                                | `unknown`                                    | Keeps cursor, native type, and raw evidence; creates no attention item.                                                                             |
| Any event with a foreign `params.threadId` or `params.thread.id`                                                                                                                | `unknown`                                    | Preserves raw evidence but cannot change this binding's activity, delivery, attention, or completion state.                                         |
| `params.thread.modelProvider`, model, and `params.turn.effort`                                                                                                                  | `activity`/observed metadata                 | Preserves native model/provider/effort values without replacing an observed provider with the binding identity.                                     |
| `thread/tokenUsage/updated` with `params.tokenUsage` or compatibility usage shapes                                                                                              | `usage`                                      | Preserves non-negative input/output counts only when present. Native `last` counts are read before `total` counts when both are present.            |
| compaction/resume/context evidence                                                                                                                                              | `continuation`                               | Records an observation; the adapter does not implement compaction or continuation.                                                                  |
| unrecognized native type                                                                                                                                                        | `unknown`                                    | Retains native type, cursor, and raw evidence reference without guessing semantics.                                                                 |

Receipt, delivery, completion, and interruption signals are returned as
fixture-local `CodexDeliverySignal` values. They are evidence for a later
control-plane transition, not automatic `DeliveryAttempt` mutations. A
transport receipt is not native completion, and a completed native turn does
not by itself prove that an arbitrary logical message was delivered.

`native-wire.jsonl` uses the installed interface's camelCase item vocabulary
and `thread/tokenUsage/updated` event shape. The normalizer has explicit
aliases for those item discriminators and keeps the earlier snake_case fixture
spellings compatible. This is a fixture-backed wire-shape check, not a claim
that every installed Codex mode emits the same records.

`native-thread-status.jsonl`, `foreign-thread.jsonl`,
`native-metadata.jsonl`, and `terminal-unknown-status.jsonl` cover structured
thread state, binding identity isolation, observed model metadata, and
contradictory terminal statuses. Foreign-thread records remain unknown
evidence so the shared projection cannot advance this binding's native state
from another thread.

Native attention correlates on the request ID. The deduplication key is
`codex-request:<threadId>:<requestId>`, derived from the request `id` and from
`params.requestId` on the resolution, so a re-announced request maps to the
same attention item and a resolution resolves only its own request. The shared
projection rejects a resolution that has no accepted request, and that would
stall the cursor. A caller that tracks accepted requests can pass
`attention_keys` to `observe_codex_event`/`normalize_codex_event`; a resolution
for any other key is then kept as `unknown` evidence. Without it the adapter
is stateless and does not know which requests were accepted. Batch helpers do
not carry this state.

Duplicate records retain their original cursor and evidence reference. The
shared `ContractStore` handles idempotent ingestion and cursor-gap projection;
the adapter preserves the order supplied by the caller so a reconnect can
replay from a stored cursor without assigning new source positions.

Malformed envelopes fail before normalization. In particular, missing source
cursors, missing raw evidence references, malformed timestamps, wrong cursor
types, and invalid usage values are not silently repaired. An unknown but
well-formed native event remains a normalized `unknown` event pointing at its
raw evidence.

A logical-message identifier may appear as `logical_message_id` or
`logicalMessageId` on the envelope, the native event, or its `params`. Null
values are ignored, but any two non-null values that differ, or an empty or
non-string value, raise `CodexAdapterError` before an event is emitted. The
adapter never picks a winner by precedence; only a single agreed identifier is
carried into `NativeEvent.logical_message_id`.

## Capability evidence

The following claims are fixture-scoped. They must not be upgraded to `live`
until a separately authorized native proof exercises the actual installed
Codex interface and transport.

`codex_fixture_capabilities()` returns the same claims as typed shared
`CapabilityResult` values, and `CodexFixtureAdapter.capabilities` exposes them.
Each proved or partial claim carries `scope="fixture"` and an `evidence_ref`
that names an existing sanitized fixture record; unsupported claims carry
`scope="fixture"` and no evidence, and live behavior is a single `unknown`
claim with unverified scope.
The declarations are separate from provider metadata on `NativeEvent`.

| Capability                                                                                   | Fixture status                                                                                                                                                                           | Live status                                                  |
| -------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------ |
| Preserve native session and event identity                                                   | proved by sanitized fixtures                                                                                                                                                             | unproved                                                     |
| Accept structured native thread status without treating it as turn completion                | proved by `native-thread-status.jsonl`                                                                                                                                                   | unproved for all live notification variants                  |
| Isolate events from a foreign native thread                                                  | proved by `foreign-thread.jsonl`; foreign activity, delivery, attention, and completion stay unknown                                                                                     | unproved for a live multi-thread stream                      |
| Preserve source cursor/order and raw evidence references                                     | proved, including gaps and reconnect replay through the shared contract                                                                                                                  | unproved for a live cursor protocol                          |
| Preserve observed model/provider/effort metadata                                             | partial: fixture proves `model`, `modelProvider`, and turn `effort` when exposed                                                                                                         | unproved for attribution and all live event shapes           |
| Distinguish receipt, delivery activity, output, completion, interruption, and quiet evidence | partial: only the listed event shapes are covered                                                                                                                                        | unproved                                                     |
| Explicit attention request and resolution                                                    | partial: complete generic payloads and the native approval, single-question user-input, and `serverRequest/resolved` shapes above; replay and re-announcement do not duplicate attention | unproved; sending an answer back to Codex is not implemented |
| Usage visibility                                                                             | partial: input/output counts when exposed                                                                                                                                                | unproved; attribution, limits, and billing remain unknown    |
| Context continuation observation                                                             | partial: compaction/resume-shaped records only                                                                                                                                           | unproved                                                     |
| Reject conflicting logical-message identifiers                                               | proved by `conflicting-logical-message.jsonl`; disagreement across envelope, event, and params is rejected and nothing is ingested                                                       | unproved                                                     |
| Send an answer to an attention request                                                       | unsupported in this adapter                                                                                                                                                              | requires a gated live proof                                  |
| Discovery                                                                                    | unsupported in this adapter                                                                                                                                                              | requires a gated live proof                                  |
| Session creation                                                                             | unsupported in this adapter                                                                                                                                                              | requires a gated live proof                                  |
| Transport ownership                                                                          | unsupported in this adapter                                                                                                                                                              | requires a gated live proof                                  |
| Steering                                                                                     | unsupported in this adapter                                                                                                                                                              | requires a gated live proof                                  |
| Process lifecycle                                                                            | unsupported in this adapter                                                                                                                                                              | requires a gated live proof                                  |
| Live native behavior                                                                         | not applicable to fixtures                                                                                                                                                               | unknown; no Codex process was started                        |

The fixture tests therefore establish deterministic normalization and recovery
inputs, not that Codex emits these records in every mode or that a native
session accepts a message. Existing production paths and the Claude Agent SDK
worker are unchanged.
