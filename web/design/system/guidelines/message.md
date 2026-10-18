# Message

Source: `docs/specs/chat.md` (Sending messages), `docs/specs/sessions.md`.

## Purpose

One user or assistant message in a conversation. A user message also carries its delivery state.

## Tokens

- User message on `bg-card`; assistant message on `bg-background`. Border `border-border`.
- Text `text-foreground`; timestamps and metadata `text-muted-foreground`.
- Failure notice `text-destructive`; unconfirmed notice uses the warning token; queued notice and bubble use the muted token.
- Radius `rounded-none`. Spacing from the spacing scale only.

## States

- **Sending:** the delivery is in flight. Show "Working" in muted text.
- **Sent:** no notice; the message is plain.
- **Queued:** a message recorded behind the turn in flight, not yet sent to the agent. Dim the whole bubble, not only the notice, and show a `Queued` notice; no Retry, since nothing has been attempted. Clears to **Sent** on its own once the delivery ledger reports the row promoted off `queued` — never on a client timer.
- **Failed (not sent):** a `Failed` notice with the short reason from the server (up to 300 characters). Retry is offered only on the newest user message, and only when nothing is in flight.
- **Unconfirmed:** an `Unconfirmed` notice. No Retry, because the original may have been accepted. The message stays locked until the server reports the outcome.
- **Assignment:** a message Mainloop wrote on the owner's behalf, such as a delegated brief (its delivery's `source` is not `user`). It is a collapsed "Assignment from Mainloop" disclosure with the time, not a message from "You". Its text opens on demand.
- **Loading:** a placeholder row of the same height as a short message, so the list does not jump.
- **Stale:** a reloaded message keeps its last known state and shows the time it was last confirmed.
- **Error:** if the history cannot load, keep the last known messages and show a retry control above the list.

## Keyboard and screen reader

- Each message is an article with a name made from the author and time.
- Failure and unconfirmed notices are text, not colour alone. Use `role="status"` for a notice that appears after a send.
- The Retry button is reachable by Tab and has the name "Retry sending this message".
- Links inside messages are reachable in reading order.

## Touch

- Retry and any message action button are at least 44 px tall.

## Do

- Show the reason exactly as the server sent it, escaped as plain text.
- Keep the message text visible when a delivery fails.
- Open links in a new tab with `rel="noopener noreferrer"`.

## Don't

- Don't render assistant content as HTML. Use the escaped Markdown renderer.
- Don't show Retry on an unconfirmed message.
- Don't re-send a failed message with its old ID; Retry creates a new message.
- Don't render a queued message the same as one already sent to the agent; the owner must be able to tell them apart at a glance.
