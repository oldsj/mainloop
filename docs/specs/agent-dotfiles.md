# Agent dotfiles

Status: the source configuration; pinning, export and recording per Session; delivery to kagent
with the read-only layout; and the explicit refresh of a running Session with its "dotfiles
changed" hint, in the API and the web, are **implemented** for
[#313](https://github.com/oldsj/mainloop/issues/313), tested with fakes and not yet proven live.
Following the source automatically is **proposed**. Each section says which. See
[ROADMAP.md](../../ROADMAP.md#agent-context) for how this fits the "Agent context" direction.

Agents in Mainloop start without the owner's standing context: no owner-level instructions,
skills or subagents beyond a repository's own `AGENTS.md`. Agent dotfiles carry that context in as
native files from one directory of a git repository, laid out read-only in each agent's own
user-level configuration directory before its first turn. Mainloop adds no prompt layer, editor
or version store: editing means committing to the dotfiles repository, and Claude and Codex load
the result themselves, including after compaction.

Agents never get Git access to the dotfiles repository. Mainloop reads it server-side and exports
only the configured directory at one commit, filtered, bounded and scanned.

## Implemented: configuration

A source is `{repository, ref, path}`:

- `repository`: a GitHub `owner/name` the Mainloop GitHub App can read (`contents: read`).
- `ref`: a branch, tag or full commit SHA; empty for the repository's default branch. A full ref
  path (`refs/...`) and anything `git check-ref-format` refuses are `422`.
- `path`: a directory in the repository, relative, with no empty, `.` or `..` segment, no
  backslash and no control character; empty for the root. One trailing slash is dropped.

There are two levels. The owner-wide source applies to every session; a project's own source, while
set, replaces it for that project's sessions. There is no per-task or per-session override.

| Route                                 | Behavior                                                                                    |
| ------------------------------------- | ------------------------------------------------------------------------------------------- |
| `GET /dotfiles`                       | The owner-wide source; `repository: null` at version 0 when none was ever set.              |
| `PUT /dotfiles`                       | Set, or clear with `repository: null`. Compare-and-swap on `expected_version`; stale `409`. |
| `GET /projects/{project_id}/dotfiles` | The project's own source, same shape; another owner's or a missing project is `404`.        |
| `PUT /projects/{project_id}/dotfiles` | Same rules as `PUT /dotfiles`. Clearing reverts the project to the owner-wide source.       |

Clearing stores no ref or path. Stale writes answer `409` with reason `stale_dotfiles_source`; a
project that is not the owner's is `404 project_not_found`. Saving a source does not check that
the App can read it: that is reported per session (see "Failures" below).

The web shows the owner-wide source under Settings → Agent dotfiles, and the project's on the
project page, which names the owner-wide source the project falls back to when it has none. Each
shows the current source linked to its tree on GitHub (`https://github.com/<repository>/tree/<ref
or HEAD>/<path>`, built only from a repository that matches `owner/name`, with the ref and path
segments URL-encoded), a form for repository, ref and directory, and a button to clear it.

Example (fictional): owner-wide `example/agent-dotfiles` at `main`, directory `home/.agents`; a
project overriding with `example/agent-dotfiles` at `project-x-stable`.

## What is read from the directory

Implemented: only these paths under `path` are exported; everything else in the repository is
ignored. A `skills/<name>/` directory without a `SKILL.md` at its root is not a skill and is
ignored whole. The destinations are where kagent lays them out (see "Delivery and layout").

| Source                             | Claude Code (`CLAUDE_CONFIG_DIR`)                     | Codex (`CODEX_HOME`)               |
| ---------------------------------- | ----------------------------------------------------- | ---------------------------------- |
| `AGENTS.md`                        | `AGENTS.md`, plus `CLAUDE.md` containing `@AGENTS.md` | `AGENTS.md`                        |
| `CLAUDE.md` (optional)             | used as-is instead of the generated one               | not mapped                         |
| `skills/<name>/**` with `SKILL.md` | `skills/<name>/`                                      | `skills/<name>/`                   |
| `agents/<name>.md`                 | `agents/<name>.md` (subagents)                        | not mapped (Codex agents are TOML) |
| `commands/<name>.md`               | `commands/<name>.md`                                  | not mapped (deprecated in Codex)   |

The destinations are the CLIs' user-level directories as kagent sets them for a session
(`CLAUDE_CONFIG_DIR=/data/claude`, `CODEX_HOME=/data/codex`), not `~/.claude` or `~/.codex`. Claude
loads only the user setting source, which is that directory. Codex still scans `$CODEX_HOME/skills`
as a user skills root (its current docs name `~/.agents/skills`; `$CODEX_HOME/skills` is kept for
compatibility), which does not depend on how `HOME` is set.

## Implemented: pinning, export and recording (Mainloop)

Before Mainloop sends a kagent CreateSession for a workspace (a code or coordination task's
session), a native agent session, or the main thread, it pins the dotfiles of that create request.
The effective source is the session's project's own source when that names a repository, else the
owner-wide one; the main thread has no project, so it uses the owner-wide one. Then, with no
transaction open:

1. The ref is resolved to a full commit SHA through the GitHub App (an empty ref is the default
   branch).
2. Unless that commit's directory was exported before, the commit is fetched server-side through
   the App's git upstream (the installation token never leaves the server) and archived.
3. Only the allowed paths are kept. The export is refused when one of them is a symlink or any
   other non-regular file, or past the limits: more than 4000 files, a file over 2 MiB, or more
   than 16 MiB in all. Submodules are not part of a git archive. A directory with none of the
   allowed files is refused as `nothing_to_export`.
4. Exactly the exported files are scanned for secrets: private keys; GitHub, AWS, Slack,
   Anthropic, OpenAI, Google and Stripe key formats; JWTs; credentials in URLs; and a literal
   assigned to a credential-named variable or key (`export X_TOKEN=...`, `"client_secret": "..."`;
   `$VAR` and `${VAR}` references are not literals). Any match refuses the export. Only the file
   path and rule name are stored, never the matched text.
5. The kept files are written as a deterministic tar (entries sorted by path, mode 0444, or 0555
   where git marked the file executable, no owner, every time the Unix epoch) and stored once by
   its `sha256:` digest.

A commit's directory always exports the same way, so a ready or refused export is kept per
`(repository, commit, path)` and not read again; an unavailable read is not kept. Each GitHub read
is bounded to 10 seconds, so a pin fits inside the delivery lease it runs under.

The pin `{repository, ref, path, commit, digest, status, reason}` is recorded per create request,
first write wins. A retried or replayed create therefore sends what its first attempt chose, while
a new native Session of the same Mainloop session (a provider switch, a replacement) pins again. A
fork records its source session's newest pin, since it starts from that Session's data. A task's
retry or reassign is a new session and pins anew. A change to either source never moves a live
Session.

A database failure while pinning holds the create and the reconciler retries it; nothing else
does.

The session page's identity strip shows the newest pin under "details", as
`<repository> @ <short commit>, <path>/` linked to `https://github.com/<repository>/tree/<commit>/<path>`
with the digest on hover, or the reason there is none. `GET /sessions/{session_id}/native` carries it
as `dotfiles` (absent for a Session created before pins were recorded, or on a server without a
GitHub App).

## Implemented: delivery and layout

A create whose pin is `ready` and that carries the session's MCP credential sends
`dotfiles = {url, digest, repository, commit, path}` on its kagent CreateSession. The url is
`<MCP origin>/dotfiles/<digest>`, on the origin of that credential, so kagent's gateway injects the
credential when the runtime downloads it; no new grant and no new egress origin is needed. The url
is recorded with the pin, so a retried or replayed create sends the same. kagent does not compare
dotfiles when it matches a replay with its first request, since a refresh can change them. A create without an MCP credential (a session whose grant is `none`, or a
server without Kubernetes) sends no dotfiles; its pin is still recorded and shown.

Mainloop's MCP listener serves `GET /dotfiles/{digest}` with the bundle (`application/x-tar`,
`no-store`). It authenticates the bearer as the MCP paths do, and answers `404` for any digest the
caller's session was never pinned to, whether or not it exists: a session gets its own bundle and
its fork source's, nothing else.

kagent refuses a url whose origin carries none of the session's credentials, downloads the bundle
(no redirects), verifies the digest and the same limits, and lays it out in the destinations above
before the first turn of each runtime process. It executes nothing from the bundle. A download or
layout failure is logged and the turn runs without new dotfiles, keeping any already laid out; the
next turn retries.

Files are 0444 (0555 where git marked them executable, so a skill's scripts still run when the
agent uses that skill), directories 0555. The harness rewrites its dotfiles entries from the pinned
bundle on every start, and again before any turn that finds one missing, so an agent's edits never
persist; permissions alone are not the guarantee, since the agent runs as the same user. The
agent's own compiler-selected skills win a name clash with a dotfiles skill. Everything lives under
`/data`, so a Data snapshot carries the bundle into a fork, which lays it out again; this is
consistent with the fork keeping its source's commit. The Claude SDK runner lays the files out
before its bridge starts. Mainloop starts it with the managed profile (`CLAUDE_CONFIG_DIR` under
`/data`), which reads them.

## Implemented: refresh

A native Session keeps the commit it was created with. Task and agent sessions are short-lived, so
a source change reaches their next session. The main thread keeps one native Session until a
provider switch or a restart, so without a refresh it can run on an old commit for a long time.

`GET /sessions/{session_id}/dotfiles` shows:

- `current`: the pin the native Session holds, from its create or its latest applied refresh.
- `source`: the source that applies now, and the commit its ref names. The ref is read from
  GitHub at most once a minute per source, never while a session is shown elsewhere.
- `behind`: whether a refresh would give the Session other files. It is true when the source
  resolved and `current` is from another commit, repository or directory, or there is no
  `current`. A ref that changed but names the same commit is not behind.
- `refreshable`: whether a refresh applies to the session.
- `refresh`: the session's latest refresh.

`POST /sessions/{session_id}/dotfiles/refresh {request_id}` refreshes the main thread or a
standalone native session to the source's current commit. A workspace session keeps the
dotfiles it was created with (its next task gets the current ones). An ended or archived
session, one with no native Session running, and a main thread switching provider are a `409`.

The refresh follows the reconciler rule:

1. Under the session's send lock, it writes a durable intent (`session_dotfiles_refreshes`), keyed
   by `request_id`, naming the native Session it updates. At most one refresh per session is
   pending; another request while one is pending continues that one.
2. With no transaction open, it pins the source as a create does, under the request
   `refresh:<request_id>`: the commit the ref names now, that commit's export, and the url on the
   MCP origin.
3. It reads the Session from kagent. A Session that already holds these dotfiles is done. Otherwise
   it calls kagent's `UpdateSessionDotfiles`, a compare-and-swap on the digest it just read, so a
   Session created before kagent stored dotfiles is refreshed too, and one whose dotfiles changed
   in between is read again.
4. The refresh is `applied`, or `failed` with a reason:
   - `no_bundle`: the pin's own `status` and `reason` say why.
   - `not_deliverable`: no MCP credential to download with, or kagent refused the url.
   - `session_gone`
   - `unsupported`: a kagent without `UpdateSessionDotfiles`.

   A failed refresh leaves `current` as it was. An attempt kagent did not answer stays `pending`
   and is a `502`. Repeating the request continues it, and so does a sweep that picks up refreshes
   left alone for a minute.

kagent lays the new bundle out before the Session's next turn, never during one. The Claude SDK
runner's bridge outlives turns, so kagent stops it between turns when it is idle, and its next start
resumes the conversation. Codex and the Claude process driver start their CLI per turn. A pending or
failed refresh's bundle can be downloaded by the session (it is pinned to it), but the session only
shows it once applied. A fork records its source's latest applied pin.

The web shows the hint under the main thread's header and under a session page's header, live
or not, only while the session is `refreshable` and `behind` or its latest refresh is pending:

- `dotfiles changed: <held commit> → <source commit>`, with a `diff` link to GitHub's comparison of
  the two when both are in one repository. A session that holds another repository or directory
  names both in full; one created without dotfiles reads `dotfiles available: <source>`.
- A Refresh button. Each refresh the owner asks for has one request id, kept until the server
  answers: retrying after a `502` or a lost reply repeats it, so the server continues the same
  refresh. A pending refresh reads `refresh not confirmed yet` and offers Retry refresh.
- A failed refresh of the source's current commit gives its reason (for `no_bundle`, the pin's own
  `status` and `reason`). An older failure is not shown.
