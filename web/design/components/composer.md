# Composer

Source: `docs/specs/chat.md` (Sending messages).

## Purpose

The text box and send button at the bottom of a conversation. It submits one user message to the main session or to a selected child session.

## Tokens

- Surface `bg-background` with `border-input`. Text `text-foreground`; placeholder `text-muted-foreground`.
- Focus ring `ring-ring` using the focus tokens. Send button `bg-primary text-primary-foreground`.
- Radius is `rounded-none` everywhere, so no rounded corners.

## States

- **Empty:** send button disabled; placeholder explains what the box is for.
- **Ready:** text present; send enabled.
- **Sending:** one delivery is in flight. Send is disabled with the reason visible ("Waiting for the current reply"). A second send is refused with `409`, so the UI must not offer one.
- **Refused:** the backend did not record the message (for example a `409` because a turn is running, or no reply at all). The bubble is removed, the text returns to the box, and the error says so ("Your message is back in the box.").
- **Failed:** the backend recorded the message but could not deliver it. It stays in the conversation with its own Failed notice. Do not refill the box with the failed text. Retry lives on the message.
- **Stale:** if the session changed while the user typed, keep the draft and show the refreshed status. Never discard typed text.
- **Offline (proposed):** keep the draft, disable send and say so. Do not mark a message failed because the browser lost its connection; the server records delivery.

## Keyboard and screen reader

- Input is labelled "Message"; the send button is named "Send message".
- Enter sends and Shift+Enter inserts a newline (proposed; confirm with the owner).
- While sending, set `aria-busy` on the composer and announce "Sending" once through a polite live region, not on every poll.
- Focus stays in the box after a send that is accepted.

## Touch

- Send button is at least 44 px tall and 44 px wide.
- The box grows to a few lines, then scrolls. It never pushes the send button off screen.

## Do

- Clear the box only after the server accepts the message.
- Keep the same text available if the user needs to edit before a retry.

## Don't

- Don't send the same text twice from one tap. Disable send at submit.
- Don't use the composer for structured approvals; those go through the HITL card.
- Don't add formatting controls the transport cannot send.
