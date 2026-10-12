# Main thread header

Source: `docs/specs/chat.md` (Main thread header), `docs/specs/sessions.md`.

## Purpose

A one-line summary above the main conversation: whether the agent is ready, the model, how many topics are waiting on the owner, and a stop button while a turn runs. A details toggle opens the identity strip and the topic index.

## Tokens

- Surface `bg-card`, border `border-border`, text `text-muted-foreground`; the state word is `text-foreground`.
- State marker: ready `bg-term-green`, idle `bg-term-fg-muted`, working and unconfirmed `bg-term-yellow` (working pulses), failed and unreachable `bg-term-red`. The marker is a square (`rounded-none`).

## States

Status words, most urgent first: **unreachable** (the backend cannot be reached, so the last state is stale), **working**, **last message failed**, **last message unconfirmed**, **idle** (session suspended), **ready**. The word always shows, so colour is never the only signal. The same words are used by the delivery receipt.

- **Model:** the model name without the `claude-` prefix; the agent kind when the session has no model.
- **Pending:** shown only when topics are waiting on the owner; the sum across topics.
- **Stop:** shown only while a turn is in flight. Disabled while the stop request runs ("stopping…") and while offline.
- **Details:** closed by default; it also holds the provider row unless the provider needs attention (see Provider row). Open shows the identity strip (supplied by the sessions slice) and the topic index, `name (status line) [n pending]`, or "none yet".

## Provider row

Inside the details while the provider is ready or not started yet. It moves up under the summary when the provider is failed, switching, unknown or unconfirmed, the thread is offline, or the last selection left a notice, so a problem is never hidden behind the toggle. It holds the owner's select (Claude or Codex), the effective provider, state words, and Retry or Restart after a failed start or write.

- **Select:** labelled "Main provider". Held while the header is held: offline, a send or stop in flight, the thread or conversation still loading, a switch pending, or a turn or delivery open. Choosing the ready provider again sends nothing.
- **State words:** offline (selection unavailable), selecting, provider state unconfirmed, state unknown, not started (starts with your next message), preparing, failed, ready, waiting to switch. Plain text beside the select, in a polite `role="status"` region so a change is announced without moving focus.
- **Notice:** a refused switch shows one line, a `role="alert"` in `text-destructive`, bounded to 300 characters. Its wording follows the refusal code; with no known code it shows the server's detail. Each notice ends "State is refreshed; no automatic retry."
- **Retry and Restart:** Retry is offered for a failed start, a failed write until a read confirms it, or a pending switch. Retry is held by the select's rule except a pending switch, so it stays available while the select is held. Restart is offered only for a recoverable failed start and is held by the select's rule.

## Keyboard and screen reader

- The state word is a polite `role="status"`.
- The details toggle sets `aria-expanded` and `aria-controls`.

## Touch

- Stop and the details toggle are at least 44 px tall.

## Don't

- Don't show a failed or unconfirmed last message as ready or working.
- Don't show the last known state as live when the backend is unreachable.
- Don't retry a refused switch on its own. The notice says nothing was retried, and the owner decides.
- Don't show a provider as ready until a read confirms it.
