# GitHub issue and pull request comments

Status: **implemented**. Three Mainloop MCP tools write to a project's repository through
Mainloop's GitHub App: `create_issue` and `comment_issue` file a GitHub issue or add a comment to
one, and `comment_pull_request` comments on a pull request or reviews it with inline comments.
Sandboxes have no `gh` and no GitHub API access, so before these tools an agent that found a bug
outside its task could only ask the owner to file it, and a reviewing agent could only report its
review. Issues and pull requests are read with `get_issue` and `get_pull_request`
([GitHub reads](github-reads.md)).

## Who holds them

The tools follow the MCP grant, not the role:

| Actor                                                       | `create_issue`, `comment_issue`, `get_issue`          | `comment_pull_request`           |
| ----------------------------------------------------------- | ----------------------------------------------------- | -------------------------------- |
| Main thread (coordination grant, depth 0)                   | Yes, in any project of its owner                      | Yes, in any project of its owner |
| Coordination task (supervisor or child, coordination grant) | Yes, in its own project                               | Yes, in its own project          |
| Code task (workspace grant)                                 | `get_issue` only; it reports findings in its `report` | Yes, in its own project          |

Code tasks do not file issues. Their findings reach the owner through `report`, and the main thread
or a coordination supervisor, which sees the wider picture and can find a duplicate, files the
issue. That keeps one place deciding what becomes an issue and keeps a busy code task from opening
issues the owner then has to triage. The guidance every session receives says the same (since guidance v7,
[delegation](delegation.md)).

`comment_pull_request` is different: reviewing a pull request is usually a code task's work, and
its comments land on a pull request the task was asked to look at, not in the owner's issue list.
So every actor that can read a pull request with `get_pull_request` can also comment on it. A
review only ever comments; no tool approves a pull request or requests changes, so an agent's review
never satisfies a required review or blocks a merge.

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
anything is written: pull-request conversation belongs to `comment_pull_request`.

## `comment_pull_request`

`comment_pull_request(project_id, pr_number, body, request_id, commit_sha?, comments?)`:

- Without `comments` it adds a conversation comment to the pull request (`kind` `pr_comment`).
  `body` is 1 to 60,000 bytes.
- With `comments` it submits one review (`kind` `pr_review`) whose summary is `body`, with the
  event `COMMENT`. `comments` holds 1 to 30 inline comments, each `{path, line, side?, body}`:
  `line` is a line number in the file as the diff shows it, `side` is `RIGHT` (the new version, the
  default) or `LEFT` (the old one), and `body` is 1 to 10,000 bytes.
- `commit_sha` is required with `comments`, and refused without them: the 40-hex commit whose diff
  the lines refer to, normally the `head_sha` `get_pull_request` returned. GitHub places the
  comments on that commit's diff even when the branch has moved on since.
- A number that names no pull request is `not_found`, and an inline comment on a file the pull
  request does not change is `unknown_path`, both before anything is written. A line outside the
  diff is GitHub's to refuse: the write ends `refused`, `github_422`, and nothing is posted.
- `request_id` works as for the issue tools, and the ids are shared with them per owner.

## Result

Both tools return the write's state:

| Field          | Meaning                                                       |
| -------------- | ------------------------------------------------------------- |
| `id`           | Mainloop's id for the write                                   |
| `kind`         | `issue`, `comment`, `pr_comment` or `pr_review`               |
| `state`        | `pending`, `dispatching`, `uncertain`, `created` or `refused` |
| `repository`   | `owner/name`                                                  |
| `issue_number` | The issue or pull request (new, or commented on)              |
| `comment_id`   | The comment, or for `pr_review` the review                    |
| `url`          | The issue's, comment's or review's `html_url`, once created   |
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
`issues: write`, or for a review `pull_requests: write`.

GitHub has no idempotency key for issues or comments, so the body ends with a hidden marker,
`<!-- mainloop:issue-write <id> -->`. Before sending, and whenever the outcome of a POST is unknown
(a lost reply, a 5xx, a timeout), the loop looks for the write instead of sending again: it lists
the issues created, the issue's or pull request's comments, or the pull request's reviews, since
shortly before the intent was written, and takes the one written by a bot account whose body
carries the marker. A review's marker is in its summary. Text a person posts with the
marker does not count.

| Outcome                                             | State                                                 |
| --------------------------------------------------- | ----------------------------------------------------- |
| POST answered, or the write found by its marker     | `created`, with number, URL and comment id            |
| Repository renamed or replaced since the call       | `refused`, `repository_mismatch`                      |
| Issue gone before a comment was sent                | `refused`, `issue_missing`                            |
| Pull request gone before a comment or review        | `refused`, `pull_request_missing`                     |
| GitHub refuses (4xx other than a rate limit)        | `refused`, `github_<status>`                          |
| Rate limited                                        | Back to `pending`, retried later                      |
| Outcome unknown                                     | `uncertain`; looked up with backoff, never sent again |
| Still not found 10 minutes after an unknown outcome | `refused`, `not_created`                              |

A write is therefore sent at most once. One that stops as `not_created` may in rare cases have
landed without being found; a new `request_id` is the caller's choice to try again.

## GitHub App permission

The App needs **Issues: read and write** and **Pull requests: read and write**. Issue writes and
pull request conversation comments mint `issues: write`, a review `pull_requests: write`; the
issue, comment and label listings mint `issues: read`, and a pull request's comment and review
listings `pull_requests: read`. See [production deployment](../production-rollout.md#github-app-permissions).

## Not covered

No tool closes, edits, labels after creation, assigns or locks an issue, or reacts. No tool
approves a pull request, requests changes, replies in a review thread, resolves a thread, or edits
or deletes a comment. The default tests use the fake GitHub (`server/internal/github/githubtest`) behind a
real `github.App`, and scratch PostgreSQL; they are not live GitHub proof.
