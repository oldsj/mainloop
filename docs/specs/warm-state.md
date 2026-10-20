# Durable project warm state

Status: proposed. Nothing in this document is implemented. It is the measure-then-design output
for [#288](https://github.com/oldsj/mainloop/issues/288): a Mainloop-held toolchain config and a
per-project warm seed so that the second session in a repository is as fast as a warmed laptop
checkout, for any language, with nothing committed to the repository. See
[ROADMAP.md](../../ROADMAP.md#durable-project-warm-state) for how this fits the rest of the
product direction, and [environments.md](environments.md) for the environment registration and
selection this extends.

## Measurement: what this task could and could not reach

The task asked for live, bounded measurement on `mainloop-dev` through the `dev_*` MCP tools
(`docs/specs/devloop.md`) before writing the design below, and to say exactly what could not be
reached rather than improvise around it. This section records both.

### What was checked

- `server/third_party/kagentapi/sessions.pb.go`, `runtime.pb.go`, `common.pb.go` (vendored from
  `oldsj/kagent` commit `ce402bf76151bf9492e5dda53f310547794c3190`, per
  `server/third_party/kagentapi/SOURCE`): `SessionService` exposes exactly `CreateSession`,
  `PrepareSessionWorkspace`, `GetSession`, `ListSessions`, `UpdateSessionName`, `SuspendSession`,
  `ResumeSession`, `DeleteSession`, and the session-share calls. There is no fork call.
  `CreateSessionRequest` carries an `Agent`, `Workspace` (repo/ref/branch/depth),
  `Credentials`, and `DevelopmentEnvironment` (image/platform/policy identity); none of these
  carry a source-session, checkpoint, or snapshot reference. `RuntimeState` has
  `CREATING, READY, SUSPENDED, FAILED, DELETING, DELETED`; `RuntimeOperation` has
  `CREATE, SUSPEND, RESUME, DELETE, NONE`. Neither has a forking or checkpointing value. Two
  comments on `Session` (`sessions.pb.go:146,151`) say "Forks receive a new conversation ID and
  new task IDs" and "forks do not inherit these \[credential\] bindings" — kagent's own schema
  anticipates forking as a concept, but the RPC that would create one is not in this vendored
  slice.
- `server/internal/kagent/{client,sessions,agent,wire,template}.go`: the Go client built on that
  vendored API. It wraps exactly the calls above (`client.go:28-35`) and has no fork, checkpoint,
  or snapshot handling anywhere.
- `scripts/sync-kagent-api.sh`: refreshing the vendored API needs a local checkout of the
  `oldsj/kagent` fork (`$KAGENT_FORK`, default `~/dev/kagent`) to run `git show` against a chosen
  commit. No such checkout exists in this workspace, so a newer kagent commit that might add a
  fork RPC could not be checked from here.

  **Update, same day, a later task on this branch:** the paths and tool above no longer exist.
  `server/third_party/kagentapi` and `scripts/sync-kagent-api.sh` were replaced by
  `server/third_party/kagentproto` (full `.proto` sources, generated Go and Connect clients, see
  its `SOURCE`), pinned to `oldsj/kagent` commit `a1164013675c2ad49ec4cc2b6b12d3dfa4a456d6`. That
  vendored set does include `checkpoints.proto`'s `CheckpointService`, with a `ForkSession` RPC —
  so "the RPC that would create one is not in this vendored slice" above no longer holds. Nothing
  reachable from this workspace could prove `ForkSession` live (same reasons as below), and
  `server/internal/kagent/client.go` still wires no behavior for `CheckpointService`: that PR
  generated the client only, deliberately out of scope for wiring it up. The next task that does
  should repeat the measurement below against `ForkSession` before relying on this document's
  "not reachable" conclusion.

- The GitHub API is unreachable from this workspace (see `AGENTS.md`), so kagent's own
  `docs/architecture/runtime-and-lifecycle.md`, referenced by issue #288, could not be read either,
  to confirm how a Data-checkpoint fork is invoked or what it preserves.
- `docs/specs/devloop.md` and the four `dev_*` MCP tools it describes: `dev_lease_acquire`,
  `dev_lease_release`, `dev_sync(commit_sha)`, `dev_status`. These sync a pushed commit's
  `server/` and `web/` trees into `mainloop-dev`'s four existing pods (backend, mcp, git,
  frontend) over a Kubernetes exec tar pipe run by `serve-api`'s reconcile loop; `serve-mcp`,
  where the tool handlers run, never imports a Kubernetes client at all (devloop.md, "What it
  does"). None of the four creates, forks, lists, or inspects a kagent Session. There is no tool
  on this surface that reaches `SessionService` at all.
- `mcp__mainloop__dev_status` was called (read-only, no lease needed) to confirm this directly:
  it returned three pods — `mainloop-backend-7774856745-6t7dm`, `mainloop-frontend-646c8c6c88-zmstj`,
  `mainloop-pg-1` — each with phase, readiness and container restart counts, and nothing else.
  That is the entire live surface this task's workspace can reach on `mainloop-dev`.
- This task's own workspace carries no Kubernetes credential, kubeconfig, or kagent control
  token (by design — see the devloop.md credential boundary and `docs/specs/credentials.md`'s
  control-service-credential section), so there is no way to call `SessionService` directly from
  here either, even if the RPC existed.

### What follows from that

The fork operation issue #288 asks this task to measure — forking a warmed session from a Data
checkpoint, onto a different task and branch than its source — **is not reachable from this
workspace or from Mainloop's current server code**. It is not vendored into the kagent API
Mainloop talks to, it is not exposed by any `dev_*` tool, and this task has no path to kagent's
own documentation or a newer commit to check whether it exists upstream. Per the task's
instruction, this is reported rather than worked around: no fork was attempted, no fork timing
number appears below, and the design in this document does not depend on forking being available
soon.

### Numbers, labeled

| Number                                                                                          | Value                                 | Label                                                                                                                                                                                                                                                                                                                    |
| ----------------------------------------------------------------------------------------------- | ------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Fork time from a warmed Data checkpoint                                                         | —                                     | **Unknown.** The operation could not be invoked (see above).                                                                                                                                                                                                                                                             |
| Fork serving a different task/branch than its source; ownership, identity, credential carryover | —                                     | **Unknown**, same reason.                                                                                                                                                                                                                                                                                                |
| Cold pull of today's cache image (zstd) + golden preparation + clone                            | ~10s pull, stated in issue #288       | **Not independently measured in this task.** Carried over from the issue text; `.github/README.md`'s "Workspace cache image" section corroborates the pipeline (BuildKit, zstd layers, a smoke step before `cache-main` moves) but states no elapsed number.                                                             |
| Cold workspace on the toolchain image only (first `go build`, first `pnpm install`)             | —                                     | **Unknown.** Would need a toolchain-only environment registered and a workspace created from it; out of this task's reach (no environment-management tool is on this task's MCP surface, and creating one against `mainloop-dev` was not attempted since it would not answer the fork question this task was scoped to). |
| `dev_status` round trip                                                                         | succeeded, 3 pods, 0–12 restarts each | **Measured**, via `mcp__mainloop__dev_status`, this session, 2026-10-20. Confirms the tool surface, not a timing fact about warm state.                                                                                                                                                                                  |

If a later task gets a path to kagent's fork RPC, now vendored as `CheckpointService.ForkSession`
in `server/third_party/kagentproto` (see the update above) but not yet wired into
`server/internal/kagent`, or to kagent's own lifecycle docs, repeat this measurement before
committing to seed-fork as the mechanism. Until then, this spec treats fork as unproven.

## Mechanism: cache archive now, seed fork later if it measures well

Issue #288 offers two mechanisms: a seed fork from a kagent Data checkpoint, or its fallback, a
per-project cache archive in object storage or the in-cluster registry (zot), restored at
workspace start. This spec chooses the **cache archive** as the first implemented mechanism, for
one reason: it is the only one of the two whose primitives are provable today. Seed fork depends
on a kagent capability this task could not find in the vendored API, could not find in local
kagent sources, and could not confirm via kagent's own docs (GitHub unreachable). Building the
warm-state design around an unverified, currently unreachable primitive would violate the
project's own evidence rule (AGENTS.md: "never present a fixture, mocked integration, or agent
claim as live proof").

The cache archive also generalizes a pattern this project already runs in production: `cache-main`
(`environments.md`, "Following a watched tag"; `.github/README.md`, "Workspace cache image") is
exactly a warm-state archive today, just baked into an image, built on a fixed schedule, for one
project only, with a recipe that lives in the repository. This design keeps the same shape —
checkout plus cache directories, captured after a successful warm-up, restored at workspace
start — and moves three things Mainloop should own instead: which projects get one, what the
toolchain and cache directories are (not hardcoded to Go and pnpm), and where the config and
refresh state live (Mainloop's database, not `images/workspace-cache/` and `.devcontainer/` in
this repository).

If kagent's fork RPC becomes reachable and a later measurement task finds it fast, cross-task, and
safe on credentials, the archive mechanism's surrounding design does not need to change: the
toolchain config, the reconciled refresh intent, credential scrubbing, owner visibility, and the
registry allowlist are all mechanism-agnostic. Only the restore step — unpack an archive vs. fork
a checkpointed session — would be swapped, behind the same seed-state row described below.

## Toolchain config: Mainloop-held, devcontainer.json-shaped

A project's toolchain config is a row Mainloop stores and the owner edits, never read from the
project's repository (the vision in #288: "nothing committed to the repository; all configuration
is Mainloop-held at project level"). Its shape borrows the devcontainer.json fields that matter
here, dropping the ones that assume a devcontainer runtime:

| Field                | Meaning                                                                                                                                          | Source                                                                     |
| -------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------ | -------------------------------------------------------------------------- |
| `base_environment`   | The registered `Environment`/`version_id` to start from (`environments.md`)                                                                      | existing environment registration                                          |
| `setup_script`       | Run once, from a clean checkout on `base_environment`, to produce the warm state (`go mod download`, `pnpm install --frozen-lockfile`, …)        | new                                                                        |
| `maintenance_script` | Run on each scheduled refresh before re-archiving; optional, defaults to `setup_script`                                                          | new, mirrors devcontainer's `updateContentCommand`                         |
| `cache_paths`        | Directories under `/data` the archive preserves beyond the checkout itself (e.g. `/opt/cache` today; project-declared, not hardcoded to Go/pnpm) | new, generalizes the `/opt/cache` env vars `agentcontext.go` already names |
| `environment`        | Extra environment variables the setup/maintenance script and later sessions need (cache directory locations, etc.)                               | new, mirrors devcontainer's `containerEnv`                                 |
| `max_seed_age`       | How stale a seed may get before it stops being offered and raises an attention item                                                              | new                                                                        |

No feature/customization layer (devcontainer's `features`) is proposed yet: #288 asks for a
language-agnostic warm-state mechanism, not a devcontainer-compatible runtime, and adding one
without a concrete need would be speculative. `base_environment` continues to go through the
existing single-registry-allowlisted, digest-pinned, statically-validated path in
`environments.md`; this config adds the setup/maintenance/cache/env fields next to it, scoped to
one project, editable without a repository change or a new PR.

## Seed refresh as a reconciled intent row

Refreshing a project's warm state (producing a new archive, or re-checkpointing a seed session
if fork is adopted later) is a durable side effect and follows the reconciler rule already used
elsewhere in this codebase (AGENTS.md; `server/internal/reconcile`; `environments.md`'s
watched-tag follow sweep; `devloop.md`'s `devloop_operations`): a `warm_seed_refresh` row with
an explicit state (`planned`, `running`, `succeeded`, `failed`) and an idempotency key of
`(project_id, default_branch_commit, config_revision)`, so a merge to the default branch, a
config edit, and a scheduled sweep that all land on the same commit and config collapse into one
row rather than racing. Triggers: a merge to the project's default branch, a config edit (new
`config_revision`), and an operator-configured schedule for projects whose default branch is
quiet. The reconcile loop claims a `planned` row, checks the remote state it would be about to
change (does an archive for this exact key already exist in the registry/object store?) before
doing any network I/O, and never holds a database lock across that I/O — the same shape as
`environment-follow` and `devloop`'s sync loop already use. A failed refresh leaves the previous
good archive in place; nothing is deleted until a new archive is confirmed written. "A bad seed
can be discarded with one action" (#288) is an owner-facing operation that marks the current
archive `discarded` and falls back to cold, without touching the refresh row's history.

## Credential scrubbing

The archive (or, later, a seed checkpoint) must never carry a credential. This project already
keeps every per-session credential — MCP, git-read, git-push — as a Kubernetes Secret reference
plus a hash, re-issued fresh per binding and never embedded in session or workspace state
(`credentials.md`: "Secrets are immutable... Mainloop needs only get, create and delete on
Secrets"; kagent's own `Session.Credentials` doc comment: "Secret references only; forks do not
inherit these bindings"). The warm-state archive follows the same rule by construction, not by a
scrubbing pass that could miss something: it is built from `cache_paths` and a checkout produced
by `setup_script`, never from a running session's environment or its credential Secrets, so there
is nothing in it to scrub unless a setup script writes a credential into the checkout or a cache
directory by mistake (a `.netrc`, a token baked into a lockfile mirror config). The refresh
reconciler should reject an archive containing recognizable credential shapes (clone tokens, the
`ml_`-prefixed Mainloop bearer format in `credentials.md`, common provider key prefixes) before
publishing it, as a defense against that mistake, the same spirit as Codespaces/Ona's
pre-publish scrubbing cited in #288 — but the primary control is that credentials are issued
fresh per binding at restore time and never flow into the archive path at all.

## Owner visibility and attention

The project page shows the toolchain build and seed state next to the existing environment
selection (`environments.md`'s "Implemented project UI"): default-branch commit the seed is warm
for, its age, its size, when it last refreshed, and the last failure's log if any. A failed
build or refresh, or a seed past `max_seed_age`, raises one persistent owner attention item with
a one-line ask (ROADMAP.md: "Repeated unchanged blockers produce one persistent attention item
rather than repeated notifications"; `inbox.md`) — not a task failure, and not a repeated
notification for the same stale state. A task always has a cold-start fallback: creating a
workspace with no usable warm state (none exists yet, the seed is discarded, or it is past its
age limit) works exactly as workspace creation does today, just without the warm start.

## Single registry allowlist

Issue #288 names a real, already-observed failure mode: Mainloop's `ENVIRONMENT_REGISTRY_ALLOWLIST`
(`server/internal/config/config.go:34`, `environments.md`), kagent's `developmentEnvironmentRegistries`,
and zot's own access rules are three independently edited lists that drifted and broke every
zot-image session with `PermissionDenied` (`oldsj/infrastructure#217`). Both kagent's control-auth
config and zot's access rules live outside this repository (`oldsj/kagent`, `oldsj/infrastructure`),
so a fix is cross-repository and out of this spec's implementation scope; this spec records the
requirement and the two options, for the owner to choose when an implementation slice touches it:
either Mainloop becomes the one source for the allowlist and pushes it to kagent's config (and zot's)
on change, or each side keeps its own list and a startup check in `serve-api` reads kagent's
configured list (if kagent exposes it) and fails loudly on a mismatch instead of letting it surface
only as a session failure. No preference is recorded here; it needs an owner decision informed by
how kagent and zot are deployed, which this task's workspace cannot inspect (no cross-repository
access, no cluster access beyond `mainloop-dev`'s four pods).

## Agent instructions

`server/internal/agentcontext/agentcontext.go`'s `Guidance` (currently `Version = 3`) tells every
agent that caches live under `/opt/cache` and to use them as set, with no mention of what is warm
or as of which commit — the gap #288 names directly. A future slice adds a line stating, when a
warm seed is in effect for the session's project: what commit the checkout and caches are warm for,
and that a checkout or cache miss means normal cold-build behavior, not a failure. This is
guidance text, not a new mechanism, and bumps `Version` per that file's own convention when it
lands. It is not implemented in this change; the config and refresh pieces above need to exist
first so there is a seed state to describe.

## Relation to #100

[#100](https://github.com/oldsj/mainloop/issues/100) proposes a personal dev-environment layer on
top of a project's environment. It overlaps this spec's toolchain config directly: both are
Mainloop-held, project-scoped (or project-plus-person-scoped), non-repository configuration that
feeds workspace creation. Rather than a second config path, #100's personal layer should be
modeled as an optional override merged onto this spec's project-level `setup_script`/`environment`/
`cache_paths` fields for one person's sessions, resolved at workspace creation the same way
environment selection already resolves once per workspace (`environments.md`, "Workspace
environment resolution"). This spec does not implement that merge; it is named here so the two
issues are not built as competing mechanisms.

## Not measured, not implemented

Everything in this document is proposed design, not running code. In particular, not measured by
this task, and not available to measure from this task's workspace, cluster access, or tool
surface (see "Measurement" above for why each is out of reach here): fork time, fork cross-task/
branch behavior and credential carryover, cold-workspace-on-toolchain-image-only timing, and
whether a newer `oldsj/kagent` commit than the vendored `ce402bf7` adds a fork RPC at all.
