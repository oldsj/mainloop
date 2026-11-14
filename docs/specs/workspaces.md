# Workspaces

A workspace is a kagent Session with a git checkout. Creating one makes the Session, and kagent
clones the repository into the Session's harness actor. The workspace belongs to the project
branch, not to one Session: if kagent replaces the Session, the replacement gets the identical
checkout request. Workspace lifecycle is separate from the session's task status, native-agent
activity, message delivery, user attention, and publication state.

Mainloop stores the checkout request (repository, ref, branch, depth), the declared preview
ports and the idle timeout (stored, no longer enforced; see below). `agent_kind` accepts a configured [provider profile](providers.md) ID
or alias; existing Claude/Codex selection is unchanged. Everything about the running state is
read from kagent.

## Go server (migration slice G5)

This section is the Go server's lifecycle in `server/internal/workspaces`. Labels: **implemented** is
in the code and covered by fake-backed tests against a scratch PostgreSQL; **proposed** is not
built; nothing here has been measured against a live kagent.

The lifecycle follows kagent's control API `kagent.api.v1` (Substrate 1.0), which has no suspend or
resume call: kagent puts an idle Session's Actor to sleep by itself and wakes it on use. This is
**implemented and tested with fakes only**; a live check on a Kind stack is pending.

History before `e776537` describes the retired Python backend. Where other text on this page differs from the Go status, the Go status is current.

### Implemented

- MCP credential (G6c): before `CreateSession` the create publishes the workspace's MCP credential (role `agent`, grant `workspace`) and sends its reference with the request; a delete that replays the create sends the same reference and issues nothing. Delete and the drop of a refused create revoke the credential in the same transaction that removes the rows. If the credential cannot be issued because the Secret name is still owed a delete, the create retries; if the binding has ended, the create is rejected. With no Secret store (outside a cluster) the workspace is created without a credential and a warning is logged (a workspace with a development environment is refused instead; see Workspace preparation).
- Git read: with `GIT_TRANSPORT_ENABLED`, a GitHub App and a cluster, the create also enrolls the workspace for git read (`gittransport.Enroller.Enroll`, idempotent) before `CreateSession` and sends the capability's reference after the MCP one. Only a GitHub repository gets one. The decision, including "no git read", is frozen on the workspace row (`git_read_decided`, `git_read_credential`) before the first `CreateSession`, so a restart that turns git read on or off cannot change the credentials of a retry or of the delete's replay: both send the frozen set, and the replay enrolls and issues nothing. The credential service revokes the capability with the workspace. With any of the three missing, the workspace is created as before (a workspace with a development environment is refused instead; see Workspace preparation).
- Git push (G11c): with the read transport and push gate configured, creation freezes an optional push reference after read enrollment and before the first `CreateSession`. A writable code-task attempt is found through its attempt-derived request ID and must hold the exact repository/branch writer generation. Every workspace with a development environment also gets the push reference without a reservation, because kagent prepares a Session only with exactly one read and one push reference (see Workspace preparation); that reference has no bearer, Secret or grant and cannot become a writer. Workspaces without an environment get none. The durable reservation is keyed by the attempt, freezes the reference, and cannot transfer to another workspace. Creation retries and deletion replays reuse that reference and never mint another reservation. Already-created Sessions receive no new reference.
- Push enrollment runs separately after the native Session is freshly observed READY with no operation and exactly one matching reference. The task caller must have attached the workspace and activated the current writer before `pushgate.Service.Enroll` can publish its grant and Secret. Creation refusal, runtime failure/deletion, task abandonment and reservation expiry release push authority. A delete request revokes push authority in the same transaction that records the delete intent, not when the delete completes; a first-attempt refusal releases it in its outcome transaction; other abandoned/expired reservations are reconciled automatically, and the release sweep also revokes any grant or push credential left behind a revoked reservation. A delete later rejected (for example because a turn is open) leaves the surviving workspace without push authority on purpose: the reservation is terminal and the workspace fails closed. A task workspace whose project has no known default branch yet waits to be created, retrying until the `project-metadata` sweep fills it or the create retry window ends (`gave_up`, with the reason recorded), instead of being created read-only for good. No database transaction spans native, metadata or Secret calls. Local tests include a real push from the workspace credential to a bare repository; live kagent and GitHub remain unmeasured.
- Caller request id: `CreateWithRequestID` takes a UUID and is idempotent on it. A repeat call, concurrent or later, returns the workspace the first call made (waiting for its create the same way) and makes no second kagent Session; `LookupByRequestID` finds one by that id. The id is also the kagent request id. The HTTP API does not take one yet; the tasks service derives it from the attempt.

- Every lifecycle change is a durable `workspace_ops` row (`create` or `delete`) with a state (`pending`, `retrying`, `succeeded`, `rejected`, `gave_up`, `cancelled`)
  and an idempotency key. One operation is open per workspace; asking for the kind that is
  already open joins it, and a different kind is `409`. A delete cancels the others. The API
  inserts the row, wakes the reconcile loop and waits up to `WORKSPACE_OPERATION_WAIT_SECONDS`
  (default 30) for the result; if the operation is still going it answers with the workspace as
  it stands and the loop carries on. A `suspend` or `resume` row queued before the upgrade is
  closed as `cancelled` without a kagent call.
- The Agent (namespace and name) is resolved when the workspace is made and stored on its row.
  Every replay and the orphan sweep use that stored reference, so a config change between
  attempts cannot turn a replay into a different request.
- The workspace id is the kagent Session id. The row stores a stable `request_id`, which
  `CreateSession` sends on every attempt, so a retry, a refresh or a restarted process can only
  get the one Session back. The loop records `attempts` before it calls kagent and holds no
  transaction or lock across the call. Outcomes are fenced on the lease, so a cancelled or
  superseded operation is never overwritten.
