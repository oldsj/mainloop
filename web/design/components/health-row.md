# Health row

Source: `docs/specs/orchestration-health.md`.

## Purpose

One task group on the health page. Collapsed, it shows the task title, the coverage state and four counts. Expanded, it shows the task link, collection gaps, blocked or failed claims, approvals and each session's identity and observations.

## Tokens

- The summary is one button, at least 44 px tall, with `text-left`. Title `font-medium`.
- Counts use `tabular-nums`. Metadata uses `text-muted-foreground text-xs`.
- Gap notes use `text-term-yellow`.
- Links use `text-foreground` with an underline.
- Radius `rounded-none`.

## Content

- **Title:** the task title. A row with no title and no task reads "Unassigned".
- **Coverage:** "Observed coverage", "Partial coverage" or "Unknown coverage", then "· Details".
- **Counts:** failures, repeated failure groups, input tokens and output tokens. An unknown count reads "Unavailable", never 0.
- **Usage line:** "Usage observed on N/M observed turns", then delivery receipts, blocked or failed claims and pending approvals.
- **Claims:** "Reported {outcome} · attempt {id} · report {id}. Agent claim; runtime failures are counted separately."
- **Approvals:** "Approval {state}; exact wait unavailable.", then the approval card.
- **Sessions:** the session title as a link, a one-line identity (provider · model · live state), and the observations control.

## States

- **Collapsed:** summary only.
- **Expanded:** the rest of the row. A row with no task has no "Open task" link.
- **No sessions:** no session section.

## Keyboard and screen reader

- The summary button sets `aria-expanded` and `aria-controls` to the panel it opens.
- The whole summary is one target, so a touch user does not need to aim at the title.

## Do

- Sort rows by failures, most first. Unknown counts go last.
- Say "unavailable" for native child attribution, cache tokens, Codex usage and exact approval waits. These are not zero.

## Don't

- Don't show an unknown count as 0.
- Don't fetch a session's observations until the owner asks.
