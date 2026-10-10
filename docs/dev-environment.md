# Mainloop development image

The optional `.devcontainer/devcontainer.json` builds a source-free Linux development
image from the repository root. Its build context is confined to the repository;
`Dockerfile.dockerignore` exports only the definition, Trunk configuration and the
dependency manifests and lockfiles; no application source enters the image.
No hooks, Features, mounts, credentials, or native agent runtime are included.

```bash
docker build -f .devcontainer/Dockerfile -t mainloop-dev .
```

The image declares `USER 65532:65532` and also supports root actors. It includes
Python 3.13.7, Node 24.8.0, pnpm 9.15.9, uv 0.8.22, PostgreSQL 16.15,
kubectl 1.34.1, and Trunk 1.25.0. The base is pinned to an OCI index digest;
downloaded tools are SHA256 checked. PostgreSQL packages have exact version
pins and use the signed PGDG repository. Debian supporting packages use signed
Debian repositories. Trunk installs the repository's exact enabled tools and
separate Go/Node/Python lint runtimes during the image build. The tool receipt
is `/opt/mainloop-lint/tool-checksums.txt`; its shared cache is under
`/home/nonroot/.cache`, readable and writable by 65532 and root.
Changing `.trunk/trunk.yaml` requires rebuilding the image to update this closure.

Copy or clone a checkout into `/workspace` and run all offline checks:

```bash
dev-postgres run make check
```

Make targets for backend tests and frontend checks, tests, builds and development
servers install their locked dependencies first. A multi-target Make invocation
installs frontend dependencies once; warm installs reuse the package caches.
Trunk supplies its own lint runtimes, so formatting and lint need no project venv.
The installers still validate the environment on each invocation: a lockfile-only
stamp would miss deleted packages, changed workspace manifests and lifecycle hooks.
`uv sync` and `pnpm install` reuse the image's prewarmed caches (below);
`make check` invokes each installer once. It does not cache successful checks or
skip checks on a warm tree.
Standalone frontend targets have a separate 120-second install phase before
the check, test or build cap. The aggregate cap is 515 seconds, including
cold installs. It fits inside `dev-postgres`'s command budget of 540 seconds
minus setup, leaving room for `make check`'s own cleanup (at most seven seconds
there). With the current image, `dev-postgres run make test-backend` still has a
550-second cap above that budget; an image that exports deadlines clamps it.

## Prewarmed dependency caches

The image build fills a uv cache and a pnpm store from `backend/uv.lock` and
`pnpm-lock.yaml`, so `make install-backend` and `make install-frontend` copy
packages locally instead of downloading and extracting them. A separate build
stage does this with a stub `models` package and without installing either
project; only the caches reach the final image. The image sets:

| Variable                           | Value                            |
| ---------------------------------- | -------------------------------- |
| `UV_CACHE_DIR`                     | `/opt/mainloop-cache/uv`         |
| `npm_config_store_dir`             | `/opt/mainloop-cache/pnpm-store` |
| `UV_LINK_MODE`                     | `copy`                           |
| `npm_config_package_import_method` | `copy`                           |

The caches sit outside `$HOME` and `/data` because workspace runtimes move `HOME`
and `XDG_CACHE_HOME` onto the durable `/data` volume, which would hide them.
Installs copy rather than hardlink: linking a file from a read-only image layer
forces an overlay copy-up per file, and measured 60–80 s for the backend against
about 1 s for a copy. The directory is writable by 65532 and root, so a lockfile
newer than the image still installs: uv and pnpm download only the missing
packages into the container's copy of the cache. Offline, a stale lockfile fails
as it would without a cache. A workspace whose `node_modules` came from a
different pnpm store is purged and reinstalled once, without a prompt.

Lockfile and manifest changes rebuild the image through the workflow below.
Until the new image is selected, workspaces download only the difference.

