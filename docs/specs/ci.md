# CI checks

Workspaces are small (a Kata microVM with 2 CPUs and 3 GiB), so the heavy checks of a project
(full race-enabled test suites, full frontend test runs, image builds) belong on the project's own
CI, not in the workspace. A worker runs a quick local loop, pushes, and reads the CI result through
Mainloop. This spec covers how an agent reads that result. Sandboxes have no `gh` CLI and cannot
reach the forge's API, so the read goes through Mainloop's MCP tools and the project's GitHub App.

Status: `check_status` and `watch_checks` are implemented. The per-project setting for where heavy
checks run and the worker guidance that uses both tools are proposed in follow-up slices and are
not described here as behavior yet.

## Providers

`server/internal/ci` reads CI through a `Provider` interface: resolve a pull request to its head
commit, list the checks on a commit, and read why one failed. GitHub is the only provider today
(`ci.GitHub`, over the existing `*github.App`). It reads **check runs** and **commit statuses**, so
it covers GitHub Actions and any other CI that reports to GitHub through either interface. Another
forge (GitLab, Bitbucket) would be a second implementation of the same interface; nothing above it
names GitHub.

The judgment of one check's result also lives in `internal/ci` and is shared with the merge
evidence (`server/internal/merge`), so the check tools and the merge decision agree:

| Check                         | Passed               | Pending                                                    | Failed                                                     |
| ----------------------------- | -------------------- | ---------------------------------------------------------- | ---------------------------------------------------------- |
| Check run (status/conclusion) | `success`, `skipped` | `queued`, `in_progress`, `pending`, `waiting`, `requested` | any other conclusion (`neutral` included) or unknown state |
| Commit status (state)         | `success`            | `pending`                                                  | anything else                                              |

A **check suite** that has not completed yet counts as one pending check when none of its own
check runs is pending: GitHub Actions creates a job's check run only when the job starts, so a
workflow whose later jobs wait on earlier ones (`needs:`) can show nothing but passed runs while
more are coming. A queued suite with no runs that is older than ten minutes is treated as
abandoned and ignored, the same rule the merge evidence uses.

Only the latest check of each name counts: the highest-id check run per (app, name), and the newest
commit status per context, so an earlier failed attempt of a rerun check is not reported.

The verdict over a commit is one of:

- `failed`: at least one check failed. Wins over pending, so the reader can start on a failure
  while other checks still run.
- `pending`: none failed and at least one is still running.
- `green`: every check passed.
- `no_checks`: nothing reported a check on the commit. Never green: a repository without CI, or a
  commit CI has not picked up yet, is not a pass.

## `check_status`

`check_status(project_id, pr_number | commit_sha)` reads the checks on one commit of the bound
project's repository now, without waiting. Give exactly one of:

- `pr_number`: the pull request's **current** head commit, resolved the same way the pull-request
  tools resolve it, so the answer reflects the latest push.
- `commit_sha`: a 40-character lowercase hex commit, so a worker can read CI on a pushed branch
  before a pull request exists (for workflows that run on push).

Both, neither, or a malformed SHA is a validation error. The tool returns:

| Field              | Meaning                                                                                                       |
| ------------------ | ------------------------------------------------------------------------------------------------------------- |
| `state`            | `green`, `failed`, `pending` or `no_checks`                                                                   |
| `sha`              | The commit read                                                                                               |
| `pr_number`        | The pull request, when one was given                                                                          |
| `counts`           | `passed`, `failed`, `pending`                                                                                 |
| `checks`           | Every check: `name`, `kind` (`check_run`/`commit_status`), `app`, `status`, `result`, `url`, times, `summary` |
| `failures`         | For up to five failed checks: `name`, `url`, `summary`, `text`, `annotations`, `log_tail`                     |
| `failures_omitted` | Failed checks beyond the first five; they are still listed in `checks`                                        |
| `text`             | The same, rendered for reading: the verdict line, the checks that are not green, and each failure             |

A failure carries the check's own output (`summary`: the check run's `output.title` and
`output.summary`, or the commit status's description; `text`: the check run's `output.text`) and
the check run's annotations (file, line, level, title, message). `log_tail` is reserved for the end
of the job log and is empty today: GitHub serves Actions job logs only to a token with
`actions: read`, which the App does not request yet. When the App gains that permission the GitHub
provider fills it; no tool or contract change is needed.

Bounds, so a noisy CI cannot flood an agent's context: `summary` is capped at 2,000 characters,
`text` and `log_tail` at 4,000 (the tail keeps the end), at most 20 annotations per failure with
each message capped at 500, and failure detail for at most five checks. Listings and the merge
reader are bounded at 1,000 check runs and 1,000 statuses per commit, as before.

