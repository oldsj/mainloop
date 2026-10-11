# Evidence link

Source: `docs/specs/layout.md`, `docs/specs/workspaces.md`, `docs/specs/inbox.md`.

## Purpose

A link to proof: a pull request, a diff, a CI run, a task result or a provider report. It tells the reader where the evidence lives and how much to trust it.

## Tokens

- Link `text-primary` with an underline on hover and focus.
- Label and metadata in `text-muted-foreground`. Unverified label uses the warning token.
- Radius `rounded-none`.

## Labels

Name the destination. Good: "Open PR", "Compare diff", "Open result", "Open CI run". Bad: "Click here", "More", a bare URL.

- Provider reports are labelled **unverified**. They are not proof of completion.
- A link to a PR or repository must be `https` on `github.com`. Anything else is shown as plain text, not a link.

## States

- **Loading:** "Checking" in muted text; no link until the destination is known.
- **Verified:** plain link with the time it was checked.
- **Unverified:** link with the "unverified" label.
- **Stale:** the destination is known but the observation is old. Show "Last checked" with the time.
- **Missing:** no evidence yet. Show "No evidence yet", not a disabled link.
- **Error:** "Could not load evidence" with a retry control.

## Keyboard and screen reader

- The link is a normal anchor and reachable by Tab in reading order.
- The focus ring uses the focus tokens and stays visible on every theme.
- External links carry a visible hint or an accessible name that says they open a new tab.

## Touch

- Links in lists and cards are at least 44 px tall, with padding around the text.

## Do

- Open external links with `target="_blank" rel="noopener noreferrer"`.
- Show when the evidence was last observed.

## Don't

- Don't present a provider claim as a verified result.
- Don't use a link to stand in for a status. A merged PR and a passing CI run are separate facts; show each one.
- Don't open a link to an unchecked host from agent-supplied text.