`make lint` requires a fetched `origin/main`. Dependency sync needs the approved
package registries. Offline, such as in an agent workspace, Trivy uses its built-in
checks and Trunk skips update checks. CI fetches the latest checks, so results can
differ slightly.
Guarded browser, live-agent, and cluster suites are separate opt-in checks.

## Foreground deadlines

Run `dev-postgres run make check` for locked dependency installation and the complete offline
check set. Formatting completes first, so checks cannot read source while it is
being rewritten. Then two lanes run concurrently:

- Backend installation and tests, with the configured backend worker count unchanged.
- Supervisor regressions, one frontend install, frontend diagnostics, unit tests,
  application build and one complete changed-file Trunk lint pass.

Frontend diagnostics and build run sequentially because they both generate
`.svelte-kit` files. The build also validates bundling and adapter output beyond
type diagnostics. Svelte-check retains all configured source, test and generated
route-type coverage; generated build output is already outside its tsconfig's
include patterns. Its one-shot CLI does not offer a persistent incremental cache.
The lint pass does not apply fixes while tests are reading source. Use `make fmt`
to apply lint fixes as well as formatting before checking again.

Every step has a deadline and a `[step]` output prefix. Failures are recorded while
independent checks continue; a failed frontend install blocks its three consumers.
The final table lists step seconds, status and total wall time, including cleanup.
Logs and machine-readable `timings.json` go to `.cache/check/`; set
`MAINLOOP_CHECK_LOG_DIR` to retain a run elsewhere. Each step log retains up to
8 MiB plus a truncation marker, while console output continues. Repeated runs
overwrite these files. `make check-non-backend` uses the same runner and caps for
measuring the non-backend lane while backend work is in progress.

The aggregate deadline still leaves room under the native harness's ten-minute
foreground limit. Each step uses `scripts/with-timeout.mjs`; cancellation forwards
INT/QUIT/TERM to active supervisors and waits for their process-group cleanup
before printing the table. When the aggregate deadline sends TERM, the inner table
records cancellation and the outer cap reports timeout 124. CI splits these checks
into bounded jobs.

| Command or phase                                             | Cap                       | Coverage                                                                        |
| ------------------------------------------------------------ | ------------------------- | ------------------------------------------------------------------------------- |
| `make check`                                                 | 515 s + cleanup           | Locked installs and complete offline check set; at most 545 s including cleanup |
| `make check-non-backend`                                     | 515 s + cleanup           | Same formatting/frontend/lint lane, without backend installation or tests       |
| `make install`                                               | 240 s total               | Frontend, backend and shared models                                             |
| `make install-frontend`, `install-backend`, `install-models` | 120 s each                | Includes lifecycle hooks, Python selection and package builds                   |
| pnpm/uv network reads                                        | 30 s, one retry           | pnpm retry delay 1–5 s; uv exports `UV_HTTP_TIMEOUT=30`, `UV_HTTP_RETRIES=1`    |
| `make test-backend`                                          | 550 s                     | Includes install; runner: 60 s/test or fixture, 180 s imports, 540 s/suite      |
| `make test-timeouts`                                         | 90 s                      | Command supervisors, parallel lanes, log bounds and cancellation cleanup        |
| `make test-timeout-integration`                              | 180 s                     | Opt-in real Make/backend and development-image PG16 cleanup regressions         |
| `make check-frontend`, root/frontend `pnpm check`            | 120 s                     | Sync and Svelte diagnostics                                                     |
| `make test-frontend`, frontend `pnpm test:unit`              | 60 s                      | Node unit tests                                                                 |
| `make frontend-build`, root/frontend `pnpm build`            | 240 s                     | Vite application build; shares CPUs with the backend lane in `make check`       |
| `make lint`, root/frontend `pnpm lint`                       | 120 s                     | Changed-file Trunk or package linters                                           |
| `make fmt`, root `pnpm format`                               | 180 s total               | Both format and follow-up check                                                 |
| `make lint-all`, `make fmt-all`                              | 240 s / 300 s             | Whole-repository Trunk                                                          |
| `make build-backend`, `make build-frontend`                  | 480 s each                | Local Docker image builds                                                       |
| `make build-all`, `make build-all-parallel`                  | 550 s / 480 s total       | Serial or parallel image builds                                                 |
| `dev-postgres run` / supplied command                        | 570 s / 540 s minus setup | Reserves 30 s for teardown; at most 595 s including cancellation cleanup        |
| PostgreSQL init / start / readiness / stop                   | 30 s / 35 s / 5 s / 15 s  | `pg_ctl` also has 30 s start and 10 s stop waits                                |
| Scripted Docker Git fetch / tool download                    | 120 s / about 95 s        | Git low-speed cutoff 30 s; curl connect 10 s, transfer 45 s, one retry          |
| Dev-image Trunk installation                                 | 300 s                     | Includes lint tool/runtime downloads                                            |
| Dev-image uv cache / pnpm store prewarm                      | 240 s each                | Downloads every locked package once per image build                             |
| CI backend / frontend checks / lint                          | 10 min / 5 min / 5 min    | Recent successful jobs: 269–505 s / 11–14 s / 18 s                              |
| CI app build and publish jobs                                | 8 min                     | Recent successful builds 12–126 s; publish 19–80 s                              |
| CI dev-image build or publish / index                        | 10 min / 3 min            | Recent native builds 162–249 s; index 12 s                                      |

