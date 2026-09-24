# Native main thread and cross-session context (plan r7)

Status labels: **Implemented** = built and exercised on the local kind cluster; **Measured** = observed with real
agents (Claude Code 2.1.278, codex-cli 0.155.1); **Proposed** = design intent not yet built. Nothing here is
production-tested. Fixtures and fakes cover the default tests; live evidence is outside the repository.

Historical note: this plan predates the 2026-09-24 cutover. The mode flag, former workspace transport, and SDK
fallback described below have been removed. Current chat always uses the native Substrate session path.

## What was built (Implemented)

- `MAIN_THREAD_MODE=native`: `POST /chat` records the user message and delivers it, through the r6 delivery ledger,
  to a Claude session under Herdr in pod `main-0` (`runtime/native_sessions.py`, `delegation.py`). The SDK chat is
  unchanged behind `MAIN_THREAD_MODE=sdk`.
- **Rotation, not compaction** (`native_sessions.rotate`): trigger = journal-reported context tokens above the
  lineage's first-turn baseline (default 20,000) or 12 turns. Sequence: one ledgered "write out anything durable"
  turn; stop the old native session; record `native_lineage` (old id -> new id, reason, write-out outcome, carry-over
  hash); start a fresh session with a generated carry-over. If the old session cannot be stopped the rotation
  aborts and nothing is switched.
- **Carry-over** (`standing.py`, `delegation.render_for_binding`): role text, CLI help, topic index, checkpoint of the
  most recently updated topic (status line, recent notes/decisions/reports), open pending intent, last 6 visible
  messages (clipped; undelivered and protocol messages excluded). Rendered from Postgres only, hash stored on the
  binding.
- **`mainloop` CLI** (`spikes/k8s-herdr-agents/bin/mainloop`, bash + curl + jq) and control-plane API
  (`runtime/agent_api.py`): `topics`, `topic open`, `note`, `decide`, `pending [--done]`, `delegate`, `status`,
  `read`, `report`, `standing`. The CLI holds no policy. Identity is a per-binding token (HMAC of the session id,
  hash stored on the binding); every verb is limited to the token's own tree.
- **Policy** (`runtime/policy.py`): 3 concurrent children per parent, 6 globally, depth limit 2, only the main
  thread may delegate in this release, allowed kinds from configuration.
- **Topics** (`topics`, `topic_records` tables): a topic is a durable record; child reports are recorded on it. The
  main thread sees only the topic index.
- **Children**: topic-tagged delegation to one worker (Claude or Codex) in `workspace-0`, scratch cwd
  `/workspace/children/<agent>`; report (or fallback last reply) is delivered to the main thread; a report that
  arrives while the main thread is busy is `queued` and sent when idle.
- **Status without native turns**: `mainloop status|read` read Postgres only.
- **Worker continuation**: a `compact_boundary` in a journal is recorded as a `continuation` event; Claude workers
  get the standing context again through a `SessionStart(compact)` hook that runs `mainloop standing`.

## Measurements (Measured; evidence in the run's `evidence/README.md`)

- The pod reaches the backend Service; no NetworkPolicy exists in `herdr-spike`; the pod has no service-account
  token. The image needed `curl`.
- `SessionStart` (`startup`, `resume`, `compact`) and `PreCompact` hooks fire in 2.1.278 and can be loaded from
  `--settings`; a `SessionStart(compact)` hook's stdout reaches the model (checked: the model could not see the
  standing text before compaction and could after).
- Per-call context size = `input_tokens + cache_creation_input_tokens + cache_read_input_tokens` of the assistant
  record. A trivial session already holds ~20.6k tokens with all tools, ~10.2k with `--tools Bash`, ~16k in the
  live main thread with its standing context. A rotation budget is therefore relative to the window's baseline.
- `compact_boundary` shape: `system/compact_boundary` with `compactMetadata.{trigger,preTokens,postTokens,...}`.
- Restricting the main thread to `Bash(mainloop:*)` (`--tools Bash`, allow rule in `--settings`, `dontAsk`,
  `--disable-slash-commands`) works and the model stayed useful: it recorded notes, delegated, answered status.
  Consequence: `/exit` is unavailable; two quick Ctrl-C key presses (`herdr agent send-keys`) stop it.
- A Herdr paste sent during a running Claude turn is not lost but interleaves with the running turn, so Mainloop
  never sends while a delivery is open and queues reports instead. Codex behaviour is unknown.
- Codex shell tools need `codex-code-mode-host` in the image; without it the child reports it cannot run commands.

## Not proved / open (Proposed or unknown)

- The native auto-compaction knobs (`CLAUDE_CODE_AUTO_COMPACT_WINDOW`, `CLAUDE_AUTOCOMPACT_PCT_OVERRIDE`,
  `DISABLE_AUTO_COMPACT`) did not change behaviour at 21k tokens in `-p` mode; their semantics are unverified and
  no knob is set. The design relies on the default threshold being far above the rotation budget.
- Rotation quality over long conversations, and its cache cost, are not measured beyond a few windows.
- Codex mid-turn delivery, Codex `compact` continuation, and `PreCompact` for Codex are unknown.
- **The token is not a security boundary** (final review A4): the control-plane API outside `/agent-api` is
  unauthenticated and reachable from the pods, so a child running with bypass permissions and `curl` could call it
  directly. A NetworkPolicy cannot fix this (same Service and port); API authentication is a required follow-up
  before any non-local use. Child reports are relayed as untrusted data (A5).
- Tokens are delivered to agents over the exec channel and stored in 0600 files on the pod volume; agents that
  share the `workspace-0` pod (same uid) can read each other's token file. Per-writer pods (D5) remove this.
- Topic supervisors, per-child turn budgets, attention/approvals, and recovery of `recorded`/`queued` deliveries
  after a backend restart are not built.
