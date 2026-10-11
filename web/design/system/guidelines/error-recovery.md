# Error recovery

Source: `docs/specs/tasks.md` (409 re-read), `docs/specs/chat.md` (failed delivery).

## Purpose

The block shown when a request failed and the owner has a next step: retry, reload, or read why. It replaces a blank panel or a silent failure.

## Tokens

- Border `border-destructive`, radius `rounded-none`, padding `p-md`.
- Title `text-destructive` medium; message `text-muted-foreground`.
- Retry is the outline button.

## States

- **failed, safe to retry:** title, message, and a Try again button.
- **failed, not safe to retry:** title and message only. Do not show a retry that would repeat an action the server already refused.
- **retrying:** the button is disabled until the request settles.
- **stale:** the server refused because the owner's view is old. The message says so, and the next step is to reload, not to retry the same request.

## Keyboard and screen reader

- The block is `role="alert"`. It is announced once, when it appears. Do not re-announce on every poll.
- The message is readable in full, including server detail up to the length the server sends.
- Retry is reachable by Tab and activated by Enter or Space.

## Touch

- The retry button is at least 44 px tall.

## Behaviour that must hold

- Server detail is shown as text, never as markup.
- A retry never replays an action the server might already have applied. An uncertain action is reloaded, not resubmitted.
- The block says what did not change, when that is known.

## Do

- Say what happened in plain words, then the next step.
- Keep the title short: "Could not load the task", "Reassign was refused".

## Don't

- Don't show a bare "Error" or a status code as the message.
- Don't offer retry for a refused request, such as a 403 or an out-of-date version.
- Don't hide the message in a tooltip.
