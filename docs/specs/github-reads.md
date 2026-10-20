# GitHub issue, Dependabot alert and check-run reads

Three read-only Mainloop MCP tools, `get_issue`, `list_dependabot_alerts` and `list_check_runs`,
let a native session read a GitHub issue, the open Dependabot security alerts, or a pull request's
GitHub Actions check runs, all of its bound project's repository. They exist because sandboxes
have no `gh` CLI and cannot reach `api.github.com` directly; before these tools, reading an
issue's text, an alert list, or which CI checks failed and why had no path other than asking the
owner to paste it into chat.

## Credential reuse

All three tools call the same `*github.App` the [pull-request tools](pull-requests.md) use
(`server/internal/github`): the GitHub App ID and private key from `GITHUB_APP_ID` /
`GITHUB_APP_PRIVATE_KEY`, minting a short-lived installation token scoped to one repository and
the least permission the call needs (`issues: read` for `get_issue`, `vulnerability_alerts: read`
for `list_dependabot_alerts`, `pull_requests: read` to resolve the pull request's head commit and
`checks: read` to read its check runs for `list_check_runs`). No second credential, broader token
or new App registration is introduced; `checks: read` and the check-runs request path were already
part of the client (the merge-evidence reader in `server/internal/merge` has used both since
before this tool existed), so neither needed adding to `permissionFields` or the allowlist
(`server/internal/github/allowlist.go`) — only the already-allowed pull-request read and
check-runs read are composed here. Every request path still passes through the client's fixed
allowlist the same way every other endpoint does: method, path pattern and the permission it
requires, checked before the request leaves the process.

All three tools are served by `server/internal/issues.Service`, a thin read layer parallel to
`prs` and `merge` but with no durable intent row or reconcile loop: a read that fails is simply
retried by the caller, so there is nothing to make idempotent. `mcpserver.Server.Issues` is nil in
a process built with no GitHub App, and then all three tools answer `[unavailable] issues are not
available`, the same pattern `PRs` and `Merge` use.

## Scoping

All three tools take `project_id` and are visible on the same actors, at the same depths, as
`open_pull_request`: wherever an actor can open a pull request on its bound project, it can also
read that project's issues, alerts and PR check runs, with no separate feature flag.
`projectAuthority` (shared with the pull-request tools) fails closed when the actor's session
project does not equal the `project_id` argument, before any GitHub read; the service then
re-resolves the project from PostgreSQL by owner and id and derives the repository from its stored
`full_name`, so a project id belonging to another owner is `not_found`, not a cross-repository
read. None of the three tools accepts a repository, owner, URL or token argument; `list_check_runs`
also never accepts a commit SHA, only the pull request number — see below.

## `get_issue`

`get_issue(project_id, issue_number)` reads one issue of the bound repository and returns:

| Field    | Type     | Meaning                                                |
| -------- | -------- | ------------------------------------------------------ |
| `number` | integer  | The issue number                                       |
| `title`  | string   | The issue title                                        |
| `body`   | string   | The issue body (empty string when GitHub reports none) |
| `state`  | string   | `open` or `closed`                                     |
| `labels` | string[] | The label names                                        |
| `url`    | string   | The issue's `html_url`                                 |

GitHub serves issues and pull requests from the same numbering space and the same endpoint. A
number that names a pull request answers `[not_found] no such issue`: this tool reads issues only.
An issue number that does not exist answers the same refusal.

## `list_dependabot_alerts`

`list_dependabot_alerts(project_id)` lists the bound repository's open Dependabot alerts (GitHub's
`state=open`) as an `alerts` array, each entry:

| Field                      | Type   | Meaning                                                                |
| -------------------------- | ------ | ---------------------------------------------------------------------- |
| `package`                  | string | The vulnerable dependency's name                                       |
| `ecosystem`                | string | Its package ecosystem (`npm`, `pip`, `go`, ...)                        |
| `severity`                 | string | `low`, `moderate`, `high` or `critical`                                |
| `vulnerable_version_range` | string | The version range the advisory matched                                 |
| `patched_version`          | string | The first version that fixes it; empty when GitHub does not report one |
| `advisory_ghsa_id`         | string | The GitHub Security Advisory id                                        |
| `advisory_summary`         | string | The advisory's short summary                                           |
| `url`                      | string | The alert's `html_url`                                                 |

GitHub's alert does not report the exact version a manifest resolved to; it reports the vulnerable
range the advisory matched (`vulnerable_version_range`) and, when known, the first fixing version
(`patched_version`). A repository with no open alerts returns `alerts: []`, not an error.

## `list_check_runs`

`list_check_runs(project_id, pr_number)` reads the GitHub Actions check runs at the bound pull
request's **current** head commit — the caller names the pull request, never a SHA; the service
resolves `pr_number` to its head commit the same way the pull-request tools do, so a stale or
guessed SHA cannot be asked about and the answer always reflects the latest push. It returns a
`check_runs` array, each entry:

| Field            | Type   | Meaning                                                                                            |
| ---------------- | ------ | -------------------------------------------------------------------------------------------------- |
| `name`           | string | The check run's name                                                                               |
| `status`         | string | `queued`, `in_progress` or `completed`                                                             |
| `conclusion`     | string | `success`, `failure`, `neutral`, `cancelled`, ... (empty until completed)                          |
| `started_at`     | string | When the run started, empty until GitHub reports it                                                |
| `completed_at`   | string | When the run finished, empty until GitHub reports it                                               |
| `output_summary` | string | The run's own short output (its `output.title` and `output.summary`), truncated to 2000 characters |

An unknown pull request number answers `[not_found] no such pull request`. A pull request with no
check runs yet, or every check run green, returns `check_runs` with their actual statuses, not an
error: this tool does not judge whether the pull request is mergeable, it only reports what
GitHub recorded for its checks.

### Why this tool returns check-run detail and `merge_pull_request` does not

`merge_pull_request` deliberately strips check-run detail from its own output before it reaches
an agent: its answer is only ever `blocked` / `checks_failed` or similar, with no check name or
upstream text, because that merge-decision path is exactly where arbitrary CI log or check-run
text must not leak into agent-visible content unfiltered. That stripping is intentional and
`list_check_runs` does not change it (`server/internal/merge/evidence.go` still reads check runs
for its own, undisclosed, merge judgment).

`list_check_runs` is a separate, narrower, diagnostic tool built to answer the question
`merge_pull_request` leaves open: _which_ checks failed and _why_. It is agent-visible by design,
not an oversight to fix, but it is still bounded: `output_summary` is capped at 2000 characters and
is only the structured `output.title` / `output.summary` GitHub's own check-run answer already
carries, never a raw or full CI log file, and never log output fetched from a separate logs
endpoint. A future reader should not "fix" this tool to strip its output the way
`merge_pull_request` does, nor change `merge_pull_request` to disclose check-run detail the way
this tool does: the two serve different, intentionally different-shaped, needs.

## Scope

These three tools read only. They do not create, close, comment on or label an issue, do not
dismiss or act on an alert, do not rerun, cancel or trigger a check, and do not grant a sandbox
direct GitHub API access or install `gh`. `list_check_runs` reads each check run's own structured
output only; it never fetches a raw or full CI log file. Issue creation and writing are a known
separate gap, tracked elsewhere. The default tests use a fake GitHub HTTP transport
(`server/internal/github/githubtest`) and scratch PostgreSQL; they are not live GitHub proof.
