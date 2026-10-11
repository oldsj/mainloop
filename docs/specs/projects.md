# Projects

The project detail page includes the current merge policy, a versioned selector and the server's
read-only protected-path patterns. Policies are **Auto** and **Approval required**. Auto still
requires server merge checks; protected-path changes always require owner approval.

When policy writes are off (the default), the current policy stays visible and the selector and
save action are disabled with an explicit “Editing disabled” explanation. No deployment setting
is enabled by the UI. With writes enabled, Save sends the current version and chosen value.
A conflict or failed/uncertain save refreshes the policy and asks the owner to review it before
saving again. Loading/read failures disable edits; Refresh reads current state without writing,
and the form stays locked until that read succeeds.

This selector changes policy only. It does not merge a pull request or approve a pending call.
See [merge policy](merge-policy.md) for server gates and unproved production prerequisites.

Project detail lists unarchived project sessions and excludes bindings with the `main` role.
Unbound sessions remain visible.

## Metadata

Mainloop owns the project row; GitHub supplies its description, URL, default branch, avatar and
open pull request and issue counts. Import makes one best-effort GitHub read, so a GitHub outage
or a missing App installation never fails an import and leaves an empty default branch.

The `project-metadata` sweep keeps that metadata current. Every minute it reads projects whose
default branch or URL is empty, or whose metadata is older than 15 minutes, empty ones first, up
to 25 per pass. It reads GitHub with no database transaction open and writes only what GitHub
answered: a GitHub error, a reply for a different repository or a reply without a usable default
branch or URL changes nothing (Mainloop never guesses a default branch). The failing project is asked again after five minutes, and other projects
are still refreshed. There is no owner refresh route; the sweep replaces it.

Every successful refresh also asks the push policy to follow GitHub's default branch (see
[push gate](push-gate.md)); that does nothing when the project has no policy or the policy already
names the branch, whether or not the push gate is on. The policy advances first and only then is
the project row written, so a policy that lags the row is repaired by the next pass. If the policy
cannot advance, the row stays as it was and the next pass retries; a manual import takes the same
path. A refresh writes only if the project's metadata is still as old as when it was read: if
another refresh stored newer metadata during the GitHub read, the older answer writes nothing and
leaves the policy alone. Proven by Postgres tests against a fake GitHub, including a real
push through the gate; not measured against live GitHub.

## React status (migration F7)

The React projects list, project page, merge policy section and new workspace from a repository
live in `web/src/features/projects`. The list drops a row that breaks the contract and renders the
rest. The page keeps its last loaded project when a refresh fails. The project page shows the F6
task tree. The desktop shell mounts the Projects panel. The compact shell has no Projects tab, as
[layout](layout.md) says. The page links to orchestration health, and its branch rows show when
each workspace was last active.

A create that gets no reply, or a 5xx, may still have made a workspace. The form then locks, the
raw error is hidden, and the owner sees recent workspaces to check against. The check lists the
typed repository's workspaces only, matched by `owner/name`, a GitHub URL or a `.git` URL, without
regard to case. The requested branch comes first; the repository's other workspaces follow. With no
repository typed, the check lists every repository's workspaces, still branch-first. The copy says
that a workspace still starting may not be listed yet. The server lists newest first, and the page
keeps that order inside each group rather than re-sorting. The form unlocks when the owner
acknowledges that check. Once that list read succeeds, a background refresh does not temporarily
disable the acknowledgment. The in-flight create and an uncertain reply, including the requested
repository and branch, stay in the app's cache through page or panel remounts. An unresolved check
lasts until it is acknowledged or the app reloads; each project and the repository panel keep their
own check. A 4xx shows the server's message as before.

Proven by offline component tests against MSW handlers; not proven against a live server. The
narrow-phone layout is not proven in a browser (deferred to F9).
