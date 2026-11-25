# Task actions

Source: `docs/specs/tasks.md`.

## Purpose

The owner's actions on one task: Retry, Reassign and Cancel, and on a task that waits on the owner, the ways to answer it in the main chat. The server decides what is allowed. The panel only sends what the owner saw, and it says so when a request's outcome is unknown.

## Tokens

- Buttons: the shared button, in one row, the actions that move the task on first. The first of them is the primary (filled) button: Retry when the server offers it, else **I fixed it, retry**. The rest use the outline style. Cancel is last, an outline button with `text-destructive`; the filled destructive style is kept for its confirmation, so a red button is never the first thing on a task that needs the owner.
- Notices: info `text-term-green`, warn `text-term-yellow`, error `text-destructive`. The notice is a plain block with `gap-xs`, `text-body`.
- Resend panel: `border-term-yellow/50` on its edge, with the original request's version, attempt and target in `text-caption`.
- Radius `rounded-none`. Tokens only.

## States

- **Available:** the button is enabled. The server reported the action available, nothing is in flight, the connection is up, and no unconfirmed request is held for this task.
- **Not available:** the button is disabled, and its reason is under the row, named, in the server's copy (for example `Retry: workspace provisioning is not available yet`).
- **Handoff not installed:** while the server reports `handoff_unavailable`, Retry and Reassign are not rendered at all, and nothing says why. A disabled control that cannot work is never shown, and how the server is built is not the owner's concern.
- **Waits on the owner:** a task with an open decision, or one that is blocked or failed, offers **I fixed it, retry** (unless the server's Retry is available) and **Answer in chat**. Each opens the main chat with a message about the task in the composer, not sent. **Open session** links to the current attempt's session. One line under the row says the first two open the chat.
- **Sending:** the button for the action in flight is disabled, and the other actions wait until it settles.
- **Offline:** every action is disabled with `Mainloop is offline.`
- **Unconfirmed (lost reply):** the request is kept as **Resend original request**, with its original version, attempt and target, and a **Discard** button. Other actions wait until the owner resends or discards it.
- **Reassign picker:** the target is chosen from the enabled other providers that offer the role. While providers load it reads `Loading providers…`; if they fail it shows the error; if none are offered it is one line, `Reassign: No other provider is available.`, with no empty picker
- **Cancel confirmation:** an in-app dialog with **Keep running** (sends nothing) and **Confirm cancel**. It is never `window.confirm`.
- **Refused:** the notice shows the backend message in words, with `Server reason: <code>` beneath. A 409 re-reads the task before the notice appears.

## Keyboard and screen reader

- Every control is a button or a labelled select, reachable in tab order. The picker is labelled `Provider to reassign to`.
- Accepted actions announce through a status region; refusals and unknown outcomes announce as an alert.
- The cancel dialog takes focus, and returns it to the Cancel button when it closes.
- A disabled button keeps its reason on screen, so the reason is read with it.

## Touch

- Every button and the picker take the control height (`min-h-control`): 44 px on touch.

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
