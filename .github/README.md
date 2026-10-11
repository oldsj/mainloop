# GitHub automation

The workflow files are the source of truth. Image publication produces registry
artifacts; it is not deployment. Changes anywhere under `.github/**` remain
owner-protected, including this guide.

## Pull request validation

[CI](workflows/ci.yml) runs on pull requests targeting `main` and pushes to `main`,
with **no path filters**. A change to this README still requires the full CI checks.
Each job checks out the exact PR head (or push SHA):

- `backend-tests`: offline unit fixtures and scratch PostgreSQL tests via
  `make test-backend`, using PostgreSQL 16 and a disposable, non-durable database;
  10-minute job timeout.
- `frontend-unit`: command deadline/process cleanup tests (`make test-timeouts`),
  frontend diagnostics and unit tests (`make check-frontend test-frontend`);
  5-minute timeout.
- `lint`: Trunk checks with full Git history and explicit base/head SHA comparison;
  5-minute timeout. This job can write check annotations.
- `build`: independent backend/frontend Docker builds, using repository-root
  contexts and the respective Dockerfiles; 8-minute timeout per matrix job.
  Local tags are `mainloop-backend:test` and `mainloop-frontend:test`; the frontend
  uses a local API build argument. There is no registry push or explicit platform
  setting in these CI builds.

CI has read-only contents/actions permissions by default. Its uv HTTP timeout is
30 seconds with one retry. It cancels superseded runs in the same workflow/ref
concurrency group; build matrix failures do not cancel the other matrix job.

## Workspace development image

[Mainloop dev image](workflows/dev-image.yml) runs on pull requests targeting
`main` and pushes to `main` only when these paths change:

- `.devcontainer/**`, `.trunk/**`, `scripts/with-timeout.mjs`
- `backend/pyproject.toml`, `backend/uv.lock`, `models/pyproject.toml`
- `package.json`, `pnpm-lock.yaml`, `pnpm-workspace.yaml`, `.npmrc`
- `.github/workflows/dev-image.yml`

There is no manual dispatch trigger. A README-only change does not trigger this
workflow. All builds use `.devcontainer/Dockerfile` with repository-root context.

- On PRs, `validate` builds without pushing on native amd64 (`ubuntu-latest`) and
  arm64 (`ubuntu-24.04-arm`) runners, with 10-minute timeouts.
- On main pushes, `publish-amd64` and `publish-arm64` publish native images by
  digest to `ghcr.io/oldsj/mainloop-dev`, each exposing a `digest` job output and
  using a 10-minute timeout.
- `merge` depends on both publishers and creates the multiarch index for
  `linux/amd64` and `linux/arm64`. Tags are the full commit SHA, `sha-<full SHA>`,
  and `latest`. Its `digest` job output and step summary record the index digest
  and immutable image reference; its timeout is 3 minutes.

Publish jobs have package-write permission; validation is read-only. Superseded
runs are cancelled per ref, and native validation matrix failures are independent.

## Production application images

[Publish images](workflows/images.yml) runs on every push to `main` (no path
filter) and on manual dispatch. Unlike live-dev publication, its `publish` job has
no main-ref guard, so manual dispatch can publish the selected ref.

The independent backend/frontend matrix builds from repository-root context using
`backend/Dockerfile` and `frontend/Dockerfile`, on `ubuntu-24.04-arm`, for
`linux/arm64` only. The frontend receives a configured production API build
argument. Images are `ghcr.io/oldsj/mainloop-backend` and
`ghcr.io/oldsj/mainloop-frontend`, tagged with the first seven source-SHA characters
(no `latest` tag). Step summaries record source commit, tag, digest and immutable
infra pin. Production pin updates require a manually reviewed infra PR.

Each matrix job has an 8-minute timeout and package-write permission. Superseded
runs are cancelled per workflow/ref; matrix failures do not cancel sibling builds.

## Shared live-development images

[Publish live dev images](workflows/live-dev-images.yml) runs on every push to
`main` (no path filter) and manual dispatch, but `publish` only runs when the ref
is `main`, including for dispatch.

The independent backend/frontend matrix uses each application's Dockerfile with
`target: dev`, repository-root context, native `ubuntu-24.04-arm` runners and
`linux/arm64`. It publishes `ghcr.io/oldsj/mainloop-backend-dev` and
`ghcr.io/oldsj/mainloop-frontend-dev`, tagged with the first seven source-SHA
characters and `latest`. Summaries record source commit, tags and digest. These
serve the shared live-development instance, which pulls `latest` and receives
DevSpace source synchronization; they are not the multiarch `mainloop-dev`
workspace image or the production application images.

Each matrix job has a 10-minute timeout and package-write permission. Superseded
runs are cancelled per workflow/ref; matrix failures are independent. Publication
alone does not deploy or reset the shared instance.
