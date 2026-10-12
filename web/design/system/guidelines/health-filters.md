# Health filters

Source: `docs/specs/orchestration-health.md`.

## Purpose

The filter form for the orchestration health page: window, project, task, session, attempt and provider. The applied filters are the page's URL, so a link or a reload shows the same view.

## Tokens

- A grid of labelled selects: two columns on phones, three from `sm`, six from `lg`.
- Labels use `text-muted-foreground text-xs`. Selects use `bg-background border-border text-foreground`, `text-sm`, `min-h-11` and `rounded-none`.
- Apply is an outline button. Clear filters is a ghost button. Both are at least 44 px tall.

## Behaviour

- The disclosure button reads "Filters · {window}" (for example "Filters · last 7 days"). It sets `aria-expanded` and `aria-controls="health-filters"`.
- The panel starts open at 640 px and wider, and closed below. An owner's toggle holds for the width it was made at. Crossing the 640 px line returns to the default.
- Edits stay in a draft until Apply, which writes them to the URL. Clear filters returns to the default window with no other filter.
- Windows: last 24 hours, last 7 days, last 30 days, last year.
- Task, session and attempt choices come from the wider scope when one of them is set, so the other choices stay available. A selected value that is not in the list keeps a plain fallback label ("Selected task").

## States

- **Default:** window 24 hours, every other select on "All …".
- **Narrowed:** a task, session or attempt is set. Choices come from the wider request.
- **Unreadable URL value:** dropped to its default. The page does not fail.

## Keyboard and screen reader

- Each select has its own visible label, with `htmlFor`, not a wrapping label. Its accessible name is the label text.
- The form is named "Health filters".

## Do

- Keep Apply as the only way the form changes the URL.

## Don't

- Don't show an error page for an unreadable filter value. Drop it.
- Don't hide Clear filters when a filter is set.
