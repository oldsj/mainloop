# Git read transport (Go server)

Status: implemented in `server/`, not proven live. `docs/specs/push-gate.md` still describes the
frozen Python sidecar; this file covers the Go replacement for the read side only. Push
(`git-receive-pack`) is a later slice and is refused today.

## What it does

A workspace's agent clones and fetches its repository through `mainloop-git-read`, never from
GitHub directly and never with a GitHub credential. `mainloop serve-git` listens on port 8003
for reads (and on 8004 with probes only, until the push slice) and streams the Git smart-HTTP
upload-pack protocol to GitHub using a GitHub App installation token that stays inside the
server.

Routes (anything else is refused):

- `GET /<owner>/<repo>.git/info/refs?service=git-upload-pack`
- `POST /<owner>/<repo>.git/git-upload-pack`
- `GET /health`, `GET /ready` (probes)

## Capability

Each workspace gets one read capability: a bearer token (`Authorization: Bearer ml_…`) issued
through the existing credential service as kind `git_read`, stored in an immutable Secret via
`k8ssecrets`, and referenced from the session by `gittransport.Reference`, which derives the
Secret name from the workspace id alone, so it can be built without issuing anything. Only the token hash is
stored. The capability is valid only while its credential is published, its `git_enrollments` row
is published and the session is live, and it names exactly one repository (`owner/name`,
compared case-insensitively) and one workspace.

Lifecycle follows the reconcile rule: `Enroller.Enroll` writes a `planned` enrollment row,
issues the credential (idempotent), then marks the row `published`. A replay or an interrupted
run resumes without a second credential. The capability is revoked with the workspace: the credential
service revokes the `git_read` credential when the session ends and deletes its Secret, and a
sweep marks the enrollment revoked. There is no separate revoke call. No database lock is held across Kubernetes or GitHub calls.

## Refusals

Errors are plain text `<code> <request_id>`, with no upstream text.

| Case                                                                                               | Status | Code                                                                                     |
| -------------------------------------------------------------------------------------------------- | ------ | ---------------------------------------------------------------------------------------- |
| Host not in the allowlist                                                                          | 404    | (host guard)                                                                             |
| Missing, malformed, revoked or ended-session capability                                            | 401    | `authentication_required`                                                                |
| Repository differs from the capability's                                                           | 403    | `repository_denied`                                                                      |
| `git-receive-pack`, in path or `service`                                                           | 403    | `push_not_served`                                                                        |
| Unknown path or service, wrong method or query                                                     | 403    | `path_invalid`, `route_denied`                                                           |
| Repeated header                                                                                    | 403    | `duplicate_header`                                                                       |
| Routing or override headers (`Forwarded`, `X-Forwarded-*`, `X-Actor*`, `X-Session*`, `Upgrade`, …) | 403    | `routing_header`                                                                         |
| Wrong content type, `Git-Protocol` other than versions 0, 1, 2, a body on discovery                | 403    | `content_type`, `git_protocol`, `http_framing`                                           |
| Content-Encoding other than identity or gzip                                                       | 403    | `http_compression`                                                                       |
| Body over the cap (256 MiB on the wire or inflated)                                                | 413    | `body_limit`                                                                             |
| Invalid gzip                                                                                       | 400    | `body_invalid`                                                                           |
| GitHub unavailable, app not installed, unexpected answer                                           | 503    | `upstream_unavailable`, `repository_unavailable`, `upstream_refused`, `upstream_invalid` |

Towards GitHub the proxy sends only the installation token (contents:read, scoped to the one
repository), the Git protocol header and a fixed content type. It does not follow redirects, and
it drops any response that is not a 200 with the exact Git media type or that reflects the token
in a header or in the body. The guard matches the raw token, the Basic credential and the
base64 fragment determined by the token at each byte alignment, including unpadded encodings,
encodings with other bytes before or after the token, and forms split across reads. A token split
by pkt-line framing, or an incomplete token/encoded fragment at the end of a response, is not
caught. This is a reflection guard, not a general decoder of upstream data.
A path must be the canonical spelling: `/<owner>/<repo>.git/...` with no doubled `.git`.

When a response is cut off or an upstream call fails, the server logs the request id, the refusal
code and known sentinel names or other error types (never error text). A mid-stream abort is logged before the connection
is cut.

Routing-header refusals include `Via`, `X-Real-IP`, `X-Envoy-*` and underscore spellings of
`X-Forwarded-*`, compared case-insensitively. No client routing header is sent to GitHub. The push
gate refuses the same set; the Host, route and header policy lives in `server/internal/gitroute`.
The 60-second idle watchdog starts before upload and resets on response reads; upload and the
wait for the first response byte share that initial bound. There is no per-capability or listener
concurrency cap; admission limits need a separate capacity policy.

Difference from the Python sidecar: gzip request bodies are accepted and inflated under the same
size cap; Python refused all compression.

## Configuration

- `GIT_TRANSPORT_ENABLED` (default false) and the GitHub App settings turn the read listener on.
  Otherwise `serve-git` answers every request with 501 and needs no database or GitHub
  connection.
- Allowed Hosts: `mainloop-git-read.<namespace>.svc.cluster.local` (read) and
  `mainloop-git-push.<namespace>.svc.cluster.local`; probes are exempt.
- The enrollment sweep is registered with the API server's reconciler, in a cluster only.

## Proven and not proven

Proven by tests: a real `git clone` and `git fetch` through the proxy against a local bare
repository served by `git http-backend` behind a fake GitHub token check, with scratch PostgreSQL
and a fake Kubernetes client; every refusal above; revocation with the workspace; idempotent and
resumable issuance; and the token-reflection guard in `internal/github`.

Not proven: any run against github.com, a deployed image or manifests, or a live agent session.
The workspace create enrolls the workspace and sends the credential reference with the session
(see `workspaces.md`); that path is proven against the fake kagent and Kubernetes client only.

## Schema notes

`git_publications.workspace_id` is plain text, not a foreign key: the ledger outlives the
workspace, so deleting a session is never blocked by its publications. The table carries the
reconcile claim columns (`attempts`, `next_attempt_at`, `lease_owner`, `lease_until`) and a claim
index over `pending`, `dispatching` and `unknown`, for the push slice.
