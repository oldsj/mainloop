# Delivery receipt

Source: `docs/specs/chat.md` (Sending messages), `docs/specs/sessions.md`.

## Purpose

The small notice and status words that say what happened to a sent message: in flight, failed, or unconfirmed. Shown on the message and in the conversation header and identity strip.

## Tokens

- Failed uses `text-destructive`. Unconfirmed uses the warning token. In flight uses `text-muted-foreground`.
- Notice background `bg-muted`; border `border-border`. Radius `rounded-none`.

## States

Status words, in order of precedence:

1. **working:** a delivery is still in flight. This takes precedence over failed or unconfirmed.
2. **last message failed:** the newest delivery failed (not sent).
3. **last message unconfirmed:** the outcome is unknown.
4. **ready:** nothing is in flight and the last delivery succeeded.

Each status has text, so colour is never the only signal.

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

## Do

- Show the reason beside the notice, not hidden behind a tooltip.
- Use the same words everywhere: failed, unconfirmed, working, ready.

## Don't

- Don't invent a fourth status for "sending". The server reports working.
- Don't show a green tick for a message that is only accepted, not confirmed.
- Don't put the reason in a tooltip only; it must be readable without hovering.
