# Merge review

Source: `docs/specs/merge-policy.md`, `docs/specs/pull-requests.md`, `docs/specs/chat.md` (Native structured input).

## Purpose

The evidence an owner reads before approving a pull request merge, shown inside the HITL card above the Approve and Reject buttons. It is the proposal as captured, not a claim that the merge is safe.

## Tokens

- Region `border-border` with `p-md` and `rounded-none`. The unavailable region uses `border-term-yellow`.
- Notices about stale or withheld evidence use `text-term-yellow`. Metadata uses `text-muted-foreground`. Errors use `text-destructive`.
- Links `text-primary`, underlined.

## Content

- PR title, a description excerpt with a truncation note, `repo #number`, file count and `+added / −deleted` with a preview of changed paths.
- "Approval required because:" with one line per reason: "Project policy requires approval." or "Protected path matched {glob}: {path}."
- Head and base with their full commit SHAs, the proposal id, policy and glob versions, and the reviewed summary SHA-256.
- CI as history: "N results recorded at {time} · passed, failed, pending at preparation". Say "Recorded CI evidence is unavailable or incomplete" when there is none.
- Links: Open PR and Compare diff (https on github.com, and only under the summary's repository), and the conversation link. Both are rebuilt from the repository and number, so no query, fragment or userinfo from the server is shown.
- Three lazy sections, loaded when first opened: PR description, Changed paths, Full CI list. Paths and CI page 50 at a time with "Load more paths" and "Load more CI results".

## States

- **Reviewable:** the evidence is ready, current, and carries a summary digest. Approve is enabled and the digest the owner saw goes with the approval.
- **Withheld, stale:** the summary is shown with the server's reason and "Approve is disabled; Reject stays available."
- **Unavailable:** no usable evidence. The region reads "Merge evidence unavailable · call {id}" and never shows an approve path. It may quote the server's note as plain text.
- **Details loading, failed, truncated:** each section shows its own status, a Try again button on failure, and keeps pages already loaded. A truncated list says so.

The page checks commit SHAs (40 hex digits), digests (64 hex digits) and the repository name (`owner/name`) more strictly than the API schema. When evidence is withheld for that reason, the copy says so.

## Keyboard and screen reader

- The region has an accessible name: "Verified merge context" or "Merge context unavailable".
- Lazy sections are native `details` elements. Loading uses a polite status line; failures use an alert.

## Touch

- Section headings, links and buttons are at least 44 px tall.

## Do

- Show every field as escaped text. A server note is quoted, never interpreted.
- Refuse a details page whose proposal, section, cursor or summary digest does not match what the owner is reviewing.

## Don't

- Don't offer an approve path built on missing or mismatched evidence.
- Don't render a link unless it is https (github.com for the PR and diff).
- Don't present historical CI as current status.
