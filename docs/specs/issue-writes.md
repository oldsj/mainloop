# GitHub issue writes

Status: **implemented**. Two Mainloop MCP tools, `create_issue` and `comment_issue`, file a GitHub
issue or add a comment to one in a project's repository through Mainloop's GitHub App. Sandboxes
have no `gh` and no GitHub API access, so before these tools an agent that found a bug outside its
task could only ask the owner to file it. Issues are read with `get_issue` ([GitHub reads](github-reads.md)).

## Who holds them

The tools follow the MCP grant, not the role:

| Actor                                                       | `create_issue`, `comment_issue`, `get_issue`          |
| ----------------------------------------------------------- | ----------------------------------------------------- |
| Main thread (coordination grant, depth 0)                   | Yes, in any project of its owner                      |
| Coordination task (supervisor or child, coordination grant) | Yes, in its own project                               |
| Code task (workspace grant)                                 | `get_issue` only; it reports findings in its `report` |

Code tasks do not file issues. Their findings reach the owner through `report`, and the main thread
or a coordination supervisor, which sees the wider picture and can find a duplicate, files the
issue. That keeps one place deciding what becomes an issue and keeps a busy code task from opening
issues the owner then has to triage. The guidance every session receives says the same (guidance v6,
[delegation](delegation.md)).

The main thread may name any project of its owner, as it may for merges: "make an issue for that"
in chat is not tied to one project. Every other actor is held to its session's project by the same
fail-closed check the pull-request tools use, before any GitHub call. In every case the service
resolves the project from PostgreSQL by owner and id and derives the repository from it, so another
owner's project is `not_found`. Neither tool accepts a repository, owner, URL or token argument.

## `create_issue`

`create_issue(project_id, title, body, labels?, request_id)`:

- `title`: 1 to 256 characters. `body`: up to 60,000 bytes, may be empty.
- `labels`: up to 10 names. Each must already exist in the repository; the match ignores case and
  the label is sent as the repository spells it. An unknown name is refused (`unknown_label`)
  before anything is written, so a call never creates labels.
- `request_id`: the caller's stable id for this write. The same id with the same arguments answers
  the same write and never files twice; with other arguments it is
  `request_payload_conflict`. The id is per owner across both tools.

## `comment_issue`

`comment_issue(project_id, issue_number, body, request_id)` adds a comment to an existing issue.
`body` is 1 to 60,000 bytes. A number that names a pull request, or nothing, is `not_found` before
anything is written: pull-request conversation belongs to the pull-request tools.

## Result

Both tools return the write's state:

| Field          | Meaning                                                       |
| -------------- | ------------------------------------------------------------- |
| `id`           | Mainloop's id for the write                                   |
| `kind`         | `issue` or `comment`                                          |
| `state`        | `pending`, `dispatching`, `uncertain`, `created` or `refused` |
| `repository`   | `owner/name`                                                  |
| `issue_number` | The issue (the new one, or the one commented on)              |
| `comment_id`   | The comment, for `comment_issue`                              |
| `url`          | The issue's or comment's `html_url`, once created             |
| `detail`       | Why a write was refused                                       |

A call waits up to 20 seconds for the write to settle. A write still pending then is finished by
the loop, and calling again with the same `request_id` reads it.

## Durable state

The tools follow the reconciler rule, as `open_pull_request` does. The MCP call checks the
arguments, reads the repository (and, for a comment, the issue) and its labels, and writes an
intent row to `issue_writes`, keyed by `issue:<owner>:<request_id>` and holding the exact text to
send. The insert runs in the transaction that re-checks the caller's MCP credential, so a revoked
credential writes nothing. The API process's loop (`mainloop_issue_writes`) then sends the one
POST, with no transaction held across it, using an installation token scoped to the repository and
`issues: write`.

GitHub has no idempotency key for issues or comments, so the body ends with a hidden marker,
`<!-- mainloop:issue-write <id> -->`. Before sending, and whenever the outcome of a POST is unknown
(a lost reply, a 5xx, a timeout), the loop looks for the write instead of sending again: it lists
the issues created, or the issue's comments, since shortly before the intent was written, and takes
the one written by a bot account whose body carries the marker. Text a person posts with the
marker does not count.

| Outcome                                             | State                                                 |
| --------------------------------------------------- | ----------------------------------------------------- |
| POST answered, or the write found by its marker     | `created`, with number, URL and comment id            |
| Repository renamed or replaced since the call       | `refused`, `repository_mismatch`                      |
| Issue gone before a comment was sent                | `refused`, `issue_missing`                            |
| GitHub refuses (4xx other than a rate limit)        | `refused`, `github_<status>`                          |
| Rate limited                                        | Back to `pending`, retried later                      |
| Outcome unknown                                     | `uncertain`; looked up with backoff, never sent again |
| Still not found 10 minutes after an unknown outcome | `refused`, `not_created`                              |

A write is therefore sent at most once. One that stops as `not_created` may in rare cases have
landed without being found; a new `request_id` is the caller's choice to try again.

## GitHub App permission

The App needs **Issues: read and write**. Writes mint `issues: write`; the issue listing, comment
listing and label listing mint `issues: read`. See [production deployment](../production-rollout.md#github-app-permissions).

## Not covered

No tool closes, edits, labels after creation, assigns or locks an issue, reacts, or comments on a
pull request. The default tests use the fake GitHub (`server/internal/github/githubtest`) behind a
real `github.App`, and scratch PostgreSQL; they are not live GitHub proof.