`scripts/with-timeout.mjs` requires the installed Node runtime, starts an isolated
process group, announces its deadline and elapsed time, and returns 124 with
"timed out after N s" on expiry. Deadlines bound command execution; cleanup may
then take up to 30 seconds, including after ordinary exits that leave descendants.
Each cap exports its absolute deadline as `MAINLOOP_TIMEOUT_DEADLINE_MS`. A nested
cap, including the backend runner's suite cap, expires up to one second before its
parent, so the innermost cap reports the timeout; its cleanup runs inside the
parent's longer grace. The startup line shows when a cap was clamped. A table's cap is therefore
a maximum: inside `make check`, the backend step, `make test-backend` and the
runner end before the aggregate deadline.
It forwards INT/QUIT/TERM unchanged, uses TERM for expiry or ordinary completion,
then KILL when the cleanup budget expires. Nested owners inherit the remaining
cleanup budget, reserving up to one second before their parent's escalation;
the backend supervisor observes this contract while keeping its own five-second
maximum. `--grace SECONDS` can assign a smaller cleanup window. Normal exit codes pass through. Make itself
returns 2 for a failed recipe and prints the helper's 124. PostgreSQL startup
explicitly retains its server on success; `dev-postgres` owns its shutdown.
If shutdown cannot confirm the server has stopped, it retains PGDATA and reports
the path instead of deleting live data. PostgreSQL admits work only with a
25-second cancellation budget: eight seconds for active-phase cleanup, 15 seconds
for stop plus one second of escalation, and one second for removal. Its supplied
command's deadline includes setup and can be tighter than a standalone Make cap.

For the explicit real-process regressions, set `MAINLOOP_TIMEOUT_TEST_IMAGE` to
an existing local development image and run `make test-timeout-integration`.
The regression never pulls an image; it runs isolated, network-disabled PG16
containers, injects a seven-second stop delay, and removes its containers.

Use the install targets rather than bare `uv sync` or `pnpm install` in agent
turns: request timeouts alone do not bound many downloads or install hooks.
The request settings also apply in CI and Docker dependency layers. Interactive
servers, log followers and opt-in live/cluster suites are outside this offline
check set. CI job caps include action setup and image pulls; Docker Git fetch
uses coreutils' process-group timeout because its Go stage has no Node runtime.

