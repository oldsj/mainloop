# Merge policy

Source: `docs/specs/projects.md`, `docs/specs/merge-policy.md` (shared UI and policy controls).

## Purpose

The merge policy section on a project page. It shows the current policy, **Auto** or **Approval required**, with its explanation and the server's read-only protected paths. When the server accepts writes, the owner can change it. It changes the policy only. It does not merge a pull request or approve a pending request.

## Tokens

- Section `border-border`, padding `p-md`, gap `gap-md`, radius `rounded-none`.
- Heading `text-lg font-medium`. Policy line and controls `text-sm`. Explanation `text-muted-foreground`.
- "Editing disabled" notice `text-term-yellow`. Save error `text-destructive`.
- Select `border-border bg-background`. Save is the primary button.

## States

- **loading:** "Loading merge policy…" as a status line. No controls yet.
- **first read failed:** an error block titled "Could not load the current policy. Refresh before editing." with "Refresh policy". No select and no save.
- **refresh failed:** the last loaded policy stays on screen, with an inline error block and "Refresh policy".
- **contract break:** "Mainloop sent a merge policy that does not match the contract." No policy is shown as if it were valid.
- **writes off on the server:** the policy and protected paths show. The select and Save are disabled, with "Editing disabled by the server. The current policy remains in effect."
- **writes on:** the select is enabled. Save is enabled only when the chosen policy differs from the saved one.
- **saving:** the button reads "Saving…" and is disabled. A second submit is blocked.
- **save refused:** the server's reason shows as text, then "Review the current policy before saving again." with "Refresh policy". The select and Save stay locked until the owner refreshes.
- **after refresh:** the form starts again from the refreshed policy, once the re-read has finished. The owner reviews it before saving.

## Keyboard and screen reader

- The select has the accessible name "Policy". The current policy and the explanation are plain text, read in order after the heading.
- A refused save is a single alert. It is not re-announced on each re-read.
- Protected paths sit in a disclosure, "Protected paths (read-only)", which opens with Enter or Space.

## Touch

- The select, Save, Refresh and the disclosure summary are at least 44 px tall.

## Behaviour that must hold

- The server's refusal reason is shown, never replaced with a generic message or hidden.
- A save sends the policy version the owner read. A refused or uncertain save does not resend the same draft.
- The protected paths are shown even when editing is off.
- The section never says a policy change merged or approved anything.

## Do

- Say what the policy does in plain words: Auto permits eligible merges after server checks; Approval requires an owner decision.
- Keep the disabled reason next to the disabled control.

## Don't

- Don't show a select or Save that looks active when the server has turned writes off.
- Don't use colour alone for "editing disabled". The sentence carries the meaning.
- Don't put a palette colour (`red-500`, `green-600`) or a hex value in the section. Tokens only.
