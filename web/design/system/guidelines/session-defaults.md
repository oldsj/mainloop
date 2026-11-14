# Session defaults

Source: `docs/specs/chat.md` (Session defaults).

## Purpose

The Settings section where the owner chooses the model and reasoning effort new turns use, by role (the main thread, or every worker) and provider. It is the default under a session's own `/model` and `/effort`.

## Tokens

- Each role is a group with a small heading and one muted line saying what it covers.
- Rows sit in one box with a `border-border-subtle` edge and `divide-border-subtle` rules between them.
- Selects are fields: `border-input bg-background text-foreground`, `min-h-control`, `text-ui`.
- Labels above each select are `text-xs text-muted-foreground`.
- The save line is `text-xs`: `text-muted-foreground` for Saving… and Saved., `text-destructive` for a refused save. The no-longer-offered note is `text-term-yellow`.

## Layout

- Wide: one line per provider, with the provider's name, then Model, then Effort.
- Narrow: the three stack.

## States

- **Loading:** one line, "Loading session defaults…".
- **Could not load:** the shared error recovery, with a retry. Nothing was changed.
- **No provider:** one line saying there is nothing to set a default for.
- **Provider default:** the first choice of each select, written "Provider default (<the profile's own default>)". Choosing it sends nothing for that field.
- **Saving:** both of the row's selects are held and the line reads "Saving…".
- **Saved:** the line reads "Saved." until the next change.
- **Refused:** the line reads "Not saved" with the server's reason, as an alert, and the select shows what was saved before.
- **Effort no longer offered:** the saved level stays in the list, marked "(no longer offered)", with a note that turns leave the effort to the provider.
- **Model not in the profile's list:** the saved model stays in the list, marked as not listed.

## Keyboard and screen reader

- Each select's accessible name says its role, provider and field, for example "Workers, Claude Code: model".
- The save line is a polite status, and an alert when a save is refused. It is always present, so the row does not change height when it speaks.

## Touch

- Selects take the control height: at least 44 px on touch.

## Do

- Save each choice as it is made, sending both of the row's fields.
- Name what "provider default" resolves to, as a hint.

## Don't

- Don't show a level the provider does not offer as a choice, except the saved one.
- Don't present the provider's listed default as something Mainloop sends: it is never sent.
