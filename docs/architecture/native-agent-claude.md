# Native Claude session adapter

Status: implemented as a sanitized fixture-backed normalizer only. This slice
does not start Claude, use a subscription, import the Claude Agent SDK, or wire
the adapter into a production call path. `ROADMAP.md` remains the intended
architecture; the existing `claude-agent/` worker and its SDK entrypoints are
unchanged.

## Boundary

`backend/src/mainloop/runtime/claude.py` accepts a small fixture envelope around
JSON-shaped native Claude observations:

```json
{
  "source_cursor": 2,
  "raw_evidence_ref": "fixture://claude/stream.json#cursor-2",
  "source_at": "2026-01-01T00:00:01+00:00",
  "event": {
    "type": "assistant",
    "uuid": "claude-event-output-002",
    "session_id": "claude-native-session-fixture",
    "message": {"content": [{"type": "text", "text": "..."}]}
  }
}
```

The envelope is test/runtime evidence, not a claim that Claude itself emits a
numeric cursor or a `fixture://` URI. The runtime that owns the native stream
must provide a stable source cursor and raw-evidence reference. Reconnects must
reuse the source cursor; the shared `ContractStore` handles duplicate
suppression, ownership fencing, source gaps, and checkpoint projection.

The input vocabulary follows the locally available native Claude stream types:
`system`, `assistant`, `user`, `stream_event`, `result`, and the control
protocol's `control_request`/`control_response` records. The adapter uses plain
Pydantic validation and does not import or execute the SDK that defines those
types.

## Normalization

| Native observation | Shared event | Evidence rule |
| --- | --- | --- |
| `system.init` | `activity` | Requires a session ID when building a binding; the binding preserves it. |
| Assistant text | `output` | Text is activity/output, never completion by itself. |
| Tool/thinking or other assistant activity | `activity` | Tool input is not interpreted as a product command. |
| `stream_event` content/message updates | `output` or `activity` | A stream stop marker is not completion. |
| `result` with `subtype=success` and `is_error=false` | `completed` | Both explicit success and the non-error flag are required. |
| Explicit result error | `interrupted` | An error result is not a successful completion; `interruption.json#cursor-3` proves the fixture projection. |
| `control_request` with `can_use_tool` | `attention` | A request ID is the correlation key for a pending boolean approval. |
| Successful permission `control_response` with nested `response.behavior=allow/deny` | `attention_resolved` | Only explicit permission evidence and its request ID resolve attention. |
| Other successful `control_response` records | `unknown` | Initialization, hooks, and permission-mode acknowledgements are not attention resolutions. |
| `system.compact_boundary` | `continuation` | The observation does not implement or prove native resume behavior. |
| Explicit `transport.lost` | `transport_lost` | The checkpoint becomes unknown; no retry or replay is implied. |
| Unrecognized but structurally valid type | `unknown` | The native type, cursor, and raw evidence reference remain available. |

`process_exit` and `quiet` are runtime observations rather than native stream
events. `ClaudeSessionNormalizer.observe_runtime()` retains their evidence and
does not turn either into `completed` or advance the native event journal.
Quiet output therefore leaves the last native status active until stronger
evidence arrives; process exit leaves native completion unproven. Transport
loss is distinct because it is an explicit normalized event that projects an
unknown native status.

Provider metadata is optional. The adapter preserves a native event UUID,
model, runtime version, effort, and input/output token counts only when present
and valid. Missing usage, model, or effort stays `None`; no zero, default model,
cost, or inferred receipt is created. A native session ID that is present on an
event must match the bound session.

## Capability evidence

The adapter exposes `claude_fixture_capabilities()` so callers can keep
fixture-backed claims separate from live-provider claims.

| Capability | State | Scope | Fixture evidence | Live status |
| --- | --- | --- | --- | --- |
| Session identity | proved | fixture | `stream.json#cursor-1` | Native discovery/attachment still needs live proof. |
| Cursor ordering and reconnect deduplication | proved | fixture | `stream.json#cursor-2` | Runtime cursor durability and authenticated reconnect are unproved. |
| Native completion parsing | proved | fixture | `stream.json#cursor-7` | A live Claude result/completion guarantee is unproved. |
| Interruption projection | proved | fixture | `interruption.json#cursor-3` | Live error, cancellation, and process semantics are unproved. |
| Permission attention request | partial | fixture | `stream.json#cursor-4` | Live exposure, user reply delivery, and resolution are unproved. |
| Usage observation | partial | fixture | `stream.json#cursor-2` | Completeness, attribution, and billing semantics are unproved. |
| Continuation observation | partial | fixture | `stream.json#cursor-6` | Native context continuation/resume behavior is unproved. |
| Delivery receipt | unsupported | fixture | No receipt record in the fixture | Requires a separately proven native/runtime signal. |
| Steering | unsupported | fixture | No send operation in this adapter | Requires an explicit runtime delivery contract. |
| History export | unsupported | fixture | A stream is not a history export | Native history ownership remains with Claude. |
| Live native behavior | unknown | unverified | No provider process was started | Must be established by a separate, authorized proof. |

`proved` and `partial` in this table mean that the normalizer behavior is
covered by sanitized fixtures. They do not mean that the corresponding live
Claude capability has been established.

## Existing SDK separation

The current `backend/src/mainloop/claude_agent.py`,
`backend/src/mainloop/services/claude_agent.py`, and `claude-agent/` service use
the existing Claude Agent SDK worker. This adapter does not call those modules,
does not parse their result wrapper as native evidence, and does not change
their production behavior. Replacing those paths requires a later architecture
decision backed by live native proof.

## Required live proof later

Before production wiring, a separately authorized proof must establish native
session discovery/creation, logical-message receipt, completion, attention or
an explicit unsupported result, cursor reconnect, duplicate suppression,
interruption, usage/context signals, and uncertain-send reconciliation. The
proof must use a disposable session, preserve private raw evidence outside the
repository, and classify every capability as proved, partial, unsupported, or
unknown.