- An applied refresh reads `dotfiles refreshed to <commit>; the next turn reads them`. A `409`
  shows the server's reason; other errors add "Try again."

The page reads `GET /sessions/{session_id}/dotfiles` once a minute while it is visible, and every
ten seconds while a refresh is pending.

Proposed: following the source automatically, if chosen, is a setting that calls the same refresh
between turns when `behind`; it needs no other mechanism.

## Implemented: failures

Dotfiles are context, not a precondition. None of these blocks session creation; the session runs
without dotfiles and records the reason where the commit would show:

| Case                                                | `status`      | `reason`                                      |
| --------------------------------------------------- | ------------- | --------------------------------------------- |
| No source set                                       | `none`        |                                               |
| App not installed on the repository, or unreachable | `unavailable` | `app_not_installed`, `repository_unavailable` |
| Ref does not exist                                  | `unavailable` | `ref_not_found`                               |
| Over a limit, or a non-regular file                 | `refused`     | `over_limits`, `unsupported_file`             |
| None of the allowed files in the directory          | `refused`     | `nothing_to_export`                           |
| Secret found                                        | `refused`     | `secret_found`                                |

## Precedence

Dotfiles only ever write the user level; repositories keep their own files. Each CLI's own
precedence applies, and Mainloop adds none. Checked against the CLIs' documentation, not live:

