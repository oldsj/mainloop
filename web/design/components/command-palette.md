# Command palette

Source: `docs/specs/layout.md`.

## Purpose

One field that gets the owner anywhere without the mouse: jump to a task, session, workspace, project, schedule or Needs-you item; open a page; change the theme; or ask Mainloop. It lists what the explorer lists, so the two never disagree about what exists.

## Tokens

- Panel `bg-popover text-popover-foreground`, edge `border-border`, radius `rounded-none`. Up to 36 rem wide (`max-w-xl`), 16 px from the screen's edges on a phone, 96 px from the top.
- Overlay `bg-background/80`.
- Field: 44 px tall, `text-ui`, no edge of its own; a `border-border-subtle` rule under it. Placeholder `text-muted-foreground`.
- Row: the control height (`min-h-control`), `text-ui`. Title in the foreground colour; the words beside it `text-muted-foreground text-xs`; the kind on the right in small capitals (`text-2xs uppercase tracking-wide`, muted).
- Highlighted row `bg-accent text-accent-foreground`. Accent is selection here, never a status.
- Key hints: one muted `text-2xs` line under the list, shown with a mouse or trackpad only.

## States

- **Open, no query:** pages first, then the explorer's rows in its order (what needs the owner first), then the themes. At most 50 rows.
- **Query:** every word must be in a row's title or the words beside it. A title that starts with the query is first, then a word that starts with it, then the rest. The last row is always **Ask Mainloop: <query>**, so a query never ends in nothing.
- **Nothing to list:** `Nothing to show yet.` Only before the first read, since the pages are always there.

## Keyboard and screen reader

- Opens with ⌘K on a Mac and Ctrl+K elsewhere, never both on one platform; the same key closes it. It works while typing in the composer.
- The field is a combobox that controls a listbox; the highlighted row is its `aria-activedescendant`, so focus never leaves the field.
- Up and down move and wrap. Enter opens the highlighted row. The platform's modifier with Enter asks Mainloop about it. Escape closes.
- It is a dialog named "Search and commands". Focus returns to where it was when the palette closes, unless a row took the owner to another page.

## Touch

- Opens from the search button in the tree's header. Rows take the control height (44 px on touch); a tap runs the row.
- The key hints are not shown.

## Behaviour that must hold

- A thing is listed once, though the explorer may show it in several groups.
- Running a row closes the palette.
- Asking Mainloop only fills the composer. Nothing is sent until the owner sends it.
- The palette reads the explorer's data; it makes no request of its own kind.

## Do

- Add a new jump target by giving the explorer's node a target; the palette picks it up.
- Keep the kind label to one or two words.

## Don't

- Don't bind Ctrl+K on a Mac: there it deletes to the end of the line in any text field.
- Don't put an action that cannot be undone in the palette. Those live on their page, behind a confirmation.
- Don't use a palette colour. Tokens only.
