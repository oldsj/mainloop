# Attention card

Source: `docs/specs/inbox.md`.

## Purpose

One item in the **Needs you** inbox. It says what is needed and links to the card or task that settles it. Read by a person, not an agent, so each card must be clear in one line.

## Tokens

- Card `bg-card` with `border-border`. Title `text-card-foreground`; detail `text-muted-foreground`.
- Action link `text-primary`. Radius `rounded-none`.

## What may appear

- Native permission requests and questions.
- Pending approval or review cards.
- Tasks explicitly blocked on an owner decision.

Idle sessions, CI waits, publication updates and provider reports do not appear here. They belong in **Activity**.

## States

- **Empty:** "Nothing needs you" with no card.
- **Loading:** skeleton cards; the badge shows its last known count.
- **Unresolved:** the default. The badge counts it.
- **Read:** still shown and still counted. Reading a card does not settle it, so do not dim it as done.
- **Stale:** the observation is old. Show "Last checked" with a time, and keep the item.
- **Error:** keep the last known items and show a warning with a retry control.
- **Offline:** show the last known items with the time they were loaded.

## Keyboard and screen reader

- The whole card's action is one link or button. Its name states the action, for example "Review approval for session X".
- The badge count is part of the Needs you tab name, so it is announced with the tab.
- Cards are in a list with a heading for the section.

## Touch

- The action target is at least 44 px tall and spans the card width.

## Do

- State what is needed in the first line: the tool, the question or the decision.
- Link to the canonical input card, so the decision happens in one place.

## Don't

- Don't clear the badge when a card is opened. Only the decision settles it.
- Don't show agent-supplied text as HTML. Escape it and keep unknown fields inspectable.
- Don't show a card for a settled or superseded request.
