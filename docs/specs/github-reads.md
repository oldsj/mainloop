# GitHub issue and Dependabot alert reads

Two read-only Mainloop MCP tools, `get_issue` and `list_dependabot_alerts`, let a native session
read a GitHub issue or the open Dependabot security alerts of its bound project's repository.
They exist because sandboxes have no `gh` CLI and cannot reach `api.github.com` directly; before
these tools, reading an issue's text or an alert list had no path other than asking the owner to
paste it into chat.

## Credential reuse

Both tools call the same `*github.App` the [pull-request tools](pull-requests.md) use
(`server/internal/github`): the GitHub App ID and private key from `GITHUB_APP_ID` /
`GITHUB_APP_PRIVATE_KEY`, minting a short-lived installation token scoped to one repository and
the least permission the call needs (`issues: read` for `get_issue`, `vulnerability_alerts: read`
for `list_dependabot_alerts`). No second credential, broader token or new App registration is
introduced. The request path is added to the client's fixed allowlist
(`server/internal/github/allowlist.go`) the same way every other endpoint is: method, path
pattern and the permission it requires, checked before the request leaves the process.

The two tools are served by `server/internal/issues.Service`, a thin read layer parallel to `prs`
and `merge` but with no durable intent row or reconcile loop: a read that fails is simply retried
by the caller, so there is nothing to make idempotent. `mcpserver.Server.Issues` is nil in a
process built with no GitHub App, and then both tools answer `[unavailable] issues are not
available`, the same pattern `PRs` and `Merge` use.

## Scoping

Both tools take `project_id` and are visible on the same actors, at the same depths, as
`open_pull_request`: wherever an actor can open a pull request on its bound project, it can also
read that project's issues and alerts, with no separate feature flag. `projectAuthority` (shared
with the pull-request tools) fails closed when the actor's session project does not equal the
`project_id` argument, before any GitHub read; the service then re-resolves the project from
PostgreSQL by owner and id and derives the repository from its stored `full_name`, so a project id
belonging to another owner is `not_found`, not a cross-repository read. Neither tool accepts a
repository, owner, URL or token argument.

## `get_issue`

`get_issue(project_id, issue_number)` reads one issue of the bound repository and returns:

| Field    | Type     | Meaning                                                      |
| -------- | -------- | ------------------------------------------------------------- |
| `number` | integer  | The issue number                                               |
| `title`  | string   | The issue title                                                |
| `body`   | string   | The issue body (empty string when GitHub reports none)        |
| `state`  | string   | `open` or `closed`                                             |
| `labels` | string[] | The label names                                                |
| `url`    | string   | The issue's `html_url`                                         |

GitHub serves issues and pull requests from the same numbering space and the same endpoint. A
number that names a pull request answers `[not_found] no such issue`: this tool reads issues only.
An issue number that does not exist answers the same refusal.

## `list_dependabot_alerts`

`list_dependabot_alerts(project_id)` lists the bound repository's open Dependabot alerts (GitHub's
`state=open`) as an `alerts` array, each entry:

| Field                      | Type   | Meaning                                                                                  |
| -------------------------- | ------ | ----------------------------------------------------------------------------------------- |
| `package`                  | string | The vulnerable dependency's name                                                          |
| `ecosystem`                | string | Its package ecosystem (`npm`, `pip`, `go`, ...)                                           |
| `severity`                 | string | `low`, `moderate`, `high` or `critical`                                                   |
| `vulnerable_version_range` | string | The version range the advisory matched                                                    |
| `patched_version`          | string | The first version that fixes it; empty when GitHub does not report one                    |
| `advisory_ghsa_id`         | string | The GitHub Security Advisory id                                                           |
| `advisory_summary`         | string | The advisory's short summary                                                              |
| `url`                      | string | The alert's `html_url`                                                                    |

GitHub's alert does not report the exact version a manifest resolved to; it reports the vulnerable
range the advisory matched (`vulnerable_version_range`) and, when known, the first fixing version
(`patched_version`). A repository with no open alerts returns `alerts: []`, not an error.

## Scope

These two tools read only. They do not create, close, comment on or label an issue, do not
dismiss or act on an alert, and do not grant a sandbox direct GitHub API access or install `gh`.
Issue creation and writing are a known separate gap, tracked elsewhere. The default tests use a
fake GitHub HTTP transport (`server/internal/github/githubtest`) and scratch PostgreSQL; they are
not live GitHub proof.
