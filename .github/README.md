# GitHub automation

The workflow files are the source of truth for triggers and job settings. Image
publication is separate from deployment; these workflows do not deploy workloads.
Changes under `.github/**` remain owner-protected.

## Pull request and main checks

[CI](workflows/ci.yml) runs on every pull request targeting `main` and every push
to `main`, without path filters. Jobs check the exact PR head or push commit:

- `command-deadlines`: command deadline and process-cleanup tests via
  `make test-timeouts` (5 minutes).
- `lint`: Trunk checks with explicit base/head SHAs and check annotations (5 minutes).

Stack-specific checks run in their own workflows, filtered by path:

- [Go](workflows/go.yml): golangci-lint, `go test -race`, generated-code drift, the
  MCP tool contract (`make check-mcp-tools`), and the prod image build from
  `server/Dockerfile`. The MCP contract still runs the Python exporter
  (`scripts/export-mcp-tools.py`), so its import closure stays under `backend/`
  (`src/mainloop/` modules, `pyproject.toml`, `uv.lock`, `README.md`) and `models/`
  until a Go exporter replaces it. Nothing else under `backend/` is kept.
- [Web](workflows/web.yml): `tsc`, ESLint, Vitest, the Vite build, design checks, API
  and theme drift, the static server test, and the `web/Dockerfile` build.
- [Contract](workflows/contract.yml): Redocly lint of `api/openapi.yaml`.

CI uses `ubuntu-latest`, read-only contents/actions permissions by default, and
check-write permission for lint. It publishes no images or job outputs.

## Multiarchitecture workspace development image

[Mainloop dev image](workflows/dev-image.yml) runs on PRs targeting `main` and
pushes to `main` only when one of these paths changes (no manual trigger):

- `.devcontainer/**`, `.trunk/**`, `scripts/with-timeout.mjs`
- `package.json`, `pnpm-lock.yaml`, `pnpm-workspace.yaml`, `.npmrc`
- `backend/pyproject.toml`, `backend/uv.lock`, `models/pyproject.toml` (the Python
  exporter's environment, see below)
- `.github/workflows/dev-image.yml`

PR-only `validate` builds `.devcontainer/Dockerfile` without pushing, separately
for `linux/amd64` on `ubuntu-latest` and `linux/arm64` on `ubuntu-24.04-arm`
(10 minutes each). Main pushes instead run `publish-amd64` and `publish-arm64`
on those native runners (10 minutes each). Each publishes
`ghcr.io/oldsj/mainloop-dev` by digest and exposes a `digest` job output.

The dependent `merge` job (3 minutes, `ubuntu-latest`) combines both digests into
a multiarchitecture index with tags `<full commit SHA>`, `sha-<full commit SHA>`
and `latest`. It validates and exposes the index `digest`, and records the source,
tags and immutable image reference in the job summary. This workspace image is
distinct from the application images below.

## Production application images

[Publish images](workflows/images.yml) runs on all pushes to `main` and manual
`workflow_dispatch`, without path filters. Its `publish` matrix builds the Go server
(`server/Dockerfile`, `prod` target) and the React app (`web/Dockerfile`) on
`ubuntu-24.04-arm`, publishing only `linux/arm64` images (15 minutes each):

- `ghcr.io/oldsj/mainloop-backend:<first seven commit SHA characters>` (the Go server;
  the image keeps its historical name because the production manifests pin it)
- `ghcr.io/oldsj/mainloop-frontend:<first seven commit SHA characters>`

The frontend uses the workflow's production API build argument. There is no
`latest` tag or job-level output. Step outputs provide the short SHA and build
digest; summaries record source, tag, digest and an immutable infrastructure pin.
Manual runs are not restricted to `main` by a job condition. Production pin
updates require a separate manually reviewed infrastructure PR, not this build.

## Shared live-development application images

[Publish live dev images](workflows/live-dev-images.yml) runs on pushes to `main`
and manual `workflow_dispatch`, without path filters. Its `publish` matrix is
explicitly restricted to `refs/heads/main`, including manual runs. It builds the
`dev` target of the Go server and the React app on `ubuntu-24.04-arm`, publishing
only `linux/arm64` images (15 minutes each):

- `ghcr.io/oldsj/mainloop-server-dev`
- `ghcr.io/oldsj/mainloop-web-dev`

Both receive `<first seven commit SHA characters>` and `main` tags; `main` is the
mutable one, and the shared `mainloop-dev` overlay pins it. Step outputs
provide the short SHA and digest; summaries record source, tags and digest, with no
job-level output. These are application dev targets, not the multiarchitecture
workspace image. Publishing them does not start or update the shared instance.

## Concurrency and publication boundaries

Each workflow cancels in-progress runs in its own workflow/ref concurrency group
(`dev-image` uses `dev-image-${{ github.ref }}`). Matrix jobs disable fail-fast so
independent builds can finish. Publication jobs have package-write and contents-read
permissions; workspace PR validation has contents-read only. Builds use GitHub
Actions caches separated by image or architecture. The job timeouts above are
limits, not measured runtimes or qualification claims.
