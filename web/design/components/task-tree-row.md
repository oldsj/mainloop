# Task tree row

Source: `docs/specs/tasks.md`.

## Purpose

One task in the project task tree, on one line. A row shows what the list can say without opening the task: a status dot, the title, the state, and how long ago it changed. Children sit under their parent, stepped in, with a guide line for each level.

## Tokens

- Status dot: the shared `StatusDot`, 8 px, in the status language the explorer uses. Amber (`status-working`) while the task works; red (`status-attention`) when it needs the owner; hollow (`status-idle` edge) at rest.
- Title `text-sm`, truncated with an ellipsis. State and age `text-muted-foreground text-xs`.
- Row height is the control height (`min-h-control`). Hover `bg-accent`. No border between rows; a `border-border-subtle` hairline above each top-level task sets it and its children apart from the one before.
- Indent is 16 px per level, with a 1 px `bg-border` guide line for each level above the row.
- Radius `rounded-none`. Tokens only: no colour literals.

## States

- **Normal:** dot, title, state, age. The state is the status and its reason (`Running`, `Blocked · awaiting your decision`), then any open operation's summary, then CI when it is known.
- **CI:** `CI passing`, `CI failing` or `CI running` only when the PR head, CI head and observation agree and are fresh, and the task is not finished. In every other case the row says nothing about CI. It never reads "CI unknown", and a finished task never mentions CI.
- **Detached:** the parent is not in the list, so the row is at the top level and its state ends `child of a task not shown here`. A filtered list never hides work.
- **Supervisor:** the task is a coordination task. Its state starts with the plain word `supervisor`. No tag, no brackets.
- **Narrow screen (below `sm`):** the state is cut to the status word alone, so the state is never colour only.

## Keyboard and screen reader

- The whole row is one link to the task. The link's name is the row's text, so the title is the first thing read, then the state and the age.
- Focus shows the link's own focus ring. Do not add a second focus target inside the row.
- The dot is decoration (`aria-hidden`): the state is always in words.

## Touch

- The row is the touch target. It takes the control height (`min-h-control`): at least 44 px on touch.
- A long title is cut with an ellipsis; the state and the age are never pushed off the row by it.

## Behaviour that must hold

- CI is never shown as passing, failing or running from the list alone. The rule is the same as on the task page: fresh evidence for the exact PR head.
- A row is never dropped because its parent is missing from a filtered list.
- The indent is one step per level. A detached child is not indented.
- A child follows its parent directly, before the next top-level task.

## Do

- Use the shared `StatusDot`, so the dot means the same here as in the explorer.
- Keep the CI decision in `publicationFacts`, not in the row.
- Keep the row to one line.

## Don't

- Don't show a CI colour from `ci_state` alone, and don't write "CI unknown".
- Don't wrap a label in brackets, or use a bordered tag for the status or the supervisor mark.
- Don't add buttons inside the row; actions live on the task page.
- Don't use a palette colour (red-500, green-600). Tokens only.
