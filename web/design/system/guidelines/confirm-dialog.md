# Confirm dialog

Source: `docs/specs/tasks.md` (cancel is two-step), `docs/specs/sessions.md`.

## Purpose

A two-step confirmation for an action that cannot be undone or that costs something: cancel a running task, delete a schedule. It states what will happen, then the owner confirms or backs out.

## Tokens

- Panel `bg-background`, text `text-foreground`, border `border-border`, radius `rounded-none`, padding `p-lg`.
- Overlay `bg-background/80`.
- The destructive confirm uses the destructive token. The plain confirm uses the primary token.
- Description `text-muted-foreground`; title `text-lg` medium.

## States

- **open:** the title says the action, the description says the consequence, and both buttons are enabled. Cancel has focus.
- **pending:** the action is in flight. Both buttons are disabled. Escape and clicks on the overlay do nothing, so the owner cannot close the dialog and lose track of the request.
- **closed:** nothing is changed. Backing out never sends the action.

## Keyboard and screen reader

- It is an alert dialog (`role="alertdialog"`), named by its title and described by its description.
- Focus moves to Cancel when the dialog opens, so Enter backs out. Confirm needs a deliberate Tab or click.
- Focus stays inside the dialog while it is open. Escape cancels unless the action is pending.
- When the dialog closes, focus returns to the control that opened it.

## Touch

- Both buttons are at least 44 px tall. On a narrow screen the buttons stack, with Confirm on top.

## Behaviour that must hold

- The confirm runs the action once. A second press while pending does nothing.
- The description names what will stop or be lost, in plain words.
- Cancel is always available unless the action is pending.

## Do

- Name the action on the confirm button: "Cancel task", "Delete schedule".
- Keep the description short and factual.

## Don't

- Don't use "OK" or "Yes" as the confirm label.
- Don't auto-confirm on a timer.
- Don't make the destructive button the default-focused control.
