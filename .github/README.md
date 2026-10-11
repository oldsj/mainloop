# GitHub automation

These workflows separate pull-request validation from image publication. Publishing an image is
not deployment; production digest pins require a manually reviewed infrastructure PR.

## CI

[ci.yml](workflows/ci.yml) runs on every pull request targeting `main` and every push to `main`,
with no path filters. A change to this README therefore runs the full CI checks. Each job checks
out the exact PR head (or push SHA), rather than a synthetic PR merge commit.

- `backend-tests`: offline fixtures and scratch PostgreSQL tests via `make test-backend`, with a
  disposable PostgreSQL 16 service; 10-minute job limit.
- `frontend-unit`: command-deadline/process-cleanup tests, frontend diagnostics and unit tests via
  `make test-timeouts` and `make check-frontend test-frontend`; 5-minute limit.
- `lint`: Trunk checks with annotations, full Git history and explicit base/head comparison;
  5-minute limit.
- `build`: independent backend/frontend Docker builds, tagged `mainloop-backend:test` and
  `mainloop-frontend:test`; 8-minute limit per matrix job. No registry push or explicit platform
  is configured. The frontend uses a local API build argument.

All CI jobs use `ubuntu-latest`. CI has read-only contents/actions permissions except lint's
check-annotation write permission. UV HTTP timeout/retries are 30 seconds/1 retry. Browser and
live-agent end-to-end suites are not invoked by these jobs.

## Workspace development image

[dev-image.yml](workflows/dev-image.yml) runs for PRs targeting `main` and pushes to `main` only
when these paths change: `.devcontainer/**`, `.trunk/**`, `scripts/with-timeout.mjs`,
`backend/pyproject.toml`, `backend/uv.lock`, `models/pyproject.toml`, `package.json`,
`pnpm-lock.yaml`, `pnpm-workspace.yaml`, `.npmrc`, or `.github/workflows/dev-image.yml`.
There is no manual-dispatch trigger; this README alone does not trigger it.

- PR-only `validate` builds `.devcontainer/Dockerfile` without pushing, separately on native
  amd64 (`ubuntu-latest`) and arm64 (`ubuntu-24.04-arm`) runners; 10-minute limits.
- Main-push-only `publish-amd64` and `publish-arm64` publish native builds by digest to
  `ghcr.io/oldsj/mainloop-dev`; each exposes a `digest` job output and has a 10-minute limit.
- `merge` waits for both publishing jobs and creates the multiarch index for `linux/amd64` and
  `linux/arm64`; 3-minute limit. Tags are the full commit SHA, `sha-<full SHA>` and `latest`.
  Its `digest` output is the index digest; the job summary records tags, source commit and
  immutable `image@digest` reference.

This multiarch workspace image is distinct from the application images below.

## Production application images

[images.yml](workflows/images.yml) runs on every push to `main` (no path filter) and manual
`workflow_dispatch`. Manual runs publish the selected ref: the `publish` job has no main-only
condition. It does not run on PRs.

The backend/frontend `publish` matrix builds their respective Dockerfiles on `ubuntu-24.04-arm`
for `linux/arm64`, with an 8-minute limit per job. It pushes
`ghcr.io/oldsj/mainloop-backend:<7-character SHA>` and
`ghcr.io/oldsj/mainloop-frontend:<7-character SHA>`; no `latest` tag is configured. The frontend
uses the workflow's production API build argument. Summaries record source commit, tag, build
digest and an immutable infrastructure pin, not a deployment result.

## Shared live-development images

[live-dev-images.yml](workflows/live-dev-images.yml) runs on every push to `main` (no path filter)
and manual dispatch, but its `publish` job is guarded to run only on `refs/heads/main`, including
manual runs. There is no PR validation job.

The backend/frontend matrix builds Dockerfile target `dev` on `ubuntu-24.04-arm` for
`linux/arm64`, with a 10-minute limit per job. It pushes
`ghcr.io/oldsj/mainloop-backend-dev` and `ghcr.io/oldsj/mainloop-frontend-dev`, each tagged with
the 7-character commit SHA and `latest`. Summaries record source commit, tags and build digest.
These images serve the shared live-development instance, whose DevSpace configuration pulls
`latest` and synchronizes source; they are not the multiarch workspace image or production images.

## Shared controls

All four workflows cancel superseded runs in their workflow/ref concurrency group (the workspace
image workflow uses `dev-image-<ref>`). Matrix builds use `fail-fast: false` and architecture/image
scoped GitHub Actions caches. Publishing jobs have package-write permission; workspace validation
has contents-read only. Application publishers record source/revision OCI labels. None of these
workflows deploys Kubernetes resources.
