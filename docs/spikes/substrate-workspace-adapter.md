# Spike: Substrate as Mainloop's Kubernetes workspace runtime

Status: local spike, not a product feature. Adapter code lives in
`backend/src/mainloop/runtime/substrate.py` and `workspace_adapter.py`; the actor manifest
lives in `spikes/substrate-workspace-adapter/`. See `docs/spikes/k8s-herdr-agents.md` for the
native-session/Herdr spike this one builds on and does not replace.

## What it shows

[Substrate](https://github.com/agent-substrate/substrate) can provide the per-session isolated
compute Mainloop's roadmap calls for ("Workspace platform"), while Mainloop stays the durable
owner of the session<->actor mapping, delivery, and audit state. A Mainloop-authored
`ActorTemplate` (Herdr + `agentctl`, the same image contents as the Herdr spike) runs as a
Substrate actor instead of a fixed StatefulSet pod, and `backend/src/mainloop/runtime/substrate.py`
drives its lifecycle through the real `kubectl ate` control-plane CLI.

## Real versus stand-in

| Layer                                                                                                                                                                                                                 | Status                                                                                                                                                                                             |
| --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| kind cluster `substrate-preview`, pinned Substrate `cdac9baef8...` (ate-system + agentgateway dataplane)                                                                                                              | Real                                                                                                                                                                                               |
| `mainloop-workspace` WorkerPool + ActorTemplate, actor create/get/resume/suspend/revert/delete                                                                                                                        | Real, driven through `backend/src/mainloop/runtime/substrate.py`'s actual code (not a separate probe script's own CLI calls)                                                                       |
| `preview-gate` WorkerPool + ActorTemplate: real Herdr server, real Vite dev server, real NGINX header-proxy, real browser (`agent-browser`), real WebSocket HMR                                                       | Real                                                                                                                                                                                               |
| `dev-service-gate` WorkerPool + ActorTemplate: real `psql`, real external `postgres:16-alpine` StatefulSet, real `EgressPolicy` (CIDR rule, created via a small gRPC tool since `kubectl-ate` has no CLI verb for it) | Real                                                                                                                                                                                               |
| Herdr + `agentctl` inside the actor images                                                                                                                                                                            | Real (same image contents as `spikes/k8s-herdr-agents`), without the real Claude/Codex CLIs                                                                                                        |
| File edits and shell commands run inside actors (a generic `herdr pane run` shim, not a native agent's own Bash tool)                                                                                                 | Stand-in -- see "Why not a real agent" below                                                                                                                                                       |
| `live-agent-gate` WorkerPool + ActorTemplate: real Claude/Codex CLIs, credential-free boot                                                                                                                            | Phase 2: golden snapshot READY; actor RUNNING; `/healthz` and one suspend/resume proved live. Counter/marker restore and worker-loss revert remain unproved. No native-agent session has been run. |
| Claude/Codex agent processes, credentials                                                                                                                                                                             | Not run in this spike (see "Limits")                                                                                                                                                               |
| `workspace_bindings` durable mapping (Postgres)                                                                                                                                                                       | Fixture/unit-tested only; not exercised against a live backend + database in this run                                                                                                              |

## Why not a real agent for the preview-gate edit (credential-injection gap)

Substrate's pinned commit has no generic secret-injection mechanism equivalent to a Kubernetes
Secret volume/env mount. `ActorTemplate` container env values are literal only (no
`envFrom`/`valueFrom`, and the template is immutable, so baking a token in would also mean
storing it permanently in a control-plane object -- unacceptable under this task's "credentials
by path, never by value" rule). `SystemInfo` volumes are limited to actor metadata and an
allowlisted CA bundle. `CredentialProvider` is an egress-gateway plugin, keyed by actor SPIFFE
identity, that injects a credential into an outbound request; it does not hand a token to the
CLI's local environment or filesystem.

The pinned commit also has experimental static-header injection from a Kubernetes Secret URI
into decrypted outbound requests. It requires Envoy with SDSMint and the experimental
credential-injection flag. Envoy 1.39.1 crashed on this host, while agentgateway does not support
the injection path. The revised Phase 3 uses a Mainloop-owned router NetworkPolicy and a per-actor
shim token instead; credentials are delivered through that closed channel. This keeps the
credential path separate from the unsupported Envoy feature, though actor snapshots will contain
credentials after delivery. The earlier unauthenticated relay is not used. The preview-gate
measurement below instead uses
a generic exec shim (`spikes/substrate-workspace-adapter/image/exec-shim.js`) that pastes text
into a real Herdr shell pane via `herdr pane run` -- a real shell executing a real command, just
not a credentialed agent's own tool call.

## Run it

There is no single demo script yet (unlike `spikes/k8s-herdr-agents/demo.sh`); the commands used
are recorded in the task's proof note. In outline:

```bash
KIND_CLUSTER_NAME=substrate-preview KUBECONFIG=/tmp/substrate-preview-kubeconfig \
  /tmp/substrate-preview-src/hack/create-kind-cluster.sh
KIND_CLUSTER_NAME=substrate-preview KUBECTL_CONTEXT=kind-substrate-preview \
  KUBECONFIG=/tmp/substrate-preview-kubeconfig \
  /tmp/substrate-preview-src/hack/install-ate-kind.sh --deploy-ate-system
KIND_CLUSTER_NAME=substrate-preview KUBECTL_CONTEXT=kind-substrate-preview \
  KUBECONFIG=/tmp/substrate-preview-kubeconfig \
  /tmp/substrate-preview-src/hack/install-ate-kind.sh --deploy-atenet --atenet-dataplane=agentgateway
# build kubectl-ate, build+push an actor image, apply one of:
#   k8s/actor-template.yaml.tmpl            -- mainloop-workspace: Herdr + agentctl
#   k8s/preview-gate-template.yaml.tmpl      -- preview-gate: real Vite dev server + exec shim
#     + k8s/preview-proxy.yaml.tmpl          -- the NGINX ate-target-actor header-proxy in front
#   k8s/dev-service-gate-template.yaml.tmpl -- dev-service-gate: real psql + exec shim
#     + k8s/postgres-target.yaml             -- the external postgres:16-alpine StatefulSet
#     + egress-tool/main.go                  -- creates the actor's EgressPolicy (no CLI verb)
# (WorkerPool via `ko resolve | kubectl apply`, ActorTemplate via `kubectl ate create actor-template -f -`)
```

## Observed behaviour

- The default Envoy-based `atenet-router` crash-looped on this cluster too (matching the prior
  `docs/spikes/../substrate-kind-preview-proof` finding); the `agentgateway` dataplane fixed it.
- A freshly created actor starts `SUSPENDED`, not running -- `create_actor` never implicitly
  starts an actor. An explicit `resume_actor` is required, and it returned `RUNNING` directly
  (no further polling needed) in every observed case.
- Force-deleting an actor's worker pod (`kubectl delete pod ... --grace-period=0 --force`, the
  same technique as the prior proof's "abrupt worker loss" trial) drove the actor to `CRASHED`
  within a few seconds, correctly observed as `WorkspaceBinding.observed_state="unavailable"`
  through `substrate.py`'s real `ActorState` -> `observed_state` mapping.
- `revert_actor` on a `CRASHED` actor returned it to `SUSPENDED` from its last completed (here:
  golden) snapshot; a subsequent `resume_actor` brought it back to `RUNNING`. Nothing in the
  adapter reverts automatically -- `workspace_adapter.revert_workspace` requires
  `acknowledge_loss=True`.
- Actor logs for a resume showed gVisor's `runsc ... restore -image-path ... restore-state`
  path (`"Actor restoring"` / `"Actor restored"`), not a fresh container boot -- consistent with
  the golden snapshot's process state (including the running `herdr` server) being restored
  rather than the entrypoint re-running. This is supporting evidence for gate 5 (native-session
  continuity) but not a full proof: no real agent session was resumed and asked to recall a
  pre-suspend nonce in this run.
- Building this adapter against the real CLI found one bug fixed in the same commit:
  `create_actor` was missing `-o json` and crashed parsing `kubectl ate`'s default table output.

### Preview/HMR gate (gate 3): proved live, three real bugs isolated and fixed

Through the actual intended route (real browser -> NGINX header-proxy -> `atenet-router`
(agentgateway) -> a real Vite dev server in a real actor), with a real WebSocket HMR socket open
the whole time: a real shell write (via the exec-shim's `herdr pane run`, not a purpose-built
`/__edit` endpoint) to `main.js` produced a genuine in-place HMR update -- confirmed by a
`window.__hmrMarker` value set before the edit surviving after it (a full reload would have reset
it) and by the console logging `[vite] hot updated: /main.js`. This held across an explicit
suspend/resume cycle too: content and the marker's own page state survived, and Vite's client
logged `server connection lost. Polling for restart...` during the suspend and reconnected
cleanly on resume, with a further post-resume edit still hot-updating correctly.

Getting there required isolating and fixing three independent, real bugs -- exactly what the
prior Kind preview proof asked for ("isolate ... rather than re-measuring as one blob"):

1. **NGINX's `proxy_pass` defaults to HTTP/1.0 upstream**, which silently breaks `Connection:
Upgrade`. Symptom: `503 upstream call failed: SendRequest: connection closed before message
completed` from `atenet-router`, which looked like a router bug until isolated by testing the
   same header-routed request directly against the router (works) versus through NGINX (fails).
   The Jupyter demo's own `nginx.conf` (the pattern this proxy was copied from) has the same gap.
   Fix: add `proxy_http_version 1.1;`.
2. **A hardcoded `hmr.clientPort` pointed the browser's WebSocket at the actor's internal port
   (80), not the port the browser actually reached the proxy on.** Symptom: `[vite] failed to
connect to websocket (Error: WebSocket closed without opened.)` in the real browser, while a
   raw `curl` WebSocket upgrade against the same actor succeeded (isolating it to the _browser's_
   target URL, not the routing path). Fix: do not set `hmr.clientPort`; let Vite infer it from
   `window.location`, which is correct for same-origin proxying.
3. **A plain shell-redirect truncate-in-place write (`cmd > file`) was never observed by Vite's
   file watcher on this gVisor-sandboxed filesystem, with or without `usePolling`; an atomic
   rename-replace write (`sed -i`, or any editor/tool that writes-then-renames, which is how most
   real editors and Node's own atomic-write helpers behave) was picked up every time.** This was
   isolated by holding the watcher config fixed and varying only the write method. The initial
   hypothesis (inotify does not work under gVisor) was wrong and is corrected here rather than
   left standing: the default inotify-based watch picked up `sed -i` edits fine, with or without
   polling enabled. `usePolling` is kept in the fixture's `vite.config.js` as defense in depth,
   but it was not the actual fix.

None of these three are Substrate bugs in the sense of "broken by Substrate" -- (1) is a gap in
the demo NGINX pattern this repo's own docs show, (2) is a Vite config default that does not
suit a proxied deployment, and (3) is a filesystem-semantics fact worth knowing about (most real
editors already write this way, so it may not affect a real native-agent's edits, which is
exactly why gate 5's live proof matters and was not reached in this run).

### Dev-service gate (gate 4): proved live, including real policy enforcement

A real `postgres:16-alpine` StatefulSet (same image/auth shape as
`k8s/apps/mainloop/overlays/test/postgres-statefulset.yaml`) in its own namespace, reached from a
`dev-service-gate` actor (real `psql`, driven through the same generic exec shim) under an
`EgressPolicy` scoped to exactly the Postgres Service's `/32` ClusterIP.

`kubectl-ate` has **no CLI verb for egress policies** -- confirmed by the pinned checkout's own
`demos/egress/README.md`: `"test-egress.sh creates and resumes the Actor but cannot create its
EgressPolicy (no CLI verb yet)"`. Its own e2e suite calls the gRPC API directly
(`internal/e2e/egresspolicy.go`). This spike does the same:
`spikes/substrate-workspace-adapter/egress-tool/main.go`, a small standalone `main` mirroring
that helper without the `testing.T` dependency (build instructions are in the file's header
comment; it must be built inside a Substrate checkout since it imports `internal/` packages).

**Result**: DNS resolution (bypasses the policy enforcement point entirely -- port 53 is always
allowed), a real `SELECT` query over the actual Postgres wire protocol, and reconnection after an
explicit suspend/resume cycle (a second query, `SELECT 43`, succeeded cleanly post-resume, no
policy re-creation needed -- the policy is attached to the actor, not the connection) all worked
on the first try. Authorization is real, not merely passive: a request to a _different_ Service's
ClusterIP (not covered by the `/32` rule) was cleanly rejected --
`HTTP 403 actor egress policy denied destination` from the egress gateway itself, not a silent
timeout or a security-group-shaped ambiguity. This is a materially better outcome than the prior
Kind preview proof's own external-backend trial (`403 -> 503`, "reconnection after wake was
therefore not proved") -- the difference was using a **CIDR rule** (works for any TCP protocol
per `docs/network-egress.md`'s "CIDR/all policy: dial now" passthrough path) instead of a
**hostname rule** (HTTP/TLS-SNI-specific, and Postgres is neither), which the prior proof's HTTP
`fetch`-based trial did not have reason to distinguish.

## Limits / not attempted in this run

- **Native-session gate, live**: no Claude or Codex session was run. The earlier attempt was
  authorized; tool policy rejected particular actions after the boot-time credential fetch
  received HTTP 403. Phase 1 removed the unauthenticated relay and repaired the harness. Phase 2
  produced a READY golden snapshot, a RUNNING actor, a healthy `/healthz` through the documented
  CONNECT port, and one successful suspend/resume. Counter/marker persistence and worker-loss
  revert were not proved. Before any credential work, a tokenless caller reached `POST /run`
  through the router and executed a harmless command. No existing ingress authorization primitive
  closed this path under the earlier plan, so that credential attempt stopped and Gate 5 remains
  partial/fixture pending the rewritten Phase 3 and Phase 4. The owner approved that plan on
  2026-09-23; no provider Secret was created or read in the earlier run.
- **`workspace_bindings` orchestration functions** (`ensure_workspace`, `resume_workspace`, ...)
  were not exercised against a live Postgres + running backend; only their extracted pure logic
  (`plan_ensure`, `_binding_from_row`, `is_crashed`) is unit tested, and the transport layer
  they call (`SubstrateControl`) is proved live as described above.

### Native-session gate addendum: harness repair and Phase 2 partial live proof

The original live attempt (outside this doc's own commits) created the live-agent-gate
infrastructure and hit a real failure during golden-snapshot creation, reviewed
in `.tasknotes/gate5-review-and-recovery-plan-2026-09-22.md`: the golden actor's entrypoint
fetched a credential from `cred-server` unconditionally at boot, that fetch was denied (`403`),
and the golden actor exited before its snapshot was captured -- `runsc exit 128` on a later
restore attempt is consistent with capturing a process that had already exited. The harness
script driving that attempt (never committed; reviewed from a scratch copy) also applied an
unresolved `ko://` WorkerPool image, never registered the atespace at the control-plane API
(a Kubernetes Namespace of the same name is not an atespace), checked for an existing
ActorTemplate with the atespace embedded in the name rather than the CLI's required `-a` flag,
and printed success from a log line reached before the state it implied was actually confirmed.

Phase 1 (recovery plan step 2) repairs the harness findings and removes the boot-time fetch:

- `entrypoint.sh` no longer fetches a credential or needs network access to reach a running
  state. There is no credential-fetch helper or relay path in the image. Credential delivery
  remains a separate, gated step and is never performed during golden-actor warmup.
- `k8s/live-agent-gate-template.yaml.tmpl` no longer sets `CRED_SERVER` in the (shared,
  immutable) container env, and its ActorTemplate name is now versioned
  (`live-agent-gate-${TEMPLATE_VERSION}`) so a failed golden snapshot is never reused.
- `backend/src/mainloop/runtime/substrate.py` gained `ensure_atespace`/`get_actor_template`/
  `create_actor_template`/`get_eligible_workers`, plus bounded, exception-raising waits for
  golden snapshots, eligible workers, actor state, and the live actor health route. Rerun
  identity reconciliation distinguishes absent, unowned, matching, and diverged actors before
  the harness creates or resumes one.
- `backend/scripts/gate5_setup.py` registers the atespace, resolves the WorkerPool image with
  `ko resolve` from the verified pinned checkout, waits for an eligible worker and a golden
  snapshot, binds reruns to persisted cluster/template/actor identity, then confirms health
  through the actor route before applying egress policy.
- `egress-tool/main.go` fails closed: exactly one of `--deny-all`, `--cidr`, or `--allow-all`
  must be explicit.

Phase 2 used pinned Substrate `cdac9baef81dd319b46086d695266e6161e9e592`, a fresh
`kind-substrate-preview` cluster, agentgateway, and image
`localhost:5001/live-agent-gate@sha256:a5ffadbede22382732067873c0239fa67a757f3bbde38b838942a5bfa20fbeda`.
ActorTemplate `live-agent-gate-v1` (UID `b16cf365-0856-4623-87a9-479112767d46`) reached a READY
golden snapshot; actor `claude-gate5` (UID `cbb4d70c-4ba9-4ce2-af08-20f866a11866`) reached
RUNNING. The harness health probe timed out because it forwarded the router's HTTP port 80 while
the documented non-default-port CONNECT listener is 8081. A direct `/healthz` through 8081
returned 200. Suspend produced snapshot
`gs://ate-snapshots/live-agent-gate/atespaces/live-agent-gate/actors/cbb4d70c-4ba9-4ce2-af08-20f866a11866/snapshots/fadab73f-5f8a-4346-8cfb-af67c85c893d`; resume returned the same actor UID to RUNNING, the health route returned 200, and logs showed gVisor's `restore -image-path` path. The marker/counter and process PID were not measured.

The first setup pass exposed one additional pinned-CLI result shape: a valid zero-match worker
query serializes as `{}`. The adapter now treats only an empty object as zero workers and still
rejects non-empty objects missing `workers`. The 9 worker-discovery regression tests pass. The
unchanged rerun did not create a duplicate actor, but waited for spare worker capacity before
checking the persisted actor UID and failed because the single worker was already occupied by
`claude-gate5`. The harness's own health check also used router Service port 80 instead of its
CONNECT listener on 8081; direct `/healthz` through 8081 worked.

Before any provider credential work, a separate tokenless Pod in `default` POSTed a harmless
command through `atenet-router:8081` to `actor-upstream:8090/run` with the actor-routing header;
the shim returned `200 OK`. The pinned router documentation says ingress treats request headers
as unauthenticated input, while ingress authorization is future roadmap work. This is not
closable with an existing Substrate actor-ingress primitive in this configuration. Under the
earlier plan this meant fallback C; the 2026-09-23 owner decision supersedes that fallback with a
Mainloop-owned NetworkPolicy, per-actor shim token, and closed-channel credential delivery. No
provider credential Secret was created or read. Phase 4 was not attempted in that run.

After deny-all egress was applied, operator `/run` calls did not complete; a pane read confirmed
the baseline counter/marker command had not run. Therefore the suspend/resume result proves the
actor and Herdr health path restored, but does not establish counter/marker persistence. The
force-delete-worker/revert portion of the lane was not attempted without those markers.

The reviewing session reports the full runtime suite passed 189/189 outside its restricted
sandbox before the Phase 2 empty-result correction. After that correction, the focused
Substrate, workspace-adapter, contract, and setup suites passed 91/91. The credential-free proof
is partial live evidence; the native-session capability remains partial/fixture, and the full
counter/marker and worker-loss checks are unproved.

## CapabilityResult

| Capability                   | State   | Scope   | Evidence and limit                                                                                                                                                                                       |
| ---------------------------- | ------- | ------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `workspace_adapter_contract` | partial | fixture | `SubstrateControl` is exercised live; `workspace_bindings` orchestration is fixture-tested, not run against a live backend and Postgres.                                                                 |
| `substrate_actor_lifecycle`  | partial | live    | READY golden creation, actor RUNNING, health, suspend, and resume ran live; marker persistence was not proved, and the exact rerun stopped at the occupied single worker before identity reconciliation. |
| `preview_hmr`                | proved  | live    | Real Vite HMR socket and edits through the actor route; survives suspend/resume.                                                                                                                         |
| `dev_service_postgres`       | proved  | live    | Real Postgres query, narrow allow rule, denied destination, and reconnect after wake.                                                                                                                    |
| `native_session_continuity`  | partial | fixture | No provider session ran. A tokenless unrelated Pod reached `POST /run` through the router; no existing Substrate ingress authorization primitive closed this path.                                       |
| `failure_recovery`           | partial | live    | Actor CRASHED/revert mechanics were proved in the earlier live lane; backend restart with a durable `recorded` attempt is covered by a fake-backed contract test, not live Postgres delivery.            |

## Recommendation status at the Phase 2b checkpoint

The earlier recommendation to defer native sessions is superseded by the owner's 2026-09-23
decision. Current live evidence still does not prove native-session support. Phase 3 must close
the router ingress boundary and deliver credentials through that channel; Phase 4 must then prove
Claude and Codex continuity. The final adopt/defer recommendation remains open until those phases
finish or stop on a named condition.

## Cleanup

Phase 0 removed the approved `cred-server` relay resources and both named Secrets, then deleted
the owner-confirmed failed-trial `kind` cluster and its `kind-registry`. The fresh Phase 2
`kind-substrate-preview` cluster, its `kind-registry`, and the exact local image tag built by
this run were deleted after evidence capture. Final inventory showed only `mainloop-test` and
`mainloop-test-control-plane`; the run-specific image tag was absent and the unrelated `latest`
tag was preserved. Root disk had 21 GiB available and RAM had 9.5 GiB available. `mainloop-test`
was outside the cleanup scope. The owner handles rotation of the Claude and Codex credentials
previously served by the relay.
