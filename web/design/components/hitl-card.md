# HITL card

Source: `docs/specs/chat.md` (Native structured input), `docs/specs/inbox.md`.

## Purpose

The structured input card for a native agent's request: tool-call approvals and questions. The same card appears in the inbox and in chat, and both use the same response route.

## Tokens

- Card `bg-card` with `border-border`. Decision buttons are outline until chosen. A chosen Approve fills `bg-primary` and a chosen Reject fills `bg-destructive`, each with a check mark and its label ("Approved" or "Rejected"), so both read at the same strength.
- Recorded answer uses `text-muted-foreground`. Errors use `text-destructive`.
- Radius `rounded-none`.

## Decisions

- **Tool batch:** every call needs an explicit approve or reject. A batch cannot be submitted with a call left undecided.
- **Rejection reason:** optional text. Claude includes it in the denial message. Codex saves it, but its runtime only receives the rejection, so the card must not say the reason reached the agent for Codex.
- **Question:** single choice, multiple choice, or free text when there are no choices.

## States

- **Pending:** controls enabled.
- **Submitting:** controls disabled; the submitted choice stays visible. Only one submit per request is in flight, across every card showing that request.
- **Recorded:** a recorded answer disables further decisions and shows what was chosen. A one-line receipt leads the answer, such as "Approved Bash, rejected Edit · 6:20 PM". After Send, focus moves to that receipt.
- **Unconfirmed:** the submission timed out (after 90 seconds) or the network failed, so it may or may not have been recorded. The card locks, says "do not submit again", and re-reads the request. It unlocks only when the re-read shows a recorded decision.
- **Stale:** the request was replaced. Show the new request in its own card with fresh controls. The old card is read-only.
- **Unavailable:** the continuation cannot run, or writes are off on the server. Say so on the card ("Responding disabled by the server."); do not show enabled controls. A refused send (503) with writes off, proven by a re-read, shows the same state and says nothing was recorded.
- **Merge call:** a call that merges a pull request shows the merge review above its decision (see `merge-review.md`). Approve stays disabled until that evidence is reviewable.
- **Loading and error:** show the request title and a retry control. Refreshing reads status only and never resubmits a decision.

The card refreshes while it is mounted and visible, about every four seconds.

## Keyboard and screen reader

- Each call's approve and reject are two buttons with names that include the call name.
- Choices are toggle buttons inside a labelled fieldset, with `aria-pressed` for the selected state. Single choice allows one pressed button; multiple allows several. Free text has a label.
- After a decision is recorded, move the announcement to "Answered" with a polite live region. Do not move focus away from the card.

## Touch

- Every approve, reject and choice control is at least 44 px tall.

## Do

- Keep the tool name and its arguments visible before the decision. Show arguments as escaped text.
- Use the dedicated response route for decisions. Never send an answer through the chat composer.

## Don't

- Don't auto-approve, or pre-select a choice.
- Don't render agent-supplied fields as HTML or Markdown.
- Don't replay a decision on refresh. Read the status instead.