An unknown pull request answers `[not_found] no such pull request`; a commit GitHub does not know
answers `[not_found] no such commit`; a GitHub read failure answers `[ci_unavailable]` with the
status code only, never upstream text. In a process without a GitHub App the tool answers
`[unavailable] CI checks are not available`.

### Credential and scoping

The tool uses the same GitHub App as the pull-request tools, with a token minted for the one
repository and the least permission each read needs: `pull_requests: read` to resolve a pull
request, `checks: read` for check runs and their annotations, `statuses: read` for commit statuses.
The annotations read (`GET /repos/{owner}/{repo}/check-runs/{id}/annotations`) is the only new
request path; it is in the client's allowlist with `checks: read`, which the App already holds.

`check_status` is visible wherever `open_pull_request` is, with no separate flag. The actor's
session project must equal `project_id` (`projectAuthority`, before any GitHub read); the service
then resolves the project by owner and id and derives the repository from its stored `full_name`,
so another owner's project id is `not_found`. The tool takes no repository, owner, URL or token
argument, and it never reruns, cancels or triggers a check.

## `watch_checks`

`watch_checks(project_id, pr_number | commit_sha, request_id, timeout_minutes?)` waits for CI
without holding the agent's turn. It records a durable watch and returns at once with `watch_id`,
`state` (`watching`), `deadline` and `superseded`. The agent then ends its turn, so its workspace
can go idle and suspend while CI runs. When the watch settles, Mainloop sends the agent's session
one message; it steers into a running turn when the agent supports that, and otherwise queues as
the next turn, like any other message from Mainloop.

The watch settles with one outcome:

| Outcome           | When                                                                             |
| ----------------- | -------------------------------------------------------------------------------- |
| `green`           | Checks exist and none is pending or failed                                       |
| `failed`          | Nothing is pending and at least one check failed                                 |
| `no_checks`       | No check appeared within three minutes of the watch starting, or by the deadline |
| `pending_timeout` | Checks were still pending (or CI could not be read) at the deadline              |
| `not_found`       | The pull request or commit does not exist                                        |

A failure does not settle the watch while other checks still run, so the message reports every
failure at once. The message carries the same rendered text as `check_status` (verdict, checks
that are not green, and the bounded failure detail), headed with the ref and the watch id, and
ends with what to do next: push a fix and watch again, or for a timeout use `check_status`, watch
again, or run the checks in the workspace.

- `request_id` is required and makes the call idempotent per session: a retry with the same id
  returns the same watch; reusing an id for a different ref is refused (`request_id_reused`).
- `timeout_minutes` defaults to 30 and may be 1 to 120.
- A pull request watch re-reads the pull request's head on every poll, so a push during the watch
  is followed.
- A new watch of the same session and ref supersedes older ones still watching, so a
  push-and-watch-again sends one message, about the latest state.

### How it runs

The tool inserts a `check_watches` row (state `watching`, idempotency key `session_id` +
`request_id`, deadline) and notifies the watch loop; it makes no GitHub request. The loop runs in
`serve-api` as a reconciler (`server/internal/reconcile`): it claims due watches, reads the checks
with no transaction open, and polls again later (every 15 seconds for the first two minutes, then
every 30 seconds until ten minutes, then every minute) until the watch settles. Settling records
the outcome and message (`watching` to `delivering`) under the claim's fence, then sends the
message under the watch id. The send is idempotent on that id, so a delivery whose result is
unknown is retried without a duplicate. A busy session is retried after a few seconds. A session
that has ended or is not ready is retried until 30 minutes after the deadline and then marked
`undeliverable`, as is a watch whose session or project is gone. Final states are `delivered`,
`undeliverable` and `superseded`.

The watch uses the same credential and scoping as `check_status`: `projectAuthority` before the
row is written, the project resolved by owner, and the watch bound to the calling session, which is
the only session it can message.

### Why this tool returns check detail and `merge_pull_request` does not

`merge_pull_request` deliberately strips check detail from its answer (`blocked` /
`checks_failed`, with no check name or upstream text): the merge decision is where arbitrary CI
text must not leak into agent-visible content unfiltered. `check_status` is the separate, bounded,
diagnostic read that answers the question `merge_pull_request` leaves open: which checks failed,
and why. It is agent-visible by design. Do not make either tool behave like the other.

`check_status` replaces `list_check_runs`, which returned a snapshot of a pull request's check runs
only: no commit statuses, no annotations or output text, and no reading by commit.

## Tests

The default tests use a fake GitHub HTTP transport (`server/internal/github/githubtest`) reached
through a real `github.App`, and scratch PostgreSQL; they are not live GitHub proof.
