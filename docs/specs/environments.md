# Development environments

Implemented: owner-managed environment metadata, immutable versions, project grants
and project selection. New workspaces resolve and pin the selected environment;
existing sessions keep their recorded environment. There are no environment MCP tools
or frontend selection controls.

## Registration and validation

`POST /environments` accepts `name`, `source_kind` (`prebuilt_image` by default),
`image`, `architecture` (`amd64` by default, or `arm64`) and optional `watched_tag`.
Images must use `registry/repository@sha256:<64 lowercase hex digits>`; mutable
references are rejected. Watched tags are metadata only.

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

Builders, executable validation/probes, ABI and reserved-path checks, package
requests/policy enforcement, approvals for build/activation,
activation of a different environment in an existing workspace, retention and environment selection UI are not implemented. Models
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

Selection requires kagent's runtime composition feature and service-token authentication
to be enabled. Static validation is metadata evidence, not a live composition proof.
