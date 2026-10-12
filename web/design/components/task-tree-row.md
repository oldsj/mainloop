# Task tree row

Source: `docs/specs/tasks.md`.

## Purpose

One task in the project task tree. A row shows what the list can say without opening the task: status, title, provider and reason, the open operation, and whether CI is known. Children sit under their parent, indented one step.

## Tokens

- Status badge: the shared status badge, with its tone from `statusTone`. The `[SUPERVISOR]` marker uses the active tone.
- Title `text-sm font-medium`. Provider, reason and CI line `text-muted-foreground text-xs`. Operation summary `text-xs`.
- Divider `border-border`, padding `p-md`, gap `gap-xs`. Indent steps are `pl-md` and `pl-lg`, and `pl-xl` beyond that; `indentClass` is the only place these are chosen.
- Radius `rounded-none`. Tokens only: no colour literals.

## States

- **Normal:** the status badge, title, provider and reason, any open operation, and `CI passing`, `CI failing` or `CI running` when the PR head, CI head and observation agree and are fresh.
- **CI unknown:** every other case reads `CI unknown`. This includes missing evidence, mismatched heads, an invalid observation time and a stale observation. Never show a CI colour without fresh matching evidence.
- **Detached:** the parent is not in the list, so the row is at the top level and says `child of a task not shown here`. A filtered list never hides work.
- **Supervisor:** the task is a coordination task. It shows `[SUPERVISOR]` beside the status.

## Keyboard and screen reader

- The whole row is one link to the task. The link's name is the row's text, so the title is the first thing read.
- Focus shows the link's own focus ring. Do not add a second focus target inside the row.
- The status badge and the CI line are read as part of the link, not announced on their own.

## Touch

- The row is the touch target. It has a minimum height of 44 px (`min-h-11`).
- Long titles wrap (`break-words`). Nothing is truncated with an ellipsis that hides the status.

## Behaviour that must hold

- CI is never shown as passing, failing or running from the list alone. The rule is the same as on the task page: fresh evidence for the exact PR head.
- A row is never dropped because its parent is missing from a filtered list.
- The indent is one step per level. A detached child is not indented.

## Do

- Use the shared status badge and `statusTone`, so the words and colours match the task page.
- Keep the CI decision in `publicationFacts`, not in the row.

## Don't

- Don't show a CI colour from `ci_state` alone.
- Don't add buttons inside the row; actions live on the task page.
- Don't use a palette colour (red-500, green-600). Tokens only.
