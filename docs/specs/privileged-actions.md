# Privileged actions

An agent asks Mainloop to run a credentialed action it cannot run itself. The owner approves that
exact action from one Needs-you card, which is pushed to their phone. Mainloop then runs it once and
sends the requesting session one message with the outcome.

**Implemented (slice 1):** the mechanism, plus two catalog types, `github.pr_merge` and
`github.pr_close`. They run through Mainloop's own GitHub App, in `serve-api`. Approved commands,
1Password and Kubernetes types, and the separate executor are later slices. They are not described
here.

## Enablement

Privileged actions are off unless `MAINLOOP_PRIVILEGED_ACTIONS_ENABLED` is exactly `true`. The
chart pins it to `false`. With it on, `serve-api` refuses to start unless `MAINLOOP_AUTH_MODE` is
`enforce`, because an approval has to be attributable to the owner. When privileged actions are off,
or there is no GitHub App, the tools are still listed but answer `[unavailable]`.

## Tools

| Tool                        | Who                                                |
| --------------------------- | -------------------------------------------------- |
| `request_privileged_action` | main (depth 0), coordinating supervisors (depth 1) |
| `get_privileged_action`     | the same                                           |
| `cancel_privileged_action`  | the same                                           |

Children, workspace agents and root code tasks cannot request. They report blocked to whoever
supervises them.

`request_privileged_action {request_id, type, args, reason}`:

- `request_id` is the caller's idempotency key, unique per session. Repeating it with the same type
  and arguments reads the same action back (`replayed: true`). Repeating it with different
  arguments is a `[conflict]`.
- A repeat with the same arguments under a new `request_id`, while the first action is undecided,
  also returns the first action.
- At most 3 undecided actions per session, and 10 per owner (`[too_many_pending]`).
- `reason` is shown on the card as the agent's claim, set apart from the facts. It is not part of
  the digest.

The tool returns at once with the action's state. The agent ends its turn. When the action ends,
the session gets one message, whose id is the action id, naming the final state, the owner's note
(if any) and the result. `get_privileged_action` returns the full record, including the audit
events. `cancel_privileged_action` withdraws an undecided action and closes its card.

## Types

`github.pr_merge {repo, pr, expected_head_sha}` squash-merges the pull request at exactly
`expected_head_sha`. The merge method is fixed.

`github.pr_close {repo, prs, comment?}` closes 1 to 20 open pull requests without merging them.
The numbers are sorted and deduplicated. The comment is posted on each pull request, so a comment
that looks like it holds a secret is refused.

Neither type bypasses a branch rule. GitHub's own protection still decides the merge.

## Preflight and the card

Mainloop canonicalizes the arguments and writes the intent row. It then reads the target with no
transaction open. A target the action cannot apply to ends the request as `preflight_failed`, and
no card opens. That covers a pull request that is missing, closed, merged or draft, or whose head
is not `expected_head_sha`.

A GitHub read that fails for another reason leaves the request `requested`. A sweep retries it, up
to 5 attempts.

On success, the facts are frozen: title, author, head and base, size, mergeability and a count of
checks with up to five failing names. They are frozen under a **digest**, the SHA-256 of the
canonical JSON of:

- the action id
- the type and its version
- the catalog version
- the arguments
- the frozen facts
- the requester (role, session, task)
- the expiry

A database trigger refuses any later change to the digest, facts, type, version or arguments.

The card is a Needs-you `approval` item with high priority, so it gets an ntfy push. It has:

- **Title:** "Merge owner/repo#N", "Close owner/repo#N", or "Close K pull requests in owner/repo".
- **Content:** the rendered facts, risk tags (checks failing, pending or unread; not mergeable),
  the labelled agent reason, the expiry, and the action id with the first 16 characters of the
  digest.
- **Options:** `Approve` and `Deny`. The card also accepts a free-text reply.
- **Context:** `privileged_action_id`, `privileged_action_digest`, `privileged_action_type`,
  `repository`, and `pr_url` for a single pull request.

## Decision

The web app sends `Approve` with `metadata.reviewed_context` set to the card's digest. A sweep
records the decision:

- An **Approve** whose `reviewed_context` equals the digest, sent before expiry, approves the
  action.
- **Any other answer** denies it:
  - `Deny` is a denial with no note.
  - Free text is a denial with that text as the note.
  - An Approve with a missing or different digest, or one sent after expiry, is a denial with a
    note that says so.
  - A card closed without an answer is a denial.

An undecided action expires after 24 hours. An approved action that no executor claims within 5
minutes also expires, so a backlog never runs an old approval late.

## Execution

The executor claims an approved action with a 2-minute lease and then:

1. Recomputes the digest from the stored row. If it differs from the approved digest, nothing
   runs and the action fails.
2. Observes the target before writing anything:
   - **Merge:** a head that moved, or a pull request closed meanwhile, makes the action `stale`.
     A merge already made at the frozen head by an earlier attempt of this action counts as
     `succeeded`; on the first attempt it counts as `stale`.
   - **Close:** each pull request is checked on its own. A merged one, or one whose head moved,
     is left as it is.
3. Writes the change.
   - **Merge:** a 409 from GitHub is `stale`. A 405 or another refusal is `failed`. A lost answer,
     a 429 or a 5xx is retried after 30 seconds, up to 5 attempts, and each retry observes first.
     After the last attempt the action is `uncertain`, and the owner gets an error item.
   - **Close:** the result lists each pull request as `closed`, `already_closed`, `merged`,
     `stale` or `failed`. The action succeeds only if every pull request ended closed.
4. Posts a best-effort comment on each pull request it changed, naming the action, plus the
   digest prefix for a merge.

States: `requested`, `pending_approval`, `preflight_failed`, `approved`, `executing`, `succeeded`,
`failed`, `stale`, `denied`, `expired`, `cancelled`, `uncertain`.

Every step appends a row to `privileged_action_events`, which cannot be updated.

## Delivery

The outcome message is sent at most once, with the action id as the message id.

- A busy session is retried after 3 seconds.
- A session that is gone marks the delivery `undeliverable`.
- Other errors are retried after 30 seconds, up to 20 tries.