- Create retries: a transient error retries after 15 seconds, doubling to 120 seconds, until 600
  seconds have passed since the first failure, then the operation is `gave_up` and the workspace
  is `failed` until **refresh**. A permanent error (`INVALID_ARGUMENT`, `NOT_FOUND`,
  `ALREADY_EXISTS`, `PERMISSION_DENIED`, `UNIMPLEMENTED`, `UNAUTHENTICATED`, a kagent
  configuration refusal) is `rejected` at once. So is `FAILED_PRECONDITION` when kagent says the
  request id belongs to a Session that was deleted: a replay cannot bring it back. Any other
  `FAILED_PRECONDITION` (the Agent has no ready revision yet, its golden preparation failed,
  composition is not configured) is transient and retries.
  Refusal of the first attempt removes the rows, publishes `workspace:updated` with the failed
  state (there is no removal event; clients drop the workspace when they next list) and returns
  `422`. A refusal after earlier attempts of unknown outcome keeps the rows, so a Session an
  earlier attempt made can still be found and deleted; the workspace shows as failed. The rows
  are dropped only while that create is still the latest and nothing is open, so a Refresh that
  lands after the refusal keeps them. Refresh sends a new create operation with a fresh window.
  The three timings are
  `WORKSPACE_CREATE_RETRY_INITIAL_SECONDS`, `_MAX_SECONDS` and `_WINDOW_SECONDS`.
- There are no suspend or resume operations, and Mainloop never asks kagent to suspend or wake a
  Session. kagent suspends an idle Session's Actor itself, after its own quiesce delay, and wakes
  it on use: the next A2A send, `PrepareSessionWorkspace`, or a connection through the Substrate
  router (a preview request or a live-view attach). A sleeping Session stays `READY`. Asleep or
  awake is read from `Session.runtime_association.current_active` on `GetSession`: present and
  false is asleep (shown as `suspended`); absent is not known and shown as `running`, since the
  Session runs on use either way. kagent reports the association whenever the Session has an
  Actor, with `phase` `active` and `current_active` true only while the Actor runs.
  `RUNTIME_STATE_SUSPENDED` and the `SUSPEND`,
  `RESUME` and `REVERT` operations can still appear briefly for kagent's own lifecycle work;
  Mainloop treats them as busy and reads again.
- Processes do not survive sleep: kagent's snapshots are data-only. The checkout is kept, but a
  dev server must be started again after a wake.
- Delete removes the rows only after kagent confirms: `DeleteSession` must report the same Session
  as deleted, or answer not found. Anything else keeps the rows and returns `502`; the operation
  keeps retrying. A workspace whose create was never sent is removed without calling kagent. One
  whose create may have been sent replays the idempotent create first and deletes whatever comes
  back. The replay carries the same credential reference the create sent (the deterministic
  `mainloop-mcp-<id>` Secret, even if the credential is revoked by now), because kagent answers a
  replay whose credentials differ with `AlreadyExists`. If the replay is refused for good (or kagent says the request id belongs to a deleted
  Session) the rows are removed: no Session exists, or the orphan sweep reclaims it. A reply
  without a Session id is not trusted and the delete retries. Delete is refused (`409`) while a delivery is open.
- Archiving a session deletes its kagent Session: a sweep (`WORKSPACE_ARCHIVED_DELETE_SECONDS`,
  default 60) deletes the Session of every archived binding that has no open delivery, and marks
  `kagent_deleted_at` only after kagent confirms. A failed delete is tried again on the next pass.
  An archived workspace whose create outcome was never known has no Session id to delete by: once
  its create op is closed (given up or succeeded without a bound id) and no op is open, the same
  sweep starts a delete op for it, which replays the create, deletes what comes back and removes the
  rows. A workspace with a create op still open is left to that op. The same sweep prunes old
  operation rows.
- No idle suspend: the `workspace-idle` sweep and `WORKSPACE_IDLE_CHECK_SECONDS` are removed. The
  manifest's `dev.idle_timeout_minutes` (1 to 1440 minutes, default 30) is still stored and
  returned but is no longer enforced: kagent's quiesce delay decides when a runtime sleeps.
- Orphan sweep: a Session in kagent that was created for `KAGENT_USER_ID`, belongs to one of the
  configured provider profiles' Agents or to any Agent a create was ever sent to (`kagent_agents`, kept after the row that named it is settled or purged), is not deleted, is at least `WORKSPACE_ORPHAN_MIN_AGE_SECONDS`
  old (default 600) and that no binding names, is an orphan. It runs on a schedule
  (`WORKSPACE_ORPHAN_SWEEP_SECONDS`, default 3600) and deletes; the command
  `mainloop sweep-orphans` lists them and `--delete` removes them. Both refuse to delete while a
  binding exists that has no Session id yet and whose create was requested (the binding records the
  create request id before the call; a create that was never sent does not count), because that
  create may own a listed Session. A create of a session that has since finished is settled by the
  session service's `resolve-session-creates` sweep, which replays it with its request id, Agent and
  credential reference and deletes what comes back, so a cancelled session never blocks the sweep for ever. Each Session is checked against the database again just before it is
  deleted. The exit status is non-zero if a delete failed or deletion was blocked, and the
  scheduled pass logs a warning when it is blocked. kagent records every Session made with the
  same ServiceAccount token (Mainloop sends a projected token for the audience
  `kagent-invocation`, which kagent verifies by TokenReview) as created by the same user, so the creator does not tell Mainloop instances
  apart: only distinct Agent names do. Instances that share a kagent must never have shared
  Agents, or one instance's sweep deletes the other's Sessions. An Agent a deployment once used
  (a workspace row or a session create named it) stays in its orphan sweep (`kagent_agents`) after
  the configuration stops naming it. `mainloop sweep-orphans` prints the Agent set it covers
  (`agent   <namespace>/<name>`) before the orphans, and `--forget-agent <namespace>/<name>`
  deletes one from that set and exits. It refuses an Agent the current configuration names, one
  that a workspace or an unsettled create still names (forgetting it would stop the sweep reclaiming
  a Session that landed there), one that is not in the set, an empty value, and a combination with
  `--delete`; the flag checks run before the command reads configuration or the database.
- Every state change publishes `workspace:updated` in the same transaction as the change. The
  event carries the workspace lifecycle; it is not a removal notice.
- `Wake(workspaceID)` is what a message to a workspace session calls before it is sent. It no
  longer resumes anything: a workspace with an open delete is refused (`RejectedError`), one that
  is gone is not found, and otherwise the call records the use (`Touch`). The send itself wakes a
  sleeping Session.
- `WakeForPreview(workspaceID)` and `Touch(workspaceID)` are the entry points the preview proxy
  calls. `WakeForPreview` refuses an open delete the same way (`409`) and records the use. While
  the workspace is still being created or has no kagent Session it returns `ErrBusy`, and the
  proxy answers its waking page (`503`, `Retry-After: 2`) instead of holding the request.
