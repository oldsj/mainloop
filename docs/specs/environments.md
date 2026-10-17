# Development environments

Implemented: owner-managed environment metadata, immutable versions, project grants
and project selection. New workspaces resolve and pin the selected environment;
existing sessions keep their recorded environment. There are no environment MCP tools
or agent-facing environment controls.

History before `e776537` describes the retired Python backend. Where other text on this page differs from the Go status, the Go status is current.

## Registration and validation

`POST /environments` accepts `name`, `source_kind` (`prebuilt_image` by default),
`image`, `architecture` (`amd64` by default, or `arm64`), optional `watched_tag` and
optional `follow_watched_tag` (default false). Images must use
`registry/repository@sha256:<64 lowercase hex digits>`; mutable references are
rejected. A watched tag is metadata unless `follow_watched_tag` is set (see
[Following a watched tag](#following-a-watched-tag)). `follow_watched_tag` needs a
prebuilt image and a watched tag; otherwise registration returns 422.

The operator setting `ENVIRONMENT_REGISTRY_ALLOWLIST` is a JSON list, defaulting to
`["ghcr.io"]`. A registry outside this list is rejected before any registry request.
The anonymous OCI client reads manifests and config only, verifies content digests
and descriptor sizes, resolves a Linux platform from an index when present, and
requires config `User` exactly `65532:65532`. Missing USER, names, root and other
numeric identities are rejected before persistence. A single-platform manifest
must also match the requested architecture. Metadata reads require identity HTTP content encoding and reject compressed
responses before consuming the body. Each read has a 4 MiB pre-append size limit,
a 30-second elapsed budget including anonymous token exchange/retry, and
20-second inactivity timeouts. Whole validation and refresh operations each have
a 90-second elapsed budget; expiration reports a registry error. Anonymous bearer exchange is supported on the registry's own HTTPS origin;
cross-host authentication and manifest redirects are unsupported. Policy
`oci-static-v2` permits one HTTP 307 hop only for an immutable config blob from
public GHCR after its same-origin anonymous bearer exchange. The target must be
HTTPS at exactly `pkg-containers.githubusercontent.com`, with no port or port 443,
no userinfo and no fragment. A fresh client sends no Authorization, cookies or
registry credentials, refuses further redirects and retains the read budget,
identity encoding, byte limit, descriptor size and digest checks. Signed URLs
stay in memory and are excluded from dependency logs and error messages. Other
registries and unauthenticated redirects remain unsupported. Versions validated
under `oci-static-v1` retain their original evidence and fail the current-policy
workspace resolution check; fresh validation is required. No image layers
are downloaded or executed. Authentication failures report “private images not
supported yet”; an inaccessible or missing private repository may also return 404.
Private pulls are not supported.

Successful registration returns `environment` and `version`, with status
`static_validated`. It records index (when present), platform manifest and config
digests, architecture, declared USER, validator version and static results. Empty
capability profiles and `probes: not_run` mean capabilities have not been tested.
Base provenance defaults to `unknown`; user-pushed provenance does not imply a
verified base. Registration does not set the accepted default automatically.

For `source_kind: definition_repo`, supply `definition` containing `repository`,
exact 40-character lowercase `commit_sha` and repository-relative `path`. This
stores the reference with `pending_build` without fetching the repository or
executing anything. Repository control and publication authorization are not
verified in this slice; registration grants no build authority. Pending versions
cannot be selected or accepted as defaults.

## Owner API and versions

All routes use Mainloop's existing configured-owner identity (`MAINLOOP_OWNER_ID`)
and its owner-only network boundary. The API accepts no owner identity in request
bodies. The storage layer checks ownership and grants independently.

| Route                                           | Behavior                                                                 |
| ----------------------------------------------- | ------------------------------------------------------------------------ |
| `GET /environments`                             | List the owner's environments                                            |
| `GET /environments/{id}`                        | Get an owned environment                                                 |
| `GET /environments/{id}/versions`               | List its versions                                                        |
| `GET /environments/{id}/versions/{version_id}`  | Get immutable evidence                                                   |
| `POST /environments/{id}/refresh`               | Supply `version_id` identifying the image repository/platform to refresh |
| `PATCH /environments/{id}`                      | Supply `follow_watched_tag` to turn following on or off                  |
| `PUT /environments/{id}/default`                | Supply `version_id` to accept a statically validated default             |
| `PUT /environments/{id}/grants/{project_id}`    | Supply `permission: use` or `derive`                                     |
| `DELETE /environments/{id}/grants/{project_id}` | Revoke the project grant                                                 |
| `PUT /projects/{id}/environment`                | Select for an owned project                                              |
| `GET /projects/{id}/environment`                | Get selection, or null if absent                                         |

Refresh resolves the stored watched tag and validates its digest, appending a new
candidate version. Even an unchanged tag produces a new candidate record. It
never rewrites old evidence, accepted defaults or selections. Validation failure
leaves prior state untouched. Versions are append-only at the database level.
The default is an explicit owner decision; `static_validated` is metadata evidence,
not runtime acceptance. Operator-default and derived-package provenance are modeled
for future use; this API does not create operator defaults or derived builds.

## Following a watched tag

An environment registered with `follow_watched_tag: true` follows its tag
automatically. About once a minute the server resolves the tag the way Refresh does.
If the platform manifest differs from the newest version's, it appends a validated
version. Either way the newest version becomes the accepted default if it is not
already, but only if it could be accepted by hand: statically validated under the
current validator policy. A newest version that is not (a stale validator, say) is
not promoted; the pass appends a fresh version from the tag instead. A failed
resolution or validation changes nothing, is logged, and is retried on the next
pass. An unchanged tag appends nothing.

An environment accepts a new image at most once per interval, so a tag that moves on
every merge does not create a version (and a runtime template) each time. The first
image is always accepted. The interval is `ENVIRONMENT_FOLLOW_MIN_INTERVAL_SECONDS`,
3600 by default, between 60 and 604800. A change that arrives inside the interval is
picked up by the first pass after it, as the newest image at that point; images the
tag passed through in between are never recorded.

`PATCH /environments/{id}` with `follow_watched_tag` turns following off or on. Turning
it off keeps the versions and the current default and stops all automatic changes.
Turning it on needs a prebuilt image and a watched tag, otherwise 422.

Projects that follow the default pick up the new image for workspaces created
afterwards; existing workspaces stay pinned. Projects that selected an explicit
version keep it, which is also the rollback: select the older version, since the next
pass would override a manual default change. Environments without the flag are never
changed automatically.

The intended use is a warm workspace image: CI publishes `cache-main` for every
merge to `main` (see `.github/README.md`), and the project's environment follows it.
The image carries the Go build cache, Go modules and pnpm store for that commit under
`/opt/cache`, so a new session's first build compiles little or nothing.
The first workspace on a new digest waits for the runtime to prepare that image, and
creation can fail briefly and succeed on retry. A session whose repository state
differs from the image's commit still works and only rebuilds what changed.
A snapshot that keeps only workspace data (data scope) still resumes on the image's
baked `/opt/cache`; only what a session wrote into `/opt/cache` is lost.

Follow-up: every accepted digest leaves a runtime template and golden snapshot in the
runtime until its own retention exists; the interval above bounds how fast they grow.

## Project selection and sharing

Selection accepts `environment_id` and either `version_id` or
`follow_default: true`, plus `expected_version`. Use `expected_version: 0` for a
first selection; later writes use the returned `revision`. Stale values return
409, including concurrent first selections. A version must belong to the selected
environment and be statically validated. Following a default requires an accepted
default; reads return its current `resolved_version_id` without rewriting the
selection. Explicit selections retain their version when the default changes.
Both modes apply to future workspace creation; existing workspaces remain pinned.

The environment owner may grant a project `use` or `derive`. Both permit selection;
`derive` reserves authority for a later build path and grants no ability to change
the environment default or other grants. Project ownership remains required for
selection. Cross-owner selection without a grant returns 403. Revocation blocks
new selections; existing selections remain visible with `access_revoked: true`.
They are not silently deleted or activated.

## Not yet implemented

Build execution, controlled-repository definition ingress, trusted publication,
resolved-package provenance, executable validation/probes, ABI and reserved-path
checks, authenticated package requests/derive enforcement, approvals for build/activation,
activation of a different environment in an existing workspace and retention are not implemented. Models
reserve parent version, structured package declaration and approval-reference
fields; they do not execute package installation or establish approval authority.

## Workspace environment resolution

Workspace creation resolves the project's explicit version or accepted default once,
re-checks its use grant, and requires `static_validated` evidence for the deployment
platform. `WORKSPACE_DEVELOPMENT_PLATFORM` defaults to `linux/arm64`; `linux/amd64`
is also supported. A version stores evidence for one platform; a missing platform,
revoked grant, missing default, pending version or stale validation policy rejects creation before kagent is called.

The workspace stores the environment and version ids, platform image digest reference,
platform, and policy identity (`version_id:validator_version`). CreateSession sends this
selection as `development_environment`. Replacement and uncertain-create recovery reuse
that stored selection. Recovery retries the persisted request id; a replacement uses a fresh
request id. Default changes and CLI/runtime updates never change the recorded environment. kagent's reported development environment and runtime composition
are stored separately when present. The workspace view displays the short resolved digest.
Projects without a selection retain the legacy CreateSession request without that field.
A workspace with a selection also needs the Git read transport and the push gate (kagent prepares it
only with one read and one push reference), so creation is refused with `git_transport_required`
when they are not configured.

Selection requires kagent's runtime composition feature and service-token authentication
to be enabled. Static validation is metadata evidence, not a live composition proof.

## Implemented project UI

The project page shows the saved environment selection separately from a draft.
Owners can register a public digest-pinned image with a name and amd64 or arm64
architecture, choose a statically validated current-policy immutable version, or
follow an accepted environment default. Accepting a selected version as the default
is an explicit action affecting all projects following that environment. Registration
shows the returned static-validation status; it does not establish runtime readiness.
Pending-build and obsolete-policy versions cannot be saved. With no selection, new
sessions use the configured native agent image, but delegated code tasks are refused with
`project_environment_required` (see [tasks](tasks.md)). Changes apply to newly started
sessions and do not migrate running sessions. Reads never select or accept defaults
automatically. Saving uses the observed selection revision; conflicts refresh saved
state while preserving the draft for review. An unconfirmed registration reply blocks
repeat registration in that form until the owner reconciles the environment list.

React status (migration F7): the React environment section (`web/src/features/environments`)
implements this UI, including the saved and draft lines, eligibility labels, accept-default and
save with the observed revision, and the uncertain-registration lock. A version list that fails
to load says so and offers a retry, and the defaults warning waits for a fresh read. A successful
registration resets its form. A failed write clears an earlier success notice. Proven by offline
component and model tests against MSW handlers; not proven against a live registry or server.

## Go server status (migration slice G4)

Implemented in `server/` and covered by offline tests that use a fake registry and a fake HTTP
transport: all eleven environment routes, the `oci-static-v2` validator and the anonymous GHCR
client with its single CDN hop, immutable versions, grants and project selection. The behaviour
above holds for these routes, with these details:

- Failures use the `{"detail": "..."}` body. Status codes: 404 for a missing environment, version
  or project, and for an environment the caller does not own; 403 `Environment use grant required`
  when selecting another owner's environment without a grant; 409 `Stale expected version`; 422
  for registry, validation and target errors.
- `GET /projects/{id}/environment` returns 204 when nothing is selected.
- Selecting needs exactly one of `version_id` and `follow_default`. Order of checks: project
  (404), environment (404), access (403), revision (409), target (422), version (404), validation
  status (422).
- Registry reads happen before any transaction opens. A refresh then locks the environment row and
  inserts the version in a second transaction. A failed registration or refresh stores nothing.
  Refresh always appends a version.
- Selection locks the project row, then the environment row. Grant and revoke lock the environment
  row first and then check the project. A concurrent grant, revoke and selection cannot deadlock;
  two concurrent selections at the same revision give one 200 and one 409.
- `follow_default` selections report `resolved_version_id` from the current default and keep
  revision unchanged when the default moves.
- The follow pass (`environment-follow` sweep) runs as a lease-based reconciler sweep in the API process,
  about once a minute, over the configured owner's environments with
  `follow_watched_tag`. It reads the registry outside any transaction, then locks the environment
  row, re-checks the flag, the interval and the newest version, inserts at most one version and
  sets the default. `last_followed_at` records the last accepted image; setting the first default
  does not start the interval. Proven by offline tests against the fake registry only.
- Registry and validation limits (4 MiB, 30 s read, 90 s validation and refresh) are measured by
  unit tests with short budgets, not against a live registry.

The package build-input compiler from the retired Python backend is not in the Go server. No live
GHCR run has been made, so the CDN hop is proven against the fake transport only.
