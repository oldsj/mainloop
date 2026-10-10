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
(cd backend && uv sync --frozen --python 3.13)
pnpm install --frozen-lockfile
dev-postgres run make test-backend
pnpm check
(cd frontend && node --test src/lib/*.test.ts)
make lint
```

`make lint` requires a fetched `origin/main`. Dependency sync needs the approved
package registries. Offline, such as in an agent workspace, Trivy uses its built-in
checks and Trunk skips update checks. CI fetches the latest checks, so results can
differ slightly.
Guarded browser, live-agent, and cluster suites are separate opt-in checks.

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
Every test, including setup, teardown, cleanups and async runner shutdown, has a
fatal 60-second deadline; module/class fixtures and discovery also have 60-second
deadlines, including suites returned by `load_tests` hooks. On a timeout,
the runner prints the active test/fixture and all thread stacks. The whole run has
a 540-second cap with up to five seconds for stack dumping and process cleanup,
leaving headroom
under the harness's ten-minute foreground limit. CI's job cap is ten minutes.
The supervisor kills the test process group on timeout or cancellation, including
test subprocesses; `dev-postgres` then removes its disposable cluster.
Class databases clone one migrated, empty template per run; migration tests still
execute the real migrations and each class retains its own independent database.

The runner reports the slowest 15 modules and tests. Module totals include class
and module fixtures; test totals include setup/teardown and async runner shutdown.
Save machine-readable
evidence with `MAINLOOP_TEST_TIMINGS=/path/to/timings.json`. A focused run uses
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