- Preview URLs are `<port>--<workspace>--preview.<host>` built from `SUBSTRATE_PREVIEW_BASE_URL`, ports sorted
  by number.
- The preview proxy (G12, `server/internal/preview`), described under "Go preview proxy" in the
  preview section below. Tests cover it, including the router's mutual TLS and allow-list, with a
  fake router and dev server; it has not been run against a real Substrate router.

### Proposed, not built

- Checkout SHA resolution.

## React status (migration slice F8)

The React workspace page (`web/src/features/workspaces`, route `/workspaces/$id`) is tested with
MSW against the OpenAPI contract. Labels follow the Go section's: **implemented** is in the code
and covered by those tests.

Implemented in the React client:

- Lifecycle and publication badges. The page has no Suspend or Resume buttons; it keeps Refresh,
  which is always available, and Delete, which asks for confirmation first. A sleeping workspace
  shows the hint "Asleep. It wakes when you send a message or open a preview."
- A control cannot be submitted twice. After a `409` the page re-reads the workspace before it
  offers a retry, and it shows the backend's message.
- A failed action keeps its message when its row or section unmounts. Moving to another workspace
  clears it.
- The repository is a link only for `http` and `https` URLs. The image digest shows its first 19
  characters, or "unknown" when it does not parse.
- Preview links are offered only for `http` and `https` URLs. A bad row is dropped and counted; the
  rest still show. Opening a sleeping workspace's preview wakes it on the Go side (G12, the
  preview proxy, described below); the React page only links to the preview.
- A failed refresh keeps the cached data on screen, with an inline error and a retry.
- A later read that cannot be parsed keeps the last good workspace on screen, with a notice that it
  could not be read. A failed action's notice and its failed detail both show.
- An action the server accepted, whose result cannot be read, says the action may have gone through.
  The page then re-reads the workspace, which settles the state.
- Delete names what is removed: the branch, or the read-only publication. A delete whose reply is
  lost is checked by a re-read; if that finds the workspace gone, the page says so and drops the
  cached copy.

Not proven:

- Live kagent behaviour. Nothing here has run against a real workspace.
- The shell mount for `/workspaces/$id`. Route tests attach the real route; the shell is unchanged.
- Live event refresh of the workspace page. The SSE applier is tested with the page mounted, not a
  real event stream.

## Lifecycle

| Observed state | User label | Meaning                                                                                                                                                                           |
| -------------- | ---------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `running`      | RUNNING    | kagent reports the Session ready with a current, active Actor.                                                                                                                    |
| `suspended`    | SUSPENDED  | The Session is ready but asleep (no active Actor). Its checkout is kept and it wakes on use.                                                                                      |
| `resuming`     | RESUMING   | The Session is being created (including automatic create retries), or kagent is doing brief lifecycle work on it (a `SUSPENDED` state, or a suspend, resume or revert operation). |
| `failed`       | FAILED     | kagent reports the Session failed, or automatic create retries stopped. `detail` carries the reason.                                                                              |
| `unknown`      | UNKNOWN    | kagent is unreachable, has no such Session, or reports a deleting one.                                                                                                            |

`suspending` remains in the contract's enum but is no longer reported.

The UI shows workspace state wherever sessions are listed and links from the session detail to
`/workspaces/{id}`. The workspace page shows the manifest, state, last activity and preview ports,
with refresh and delete controls. Session badges and status are not changed by
workspace operations.

## Create, sleep, delete

- A workspace is created from a project or straight from a repository. Naming a repository
  (`owner/name` or `https://github.com/owner/name[.git]`) finds the owner's project for it, or
  creates one, in the same request. Repository names are matched case-insensitively (GitHub's
  rule): `Foo/Bar` and `foo/bar` are one project, shown with the case GitHub reports once its
  metadata has been read (the import and the `project-metadata` sweep rewrite it from GitHub).
  The desktop sidebar's **New workspace** control (next to the project list) takes a repository
  and an optional branch, and both it and the project page let the owner choose Claude Code
  (the default) or Codex. The selected agent kind is stored in the workspace manifest and shown
  in workspace and session lists. Creation opens the workspace; a `422` and other API errors show
  inline.
- A project created this way goes through the same import as `POST /projects`, so there is one
  way to create a project. It stores the canonical `https://github.com/owner/name` URL and, when
  the GitHub App can read the repository within the import's time limit, GitHub's default branch;
  otherwise the default branch stays empty until the `project-metadata` sweep fills it. There is
  no refresh route. A project that already exists for that repository is reused and its metadata
  is refreshed from GitHub when the App can read it, so its URL and display case follow GitHub. A bad `branch` or `ref` is refused before any project is created; the project does
  outlive a workspace that kagent then rejects, so a retry finds it again.
- With both `GIT_TRANSPORT_ENABLED` and `PUSH_GATE_ENABLED` enabled, enrollment resolves the
  checkout base through the repository-scoped GitHub App client to a full commit SHA before
  storing the session, workspace or frozen create plan. An empty owner request ref selects
  GitHub's current default branch, even if the project has a different stored default; branches,
  tags and explicit SHAs are verified through the commits endpoint with `contents: read`.
  An unavailable ref returns a clear `422` refusal before
  workspace creation. The repo route resolves before inserting or touching the project, and
  enrollment reuses that verified SHA. The selected workspace branch is unchanged. Create
  retries and runtime replacements reuse the stored SHA; existing workspace refs are never
  rewritten. A workspace created with a selected development environment resolves its
  checkout the same way whenever the GitHub App client is configured, because preparation needs
  the full SHA. With either
  flag off and no environment, an empty request uses the stored project default if present, otherwise the remote
  default at clone time; no GitHub ref resolution is performed during admission.
- Owner workspace preparation (no task attempt) selects kagent's `agent` profile, preserving
  the selected development environment. Its
  fixed standing context permits direct work with the owner, with no delegation or task/report
  duties. The first send waits for confirmed preparation (below).
- Before the first kagent call, creation transactionally stores the owner project, session,
  checkout and a per-binding `workspace` MCP grant with its non-secret credential reference.
  The reference uses a unique Secret key for this session. Claude and Codex workspaces use the
  same contract. The normal workspace path verifies that the repository matches the
  owner-owned project. The grant is scoped to that project's repository and the exact stored
  feature branch; it allows `whoami` and `open_pull_request`, plus merge tools only under the
  existing merge enablement gates. It does not expose topic edits, reports or delegation.