- Claude: user and project `CLAUDE.md` both load. On a name clash a personal skill wins over a
  project skill, while a project subagent wins over a user one.
- Codex: the user `AGENTS.md` comes first, then project files from the repository root to the
  working directory. Two skills with the same name both appear.

So a dotfiles skill should not reuse the name of a repository's skill.

## Relation to other agent context

- Dotfiles `AGENTS.md` is the owner-level instruction document the roadmap describes: owner-wide
  communication style, evidence rules and review expectations, versioned by git.
- Mainloop's shared agent guidance ([delegation.md](delegation.md#shared-agent-guidance)) stays: it
  states the session's role, workspace and tools, which a dotfiles file cannot know.
- Per-project instructions and memory stay in Mainloop ([project-context.md](project-context.md)),
  editable from a phone and rendered into briefs. Repository facts stay in the repository's
  `AGENTS.md`.

## Open questions

- Whether a refused or unavailable export should block session creation instead of running
  without dotfiles.
- Whether to also map `agents/*.toml` to Codex custom agents.
- Garbage collection of bundles no session references.
- Whether the main thread follows its source automatically (see "Implemented: refresh").
- Live proof: nothing here has run against GitHub, kagent or real CLIs; the export is tested with a
  local git repository, delivery and layout with fakes. It needs a main-thread and a workspace
  session on mainloop-dev with a fictional dotfiles repository, checking that each runtime fetches
  the bundle through the gateway, that each CLI loads the instructions and skills, that the files
  are not writable and are restored after a restart, and that the session shows the commit; then a
  commit to the fictional repository, a refresh of the main thread, and its next turn reading the
  new `AGENTS.md` with the conversation intact.
