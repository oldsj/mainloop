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
dev-postgres run bash -c 'cd backend && uv run --no-sync python -m unittest discover -s tests -t . -v'
pnpm check
(cd frontend && node --test src/lib/*.test.ts)
make lint
```

`make lint` requires a fetched `origin/main`. Dependency sync needs the approved
package registries. Offline, such as in an agent workspace, Trivy uses its built-in
checks and Trunk skips update checks. CI fetches the latest checks, so results can
differ slightly.
Guarded browser, live-agent, and cluster suites are separate opt-in checks.

Local amd64 verification ran the commands above successfully as both 65532 and
root, with networking disabled after dependency sync.
The backend ran 615 tests without skips. Frontend unit tests passed 34 tests;
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

The **Mainloop dev image** workflow builds amd64 and arm64 on pull requests.
Only pushes to `main` publish `ghcr.io/oldsj/mainloop-dev`; the job output and
run summary record its digest. No image is published by local builds.
Once environment registration is available, register the published immutable
`ghcr.io/oldsj/mainloop-dev@sha256:…` digest, or register this definition at an
exact source commit, then select the accepted version for the project. This
document does not claim that registration or runtime composition is implemented.
