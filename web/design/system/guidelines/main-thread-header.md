# Main thread header

Source: `docs/specs/chat.md` (Main thread header), `docs/specs/sessions.md`.

## Purpose

The one row above the main conversation: the session's status, how many topics are waiting on the owner, and then its controls: stop while a turn runs, and a **Session info** toggle that opens the identity strip and the topic index. It is drawn by the shared session row (`SessionBar`), which the live view also uses, so there is one row whether the conversation is the chat view or the live view.

## Tokens

- Surface `bg-card`, rule `border-border-subtle`, text `text-muted-foreground`; the state word is `text-foreground`.
- State marker, in the status-dot language: working, starting and waking are filled amber (`bg-status-working`, pulsing); needs you and unconfirmed are filled red (`bg-status-attention`); failed, unreachable and unavailable are filled red (`bg-status-failed`); ready, idle, asleep and paused are hollow (`border border-status-idle`); connecting and reconnecting are hollow and pulse. The marker is a square (`rounded-none`).

## States

There is one status word. In the chat view, most urgent first: **unreachable** (the backend cannot be reached, so the last state is stale), **working**, **last message failed**, **last message unconfirmed**, **idle** (session suspended), **ready**. The word always shows, so colour is never the only signal. The same words are used by the delivery receipt.

Under the live view the row is the same one, and the live channel's state joins the thread's in a single order: **unreachable**; **needs you** (a request or permission prompt is waiting); **working** or **starting**; **last message failed**; **last message unconfirmed**; then what the channel is doing: **asleep**, **paused**, **waking…**, **connecting…**, **reconnecting…**, **unavailable**, or **ready** once attached. A sleeping session reads "asleep", never "ready" or "idle" beside it.

- **Wake now / Resume:** in the row while the live view is asleep or paused. There is no banner; the composer's placeholder says a message wakes or resumes the session.
- **Provider, model and effort** are not in this row. They are the session controls chip by the composer (see `session-controls.md`).

- **Pending:** shown only when topics are waiting on the owner; the sum across topics.
- **Stop:** shown only while a turn is in flight. Disabled while the stop request runs ("stopping…") and while offline.
- **Session info:** an icon button at the end of the row, labelled "Session info", closed by default; it also holds the provider row unless the provider needs attention (see Provider row). Open shows the identity strip (supplied by the sessions slice) and the topic index, `name (status line) [n pending]`, or "none yet".

## Provider row

Inside the session info while the provider is ready or not started yet. It moves up under the row when the provider is failed, switching, unknown or unconfirmed, the thread is offline, or the last selection left a notice, so a problem is never hidden behind the toggle. It holds the effective provider, state words, and Retry or Restart after a failed start or write. The choice of provider is in the session controls.

- **State words:** offline (selection unavailable), selecting, provider state unconfirmed, state unknown, not started (starts with your next message), preparing, failed, ready, waiting to switch. Plain text in a polite `role="status"` region so a change is announced without moving focus.
- **Notice:** a refused switch shows one line, a `role="alert"` in `text-destructive`, bounded to 300 characters. Its wording follows the refusal code; with no known code it shows the server's detail. Each notice ends "State is refreshed; no automatic retry."
- **Retry and Restart:** Retry is offered for a failed start, a failed write until a read confirms it, or a pending switch. Retry is held by the provider choice's rule except a pending switch, so it stays available while the choice is held. Restart is offered only for a recoverable failed start and is held by the choice's rule.

## Keyboard and screen reader

- The state word is a polite `role="status"`.
- The Session info toggle has the accessible name "Session info" and sets `aria-expanded` and `aria-controls`.
- There is one `role="status"` word for the session, in the chat view and under the live view.

## Touch

- Stop, Wake now, Resume and the Session info toggle take the control height (`min-h-control`): at least 44 px on touch.

## Don't

- Don't show a failed or unconfirmed last message as ready or working.
- Don't show the last known state as live when the backend is unreachable.
- Don't retry a refused switch on its own. The notice says nothing was retried, and the owner decides.
- Don't show a provider as ready until a read confirms it.
