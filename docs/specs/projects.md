# Projects

A project read (`GET /projects`, `GET /projects/{id}`, the detail) carries `supervisor`, the
project's [standing supervisor](project-supervisor.md), or null until it is first used.

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

The **Checks** section sets where the project's heavy checks run: Automatic, CI, Hybrid or
Workspace. It states what the setting resolves to and why (the owner's choice, or for Automatic the
CI Mainloop found on the default branch, or that none was found yet), what agents do under it, and
when the last re-check of the project's CI failed. Saving takes effect at once, with no version
check. See [CI checks](ci.md#where-heavy-checks-run).

The owner's per-project **instructions** for agents are edited in the Instructions section, first
under Settings, and carried in every task brief of the project. See
[project context](project-context.md#on-the-project-page).

A project can declare a **workspace service**, a development server kagent keeps running in each
of its workspaces across sleep, and the preview ports of those workspaces, with
`GET`/`PUT /projects/{id}/workspace-service`. The Workspace service section, last under Settings,
shows the saved command and ports. It edits the command one argument per line and the ports one
`name=number` per line (spaces at each line's ends dropped, blank lines skipped), and refuses a
draft over the contract's bounds (61 arguments of 4096 bytes; 16 ports, named and numbered as
`dev.ports` are, unique) before sending. An empty command removes the service; the ports stay
unless they are cleared too. A save applies to workspaces created afterwards, task workspaces
included. See [workspaces](workspaces.md#workspace-service).

The owner imports a repository as a project from **Import a project** on the Settings page
(`/settings#import-project`), with `owner/name` or a GitHub URL (`POST /projects`). Importing a
repository that is already a project returns that project; neither starts a workspace or session.
After an import the section links to the project page.

Both sections are proven by offline component tests against MSW handlers; not proven against a
live server.

## Onboarding proposals

An agent can propose a repository as a project, and the owner approves it with one tap. Main and
coordination supervisors (a project's standing supervisor or an ad-hoc one; not code tasks or
workspace agents) call the MCP tool `project_propose` with a `request_id`, the `repository`
(`owner/name` or a GitHub URL), `why`, and optionally the settings to apply:

- `environment_image`: digest-pinned, with an optional `environment_architecture`, which defaults to
  `amd64`.
- `verification_mode`: `auto`, `ci`, `hybrid` or `local`.
- `workspace_service`: the same shape and bounds as `PUT /projects/{id}/workspace-service`.
- `services` (at most 8) and `workspace_size`: recorded, not applied. Mainloop has no declared
  services or workspace sizes yet, so the card says so. A service such as Postgres can run inside the
  environment image instead.

Before it stores anything, Mainloop checks the repository with no transaction open:

- **GitHub App.** It asks whether its GitHub App is installed on the repository: `installed`,
  `not_installed`, or `unknown` with the reason (no App configured, GitHub unreachable). When the App
  is missing, the card and the tool result tell the owner to install it from GitHub's
  installed-apps settings before approving, because without it workspaces cannot clone or push.
- **Existing project.** It checks whether the repository is already one of the owner's projects. If
  so, approving applies the settings to that project.
- **Environment image.** It checks the image as environment registration does
  ([environments](environments.md)). A refused image fails the call (`[validation]`) with the
  registry's reason, and nothing is stored.

The proposal is then a durable row (`project_proposals`, state `proposed`), and an approval card
opens in Needs you in the same transaction. The card uses the existing queue path (kind
`approval`, queue item `project_proposal_<id>`) with the answers **Approve** and **Reject**. It
reads "Approve onboarding owner/name as a Mainloop project?" and its text lists:

- who proposed it, and why
- the GitHub App's installation
- whether the repository is new or already a project
- the image and the platform manifest digest the check found
- the checks mode and workspace service
- what is recorded but not applied

The owner answers with `POST /queue/{id}/respond`, which records the answer on the card and closes
it.

The same `request_id` returns the same proposal without checking again; the result is marked
`replayed`. The same `request_id` with a different proposal is `[conflict]`. While a repository has
a proposal that is proposed or approved and not yet applied, proposing it again under any request id
(the repository's case does not matter) returns that proposal and opens no second card. The result
carries the proposal id, its state, the queue item id, `/inbox?item=<queue item id>` and the facts
above.

### Applying an approved proposal

The `project-proposals` sweep in serve-api runs every five seconds.

**Recording the decision.** The sweep reads answered cards. **Approve** approves the proposal. Any
other answer rejects it and applies nothing, and the audit records the exact answer when it was not
**Reject**. A rejected proposal is finished, so the repository can be proposed again.

**Applying.** The sweep claims each due approved proposal with a two-minute lease and applies it
through the same services the owner's routes use. No transaction stays open across a step, and each
step reads the current state first:

1. **Import** (`POST /projects`'s path). This is idempotent per repository and returns the existing
   project when there is one.
2. **GitHub App.** It checks the installation again and records the result. A missing App does
   not stop the import; the audit line carries the install hint.
3. **Environment**, when an image was proposed. It reuses the owner's statically validated
   environment version for the same registry, repository, architecture and digest (index or
   platform manifest), or registers a new environment named after the repository. It then selects
   that version for the project, unless the project already uses it.
4. **Checks mode** (`verification_mode`), when one was proposed.
5. **Workspace service**, when one was proposed. It replaces the project's service and ports.

**Ending.** A proposal that applies is `applied` with its `project_id`.

- **Refused requests.** A request the API refuses (a 4xx other than 409 or 429, for example a
  registry that no longer serves the image) fails at once.
- **Other errors.** Any other error is retried after 30 s, doubling each time, for five attempts
  in all.
- **Failure.** A failed proposal opens a high-priority error item in Needs you ("Onboarding
  owner/name failed") with the reason.

**Audit.** Every step appends a line to `project_proposal_events`: the decision, import, GitHub App,
environment, checks, workspace service, each retry, and the end.

**Reading the outcome.** The proposer reads it back by calling `project_propose` again with the same
`request_id`: the state, the project id, attempts and the last error.

**Not built:**

- A web view of a proposal's audit.
- Telling the proposer when applying ends (it reads the outcome back instead).
- Declared services and workspace sizes.

Proven by Postgres tests with a fake GitHub App, a fake registry and the real project, environment
and checks services. Not proven against live GitHub or a registry.

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
each workspace was last active. While the default branch is still empty, the workspace form shows
the branch as pending and New workspace stays disabled until GitHub metadata arrives. A project
with no default branch yet stays listed.

The project page opens on the work: Tasks first, then Open Pull Requests, then Sessions, Branch
workspaces and Recent Commits. Settings comes last. It holds the instructions, the merge policy,
the checks setting and the environment, and it is collapsed by default. The owner opens it with its heading button, which is keyboard
operable and reports whether it is expanded. Its open or closed state is kept while the owner stays
on the page.

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
