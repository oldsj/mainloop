# Durable project warm state

Status: proposed. The toolchain-config and cache-archive design below is not implemented. It is
the measure-then-design output for [#288](https://github.com/oldsj/mainloop/issues/288): a
Mainloop-held toolchain config and a per-project warm seed so that the second session in a
repository is as fast as a warmed laptop checkout, for any language, with nothing committed to the
repository. See [ROADMAP.md](../../ROADMAP.md#durable-project-warm-state) for how this fits the
rest of the product direction, and [environments.md](environments.md) for the environment
registration and selection this extends.

One piece is implemented, experimental and owner-only, ahead of the rest: a checkpoint and fork API
directly over kagent's `CheckpointService`, built so the owner can run the live fork-vs-cache-image
measurement this spec's choice of mechanism depends on. See "Checkpoint and fork API (implemented,
experimental)" below; it is not the chosen warm-state mechanism, only how that choice gets measured.

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

**Update, 2026-10-20, a later task (issue #288, "warm-state comparison"):** `ForkSession` is now
wired into `server/internal/kagent` (`CreateCheckpoint`, `GetCheckpoint`, `ListCheckpoints`,
`DeleteCheckpoint`, `ForkSession`, in `server/internal/kagent/checkpoints.go`) and exposed as an
**experimental, owner-only** HTTP API — see "Checkpoint and fork API (implemented, experimental)"
below. That closes the gap this section reported ("no tool on this surface... reaches
`SessionService` at all"): the API now exists and this task's own `kagenttest` fakes prove the
client-side contract (idempotent create and fork, a reconciled retry after an uncertain kagent
reply, and kagent's error codes mapped to this API's HTTP statuses). That is a **fixture-backed
proof of the client code**, not a live measurement: nothing in this task's workspace can start a
real kagent session, take a real checkpoint, or fork one, so fork time, cross-task/branch forking
behavior, and credential carryover remain exactly as unknown as the table above says. The owner
runs the live measurement on `mainloop-dev` and records it in "Measured: fork from warmed
checkpoint" below; until that section is filled in, this spec still treats fork as unproven for
the purpose of choosing it over the cache archive.

## Checkpoint and fork API (implemented, experimental)

Issue #288's "warm-state comparison" asked for the owner to measure a fork from a warmed checkpoint
against the cache-image environment before either mechanism is chosen for real. That needs an API
the owner can call from `mainloop-dev`, so this slice implements one, ahead of the rest of this
spec's design (the toolchain config, the reconciled refresh intent, credential scrubbing, and the
registry allowlist below remain proposed, not implemented): a thin, explicitly experimental and
owner-only layer directly over kagent's `CheckpointService`, with no web UI and no seed-fork
automation. It exists to produce the numbers below, not to be the warm-state mechanism itself; see
"Mechanism" below for why the cache archive, not seed fork, is still the chosen path until a
measurement says otherwise.

- `POST /sessions/{session_id}/checkpoints` takes a checkpoint of the session's native kagent
  Session at its latest terminal turn. `expected_head_task_id` (kagent's term) is the session's own
  terminal task, read from Mainloop's delivery ledger, not supplied by the caller; the route refuses
  (409) while a turn is in flight, or before the session has ever completed one. `request_id` is the
  idempotency key.
- `GET /checkpoints/{checkpoint_id}` and `GET /sessions/{session_id}/checkpoints` read live from
  kagent (no local mirror of checkpoint state), scoped to the owner's own session.
- `POST /checkpoints/{checkpoint_id}/fork` forks a checkpoint into a new Mainloop session bound to
  the forked kagent Session, with the same project, provider profile and Agent as the checkpoint's
  source session, and `parent_session_id` set to it. The response carries `requested_at`,
  `accepted_at` and `ready_at` (each set as the fork reaches that point; repeating the idempotent
  request_id reads them as they fill in), so time-to-running is `ready_at` minus `requested_at` once
  both are set.
- `DELETE /checkpoints/{checkpoint_id}` deletes one; already-gone is not an error. Checkpoint
  snapshot retention itself is tracked separately, in
  [#302](https://github.com/oldsj/mainloop/issues/302).

Follows the reconciler rule (AGENTS.md): both create-checkpoint and fork write a durable intent row
(`session_checkpoints`, `checkpoint_forks`) keyed on the caller's `request_id` before calling kagent.
kagent's `CreateCheckpoint` and `ForkSession` are themselves idempotent by `request_id`, the same
contract `CreateSession` already has, so reconciling an uncertain outcome is a repeat of the same
call, not a blind replay of a non-idempotent send: kagent answers from what it already holds for
that `request_id`. A definite refusal (kagent's invalid argument, not found, failed precondition,
mapped to this API's 422, 404, 409) is recorded once and never retried, since the frozen request
would fail the same way again. See `server/internal/sessions/checkpoints.go`.

## Mechanism: cache archive now, seed fork later if it measures well

Issue #288 offers two mechanisms: a seed fork from a kagent Data checkpoint, or its fallback, a
per-project cache archive in object storage or the in-cluster registry (zot), restored at
workspace start. This spec chooses the **cache archive** as the first implemented mechanism.
The fork RPC itself is reachable today — `CheckpointService.ForkSession` exists at the deployed
kagent controller `a116401`, and Mainloop's API calls it (see "Checkpoint and fork API" above) —
but the fork _operation_ remains unproven: the live measurement (see "Measured: fork from warmed
checkpoint" below) found production's harnesses take `Full` checkpoints, which kagent refuses to
fork (409), and the one harness configured for the `Data` checkpoints kagent does fork runs only
on `mainloop-dev`, which cannot provision a workspace on the cache environment at all
(`git_transport_required`). So no fork time, cross-task/branch behavior, or credential carryover
number exists yet. Building the warm-state design around an operation with no proven timing or
safety numbers would violate the project's own evidence rule (AGENTS.md: "never present a
fixture, mocked integration, or agent claim as live proof").

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

## Measured: fork from warmed checkpoint

The fork RPC is no longer unreachable: `CheckpointService.ForkSession` exists at the deployed
kagent controller `a116401`, and Mainloop's API calls it (see "Checkpoint and fork API" above).
The owner ran the live measurement issue #288 asked for, against `mainloop-dev`, but
`mainloop-dev` refused to provision a workspace at all: it requires `git_transport_required`
(`GIT_TRANSPORT_ENABLED` and `PUSH_GATE_ENABLED`), and dev has both off. The measurement below
therefore ran on **production** (`mainloop-api.olds.network`) instead, not `mainloop-dev` as
#288 asked.

**Measured 2026-10-21, production**, `oldsj/mainloop` project, cache environment
(`36b25532`, version `2648b9fe`, image
`registry.olds.network/local/mainloop-dev@sha256:889a94bb`, `linux/arm64`), `claude-workspace`
agent, kagent controller `a116401`:

- Time to running: task created 17:48:11.7Z, session active by 17:48:19 (10s poll) — **≤ 7.6 s**.
- Checkout prepared at `/data/workspace` (no clone), HEAD `c58164b`. `/opt/cache` 2.3 GB;
  `GOCACHE=/opt/cache/go-build`, `GOMODCACHE=/opt/cache/go-mod`, pnpm store
  `/opt/cache/pnpm-store/v3`.
- `go build ./...` in `server/`: first build **28.97 s** real (user 85 s), rebuild **14.95 s**.
- Identity/credentials in the workspace: git `user.name`/`user.email` set (Mainloop agent /
  noreply); no git credential helper; no `gh` binary; `ANTHROPIC_API_KEY`,
  `CLAUDE_CODE_OAUTH_TOKEN`, `GITHUB_TOKEN`, `GH_TOKEN` all unset; Mainloop MCP tools reachable.
- Checkpoint: accepted at 19:16:30Z and READY immediately, but only 15–30 minutes after the
  session's last turn. kagent requires the Session READY _and_ its Actor suspended on the
  snapshot recorded for the head task. Production sets `KAGENT_QUIESCE_DELAY=15m`, so kagent's
  lifecycle worker suspends the Actor and records that snapshot 15 minutes after a turn;
  Mainloop's own workspace idle suspend (`idle_timeout_minutes` default 30) then suspends the
  Session, and kagent refuses a SUSPENDED session (409 conflict). Resuming the workspace moves
  the Actor off the recorded snapshot, and the checkpoint tag then fails ("snapshot changed").
- Fork: **NOT MEASURED, refused.** `POST /checkpoints/{id}/fork` → 409 "Checkpoint includes
  process state and cannot be forked". Production's `claude-workspace-harness` and
  `codex-workspace-harness` use `snapshotPolicy: onQuiesce: Full`; kagent forks only Data
  checkpoints. Only `claude-workspace-dev-harness` (`mainloop-dev`) uses Data, and
  `mainloop-dev` cannot provision cache-env workspaces (see `git_transport_required` above). So
  no fork time, first-build-after-fork, or carryover numbers exist yet.

**Measured 2026-10-22, mainloop-dev**: the owner enabled dev's Git transport and push gate
(`GIT_TRANSPORT_ENABLED` and `PUSH_GATE_ENABLED` in `mainloop-dev`), removing the
`git_transport_required` refusal the production block above hit. The dev project used the same
cache image as the production run above
(`registry.olds.network/local/mainloop-dev@sha256:889a94bb`), selected as env `48157344`,
version `c97e3803`.

- Measured, session start: task created 02:52:21.1Z; the session was creating at 02:52:53 and
  active by 02:53:14 (10 s poll) — **≈ 52 s** to an active session, against ≤ 7.6 s on
  production. This is time to an active session, not to a usable workspace, because preparation
  then failed (next bullet). The gap is not explained yet.
- Not measured, and why: no dev workspace could be prepared. Two kagent single-instance
  assumptions blocked the second Mainloop instance:
  - Git proxy origins were hard-coded to production's instance. Fixed in `oldsj/kagent#46`.
  - Preparation required an MCP server keyed literally `mainloop`, but dev's is `mainloop-dev`.
    It surfaced as "Current prepared runtime identity is unavailable" and was diagnosed through
    #47's logging. Fixed in `oldsj/kagent#48` (`b785c99`), which binds the MCP server to the
    session's own instance by URL origin. Production runs `b785c99`
    (`oldsj/infrastructure#227`) and was verified with a task.
- Measured, source session (probe task `8e41c4f1`): task created 11:43:28.2Z, session active
  11:44:19.5Z (≈ 51 s, matching the earlier ≈ 52 s). The workspace prepared at `/data/workspace`
  on HEAD `3facbea`, with caches under `/opt/cache` (2.3 GB). `go build ./...` in `server/`: first
  build 26.9 s, rebuild 11.5 s. The turn ended 11:47:35Z.
- Measured, checkpoint: requested 12:14:57Z, about 27 min after the turn, inside the 15–30 min
  window described above. It returned `ready` in the same call. Dev's workspace harness snapshots
  `Data` on quiesce, so the fork was allowed (production's `Full` snapshots were refused, as
  above).
- Measured, fork: `POST /checkpoints/{id}/fork` with `fresh_conversation: false` took 0.8 s
  (12:15:03.4 to 12:15:04.2Z) and returned a new session, already `active`, with
  `parent_session_id` set. A message sent at 12:15:21Z ran its first command at 12:15:46Z (≈ 25 s,
  including model latency), and the turn ended at 12:16:39Z.
- Measured, carryover into the fork:
  - The conversation carried over: it quoted the source's build times from memory.
  - The workspace carried over: the marker file written before the checkpoint was present with
    its original contents, on the same HEAD and branch, and the file was untracked as expected.
  - Git identity carried over: same `user.name` and `user.email`.
  - Credentials: as in the source, no credential helper and no token environment variables, so
    nothing carried over or leaked.
- Measured, first build in the fork: 24.2 s (rebuild 10.7 s). That is close to the source's first
  build (26.9 s), not to its rebuild (11.5 s).
- Inferred, not verified: build-cache entries the source wrote under `/opt/cache`, which is image
  content outside `/data`, did not survive the `Data` snapshot. A fork therefore gives no
  build-time advantage over a fresh session on the same cache image.
- Open, #311: the fork has no Mainloop workspace row. Its MCP calls were rejected (401
  `AUTH_HEADER_REJECTED`, where the source's succeeded). It can't be deleted through Mainloop.
  `DELETE /checkpoints/{id}` answers 200 while kagent keeps the checkpoint, because the fork still
  pins it.
- Related, Open: #309. A session whose stuck turn is stopped can't take another turn (its
  workspace preparation stays stale). Any fork or checkpoint run that needs a stopped turn hits
  it.

**Recommendation** (owner decision, not a measured fact): keep the cache-image environment as
the warm-state mechanism for now. It already gives ≤ 8 s to running and a 29 s first build with
nothing to fork. Seed fork stays a later option and needs two owner decisions first before it can
be measured at all:

1. Data snapshots for workspace harnesses — either switching `claude-workspace-harness` and
   `codex-workspace-harness` from `Full` to `Data` (which drops process state across every
   suspend/resume, not just the measured one), or standing up a dedicated seed harness on `Data`.
2. A checkpoint window that does not depend on racing the 15/30-minute quiesce and idle-suspend
   timers — an explicit quiesce-then-checkpoint operation, or changing Mainloop's idle suspend so
   it does not suspend the kagent Session underneath an in-progress checkpoint.

The dev data so far adds no evidence for seed fork, and the ≈ 52 s dev session start is a reason
to measure dev's time to running separately once workspaces prepare. The dev fork data supports
this: a fork kept the workspace and conversation, but gave no faster first build than a fresh
cache-image session.

Two Mainloop bugs surfaced by this measurement, filed as
[#299](https://github.com/oldsj/mainloop/issues/299): the stored-409 replay for kagent's retryable
`KAGENT_CHECKPOINT_SNAPSHOT_PENDING` (treated as a terminal failure instead of retried), and
[#300](https://github.com/oldsj/mainloop/issues/300): a checkpoint row recorded `succeeded` while
kagent's own checkpoint state was `FAILED`.

## Not measured, not implemented

Everything in this document's cache-archive and toolchain-config design (from "Mechanism" onward)
is proposed, not running code; the checkpoint and fork API above is implemented but experimental
and owner-only, ahead of the rest. The fork RPC itself is now reachable and was exercised live
(see "Measured: fork from warmed checkpoint" above), but the fork operation it performs is still
not measured: fork time, fork cross-task/branch behavior, and credential carryover remain unknown,
because production's harnesses take `Full` checkpoints and kagent only forks `Data` ones, and the
one harness that does take `Data` checkpoints runs on `mainloop-dev`, which refuses to provision a
workspace on the cache environment (`git_transport_required`). Also not measured: cold-workspace-
on-toolchain-image-only timing, and whether a newer `oldsj/kagent` commit than the vendored
`a1164013` changes `ForkSession`'s behavior from what the `kagenttest` fakes assume.
