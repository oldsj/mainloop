# Chat

The main thread is a continuous conversation with Claude that persists across devices.

## Sending Messages

- Input field with placeholder "Enter command..."
- EXEC button submits the message
- Message appears in conversation immediately
- Assistant response streams in below

## Conversation History

- Messages persist across page reloads
- Context maintained in follow-up messages
- User messages and assistant responses displayed in sequence

## Spawning Sessions

From the main thread, you can ask Claude to spawn sessions:

- Sessions appear as colored thread blocks in the timeline
- Session messages surface as thread notifications
- Click to expand inline or zoom to fullscreen view

## Native Agent Session Chat (implemented in the local kind slice)

In a session bound to a native agent (see `sessions.md`), the chat tab shows the user's messages and the agent's
replies. Replies are read only from the agent's native journal (Claude transcript, Codex rollout), never from the
terminal, and are mirrored into the conversation once per completed turn. The chat refreshes every few seconds
while the page is open.

## Native Main Thread (implemented in the local kind slice; flag `MAIN_THREAD_MODE=native`)

With `MAIN_THREAD_MODE=native` the home chat talks to a native Claude Code session running under Herdr in its own
pod (`main-0`), instead of running a Claude Agent SDK query per message. The SDK path is unchanged with
`MAIN_THREAD_MODE=sdk` (the default).

- **One main thread.** The conversation is the user's most recent main-thread conversation. A message is recorded,
  then delivered once through the delivery ledger; the reply is mirrored from the native journal, so the page polls
  until the turn completes. While a turn (or a rotation) is in flight a second message is rejected (`409`).
- **Identity strip.** Above the chat: agent, model (from the journal), policy, native session id, Herdr pane, pod,
  generation, window number, turns in the window, last context size and its baseline, and native compaction count.
- **Short window by rotation.** The native session is disposable. When the context grew by 20,000 tokens over the
  window's first-turn baseline (or after 12 turns), Mainloop asks the agent to write anything durable through the
  CLI (one ledgered turn), stops it, and starts a fresh native session whose start-up context is generated from
  Postgres: standing context, topic index, checkpoint, open pending intent and the last 6 visible messages. The
  new session's transcript contains none of the earlier conversation. Native auto-compaction is left at its
  default; no compaction was observed below the rotation budget in the measured sessions (the default threshold is
  assumed, not verified).
- **Dispatcher only.** The agent's only tool is Bash restricted to `mainloop ...`; it has no repository. It records
  facts with `mainloop note|decide|pending`, files work with `mainloop delegate --topic ... --kind claude|codex`,
  and answers "what is the child doing" from `mainloop status|read`, which read Postgres and never message the
  child. On request it ends a running child with `mainloop cancel <id>` and tidies the user's list with
  `mainloop clear` (finished children only; records are kept).
- **Topics.** A topic is a durable record (name, status line, notes, decisions, pending intent, child reports), not
  a session. The topic index (names, status, pending counts) is shown under the identity strip.
- **Child reports.** A delegated child appears in the session list marked `↳` with its topic. Its
  `mainloop report` (or, as a fallback, the last reply of a turn that ended without one) is recorded on the topic
  and delivered to the main thread as a message. A report that arrives while the main thread is busy is queued
  and delivered when it is idle.
- **Server-side policy.** At most 3 concurrent children per parent (6 in total), depth limit 2, and only the main
  thread may delegate in this release; refusals are shown to the agent as `[concurrency]`, `[role]`, `[depth]`.

- **Security limits.** The per-binding token scopes what the `mainloop` CLI may do; it is not a security boundary.
  The rest of the backend API is unauthenticated and reachable from the workspace pods, and agents that share a pod
  can read each other's token files, so a hostile agent could bypass the policy. Child reports are relayed to the
  main thread as untrusted data (the main thread is told not to obey them). A prompt whose turn never completes
  becomes `uncertain` (agent gone or after 30 minutes) and never blocks the session; a rotation closes the old
  window's open deliveries the same way.

Not implemented: topic supervisors, per-child turn budgets, approvals/attention for children, a UI for correcting
a topic assignment, and recovery of a queued or `recorded` delivery after a backend restart.
