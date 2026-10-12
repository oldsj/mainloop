# Source observations

Source: `docs/specs/orchestration-health.md`.

## Purpose

The source events for one session in the chosen window: tool calls, turns and usage. They load only when the owner asks, so a page of rows makes no event reads until then.

## Tokens

- A text button with a `border-border` edge, at least 44 px tall. It dims while loading (`disabled:opacity-50`).
- Event lines use `text-muted-foreground text-xs`. Each event is a list item; its lines are stacked spans.
- Partial and dropped notes use `text-term-yellow`. The empty state uses `text-muted-foreground`.
- Errors use the shared error recovery block.

## Content

- Each event shows three lines: `{kind} · sequence {n} · {source}`, the event id, then for a tool event `{tool class} · {outcome} · {category}` with the path when there is one.
- A partial list says so. An event that cannot be read is left out and counted.

## States

- **Not loaded:** "Load source observations".
- **Loading:** "Loading…" on the button.
- **Loaded:** the list, with the partial and dropped notes when they apply.
- **Empty:** "No retained source events in this window."
- **First load failed:** "Observations could not be loaded." with Try again.
- **Reload failed with events shown:** "Couldn't refresh observations." The events already shown stay, and Try again is offered.
- **Unreadable list:** "Observations could not be read."

## Behaviour

- Pressing the control again reloads the events.
- A page refresh clears them. The events come back only when asked for again.

## Do

- Say when a list is partial.

## Don't

- Don't load events for every row on page load.
- Don't keep the old events on screen after a refresh without a way to re-read them.