Earlier amd64 image qualification ran uncapped backend discovery and the
frontend/lint checks as both 65532 and root, with networking disabled after
dependency sync. That historical backend run covered 615 tests without skips.
Frontend unit tests passed 34 tests;
the two existing integration-fixture tests skipped without their optional inputs.
Local arm64 verification awaits an available arm64 builder or QEMU registration.

`dev-postgres run COMMAND` creates scratch PGDATA at `/data/test-postgres`, with
socket and temporary directories at `/data/test-postgres-socket` and
`/data/test-postgres-tmp`. Override `DEV_POSTGRES_ROOT` or `DEV_POSTGRES_PORT`
(default 5432) for an isolated test run. It refuses existing directories, waits
for readiness, prints and exports `MAINLOOP_TEST_DATABASE_URL`, and stops and
removes its scratch directories on command completion, failure, or termination.
The scratch superuser uses trust authentication on loopback and the private
socket only. Root launches PostgreSQL as uid 65532; the supplied command retains
the caller's user. This helper is for disposable test environments.
Stop PostgreSQL before parking a workspace; Full-restore support is unproven.

Scratch PostgreSQL skips `initdb`'s sync and disables `fsync`,
`synchronous_commit`, and `full_page_writes`.
These settings remove disk durability costs from SQL integration tests; never use
this helper for persistent data. CI uses the same settings and a 512 MiB tmpfs for
PGDATA, with `max_wal_size=128MB` to keep WAL inside that memory budget.
In a workspace with sufficient shared memory, `DEV_POSTGRES_ROOT` may
point to a dedicated directory under `/dev/shm`; the default remains `/data`.
Keep build/package caches and `TMPDIR` at their usual locations, outside `/tmp`.

`make test-backend` runs the same offline unittest discovery in CI and workspaces.
It requires `MAINLOOP_TEST_DATABASE_URL` and already-synced backend dependencies.
`MAINLOOP_TEST_WORKERS` overrides the default `min(os.cpu_count(), 6)` processes,
further limited to one per 512 MiB of available memory; set it to `1` for serial execution.
`make check` sets `MAINLOOP_TEST_RESERVE_CPUS=1`, so the backend uses one CPU fewer
(at least one worker) and the frontend lane keeps a CPU.
Workers are separate interpreters started with fork and exec, not forked copies
of the supervisor. The run summary starts with the run's status (`complete`,
`failed`, `timed_out` or `cancelled`) and reports tests run out of those planned.
After it, each worker that died from a signal or never reported a result gets a
line naming the signal or exit code, the last deadline phase it entered, and its
modules; ordinary test failures appear in the merged failure summary instead.
Every test, including setup, teardown, cleanups and async runner shutdown, has a
fatal 60-second deadline; module/class fixtures also have 60-second
deadlines, including suites returned by `load_tests` hooks. Discovery and imports
have 180 seconds: parallel workers import beside other checks on a shared host. On a timeout,
the runner prints the active test/fixture and all thread stacks. The whole run has
a 540-second cap (earlier under an enclosing deadline) with up to five seconds for stack dumping and process cleanup,
leaving headroom under the harness's ten-minute foreground limit. The Make wrapper additionally
bounds uv startup. CI's job cap is ten minutes.
The supervisor kills every worker process group on timeout or cancellation, including
test subprocesses, then drops this run's class/template databases within the same
cleanup budget; `dev-postgres` also removes its disposable cluster. Success requires
every planned ID to execute exactly once, including skips and expected failures.
Each worker migrates one empty template in its own database namespace. Schema and
fixture-isolation tests retain independent class clones. Explicitly qualified
data-only classes reuse a worker database after all rows and sequences are reset;
subclasses must opt in themselves. Task fixture cleanup deletes the same foreign-key
table closure as TRUNCATE CASCADE without rewriting relation/index files. It uses
replication mode only inside its own cleanup transaction in an owned scratch database;
normal security and foreign-key triggers are restored before test assertions run,
including when cleanup fails. Migration tests still execute the real migrations.
The fixture role needs permission to set `session_replication_role`; the disposable
server from `dev-postgres` provides this.
Git transport fixtures copy an immutable worker seed into independent writable
repositories; actual Git clients, upload/receive-pack and validation remain real.
Scratch PostgreSQL allows 100 connections, matching CI's default, to accommodate
six worker pools and their fixture/admin connections.