- Create sends `CreateSession` with the stored workspace and exactly its credential reference.
  A Secret publish failure leaves the durable create pending and sends no workspace brief. An
  unknown create reply is retried with the original request id, checkout and credential
  reference. If the owner cancels before that outcome is known, the bearer hash is revoked and
  Secret cleanup proceeds; a later **refresh** or **delete** still retries the original request
  with that frozen reference, without publishing it again or restoring the hash. Refresh reports
  the recovered runtime; delete immediately disposes of it. Neither path sends a brief or creates
  a second writer. If kagent rejects the repository (for example
  its host is not in the Agent Harness `git.origins`) nothing is kept and the API returns `422`.
  If the outcome is unknown, the rows are kept and Mainloop retries the same request on its own.
- Automatic create retries run in the backend's native reconcile pass. A transient outcome
  (for example `UNAVAILABLE` while the development environment's snapshot is prepared, a
  deadline, an unreachable gateway, a lost reply or a Secret publish failure) schedules the next
  retry after 15 seconds, doubling to at most 2 minutes. Each retry is the same idempotent
  `CreateSession` that **refresh** sends (stored request id, checkout and credential reference)
  under the same per-session lock, so it can only return the one Session. While retrying, the
  workspace is `resuming` and `detail` says it is retrying automatically with the last error's
  classified reason (for example `kagent unavailable (gRPC 14)`); raw error text is only logged.
  A permanent error (`INVALID_ARGUMENT`, `NOT_FOUND`, `ALREADY_EXISTS`, `PERMISSION_DENIED`,
  `UNIMPLEMENTED`, `UNAUTHENTICATED`, a request id whose Session was deleted, or a kagent
  configuration refusal) stops retries at once, and transient errors stop once 10 minutes have
  passed since the first failure. The workspace is then `failed` with the reason. **Refresh**
  still retries the same request and restarts the automatic retries. The retry state is stored
  on the workspace row, so it survives a backend restart; a workspace whose create never
  recorded an outcome (the backend stopped mid-create) is retried once it is 15 seconds old,
  but only while it is younger than the 10-minute window. An older one (for example left
  unconfirmed before automatic retries existed) is `unknown` and waits for **refresh**. The
  state belongs to one create identity: it is cleared when the binding gets a Session by any
  path (including a message delivery), and a replacement Session's new request id starts a
  fresh retry schedule. Completed, failed and cancelled sessions are never retried
  automatically (refresh and delete still reconcile them), and task-attempt workspaces are
  excluded: task provisioning retries its own creates.
- There is no suspend or resume. kagent puts an idle workspace's Actor to sleep after its own
  quiesce delay; the workspace then reads `suspended` and keeps its checkout, its workspace grant
  and its token hash. The next message, a workspace preparation, a preview request or a live-view
  attach wakes it. Processes do not survive sleep, so the agent has to start a dev server again
  after a wake.
- Delete is refused (`409`) while a delivery is open. If the create's outcome was never known,
  delete first resolves it with the same idempotent `CreateSession` (the stored request id and the
  same credential reference) that **refresh** uses, including after cancellation has revoked the
  bearer: a Session found there is deleted without republishing the credential or restoring its hash.
  Removing the rows deletes the session row before revoking the binding's credentials, so a concurrent
  `Issue` (which locks the session row `FOR SHARE`) either finishes first and is revoked, or finds the
  binding gone. A rejected retry or an outcome that is still
  unknown returns `502` and keeps the rows: a rejected retry cannot prove that the earlier
  unknown request created nothing. kagent's `DeleteSession`
  must confirm (a Session it does not know counts as confirmed) before the rows are removed;
  otherwise the API returns `502` and keeps them. Before deleting a confirmed runtime, Mainloop
  revokes the DB hash. Secret cleanup is best effort and its durable record survives deletion of
  the workspace rows.
- Archiving a session deletes its kagent Session the same way. A failed delete is retried by the
  reconcile loop until kagent confirms. A create whose outcome was never known stores no Session
  id; deleting the workspace resolves it first (above), and the operator sweep below finds any
  that were never recorded.

## Preview and sleep

The preview URL has the form `<port>--<workspace>--preview.<domain>`: one DNS label directly
under `<domain>`, so a single-label wildcard certificate (`*.<domain>`) and listener cover every
preview. A host with more labels (`x.<port>--<workspace>--preview.<domain>`) is not a preview.
`<domain>` (and the scheme and port) come from `SUBSTRATE_PREVIEW_BASE_URL`, for example
`http://localhost:8001` (giving `http://3000--<workspace>--preview.localhost:8001`) or
`https://<domain>` in production. Mainloop checks that the workspace is the configured owner's and
that the port is one the workspace declares, then opens
`CONNECT actor-upstream:<port>` through the Substrate router to the Session's actor. The target
is `<atespace>/<actor name>` from the kagent Session's runtime association when kagent reports one,
current or not; otherwise from the Session's A2A authority (`<actor>.<atespace>.actors.resources.substrate.ate.dev`, which kagent records when it creates the Actor); failing both,
`<KAGENT_ACTOR_ATESPACE>/session-<kagent session id, lower case>`, kagent's stable Actor name. The
router authenticates the backend by its pod certificate (mutual TLS, see "Tunnel" below) but knows
nothing of owners or ports, so the owner check is Mainloop's. Mainloop does not ask
kagent to wake anything: the `CONNECT` through the router is what wakes a sleeping Actor. A
Session that is not `READY` shows the waking page and the router is not contacted. Processes do
not survive sleep, so a preview of a just-woken workspace can fail until the agent has started the
dev server again, unless the project declares a workspace service (below).

## Workspace service

A project can declare a workspace service: the argv of a long-running process, such as a
development server, set with `PUT /projects/{id}/workspace-service` (`{"command": [...]}`, at most
61 non-empty arguments of at most 4096 bytes; `[]` declares none) and read with `GET`. It is
owner-only Mainloop configuration, never read from the repository.

