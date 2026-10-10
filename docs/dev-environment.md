# Mainloop development image

The optional `.devcontainer/devcontainer.json` builds a source-free Linux development
image from the repository root. Its build context is confined to the repository;
`Dockerfile.dockerignore` exports only the definition and Trunk configuration.
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

Copy or clone a checkout into `/workspace` and sync its locked dependencies:

```bash
make install-backend install-frontend
dev-postgres run make test-backend
pnpm check
make test-frontend
make lint
```

`make lint` requires a fetched `origin/main`. Dependency sync needs the approved
package registries. Offline, such as in an agent workspace, Trivy uses its built-in
checks and Trunk skips update checks. CI fetches the latest checks, so results can
differ slightly.
Guarded browser, live-agent, and cluster suites are separate opt-in checks.

## Foreground deadlines

Run `dev-postgres run make check` after dependency sync for the complete offline
check set: helper tests, backend tests, frontend diagnostics, unit tests and build,
then formatting and lint. Its aggregate deadline leaves room under the native
harness's ten-minute foreground limit. CI splits these checks into bounded jobs.

| Command or phase                                             | Cap                       | Coverage                                                                     |
| ------------------------------------------------------------ | ------------------------- | ---------------------------------------------------------------------------- |
| `make check`                                                 | 560 s + cleanup           | Complete offline check set; at most 590 s including cleanup                  |
| `make install`                                               | 240 s total               | Frontend, backend and shared models                                          |
| `make install-frontend`, `install-backend`, `install-models` | 120 s each                | Includes lifecycle hooks, Python selection and package builds                |
| pnpm/uv network reads                                        | 30 s, one retry           | pnpm retry delay 1–5 s; uv exports `UV_HTTP_TIMEOUT=30`, `UV_HTTP_RETRIES=1` |
| `make test-backend`                                          | 550 s                     | Runner: 60 s/test or fixture, 540 s/suite                                    |
| `make test-timeouts`                                         | 30 s                      | Shared command-supervisor regressions                                        |
| `make test-timeout-integration`                              | 180 s                     | Opt-in real Make/backend and development-image PG16 cleanup regressions      |
| `make check-frontend`, root/frontend `pnpm check`            | 120 s                     | Sync and Svelte diagnostics                                                  |
| `make test-frontend`, frontend `pnpm test:unit`              | 60 s                      | Node unit tests                                                              |
| `make frontend-build`, root/frontend `pnpm build`            | 120 s                     | Vite application build                                                       |
| `make lint`, root/frontend `pnpm lint`                       | 120 s                     | Changed-file Trunk or package linters                                        |
| `make fmt`, root `pnpm format`                               | 180 s total               | Both format and follow-up check                                              |
| `make lint-all`, `make fmt-all`                              | 240 s / 300 s             | Whole-repository Trunk                                                       |
| `make build-backend`, `make build-frontend`                  | 480 s each                | Local Docker image builds                                                    |
| `make build-all`, `make build-all-parallel`                  | 550 s / 480 s total       | Serial or parallel image builds                                              |
| `dev-postgres run` / supplied command                        | 570 s / 540 s minus setup | Reserves 30 s for teardown; at most 595 s including cancellation cleanup     |
| PostgreSQL init / start / readiness / stop                   | 30 s / 35 s / 5 s / 15 s  | `pg_ctl` also has 30 s start and 10 s stop waits                             |
| Scripted Docker Git fetch / tool download                    | 120 s / about 95 s        | Git low-speed cutoff 30 s; curl connect 10 s, transfer 45 s, one retry       |
| Dev-image Trunk installation                                 | 300 s                     | Includes lint tool/runtime downloads                                         |
| CI backend / frontend checks / lint                          | 10 min / 5 min / 5 min    | Recent successful jobs: 269–505 s / 11–14 s / 18 s                           |
| CI app build and publish jobs                                | 8 min                     | Recent successful builds 12–126 s; publish 19–80 s                           |
| CI dev-image build or publish / index                        | 10 min / 3 min            | Recent native builds 162–249 s; index 12 s                                   |

`scripts/with-timeout.mjs` requires the installed Node runtime, starts an isolated
process group, announces its deadline and elapsed time, and returns 124 with
"timed out after N s" on expiry. Deadlines bound command execution; cleanup may
then take up to 30 seconds, including after ordinary exits that leave descendants.
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
`MAINLOOP_TEST_WORKERS` overrides the default `min(os.cpu_count(), 6)` processes; set it to `1` for serial execution.
Every test, including setup, teardown, cleanups and async runner shutdown, has a
fatal 60-second deadline; module/class fixtures and discovery also have 60-second
deadlines, including suites returned by `load_tests` hooks. On a timeout,
the runner prints the active test/fixture and all thread stacks. The whole run has
a 540-second cap with up to five seconds for stack dumping and process cleanup,
leaving headroom under the harness's ten-minute foreground limit. The Make wrapper additionally
bounds uv startup. CI's job cap is ten minutes.
The supervisor kills every worker process group on timeout or cancellation, including
test subprocesses; `dev-postgres` then removes its disposable cluster.
Class databases clone one migrated, empty template per worker, with distinct worker
namespaces; migration tests still
execute the real migrations and each class retains its own independent database.
Scratch PostgreSQL allows 100 connections, matching CI's default, to accommodate
six worker pools and their fixture/admin connections.

The runner reports the slowest 15 modules and tests. Module totals include class
and module fixtures; test totals include setup/teardown and async runner shutdown.
Save machine-readable
evidence with `MAINLOOP_TEST_TIMINGS=/path/to/timings.json`; completed test and fixture
records are saved incrementally and survive timeouts. Existing module timings at
that path balance the next run; otherwise modules are assigned round-robin.
The runner verifies every worker's inventory against serial discovery. A focused run uses
`make test-backend TEST_ARGS='tests.runtime.test_merge_acceptance'`.
Successful HTTP request logs are suppressed; warning/error logs and all test
assertions remain enabled.
Asyncio debug checks remain enabled. The runner limits callback/future/task
creation stacks to one frame to reduce diagnostic overhead on SQL-heavy tests;
ordinary exception and timeout tracebacks remain complete. Set
`MAINLOOP_TEST_DEBUG_STACK_DEPTH=10` when diagnosing resource creation sites.

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