The runner reports the slowest 15 modules and tests. Module totals include class
and module fixtures; test totals include setup/teardown and async runner shutdown.
Save machine-readable
evidence with `MAINLOOP_TEST_TIMINGS=/path/to/timings.json`; completed test and fixture
records are saved incrementally and survive timeouts. Existing module timings at
that path balance the next run, longest module first. Modules without a recording
use the committed weights in `backend/tests/module_weights.json`, so a fresh
checkout is balanced too. `make check` saves timings to
`.cache/check/backend-timings.json` (or under `MAINLOOP_CHECK_LOG_DIR`). Refresh the
committed weights from a complete run with
`jq -S '.modules | map_values(. * 100 | round / 100 | if . < 0.01 then 0.01 else . end)' TIMINGS > backend/tests/module_weights.json`.
The runner verifies every worker's inventory against serial discovery. A focused run uses
`make test-backend TEST_ARGS='tests.runtime.test_merge_acceptance'`.
Successful HTTP request logs are suppressed; warning/error logs and all test
assertions remain enabled.
Asyncio debug checks and slow-callback warnings remain enabled. Standalone workers
capture at most five callback/future/task creation frames without consulting
linecache; source lines load only when a diagnostic renders them. Asyncio removes
some internal frames, so the retained creation stack can be shorter. Ordinary
stack extraction, exception and timeout tracebacks remain complete. Set
`MAINLOOP_TEST_DEBUG_STACK_DEPTH=10` when diagnosing resource creation sites.
Python 3.13's accelerated Future/Task caches the stack helper at import time;
the standalone worker installs it before importing asyncio. Importing the runner
into a process that already loaded asyncio only updates Handle capture.

PostgreSQL classes explicitly marked `reuse_database = True` or `reuse_pool = True`
keep one debug loop and pool per class. Each test still has its own context and
clears all fixture rows and sequences before setup; leftover tasks are cancelled
and asynchronous generators closed after cleanup, including failed setup.
These flags are not inherited. `reuse_pool` retains a separate class database;
`reuse_database` also allows a shared worker database. Qualified classes must keep
the table/sequence catalog stable. Schema-changing classes otherwise retain fresh
loops and per-test pools. Cached resets reject existing transactions, check the
exact owned database, and use a single implicit transaction; FK/security triggers
are restored on failure.

Changes to `dev-postgres` require publishing a new development image through the
workflow below, then pinning/selecting its immutable digest for workspaces.
Editing the checkout alone does not update the installed helper in existing images.

The **Mainloop dev image** workflow validates pull requests with native amd64
(`ubuntu-latest`) and arm64 (`ubuntu-24.04-arm`) builds, without registry login
or publishing. Each architecture uses its own GitHub Actions cache. Only pushes
to `main` publish `ghcr.io/oldsj/mainloop-dev`: the native jobs push by digest,
then a merge job publishes the combined multi-arch index with `<full SHA>`,
`sha-<full SHA>`, and `latest` tags after both builds succeed. The merge job output
and summary record the index digest for immutable registration. New runs cancel
older runs for the same ref; a cancelled run may leave untagged architecture
digests, so use a fully successful run. No image is published by local builds.
Once environment registration is available, register the published immutable
`ghcr.io/oldsj/mainloop-dev@sha256:…` digest, or register this definition at an
exact source commit, then select the accepted version for the project. This
document does not claim that registration or runtime composition is implemented.

## Live dev instance

`mainloop-dev` is a shared, prototype instance for fast iteration. It runs in
namespace `mainloop-dev` on the production cluster, next to production
`mainloop`, but DevSpace deploys and owns it directly; it is not Argo-managed.
Production still changes only through pull requests, CI and GitOps.