- **Create.** A workspace created afterwards stores the command it sends kagent in
  `workspaces.service_command`: the declared argv run through `env` with
  `MAINLOOP_WORKSPACE_ID=<workspace id>` and
  `MAINLOOP_PREVIEW_URLS=<port>=<preview URL>,...` (one per declared port, by port number, built
  like the preview URLs the API returns). For example
  `env MAINLOOP_WORKSPACE_ID=ws1 MAINLOOP_PREVIEW_URLS=3000=https://3000--ws1--preview.example scripts/dev/workspace.sh run`.
  A service sets its dev origin from these. The create request carries it as kagent's
  `Workspace.service_command`; because it is stored, a replay sends the same bytes, and changing
  the project later changes only workspaces created after it. A command that with its preview URLs
  is over kagent's limits (64 arguments of 4096 bytes) fails the create with `422`.
- **Runtime (kagent).** kagent starts the command in the checkout at every start and resume of the
  workspace's runtime, once the checkout and any preparation are ready, without waiting for a turn,
  so a preview that wakes the workspace also starts its service. kagent's harness owns the process
  tree, caps its log at two 8 MiB files under `/data/workspace-service/`, and writes
  `/data/workspace-service/status.json` (`command`, `state`, `pid`, exit status). The agent's
  instructions say a declared service is already running and how to restart it; the agent reads
  the workspace id and preview URLs from `command` there. A service that exits is not restarted
  until the next resume.
- **Preview hold.** For a workspace that declares a service, the preview proxy keeps retrying a
  tunnel the router refuses (backoff from 250 ms to 2 s) for up to 10 seconds per request instead
  of once. Past that, and for a `GET`, `HEAD` or `OPTIONS` whose tunnel closes before any response
  (nothing listening on the port yet), it answers `503` with `Retry-After: 2` and a "Workspace
  service starting" page that reloads itself every 2 seconds and names the service log. Other
  methods keep the `502` "outcome unknown" answer and are never replayed. A workspace with no
  service keeps the single retry and the plain waking page.
- **Rollout.** kagent must run a version that knows `service_command` before Mainloop sends one: an
  older kagent keeps the field as unknown and runs nothing.

Known limits: a [forked workspace](#forked-workspaces-issue-311) keeps the source workspace's
command, so its service sees the source's `MAINLOOP_WORKSPACE_ID` and preview URLs (kagent's fork
copies the Session's workspace). The variables are in the service's environment, not the agent's
process environment. There is no UI or MCP tool for the setting yet. Tested against fakes (the fake
router and fake kagent); not measured against a live router or kagent.

Mainloop has one owner, set by `MAINLOOP_OWNER_ID` (default `local-dev-user`, which Kind and
local development use). Every REST handler and the preview proxy take the user from the configured owner.
It **ignores
`X-User-ID`**: nothing authenticates that header, so honoring it would let any caller choose an
identity. Routes that fetch a row by id (threads, conversations, queue items, projects, sessions,
notifications) also check that the row belongs to that user and answer `404` otherwise. A later
multi-user setup reads the identity a trusted gateway sets, in `current_user()` only.
Access to the tailnet, plus the in-cluster NetworkPolicy, is the security boundary. The API also
refuses a non-GET request that carries an `Origin` other than the frontend's, a loopback
development origin (`localhost`, `127.0.0.1` or `[::1]`, any port; development mode only) or its own, so another origin's page (the agent's preview page, for one)
cannot make a browser post a cancel, an archive or a delete.

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
port is `403` (close `4403`). A refused request does not record activity on the workspace; only a
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

Mainloop does not suspend a workspace after a preview, or at any other time. kagent puts the
Actor to sleep again after its own quiesce delay, and the workspace's idle timeout is not
enforced.

## Go preview proxy

Status: implemented in the Go server (G12, tests against fakes).

The Go server routes a request whose `Host` parses as a preview host to the proxy, ahead of the
API middleware (so the Host guard, API Origin guard, CORS and body validation never see it); the
request id, access log and panic recovery still wrap it. Any other host is the API.

- **Host.** `<port>--<workspace>--preview.<domain>` exactly as above, matched case-insensitively
  against `SUBSTRATE_PREVIEW_BASE_URL`: the Host's port must equal the base URL's port, and a base
  URL with no port means the Host has no port (so `:443` is refused for `https://x`, and no port is
  refused for `https://x:443`). The label is at most 63 characters, the port is 1 to 65535 with no
  leading zero, and the workspace id is a lower-case DNS-label-shaped id that may contain `--`.
- **Admission.** The owner's workspace must exist, not be archived and have a live kagent Session,
  and the port must be in its declared `ports`: `404` otherwise for the workspace, `403` for the
  port. The lookup repeats at every tunnel dial, so a workspace archived or re-bound between the
  check and the connect gets no tunnel (`404`).
- **Origin.** As specified above, checked first. A refused WebSocket handshake is an HTTP `403`
  (the Go proxy answers before upgrading, so there is no close code `4403`), and an `Upgrade` that
  is not a `GET` WebSocket handshake is `400`.
- **Methods.** `CONNECT`, `TRACE` and `TRACK` are `405` before any lookup, wake or tunnel.
- **Wake.** `workspaces.Service.WakeForPreview`, whose result is trusted for 5 seconds so a page
  load's many requests do not each read the workspace; activity is still recorded on each request.
  It asks kagent for nothing. A workspace that is still being created, or has no kagent Session,
  shows a `503` "Workspace waking up" page with `Retry-After: 2`; one that is being deleted is
  `409`; any other failure is `502`.
