# Agent column

Source: `docs/specs/layout.md` (Sidebar, The main thread, The agents).

## Purpose

The sidebar: who is working and what needs the owner, at a glance. The main thread is pinned at the top, then the **Agents** heading with its buttons, then the agents grouped by project. Every agent row says what it is, where it stands and how long ago, in the same shape.

## Tokens

- Surface: the page background. The current row is `bg-card`; a hovered row `bg-card/50`. Both keep `text-muted-foreground` and every status colour at 4.5:1 or better in all five themes (the accent surface does not: attention text fell to 3.1:1 on it).
- Main thread: `LogoMark` in a bordered box, **Mainloop** `text-title font-semibold`, and one `text-caption` line: the Needs-you count in `text-status-attention` with its icon, then the working count, or `Idle`. A `border-border-subtle` rule under it.
- **Agents** heading: `text-label font-semibold uppercase tracking-wide text-muted-foreground`, with ghost icon buttons (`Plus`, `Search`) and the sidebar's hide button at its end.
- Project heading: the short name in `text-body font-semibold`; a Needs-you badge (`bg-status-attention text-background text-label`, square) and a `LoaderCircle` with the working count in `text-status-working`. No project's heading is the name alone, muted.
- Agent row: `StatusIcon` (16 px), the title `text-body font-medium` truncated, the age `text-caption text-muted-foreground` on the right, and one `text-caption text-muted-foreground` line. **Needs you** and **Failed** lead the line in their status colour.
- Children are indented 20 px per level. Radius `rounded-none`. Tokens only.

## States

- **Status icon:** an alert (`CircleAlert`, attention) for needs you; a cross (`CircleX`, failed); a broken ring (`LoaderCircle`, working); a check (`CircleCheck`, unseen teal, or green for done); an empty ring (`Circle`, idle). Each state has its own shape, so it reads without colour.
- **Kinds:** a task is a plain row; a standing supervisor is titled **Supervisor**; a coordination task's line starts `Supervisor ·`; a session with no task starts `Session ·`.
- **Current:** the agent whose page shows is `aria-current="page"`; the project heading (or the main thread) the page belongs to is `aria-current="true"`.
- **Loading:** placeholder rows, with the heading and its buttons already there. **Failed:** an alert, "Couldn’t load the agents."

## Keyboard and screen reader

- One Tab stop with a roving focus: up and down move through the main thread, the project headings and the agents; Home and End go to the ends; Enter opens. **a** asks Mainloop about the focused agent; Shift+F10 or the menu key opens its row menu.
- The icons are `aria-hidden`; the words say the state. The main thread's and a project heading's accessible names spell their counts out (`widgets, 1 needs you, 2 working`).

## Touch

- Rows are at least the control height (44 px on touch). The **Ask Mainloop** button is always shown below the desktop width (`lg`), and on hover or focus above it; its place is kept, so the age never shifts.
- At 390 px a long title or line is cut with an ellipsis; the age is never pushed off the row.

## Do

- Use `StatusIcon` and `STATUS_ICON` so the column, the tabs and the headers speak one status language.
- Keep the order agents were started in: a row does not move as its state changes.

## Don't

- Don't list the main thread among the agents as well as pinning it.
- Don't use lowercase list names, floating text buttons, or square dots here.
- Don't fill the current row with `bg-accent`: it drops the muted and status text below contrast.