```bash
make dev-live          # deploy mainloop-dev, then sync and stream logs (Ctrl-C stops sync)
make dev-live-status   # pods, database, route and dev agents
make dev-agents        # apply the *-dev kagent agents (shared kagent namespace)
make dev-harness       # build arm64 harness images from ../kagent, apply them to the dev agents
make dev-live-purge    # delete namespace mainloop-dev and the *-dev agents (typed confirmation)
```

All targets use kube context `admin@internal-01` through DevSpace profile `live` in
`devspace.yaml`, whose pipelines (`build`, `dev`, `deploy`, `deploy-agents`, `purge`)
refuse any other context or namespace. Use these targets or `scripts/dev-live.sh`:
raw `devspace sync`, `restart`, `enter`, `logs` and `attach` with `-p live` are not
guarded and act on whatever `--kube-context` and `-n` say. `make dev` and the Kind
setup are unchanged.

What syncs:

- `backend/src` and `models` sync into the backend and MCP containers, and
  uvicorn reloads. `backend/pyproject.toml` and `backend/uv.lock` run `uv sync`
  in the pod.
- `frontend/src`, `packages` and `frontend/vite.config.ts` sync into the Vite dev
  server, which hot-reloads. `package.json` and `pnpm-lock.yaml` run
  `pnpm install` in the pod.

The UI is `https://mainloop-dev.olds.network` (tailnet only, through the same
Envoy gateway as production). The browser calls `/api` on that host, and Vite
forwards it to the backend Service, so the backend needs no route of its own.

Images: the **Publish live dev images** workflow builds the `--target=dev`
backend and frontend images for arm64 on each push to `main` and pushes
`ghcr.io/oldsj/mainloop-{backend,frontend}-dev` with `<short SHA>` and `latest`
tags. Nothing is built locally on each run; the pods pull `latest` and the sync
overlays the working tree. The packages are public, so no pull secret is needed.

Data: a one-instance CloudNativePG cluster on `local-path` storage. CNPG creates
the database credentials. Purging deletes the data.

Settings (`k8s/apps/mainloop/overlays/dev-live`): `DEV_MODE=true`; merge tools,
owner writes, Git transport and the push gate are off; every kagent role uses
`claude-workspace-dev` or `codex-workspace-dev`. Dev work targets `oldsj/testrepo` only
through the dev agents' instructions: Mainloop has no repository allowlist, and the
shared production GitHub App can reach every repository it is installed on. With Git
transport off, nothing reaches GitHub today.

Secrets are not in this repository. The infrastructure repository supplies them from
production's existing items (mainloop-dev shares production's GitHub App, agent token
key and kagent control principal). Each reference is optional, so the instance starts
without them, and the features that need them stay unavailable:

| Secret in `mainloop-dev` | Keys                                                         | Without it                                                                  |
| ------------------------ | ------------------------------------------------------------ | --------------------------------------------------------------------------- |
| `mainloop-secrets`       | `github-app-id`, `github-app-private-key`, `agent-token-key` | No GitHub access; the agent token key falls back to the database password   |
| `kagent-control-token`   | `current`                                                    | No kagent sessions; also enable the `control-auth` component in the overlay |
| `kagent-control-ca`      | `ca.crt` (cert-manager)                                      | As above                                                                    |

Agent sessions also need code changes before they can work here. The MCP host
allowlist, the agent credential MCP origin and the Git proxy origins are fixed to
the production namespace (`mainloop-*.mainloop.svc.cluster.local`), and per-binding
credential Secrets share production's `mainloop-mcp-*` names in the `kagent`
namespace. Until those are parameterised per namespace, the dev agents can be
created and their harness images swapped, but they cannot call Mainloop tools.

Teardown: `make dev-live-purge` deletes everything DevSpace deployed, including
the namespace, the database and the dev agents.
