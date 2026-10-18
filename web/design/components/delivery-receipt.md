# Delivery receipt

Source: `docs/specs/chat.md` (Sending messages), `docs/specs/sessions.md`.

## Purpose

The small notice on a message, and the status words in the conversation header and identity strip, that say what happened to a sent message: queued, in flight, failed, or unconfirmed.

## Tokens

- Failed uses `text-destructive`. Unconfirmed uses the warning token. Queued and in flight use `text-muted-foreground`.
- Notice background `bg-muted`; border `border-border`. Radius `rounded-none`.

## States

The header and identity strip show one status word for the whole thread, in order of precedence:

1. **working:** a delivery is still in flight. This takes precedence over failed or unconfirmed.
2. **last message failed:** the newest delivery failed (not sent).
3. **last message unconfirmed:** the outcome is unknown.
4. **ready:** nothing is in flight and the last delivery succeeded.

Each status has text, so colour is never the only signal. The per-message notice is more specific than the thread's one status word: a message can show `Queued`, `Not sent`, `Failed` or `Delivery unconfirmed`, matching its own delivery row rather than the thread as a whole — a queued message does not make the thread status "working", because nothing of its own is in flight yet.

## Keyboard and screen reader

- The notice is text next to the message it belongs to.
- Use `role="status"` so a change to failed or unconfirmed is announced once.
- Do not announce on every poll. Announce only when the state changes.
- The reason text is readable by a screen reader in full (up to 300 characters).

## Touch

- Any control in the notice (Retry, details) is at least 44 px tall.

## Behaviour that must hold

- A failed delivery keeps its reason after a reload. It is read from the stored delivery.
- An unconfirmed delivery is never replayed by the UI. Refreshing reads the status and never resubmits.
- Failed and unconfirmed are different words. Do not call an unconfirmed send "failed".
- A queued message's notice comes only from its own delivery row being `queued`; it clears when a read shows that row promoted, never on a client-side timer or guess.

## Do

- Show the reason beside the notice, not hidden behind a tooltip.
- Use the same words everywhere: failed, unconfirmed, working, ready, queued.

## Don't

- Don't invent a fourth status for "sending". The server reports working.
- Don't show a green tick for a message that is only accepted, not confirmed.
- Don't put the reason in a tooltip only; it must be readable without hovering.
- Don't let a queued message's bubble or notice imply it was sent to the agent; it has not been.
