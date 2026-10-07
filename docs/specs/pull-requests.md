# Pull request creation

The `open_pull_request` Mainloop MCP tool is available to authenticated `main` and `child`
bindings. Unknown roles cannot discover or invoke it. Standalone workspace sessions have no
MCP identity; this tool does not add one.

## Inputs and authority

Inputs are `project_id`, `branch`, `expected_sha` (40 lowercase hexadecimal characters),
`title`, `body`, and a stable `request_id`. The request cannot select a repository URL,
remote, base branch, API host, or token. Branch inputs must be feature branch names, not
revision expressions or fork-qualified heads.

Mainloop resolves the active binding and owner-owned project from PostgreSQL. A child also
needs its session's project, repository, and workspace repository to match the project, and
its requested branch to match the workspace branch. Revoked, archived, finished, or deleted
bindings cannot create PRs. The server verifies the repository identity and remote branch
SHA against GitHub. The head must be in the project's repository and cannot be its default
branch. The base comes from GitHub's current default branch, rather than cached project
metadata. Identity and authority are refreshed before creation.

Mainloop uses the backend `GITHUB_TOKEN` setting. Requests go only to `https://api.github.com`,
with bounded timeouts, response sizes, and pagination; redirects and environment proxies
are disabled. Each API call has a 15 second total deadline, a 10 second network timeout,
a 2 MB response limit, and PR listing stops at 10 pages of 100 results. Hitting a limit
fails closed. GitHub error bodies, transport errors, and credentials are never returned to
the agent. Returned PR links are constructed from verified repository identity and number.
The REST endpoints are described in [GitHub's pull request API](https://docs.github.com/en/rest/pulls/pulls).

## Deduplication and uncertain results

A PostgreSQL ledger records request IDs and normalized payload hashes per owner, and
serializes creation for each owner/repository ID/head/base tuple. Reusing an ID with another
payload conflicts. Another request ID for the same tuple and payload shares its creation;
a changed payload conflicts. Reusing a branch for a different candidate is therefore not
supported by this slice; use a new feature branch.

Before creating, Mainloop lists matching PRs, including closed PRs, and verifies their
repository IDs, branches, and head SHA. A single matching PR returns `state: created`,
`pr_number`, `url`, `head_sha`, and `base`. Several matches leave the result uncertain;
a mismatching head, base, or fork repository is refused.

Only the caller that first persists an intent may send its single creation POST. No database
transaction spans a GitHub request. After a timeout, crash, or unusable creation response,
retry the same request ID to reconcile by listing the persisted tuple. Mainloop does not
send another creation POST, even if the list is empty: absence cannot prove that an earlier
creation will never appear. A crash before the first POST can likewise leave an intent
uncertain. Uncertain results return `state: uncertain` and the request ID; they do not claim
success or failure. Already recorded results are returned without another creation.

GitHub does not provide a head-SHA precondition for PR creation. Mainloop validates the head
immediately before POST and verifies the response; if the branch changes during creation,
the outcome stays uncertain. Default-branch movement before dispatch refuses creation; no
implicit retargeting occurs.

## Scope and evidence

This tool creates PRs only. It does not merge, edit merge policy, create inbox items, approve
changes, or grant agents GitHub API access. Existing native-session delivery behavior is
unchanged. The default tests use scratch PostgreSQL and a fake GitHub HTTP transport;
they are not live GitHub or deployment proof.

Project policy and the generic decision storage contract are described in
[Merge policy](merge-policy.md). That foundation does not yet enable merge tools or
native HITL inbox actions.
