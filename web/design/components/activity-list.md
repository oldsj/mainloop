# Activity list

Source: `docs/specs/inbox.md` (Activity) and `docs/specs/layout.md`. Built in `web/src/features/inbox/ActivityList.tsx`.

## Purpose

The quieter log of FYIs: merged PRs, provider reports and unavailable input notices. A person reads it for context. Nothing here needs a decision, and it never adds to the badge. Decisions go to **Needs you**.

## Tokens

- Rows divided by `border-border`, padding `p-md`, radius `rounded-none`.
- Title `text-foreground` medium. Content and time `text-muted-foreground`.
- **View task** is the ghost button.

## What appears

- Merged PRs, provider reports and unavailable input notices.
- Provider reports carry the server's `unverified` label in their content, for example "Provider report (completed; unverified): …". Show the text as sent. Do not drop the label or style the row as success.

## States

- **Loading:** "Loading activity…" as a status line.
- **Empty:** "No activity yet".
- **Rows:** title, content, a **View task** link when the row names a task, a **Dismiss notice** button when the server marks the row `dismissible`, and a local time.
- **Error:** the recovery block "Could not load activity." with a retry. Rows already shown stay above it.
- **Dismiss refused:** the server's reason in an alert above the list. The row comes back, and the alert stays while that row is listed. It clears once the re-read removes the row, or on the next dismiss.

## Keyboard and screen reader

- The list is labelled **Activity**. Each row's title is a heading, so the list can be navigated by heading.
- Times use a `time` element with a machine-readable value.
- Content is escaped text, never HTML.

## Touch

- Every link and button is at least 44 px tall. The row spans the available width; the **View task** target does not need to.

## Do

- Link each row to its task, so the owner can see what the report refers to.

## Don't

- Don't count Activity rows in the badge.
- Don't show a provider report as proof that a task is complete.
