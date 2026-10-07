# Workspaces

A workspace is a kagent Session with a git checkout. Creating one makes the Session, and kagent
clones the repository into the Session's harness actor. The workspace belongs to the project
branch, not to one Session: if kagent replaces the Session, the replacement gets the identical
checkout request. Workspace lifecycle is separate from the session's task status, native-agent
activity, message delivery, user attention, and publication state.

Mainloop stores the checkout request (repository, ref, branch, depth), the declared preview
ports and the idle timeout. Everything about the running state is read from kagent.

## Lifecycle

| Observed state | User label | Meaning                                                                |
| -------------- | ---------- | ---------------------------------------------------------------------- |
| `running`      | RUNNING    | kagent reports the Session ready.                                      |
| `suspending`   | SUSPENDING | A suspend is in progress.                                              |
| `suspended`    | SUSPENDED  | The Session is suspended. Its checkout is kept.                        |
| `resuming`     | RESUMING   | The Session is being created or resumed.                               |
| `failed`       | FAILED     | kagent reports the Session failed. `detail` carries kagent's reason.   |
| `unknown`      | UNKNOWN    | kagent is unreachable, has no such Session, or reports a deleting one. |

The UI shows workspace state wherever sessions are listed and links from the session detail to
`/workspaces/{id}`. The workspace page shows the manifest, state, last activity and preview ports,
with suspend, resume, refresh and delete controls. Session badges and status are not changed by
workspace operations.

## Create, suspend, resume, delete

- A workspace is created from a project or straight from a repository. Naming a repository
  (`owner/name` or `https://github.com/owner/name[.git]`) finds the owner's project for it, or
  creates one, in the same request. Repository names are matched case-insensitively (GitHub's
  rule): `Foo/Bar` and `foo/bar` are one project, shown with the case it was first created with.
  The desktop sidebar's **New workspace** control (next to the project list) takes a repository
  and an optional branch, creates the workspace and opens it; a `422` and other API errors show
  inline. The project page creates one from an existing project.
- A project created this way stores the canonical `https://github.com/owner/name` URL and no
  default branch: nothing asks GitHub at this point. With no `ref` the clone uses the remote's
  default branch. `POST /projects/{id}/refresh` (no UI calls it yet) records the default branch
  from GitHub. A project that already exists for that repository keeps its stored URL and
  metadata. A bad `branch` or `ref` is refused before any project is created; the project does
  outlive a workspace that kagent then rejects, so a retry finds it again.
- Create sends `CreateSession` with the workspace. If kagent rejects the repository (for example
  its host is not in the Agent Harness `git.origins`) nothing is kept and the API returns `422`.
  If the outcome is unknown, the rows are kept and **refresh** retries the same request.
- Suspend is refused (`409`) while the native delivery ledger has a recorded, queued (unless held after a stop), sending or
  delivered-but-incomplete delivery. The check and `SuspendSession` run under the REST
  process's per-session lock. The MCP container is a second writer that this lock does not
  cover, so a message it records during a suspend can still arrive; it resumes the Session
  before it is sent. A message that arrives during a suspend waits for it, then resumes the
  Session before it is sent.
- Resume calls `ResumeSession` only when the Session is suspended and counts as activity.
- Delete is refused (`409`) while a delivery is open. If the create's outcome was never known,
  delete first resolves it with the same idempotent `CreateSession` (the stored request id) that
  **refresh** uses: a Session found there is deleted. A rejected retry or an outcome that is still
  unknown returns `502` and keeps the rows: a rejected retry cannot prove that the earlier
  unknown request created nothing. kagent's `DeleteSession`
  must confirm (a Session it does not know counts as confirmed) before the rows are removed;
  otherwise the API returns `502` and keeps them.
- Archiving a session deletes its kagent Session the same way. A failed delete is retried by the
  reconcile loop until kagent confirms. A create whose outcome was never known stores no Session
  id; deleting the workspace resolves it first (above), and the operator sweep below finds any
  that were never recorded.

## Preview and idle-out

The preview URL has the form `<port>--<workspace>--preview.<domain>`: one DNS label directly
under `<domain>`, so a single-label wildcard certificate (`*.<domain>`) and listener cover every
preview. A host with more labels (`x.<port>--<workspace>--preview.<domain>`) is not a preview.
`<domain>` (and the scheme and port) come from `SUBSTRATE_PREVIEW_BASE_URL`, for example
`http://localhost:8001` (giving `http://3000--<workspace>--preview.localhost:8001`) or
`https://<domain>` in production. Mainloop checks that the workspace is the configured owner's and
that the port is one the workspace declares, then opens
`CONNECT actor-upstream:<port>` through the Substrate router to the Session's actor
(`kagent/session-<kagent session id>`). The router has no authentication of its own, so the owner
check is Mainloop's. With `onQuiesce: Full` on the harness, the CONNECT would wake a suspended actor behind kagent's back,
so a preview of a suspended workspace first resumes its Session through kagent (the same as the
resume endpoint), once for any number of concurrent previews, and only then connects. If the resume
fails the preview is `502` (WebSocket close `1013`) and the router is not contacted. The idle
timeout then suspends the workspace again.

