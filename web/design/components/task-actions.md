# Task actions

Source: `docs/specs/tasks.md`.

## Purpose

The owner's actions on one task: Retry, Reassign and Cancel. The server decides what is allowed. The panel only sends what the owner saw, and it says so when a request's outcome is unknown.

## Tokens

- Buttons: the shared button. Destructive Cancel uses the destructive token; Retry and Reassign use the outline style.
- Notices: info `text-term-green`, warn `text-term-yellow`, error `text-destructive`. The notice is a plain block with `gap-xs`, `text-sm`.
- Resend panel: `border-term-yellow/50` on its edge, with the original request's version, attempt and target in `text-xs`.
- Radius `rounded-none`. Tokens only.

## States

- **Available:** the button is enabled. The server reported the action available, nothing is in flight, the connection is up, and no unconfirmed request is held for this task.
- **Not available:** the button is disabled, and the reason beside it is the server's copy (for example `workspace provisioning is not available yet`).
- **Handoff not installed:** while the server reports `handoff_unavailable`, Retry and Reassign are not rendered at all. One plain line says the provider handoff is not installed yet. A disabled control that cannot work is never shown.
- **Sending:** the button for the action in flight is disabled, and the other actions wait until it settles.
- **Offline:** every action is disabled with `Mainloop is offline.`
- **Unconfirmed (lost reply):** the request is kept as **Resend original request**, with its original version, attempt and target, and a **Discard** button. Other actions wait until the owner resends or discards it.
- **Reassign picker:** the target is chosen from the enabled other providers that offer the role. While providers load it reads `Loading providers…`; if they fail it shows the error; if none are offered it says `No other provider is available.`
- **Cancel confirmation:** an in-app dialog with **Keep running** (sends nothing) and **Confirm cancel**. It is never `window.confirm`.
- **Refused:** the notice shows the backend message in words, with `Server reason: <code>` beneath. A 409 re-reads the task before the notice appears.

## Keyboard and screen reader

- Every control is a button or a labelled select, reachable in tab order. The picker is labelled `Provider to reassign to`.
- Accepted actions announce through a status region; refusals and unknown outcomes announce as an alert.
- The cancel dialog takes focus, and returns it to the Cancel button when it closes.
- A disabled button keeps its reason on screen, so the reason is read with it.

## Touch

- Every button and the picker have a minimum height of 44 px (`min-h-11`).

## Behaviour that must hold

- A submit carries the version and attempt the owner saw. A second press while a request is in flight sends nothing.
- A lost reply keeps the same request ID on resend. Discard is the only way to drop it.
- A 409 or 404 re-reads the task before the owner is told what happened.
- Cancel is never one tap. The "Keep running" path sends nothing.
- Nothing shows a request as done until the server accepted it. Progress comes from the task, not from the button.

## Do

- Show the server's reason text and quote its code beside it.
- Keep the eligibility decision in `actionAvailability`, and the retry rules in the tracker.

## Don't

- Don't enable an action from the client's own view of the state. The server's `available` is the rule.
- Don't add a silent retry. A resend is the owner's choice.
- Don't use `window.confirm`, and don't use palette colours (red-500, green-600). Tokens only.
