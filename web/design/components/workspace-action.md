# Workspace action

Source: `docs/specs/workspaces.md`, `docs/specs/layout.md`.

## Purpose

The controls on a workspace page: refresh, suspend, resume and delete. Each one changes the runtime, so the control must show what it will do and whether it can run now.

## Tokens

- Primary action `bg-primary text-primary-foreground`. Destructive action (delete) `bg-destructive text-destructive-foreground`.
- Disabled controls keep their label and use `text-muted-foreground`.
- Radius `rounded-none`.

## Workspace states

The page shows one of six states, each with a plain-language word: running, suspending, suspended, resuming, failed, unknown.

- **running:** suspend and refresh enabled; resume disabled.
- **suspended:** resume and refresh enabled; suspend disabled.
- **suspending and resuming:** the controls are disabled with the reason, and a progress indicator shows.
- **failed:** show the server's detail. Refresh and delete are enabled.
- **unknown:** the runtime is unreachable or reports no such session. Refresh is the only useful action.

## When a control is refused

- Suspend and delete return `409` while a delivery is open. Disable the control and say why ("A reply is still in progress").
- Delete returns `502` if the runtime cannot confirm deletion. Show that the workspace was kept and offer to try again.

## States

- **Loading:** the buttons show the last known state with a spinner beside the status word.
- **Stale:** show "Last checked" with the time.
- **Error:** show the error text from the server and keep the controls that are still valid.

## Keyboard and screen reader

- Each control is a button with a name that states the action: "Suspend workspace".
- Delete opens a confirmation dialog. Focus moves into the dialog, Escape cancels, and focus returns to the delete button.
- The status word is in a live region so a state change is announced once.

## Touch

- Every control is at least 44 px tall, with at least 8 px between destructive and other actions.

## Do

- Ask before delete. Name the workspace in the confirmation.
- Disable with a visible reason rather than hiding the control.

## Don't

- Don't change a session's badge or status from a workspace action. The two are separate.
- Don't offer delete as the default or the first control on a touch screen.
- Don't suspend a workspace from the browser while a delivery is open.