Mainloop has one owner, set by `MAINLOOP_OWNER_ID` (default `local-dev-user`, which Kind and
local development use). Every REST handler and the preview proxy take the user from one function,
`current_user()` (`backend/src/mainloop/identity.py`), which returns the owner. It **ignores
`X-User-ID`**: nothing authenticates that header, so honoring it would let any caller choose an
identity. Routes that fetch a row by id (threads, conversations, queue items, projects, sessions,
notifications) also check that the row belongs to that user and answer `404` otherwise. A later
multi-user setup reads the identity a trusted gateway sets, in `current_user()` only.
Access to the tailnet, plus the in-cluster NetworkPolicy, is the security boundary. The API also
refuses a non-GET request that carries an `Origin` other than the frontend's, a localhost
development origin (development mode only) or its own, so another origin's page (the agent's preview page, for one)
cannot make a browser post a cancel, archive, suspend or resume.

**Host check.** The API answers `404` to any `Host` that is not the frontend domain
(`FRONTEND_DOMAIN`), the configured `API_DOMAIN` (with or without a port), a name in `MAINLOOP_API_HOSTS`
(comma-separated, for in-cluster Service names), loopback (development) or a preview host. The
kubelet health probe (`/health`) is exempt because it addresses the pod by IP. Preview hosts are
handled before this check. The preview base domain is a wildcard (an owner-controlled domain, or a
name from a wildcard DNS service such as `sslip.io`), so preview origins and the API can be _same-site_ (they
share a registrable domain for a deployment whose previews sit under the same domain as the API), and the agent writes the page served
from a preview origin. The Origin guard, the Host check and the unauthenticated tailnet boundary
are the only things between that page and the API. This is a known, accepted risk for one owner;
a deployment that cannot accept it must serve previews under a registrable domain of their own.
The preview listener never reads an identity header (`X-User-ID` included): a
workspace that is not the configured owner's is `404` (WebSocket close `4404`), and an undeclared
port is `403` (close `4403`). A refused request does not touch the workspace's idle clock; only a
request that passes both checks does.

**Preview Origin check.** An unsafe HTTP method (anything but `GET`, `HEAD` and `OPTIONS`) and
every WebSocket handshake to a preview host must carry either no `Origin` or the preview's own
origin (the configured base URL's scheme with the request's `Host`). A different origin is
refused with `403` (WebSocket close `4403`) _before_ the workspace is looked up, touched or woken
and before the router is contacted, so the dev server never sees it. This covers a foreign site,
the API's origin, a sibling preview (`*.<domain>`, including another workspace's preview), another
scheme or port, and `null` (a sandboxed or redirected page). Same-origin requests, including a dev
server's HMR WebSocket, pass. `GET`, `HEAD` and `OPTIONS` navigation is not checked. A request with
**no `Origin`** is allowed: a browser always sends `Origin` on a cross-origin write or WebSocket
handshake, so only a non-browser client (curl, a script) omits it, and a page cannot cause it. The
dev server's own CSRF protection remains its own responsibility for same-origin pages.

The proxy replaces the client Host with exactly one upstream Host header. It does not forward `Cookie`, `Authorization`, `X-User-ID`, `Forwarded`, `X-Real-IP`,
`X-Forwarded-*` or `Tailscale-*` headers to the dev server (the agent writes the page and runs the
server, so anything forwarded is visible to it), nor hop-by-hop headers, on HTTP requests and
WebSocket upgrades. Every other request header is forwarded, because apps send their own
(CSRF tokens, `X-Requested-With`, GraphQL and RPC client headers); it is a denylist, not an
allowlist. A header the gateway adds under another name is not stripped. It retries only a router CONNECT failure; after forwarding,
a disconnect has an unknown outcome and the request is not replayed. Active HTTP streams and
WebSocket connections refresh the workspace's activity every 20 seconds until they close.

A preview-only wake does not make kagent suspend the actor again, so Mainloop does. The reconcile
loop checks once a minute. A workspace is suspended when it has been quiet for its idle timeout
(1 to 1440 minutes, default 30): no preview traffic, no resume, and no delivery activity, and no
open delivery. It is skipped when the Session is not ready, is mid-operation, or is already
suspended. The main thread is excluded by role and never suspended by Mainloop idle-out.
Held reports after a stopped turn do not prevent parking.

## Orphaned kagent Sessions

A Session kagent holds for `KAGENT_USER_ID` with no `native_bindings` row cannot be found from
Mainloop's side: for example a create whose reply was lost and whose row was then removed by hand.
The operator sweep lists them and deletes only when asked:

