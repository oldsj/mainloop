# Page header

Source: `docs/specs/layout.md`.

## Purpose

The top of a page: the way back, the page's name, its status and its own actions, on one row, with a short description under it. One component, so the title is the same size on every page and "back" is always in the same place.

## Tokens

- Title: the page's one `h1`, `text-lg font-medium`. No page uses a larger or bolder title.
- Back: the shared icon button (`ghost`, `icon` size) with a left arrow, first on the row. Its class is `BACK_LINK`; the link is the caller's own, so its route is type-checked.
- Description `text-muted-foreground text-sm`, no wider than a reading measure (`max-w-prose`).
- Row gap `gap-sm`; the row wraps on a narrow screen, so actions drop under a long title.
- Radius `rounded-none`. Tokens only.

## States

- **Top-level page** (Settings, Scheduled runs): no back link. The shell's navigation is the way out.
- **Page under another** (a schedule, a workspace): a back link to its parent.
- **With a status:** a badge after the title, before the actions.
- **With actions:** the page's own buttons on the right of the row. The primary one is the filled button.

## Keyboard and screen reader

- The back link is named by where it goes (`Back to scheduled runs`), never just "Back". The arrow is `aria-hidden`.
- The title is the only `h1` on the page. Sections under it are `h2`.
- Tab order is the reading order: back, then the actions.

## Touch

- The back link and the actions take the control height (44 px on touch).

## Behaviour that must hold

- One `h1` per page, and it is this one.
- The back link goes up one level, to a fixed place. It is not the browser's history.

## Do

- Pass the link itself as `back`, with `BACK_LINK` as its class and `BackIcon` inside it.
- Keep the description to what the page is for. Facts about the thing belong in the page.

## Don't

- Don't write a page title by hand with its own size.
- Don't use a text link ("Home", "Scheduled runs") above the title as the way back.
- Don't put more than two or three actions in the header. More belong in the page.