- **Router TLS.** The Substrate 1.0 router has no plain-HTTP listener. `SUBSTRATE_ROUTER_ADDRESS`
  is its CONNECT-over-TLS listener (default
  `https://atenet-router.ate-system.svc.cluster.local:8444`; an `http` address is refused at
  startup). The backend verifies the router's certificate as `SUBSTRATE_ROUTER_SERVER_NAME`
  (default `atenet-router.ate-system.svc`) against the trust bundle at `SUBSTRATE_ROUTER_CA`
  (signer `servicedns.podcert.ate.dev/identity`), and presents the pod certificate at
  `SUBSTRATE_ROUTER_CLIENT_CREDENTIAL` (signer `podidentity.podcert.ate.dev/identity`, key and
  chain in one file). The kubelet rotates both, so both are read on every connect. The router
  admits only SPIFFE IDs on its allow-list (`atenetRouter.ingressAllowedSPIFFEIDs`); the backend's
  is `spiffe://cluster.local/ns/<namespace>/sa/mainloop-backend`. The router answers a client it
  does not admit with `200` to the `CONNECT` and then closes the tunnel ("network authorization
  denied" in the router's log; measured on Substrate 1.0.1 on Kind). By then the request has been
  forwarded, so it is a `502` and is not replayed. A missing or unreadable credential file, or a
  router certificate the trust bundle does not sign, fails the dial before anything is sent: after
  the one retry (or the service hold, see [workspace service](#workspace-service)) the owner sees the waking page, logged as "preview tunnel not ready" with the
  reason. The chart's `substrateRouterAuth.enabled` mounts both files into the API container only.
- **Tunnel.** `CONNECT actor-upstream:<port>` to `SUBSTRATE_ROUTER_ADDRESS` with
  `ate-target-actor: <atespace>/<actor name>` from the kagent Session's runtime association (read
  per request), current or not. With no association it is the actor the Session's A2A authority
  names, and with neither
  `<KAGENT_ACTOR_ATESPACE>/session-<kagent session id, lower case>`, kagent's stable Actor name.
  The `CONNECT` is what wakes a sleeping Actor. A Session that is not `READY` shows the waking
  page and the router is not contacted. The connect is timed by
  `SUBSTRATE_PREVIEW_CONNECT_TIMEOUT_SECONDS`. A router refusal before anything was sent is retried
  once, then the waking page; a failure after the request was forwarded is `502` and is never
  replayed. One tunnel per request (no pooling), no compression, streaming responses are flushed
  at once, and the dev server has 60 seconds to start answering.
- **Headers.** The `Host` sent upstream is `actor-upstream:<port>`. Besides the headers listed above,
  requests also lose any `X-Envoy-*` header (the gateway's client address), and hop-by-hop headers
  go. Responses lose every upstream `Set-Cookie`, including on the WebSocket `101`, so a dev server
  cannot set cookies on the owner's domain.
- **Request cap.** A body over 10 MiB is `413`: at once when `Content-Length` says so, and cut off
  when a streamed body runs over.
- **Keep-awake.** A WebSocket takes a lease row (`preview_leases`) when it opens; an HTTP request
  takes one if it is still running after 10 seconds. The holder renews every 20 seconds (lease TTL
  60 seconds) and deletes the row on close. A reconcile sweep every 20 seconds, under the runner's
  sweep lease, calls `Touch` for every workspace with an unexpired lease and prunes rows expired
  for over a minute. A holder that crashes stops renewing, so its lease lapses without cleanup.

Not covered: WebSocket close codes `4403`/`4404`/`1013` (see Origin above), per-session pooling of
tunnels, and behaviour against a real router.

## Orphaned kagent Sessions

A Session kagent holds for `KAGENT_USER_ID` with no `native_bindings` row cannot be found from
Mainloop's side: for example a create whose reply was lost and whose row was then removed by hand.
The operator sweep lists them and deletes only when asked:

```bash
# In the production backend container (the image entrypoint is the mainloop binary):
mainloop sweep-orphans            # list only
mainloop sweep-orphans --delete   # delete what it listed

# Local development, from server/:
go run ./cmd/mainloop sweep-orphans
go run ./cmd/mainloop sweep-orphans --delete
```

It uses the backend's environment (`DB_HOST`, `DB_PORT`, `DB_NAME`, `DB_USER`, `DB_PASSWORD`, `KAGENT_GATEWAY_URL`, `KAGENT_USER_ID`) and pages
through kagent's `ListSessions`. Run it while Mainloop is quiet. Each Session is checked against
the database again just before it is deleted, and the command refuses deletion when bindings exist that have
no Session id yet, because a create with an unknown outcome may own a listed Session: refresh or
delete those workspaces first. The exit status is non-zero if any delete failed.

## API

- `POST /projects` imports a repository for the current owner: `{repo}` with no extra fields.
  It accepts the same strictly validated GitHub references as workspace creation below.
  Returns the stored `Project` with `201`, including when it already exists. Repeated and
  concurrent mixed-case imports share one project per owner; an existing row is reused, updates
  its last-used time and takes its URL, display case and metadata from GitHub when the App can read
  it. Another owner's row is
  never reused. Invalid repository references return a sanitized `400`; invalid request
  shapes return `422`, before any database effects.
  Import starts no workspace or native Session. It makes one best-effort GitHub read, bounded
  by a time limit; if that read fails, the project is still recorded with an empty (unknown)
  default branch and the `project-metadata` sweep fills it later. There is no refresh route. The
  import creates no task, capability or push grant, selects no environment, and does not
  enable merging. This owner REST endpoint allows project import before explicit environment
  selection and Actor creation; a frontend import control is not implemented by this endpoint.
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
    The associated session's `base_branch` preserves that ref, including the empty string; it is
    never replaced by a fixed `main`.
    Legacy NULL base refs are read as empty, so they do not break session list or detail requests.
  - `branch` is the local branch to create or switch to. When empty it is always a new
    `mainloop/<8 hex>` branch, whatever `ref` or the project's default is, so the working branch
    never depends on hidden project state and never shadows `origin/<default>` with a different
    commit. Send the project's default branch explicitly to work on it.
  - A bad `branch`, `ref` or other field is `422` with the first problem, for example
    `branch: Value error, ...`.
- `GET /workspaces` and `GET /workspaces/{id}` return lifecycle records for the current user.
- `POST /workspaces/{id}/refresh`. The suspend and resume endpoints are removed.
- `DELETE /workspaces/{id}` returns `204`.
- `GET /workspaces/{id}/ports` lists the declared preview ports and their URLs.
- Lifecycle changes are published on the event stream as `workspace:updated`.

## Scope and evidence

Existing sessions are not migrated, backfilled or given credentials retroactively. Standalone `/agents` sessions have no workspace grant. New owner and
delegated workspaces use the shared enrollment path.

Fake-backed unit tests and opt-in PostgreSQL tests cover these paths. Earlier revisions ran
against a scratch PostgreSQL; the M7 continuation tests and manifests still require supervisor
execution against the frozen candidate. Source inspection is not execution evidence.

**Verified live before the cutover**, on the Python backend, on a Kind cluster running the kagent
fork and Substrate (images built from this work before the review repairs; they were not
redeployed after them). Not re-run on the Go server. These runs used the previous kagent API
(Substrate 0.4), which had `SuspendSession`, `ResumeSession` and `onQuiesce: Full`; the items that
name a suspend, a resume or idle-out record behaviour that has since been removed and say nothing
about the current API:

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

**Not verified live on the current kagent API (`kagent.api.v1`, Substrate 1.0):** everything
above about sleep and wake. That kagent suspends an idle Actor and keeps the Session `READY`,
that a send, a preparation, a preview request or a live-view attach wakes it, the stable Actor
name for a Session with no association, and the observed-state mapping are implemented and tested
with fakes only. A live check on a Kind stack is pending.

**Not verified live:**

- The cold-clone first turn against the client's 30 second timeout (a depth-1 public clone
  finished within about a minute; create-to-ready was not measured).
- The K1 negative cases (the agent removes `done`, so the next turn fails and the repo is left
  untouched) and the markers after a snapshot restore. The kagent end-to-end tests cover the
  first.
- Stopping a parked (`input-required`) turn.
- WebSocket previews against a sleeping workspace, and kagent's `DeleteSession` idempotency.
- The review repairs: the archived-session delete retry, the preview header filtering and the
  cross-origin write check.

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
Projects without a selection retain the legacy CreateSession request without that field;
delegated code tasks for them are refused (see [tasks](tasks.md)).

Selection requires kagent's runtime composition feature and service-token authentication
to be enabled. Static validation is metadata evidence, not a live composition proof.

## Git publication contract (default off)

Runtime readiness and workspace preparation are separate observations. The owner native
GET exposes bounded optional preparation evidence from the GetSession response it already
reads; see [session detail](sessions.md#session-detail). A missing association means that
read did not verify a current active runtime; it does not prove a generation change. A
saved receipt can retain the original generation even when the current runtime differs.
Missing or unavailable receipts do not mean unprepared, and confirmed historical evidence
does not authorize current work. GET neither refreshes receipt timestamps nor re-Prepares,
wakes, dispatches, or changes lifecycle/publication state. These fields are implemented with
fake-backed coverage (`kagenttest`); they are not live proof of preparation or a diagnosis of
installed configuration and runtime admission failures.

The [push gate contract](push-gate.md) defines feature-branch publication scope and
protected branches. Source supports frozen native enrollment, deferred read/push publication
and PostgreSQL authority for injectable listeners. Both enablement flags default off;
no production listener or containment is installed. Workspace responses include `publication_mode` and
`publication_reason`, independently of runtime state. The badge and detail page show
read-only reasons (default/protected branch, missing metadata, no grant, or disabled). Current
actor GitHub credentials remain unchanged until the separately verified infrastructure switch.
The complete MCP/read/optional-push tuple commits before original CreateSession bytes. Durable
same-session non-turn Suspend/Resume warmup and final fresh GetSession precede usable publication.
UUID storage alone grants no authority. Unknown outcomes recover the original tuple; history
without a plan is held rather than retrofitted. Default/protected workspaces receive read only.

A workspace created with a selected development environment is prepared before it takes its
first message: kagent admits nothing to such a Session until its workspace preparation is
confirmed, and it words the refusal as a bad reply. This applies to owner workspaces and to
delegated supervisor and child workspaces alike. kagent prepares a Session only if it holds exactly one read and one push Git proxy
reference, so every environment workspace carries both: the push reference is frozen even when no
writable attempt exists (an inert reference, see Git push), and creation is refused with
`git_transport_required` (422) when the server has no read transport, push gate or Secret store for
the repository. A create that reaches the operation without them is rejected with the same reason
instead of retrying. The reason names the way out: enable the git read transport and the push gate, or clear the
project's development environment selection. Workspaces without an environment have no preparation and keep the
preparation-free behavior. The checkout must be a full commit SHA, so
creation refuses with `checkout_ref_unavailable` when a selected environment is set and the ref
cannot be pinned (GitHub does not have it, or no GitHub App client is configured).

Preparation is a durable intent, not a side effect of the first send. Creating the workspace
stores a `workspace_preparations` row (`planned`, `requested`, `confirmed` or `failed`) in the
same transaction, keyed by the create request ID. The `workspace-preparation` loop:

- waits for a READY Session with no operation in progress. It does not wake a sleeping one:
  `PrepareSessionWorkspace` wakes the runtime itself;
- reads the Session's receipt before anything else. A confirmed receipt settles the row. A
  definite failure, a receipt for another action, or a historical receipt fails it at once
  (kagent keeps one preparation per Session, so a historical receipt never becomes current);
  a pending or uncertain receipt is waited on without calling Prepare again, which would open
  a new challenge;
- otherwise calls `PrepareSessionWorkspace` with the checkout stored on the workspace. The setup
  profile is chosen from the role of the task attempt the workspace serves (supervisor and child;
  `agent` when there is none) on the first call and then frozen on the row. The pinned setup
  digests belong to the kagent release the harnesses run and must change with it; a test fails
  when the vendored kagent API pin moves without them. `InvalidArgument`, `PermissionDenied` and
  `AlreadyExists` replies fail the row; other errors retry with the loop's backoff. The call
  carries the generation id and actor uid from the last `GetSession`, even when the association is
  not current. The generation id is derived from the Session and its Actor, so sleep and wake keep
  it; only a replaced Actor changes it. kagent answers `FAILED_PRECONDITION` or `ABORTED` when they are stale, and the
  next pass reads the Session again and retries. With no association at all the loop waits.

A row that is not confirmed 30 minutes after creation fails, but only after a last read of the
receipt, so a late confirmation still counts. A failed row's detail names the cause.

A send waits (bounded by the delivery hold) while the row is not `confirmed` and settles as
failed, with that detail, once it is `failed`. Disabling either flag does not change this gate.
The workspace lifecycle view does not report preparation; the owner sees a failed preparation in
the delivery detail ("workspace preparation failed; replace the session") and the logs.

Push enrollment also needs an awake Actor: kagent attests the runtime only while one runs, and
puts an idle Actor to sleep. Enrollment no longer wakes one. It waits until the preparation or the
first message has woken the Actor, instead of treating an unattested runtime as final.

## Task-backed workspaces

Owner task creation and authenticated supervisor child creation use the same transaction-aware
session enrollment as ordinary owner workspaces. A root task has a supervisor at depth 1; its
direct child has a child attempt at depth 2. Children and owner workspace agents cannot delegate.
Every coding attempt has its own session, binding, workspace, checkout branch and per-binding
MCP Secret. Its environment is resolved and pinned before native creation. Claude and Codex
attempts use their immutable role AgentRef, not a subsequently reloaded provider route.

A canonical owner/repository/branch claim excludes another ordinary or delegated writer, including
repository spelling variants. A creating or uncertain attempt retains its capacity and claim. A proven first-dispatch
input/origin rejection or control-authentication refusal atomically records audit evidence and
moves the attempt to draining, closing all later create admission, including workspace refresh.
Recovery settles that rejected identity without another create; retry requires a successor attempt.
A known runtime must still be deleted by exact identity before branch or capacity release,
even if rejection evidence exists. A dispatch marker persisted before the call makes a lost outcome conservative; a
later refusal cannot serve as no-start evidence.
The first brief is recorded once, only after native readiness and a transaction that rechecks
current attempt, writer generation, binding and parent authority. Provider capability evidence must
qualify the configured role; unknown evidence cannot enable task creation. Test fixtures explicitly
opt into fake qualification and are not live provider evidence.

Cancellation records durable intent, drains the attempt, revokes MCP/push authority, confirms
exact-identity native deletion (or proven no-start rejection), then fences and releases its pinned writer generation. A lost delete response
keeps capacity and branch ownership. A create retry refused after an unknown reply (including a
missing template or changed control authorization) does not prove runtime absence; its attempt
stays draining until the original runtime can be found and deleted. The attempt audit remains even when a rejected, never-started
workspace's resource rows are removed. API cancellation of a delegated session uses this same
path. Runtime creation, turn dispatch, resume and preview admission consult the central attempt
guard. Each HTTP/WebSocket router CONNECT and pre-forward retry re-resolves the target under
that guard, after bounded request-body reading and before forwarding. Long response streams do
not retain the admission lock. Runtime actions and scoped publication use the same persisted
ancestry checks and root-tree authority lock as parent draining and credential revocation.
Siblings serialize conservatively on that root key. Lock order is project policy → tree
authority → publication → runtime → task admission → rows, on the caller connection.

Verified merged coding tasks use the shared result cleanup sequence: wait for outstanding native
deliveries and children, drain, revoke credentials, confirm exact-identity Session deletion, then
release capacity and the pinned branch claim. A delete reply with another identity, a pending
delete operation or an unknown outcome retains the reservation for retry. Persisted confirmed
deletion resumes settlement after restart without another delete call. The workspace/session
rows and attempt audit remain available; task merge results remain owner-readable after agent
status access ends at credential revocation. Failed/cancelled held coding attempts and drained
authoritative failed first briefs are recovered too; active blocked work is not disposed.

Delegated PR/merge scope requires the active current attempt, exact generation, owner/project,
role/depth and ancestry. A sibling's or parent's branch confers no authority. Coordination attempts
have no checkout, writer claim or repository authority. A native turn finishing does not complete
its task or release its writer. Task tools and publication completion are separate slices.

Push enrollment runs again after activation commits the delegated attempt proof. The create
operation remains pending at `target_ready` until this idempotent step reconciles; restart
recovery revalidates current authority without recording or replaying another first brief. `PUSH_GATE_ENABLED`
remains false by default. The delegated-role push resolver is integrated with the same live parent authority checks;
Git planning remains independently default off. Read publication follows exact current runtime
observation; push follows active admission. Native send confirms required publications before
turn bytes, resolving the original create identity's enrollment even when revoked. The final
send guard rechecks the current binding and owner session after readiness; committed cancellation
or revocation prevents a prepared turn from starting. Resume and preview admission reuse the
guard's connection for stored-contract and permission reads. The complete credential envelope
is retried unchanged after unknown outcomes. This
source slice claims no live push prevention.

UNKNOWN/DISPATCHING publication retains the branch claim across cancellation, confirmed runtime
deletion, restart and successor requests. Settlement and destructive cleanup retain their durable
operation/source identity while unresolved. Both Git purposes revoke with flags off; independent
UID-conditional cleanup survives binding deletion and outages.

The guard denies new preview admissions after draining; it does not terminate already-open router
connections. Write-enabled handoff remains unqualified until retained-source isolation and preview
fencing are proven by the handoff/runtime slices. Superseded history cannot use the ordinary
immediate-delete archive path; its retention is a separate slice.

## Forked workspaces (issue #311)

A checkpoint fork ([warm-state](warm-state.md#checkpoint-and-fork-api-implemented-experimental),
`POST /checkpoints/{id}/fork`) of a session that is itself a workspace gets its own workspace row
in the same transaction as its session and agent binding
(`server/internal/sessions/checkpoints.go`'s `commitFork`): the manifest (repository, ref, branch,
depth, agent kind, dev settings and development environment) is copied from the source workspace's
row, and the Agent is the one the forked kagent Session actually reports, falling back to the
source's resolved Agent if kagent names none. Unlike an ordinary workspace, whose binding gets its
kagent Session id only once its first `CreateSession` reply lands, the forked binding already has
it (kagent's `ForkSession` returned it) at the moment the workspace row is written, so this
workspace is created with no `workspace_ops` create operation: there is nothing to retry and
nothing to replay. See [sessions](sessions.md#forked-sessions-issue-311) for what is deliberately
not copied (an MCP credential) and why.

Because the row is ordinary once written — a `workspaces` row joined to an `agent_bindings` row
that already names a kagent Session — every lifecycle path above applies unchanged: `GET
/workspaces/{id}`, refresh, sleep and wake, and delete. In particular, deleting or archiving
a fork's workspace goes through the same reconciler-rule delete op as any other workspace (a
durable `workspace_ops` intent row, written before any kagent call): `doDelete` finds the Session
already bound and calls kagent's `DeleteSession` directly, with no create to replay first, which
releases kagent's `pinned_checkpoint_id` on the checkpoint the fork came from. A fork of a session
that is not a workspace (a plain `/agents` session, or the main thread) gets no workspace row:
there is no manifest to copy, and nothing in this change invents one.

Tested against the fake kagent only: a fork of a workspace gets the workspace row with the copied
manifest and no create operation, and deleting it calls kagent's `DeleteSession` and removes the
rows. Not measured against a live kagent; the owner verifies this on `mainloop-dev` separately
(see warm-state.md's live fork measurement).