```bash
# In the production image (it has no uv; the command is on PATH):
mainloop-sweep-kagent-sessions            # list only
mainloop-sweep-kagent-sessions --delete   # delete what it listed

# Local development, from the repository:
cd backend
uv run mainloop-sweep-kagent-sessions
uv run mainloop-sweep-kagent-sessions --delete
```

It uses the backend's environment (`DB_HOST`, `DB_PORT`, `DB_NAME`, `DB_USER`, `DB_PASSWORD`, `KAGENT_GATEWAY_URL`, `KAGENT_USER_ID`) and pages
through kagent's `ListSessions`. Run it while Mainloop is quiet. Each Session is checked against
the database again just before it is deleted, and the command refuses deletion when bindings exist that have
no Session id yet, because a create with an unknown outcome may own a listed Session: refresh or
delete those workspaces first. The exit status is non-zero if any delete failed.

## API

- `POST /workspaces` creates a workspace:
  `{project_id | repo, branch?, ref?, depth?, dev?, agent_kind?}`. Send exactly one of
  `project_id` and `repo`, otherwise `422`. Returns `201`.
  - `repo` is a GitHub repository as `owner/name` or `https://github.com/owner/name[.git]`
    (one optional trailing slash). It is validated strictly: github.com over https only, an
    owner of letters, digits and hyphens (at most 39, no leading or trailing hyphen), a name of
    letters, digits, `.`, `_` and `-` (at most 100), and nothing else: extra path segments,
    credentials, a port, a query or a fragment are `422`. The project is found or created
    atomically by `(user, owner/name)`, so concurrent requests share one project. An unknown
    `project_id` is `404`.
  - `ref` defaults to the project's default branch, or is empty (the remote's default) when the
    project has none recorded.
    The associated session's `base_branch` preserves that ref, including the empty string.
    Legacy NULL base refs are read as empty, so they do not break session list or detail requests.
  - `branch` is the local branch to create or switch to. When empty it is always a new
    `mainloop/<8 hex>` branch, whatever `ref` or the project's default is, so the working branch
    never depends on hidden project state and never shadows `origin/<default>` with a different
    commit. Send the project's default branch explicitly to work on it.
  - A bad `branch`, `ref` or other field is `422` with the first problem, for example
    `branch: Value error, ...`.
- `GET /workspaces` and `GET /workspaces/{id}` return lifecycle records for the current user.
- `POST /workspaces/{id}/suspend`, `/resume`, `/refresh`.
- `DELETE /workspaces/{id}` returns `204`.
- `GET /workspaces/{id}/ports` lists the declared preview ports and their URLs.
- Lifecycle changes are published on the event stream as `workspace:updated`.

## Scope and evidence

Fake-backed unit tests and opt-in PostgreSQL tests cover these paths. Earlier revisions ran
against a scratch PostgreSQL; the M7 continuation tests and manifests still require supervisor
execution against the frozen candidate. Source inspection is not execution evidence.

**Verified live** on a Kind cluster running the kagent fork and Substrate (images built from this
work before the review repairs; they were not redeployed after them):

- Creating a workspace, a first turn that sees the checkout, a second turn on the same Session,
  and the K1 markers outside `.git`.
- Idle-out, and a message to a suspended workspace (`ResumeSession`, the turn completing, files and
  the dev server surviving the `onQuiesce: Full` snapshot).
- A suspend and a message fired together at three offsets: each time the suspend landed first, the
  message resumed the Session and the turn completed. kagent's own end-to-end tests cover the
  other orderings (`SuspendSession` refused while a turn is in flight; a send refused during a
  suspend, then resumed and resent).
- A preview request to a suspended workspace: the wake first went through the router alone and left
  kagent and Mainloop `suspended`; after the resume-through-kagent fix, four concurrent previews
  produced one `ResumeSession`, and idle-out then suspended it again.
- A preview to a ready workspace, an undeclared port (`403`) and an unknown workspace (`404`).
- Deleting a workspace deletes the kagent Session.
- Stopping a running turn: `stopped`, then `no_open_turn`, the Session still usable.

**Not verified live:**

- The cold-clone first turn against the client's 30 second timeout (a depth-1 public clone
  finished within about a minute; create-to-ready was not measured).
- The K1 negative cases (the agent removes `done`, so the next turn fails and the repo is left
  untouched) and the markers after a snapshot restore. The kagent end-to-end tests cover the
  first.
- Stopping a parked (`input-required`) turn.
- A suspend that arrives in the middle of a turn, through Mainloop (kagent's end-to-end test covers
  it).
- WebSocket previews against a suspended workspace, and kagent's `SuspendSession` and
  `DeleteSession` idempotency.
- The review repairs: the idle re-check under the lock, the archived-session delete retry, the
  preview header filtering and the cross-origin write check.

The tracked [Kind overlay](../../k8s/apps/mainloop/overlays/kind/README.md) includes dedicated
workspace Harnesses with `git.origins`, `sessionIdleTTL: 0s` and `onQuiesce: Full`. Production
Harnesses remain the responsibility of the kagent installation.
