# GitHub issue, pull request and Dependabot alert reads

Three read-only Mainloop MCP tools, `get_issue`, `get_pull_request` and `list_dependabot_alerts`,
let a native session read a GitHub issue, a pull request under review, or the open Dependabot
security alerts of its bound project's repository. They exist because sandboxes have no `gh` CLI
and cannot reach `api.github.com` directly; before these tools, reading an issue's text, a pull
request's diff or an alert list had no path other than asking the owner to paste it into chat. CI
results are read with `check_status`, specified in [CI checks](ci.md).

## Credential reuse

All three tools call the same `*github.App` the [pull-request tools](pull-requests.md) use
(`server/internal/github`): the GitHub App ID and private key from `GITHUB_APP_ID` /
`GITHUB_APP_PRIVATE_KEY`, minting a short-lived installation token scoped to one repository and
the least permission the call needs (`issues: read` for `get_issue`, `pull_requests: read` for
`get_pull_request`, `vulnerability_alerts: read` for `list_dependabot_alerts`). No second credential, broader token or new App registration is
introduced. Every request path still passes through the client's fixed
allowlist the same way every other endpoint does: method, path pattern and the permission it
requires, checked before the request leaves the process.

All three are served by `server/internal/issues.Service`, a thin read layer parallel to
`prs` and `merge` but with no durable intent row or reconcile loop: a read that fails is simply
retried by the caller, so there is nothing to make idempotent. `mcpserver.Server.Issues` is nil in
a process built with no GitHub App, and then the tools answer `[unavailable] issues are not
available` (`[unavailable] pull requests are not available` for `get_pull_request`), the same
pattern `PRs` and `Merge` use.

## Scoping

All three tools take `project_id` and are visible on the same actors, at the same depths, as
`open_pull_request`: wherever an actor can open a pull request on its bound project, it can also
read that project's issues, pull requests and alerts, with no separate feature flag. The
coordination grant also holds `get_issue` and `get_pull_request`, alongside the
[issue writes](issue-writes.md), so a supervisor can read an issue or review a pull request before
commenting; the main thread may read an issue or a pull request in any of its owner's projects.
`projectAuthority` (shared with the pull-request tools) fails closed when the actor's session
project does not equal the `project_id` argument, before any GitHub read (the main thread excepted
for `get_issue` and `get_pull_request`); the service then
re-resolves the project from PostgreSQL by owner and id and derives the repository from its stored
`full_name`, so a project id belonging to another owner is `not_found`, not a cross-repository
read. No tool accepts a repository, owner, URL or token argument.

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

## `get_pull_request`

`get_pull_request(project_id, pr_number, paths?)` reads one pull request of the bound repository
for review. It is a read: it writes no row, and a failed read is retried by the caller.

| Field                                                 | Type    | Meaning                                                                                        |
| ----------------------------------------------------- | ------- | ---------------------------------------------------------------------------------------------- |
| `number`, `title`, `url`                              | -       | The pull request                                                                               |
| `body`, `body_truncated`                              | -       | The description, cut at 8,000 bytes                                                            |
| `state`                                               | string  | `open`, `closed` or `merged`                                                                   |
| `draft`, `author`                                     | -       | Whether it is a draft; the login that opened it                                                |
| `head_ref`, `head_sha`, `base_ref`                    | string  | The branches, and the head commit read                                                         |
| `mergeable`, `mergeable_state`                        | -       | GitHub's view; `mergeable` is absent while GitHub is still working it out                      |
| `additions`, `deletions`, `changed_files`             | integer | GitHub's totals                                                                                |
| `files[]`                                             | array   | `path`, `previous_path` (a rename), `status`, `additions`, `deletions`, `patch`, `patch_state` |
| `files_truncated`                                     | boolean | Fewer files are listed than the pull request changes                                           |
| `paths_not_found`                                     | array   | Named `paths` no listed file has                                                               |
| `feedback[]`                                          | array   | `kind`, `author`, `state`, `path`, `line`, `body`, `body_truncated`, `url`, `at`               |
| `feedback_omitted`, `feedback_ignored`                | integer | Older trusted items left out; items from untrusted authors, counted and never read out         |
| `checks.state`, `checks.counts`, `checks.not_green[]` | -       | The CI summary of the head commit, from the same reader as `check_status`                      |

The answer's text content is the same read rendered as one document (header, description, checks,
file list, diff, feedback), so a client that shows the model only text still gets the diff.

Bounds keep one answer within what an MCP client takes from a tool result (about 25k tokens in
Claude Code):

- **Files.** Up to 300 files, 30 to a page. A page GitHub answers larger than the client's 2 MiB
  cap ends the listing, with `files_truncated` set, instead of failing the read.
- **Patches.** All patches share one 32,000-byte budget, in the order GitHub lists the files, and one
  file's patch is cut at 16,000 bytes, at a line break. `patch_state` says what each file carries:
  `full`, `truncated`, `omitted` (the budget ran out; less than 1,000 bytes left is not spent on a
  fragment), `unavailable` (GitHub sends no patch for a binary file or a very large diff) or
  `not_requested`. With `paths` (up to 50), only the named files carry patches, so a reviewer reads
  a large pull request in several calls.
- **Feedback.** Submitted reviews, inline review comments and conversation comments, read the same
  way as `watch_checks` reads them (reviews up to three pages of 100; each comment listing one page
  of 100, oldest first). Only trusted authors' text is returned: the repository's `OWNER`, `MEMBER`
  or `COLLABORATOR`, or a bot. Unlike `watch_checks`, the pull request's own author is included, since
  a reviewer needs the author's replies. A pending review, and a `COMMENTED` review with no text
  (it only groups inline comments), are skipped. The newest 30 are kept, each cut at 1,500 bytes.
- **Checks.** Only the verdict, the counts and the checks that are not green. A failed check's own
  output and annotations come from `check_status`. A CI read that fails reports
  `checks.state: unavailable` and does not fail the pull-request read.

The description, patches and comments are other people's text. The tool description tells the
agent to treat them as data, not instructions; the trust filter keeps a stranger's comment on a
public repository from reaching the agent at all, but a pull request's own diff and description
always do. A number that names an issue, or no pull request, answers `[not_found] no such pull
request`. GitHub serves the files, reviews and comments under the App's existing Pull requests read
permission, so no new App permission is needed.

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

## Scope

These tools read only. They do not close or label an issue, do not review, approve or comment on a
pull request, do not dismiss or act on an alert,
and do not grant a sandbox direct GitHub API access or install `gh`. Filing an issue and commenting
on one are [issue writes](issue-writes.md). The default tests use a fake GitHub HTTP transport
(`server/internal/github/githubtest`) and scratch PostgreSQL; they are not live GitHub proof.
