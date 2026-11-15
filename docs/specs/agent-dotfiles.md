# Agent dotfiles

Status: the source configuration is **implemented** (owner-wide and per-project, API and UI).
Pinning, export, delivery and layout are **proposed** for
[#313](https://github.com/oldsj/mainloop/issues/313); each section says which. See
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

## Proposed: what is read from the directory

Only these paths under `path` are exported; everything else in the repository is ignored.

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

## Proposed: pinning and export (Mainloop)

When Mainloop creates a kagent Session — a workspace, a coordination task's session, or the main
thread's — it picks the effective source (the project's own, else the owner-wide one; the main
thread uses the owner-wide one), and before the first CreateSession:

1. Resolves the ref to a full commit SHA through the repository-scoped GitHub App client, as a
   workspace's checkout ref is resolved.
2. Fetches that commit server-side with an installation token that never leaves the server, and
   reads the directory at that commit.
3. Keeps only the allowed paths, and refuses the export when it holds a symlink, a submodule or any
   other non-regular file, more than 4000 files, a file over 2 MiB, or more than 16 MiB in all.
4. Scans exactly the exported files for secrets (private keys, provider token formats, credentials
   in URLs, literal credential assignments). A match, or a scan that cannot finish, refuses the
   export. Only file paths and rule names are recorded, never matched text.
5. Writes a deterministic tar (sorted entries, modes 0444 or 0555, no times or owners), stored once
   per `sha256` digest.

The session records `{repository, ref, path, commit, digest, status, reason}`, pinned once and
frozen: a replayed create, a replaced kagent Session or a provider switch reuses it; a fork copies
its source session's; a task's retry or reassign is a new session and pins anew. A change to either
source never moves a live session. The workspace manifest and the session page show the commit as
a short SHA linked to `tree/<commit>/<path>`.

## Proposed: delivery and layout (kagent)

CreateSession carries `dotfiles = {url, digest, repository, commit, path}`. The url is the bundle
on Mainloop's MCP listener, authenticated by the session's existing MCP credential, which kagent's
gateway already injects for that origin; no new grant and no new egress origin is needed. kagent
refuses a url whose origin carries none of the session's credentials, downloads the bundle,
verifies the digest and the same limits, and lays it out in the destinations above before the
first turn. It executes nothing from the bundle.

Files are 0444 (0555 where git marked them executable, so a skill's scripts still run when the
agent uses that skill), directories 0555. The harness rewrites its dotfiles entries from the pinned
bundle on every start, so an agent's edits never persist; permissions alone are not the guarantee,
since the agent runs as the same user. The agent's own compiler-selected skills win a name clash
with a dotfiles skill. Everything lives under `/data`, so a Data snapshot carries the laid-out files
into a fork, consistent with the fork keeping its source's commit.

## Proposed: failures

Dotfiles are context, not a precondition. None of these blocks session creation; the session runs
without dotfiles and records the reason where the commit would show:

| Case                                                | `status`      | `reason`                                      |
| --------------------------------------------------- | ------------- | --------------------------------------------- |
| No source set                                       | `none`        |                                               |
| App not installed on the repository, or unreachable | `unavailable` | `app_not_installed`, `repository_unavailable` |
| Ref does not exist                                  | `unavailable` | `ref_not_found`                               |
| Over a limit, or a non-regular file                 | `refused`     | `over_limits`, `unsupported_file`             |
| Secret found, or scan failed                        | `refused`     | `secret_found`, `scan_failed`                 |

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
- Live proof: none of the proposed behavior has run against real CLIs. It needs a main-thread and a
  workspace session on mainloop-dev with a fictional dotfiles repository, checking that each CLI
  loads the instructions and skills, that the files are not writable, and that the session shows
  the commit.
