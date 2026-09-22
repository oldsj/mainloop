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

| Layer                                                                                                                                                                                                                 | Status                                                                                                                       |
| --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------- |
| kind cluster `substrate-preview`, pinned Substrate `cdac9baef8...` (ate-system + agentgateway dataplane)                                                                                                              | Real                                                                                                                         |
| `mainloop-workspace` WorkerPool + ActorTemplate, actor create/get/resume/suspend/revert/delete                                                                                                                        | Real, driven through `backend/src/mainloop/runtime/substrate.py`'s actual code (not a separate probe script's own CLI calls) |
| `preview-gate` WorkerPool + ActorTemplate: real Herdr server, real Vite dev server, real NGINX header-proxy, real browser (`agent-browser`), real WebSocket HMR                                                       | Real                                                                                                                         |
| `dev-service-gate` WorkerPool + ActorTemplate: real `psql`, real external `postgres:16-alpine` StatefulSet, real `EgressPolicy` (CIDR rule, created via a small gRPC tool since `kubectl-ate` has no CLI verb for it) | Real                                                                                                                         |
| Herdr + `agentctl` inside the actor images                                                                                                                                                                            | Real (same image contents as `spikes/k8s-herdr-agents`), without the real Claude/Codex CLIs                                  |
| File edits and shell commands run inside actors (a generic `herdr pane run` shim, not a native agent's own Bash tool)                                                                                                 | Stand-in -- see "Why not a real agent" below                                                                                 |
| `live-agent-gate` WorkerPool + ActorTemplate: real Claude/Codex CLIs, credential-free boot                                                                                                                            | Phase 1 harness repair complete; credential-free lifecycle rerun is pending. No native-agent session has been run.           |
| Claude/Codex agent processes, credentials                                                                                                                                                                             | Not run in this spike (see "Limits")                                                                                         |
| `workspace_bindings` durable mapping (Postgres)                                                                                                                                                                       | Fixture/unit-tested only; not exercised against a live backend + database in this run                                        |

## Why not a real agent for the preview-gate edit (credential-injection gap)

Substrate's pinned commit has no generic secret-injection mechanism equivalent to a Kubernetes
Secret volume/env mount. `ActorTemplate` container env values are literal only (no
`envFrom`/`valueFrom`, and the template is immutable, so baking a token in would also mean
storing it permanently in a control-plane object -- unacceptable under this task's "credentials
by path, never by value" rule). The only credential-shaped primitives are `SystemInfo` volumes
(`actorMetadata`: the actor's own name/atespace/uid; `trustBundle`: a named, allowlisted CA
bundle -- today only `egress-mitm.ate.dev`) and `pkg/proto/credproviderpb` (`CredentialProvider`,
a plugin the _egress gateway_ calls to inject a credential into an actor's _outbound_ request,
keyed by the actor's SPIFFE identity -- not a way to hand the actor's own process a local file or
env var it can read directly, which is what the Claude Code / Codex CLIs need). A real
native-agent proof (gate 5) therefore needs either an unsafe workaround or new plumbing (e.g. an
authenticated credential-relay using the `MintActorJWT`/`MintActorCertificate` RPCs already in
`ateapipb.Control`), out of scope for this spike. The preview-gate measurement below instead uses
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

- **Native-session gate, live**: a prior owner-authorized attempt reached golden-snapshot
  creation but failed when the boot-time credential fetch received HTTP 403. The earlier
  description that the run was declined by a safety classifier was inaccurate: the run was
  authorized, while tool policy rejected particular actions. Phase 1 removes the boot-time
  fetch and repairs the harness; the credential-free lifecycle proof is pending. No Claude or
  Codex session has been run. The old relay manifest and fetch helper have been removed. See
  the finish plan for the bounded lifecycle proof and the separately gated Claude-only
  credential-boundary attempt.
- **`workspace_bindings` orchestration functions** (`ensure_workspace`, `resume_workspace`, ...)
  were not exercised against a live Postgres + running backend; only their extracted pure logic
  (`plan_ensure`, `_binding_from_row`, `is_crashed`) is unit tested, and the transport layer
  they call (`SubstrateControl`) is proved live as described above.

### Native-session gate addendum: a later live attempt failed at golden creation, now repaired

After the run above, a separate live attempt (outside this doc's own commits) did create the
live-agent-gate infrastructure and hit a real failure during golden-snapshot creation, reviewed
in `.tasknotes/gate5-review-and-recovery-plan-2026-09-22.md`: the golden actor's entrypoint
fetched a credential from `cred-server` unconditionally at boot, that fetch was denied (`403`),
and the golden actor exited before its snapshot was captured -- `runsc exit 128` on a later
restore attempt is consistent with capturing a process that had already exited. The harness
script driving that attempt (never committed; reviewed from a scratch copy) also applied an
unresolved `ko://` WorkerPool image, never registered the atespace at the control-plane API
(a Kubernetes Namespace of the same name is not an atespace), checked for an existing
ActorTemplate with the atespace embedded in the name rather than the CLI's required `-a` flag,
and printed success from a log line reached before the state it implied was actually confirmed.

This Phase 1 change (recovery plan step 2) repairs those bugs and removes the root cause:

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

**Scope of this repair**: code and fixture tests only. The focused Substrate, setup, and contract
tests pass; the owner reports the full runtime suite passes 189/189 outside the restricted
sandbox. No fresh lifecycle measurement is claimed here: `gate5_setup.py` has not yet been run
against a fresh `kind-substrate-preview` cluster. Phase 2 must confirm the golden snapshot and
restored readiness before this addendum can report a live result.

## Cleanup

Each of the three cluster lanes in this spike (adapter/CRASHED, preview-gate, dev-service-gate)
deleted its own test actors and target resources, then the `substrate-preview` cluster and its
`kind-registry` (`hack/delete-kind-cluster.sh`), and pruned the locally built, unpushed-elsewhere
Docker images. Final `kind get clusters` / `docker ps` showed only `mainloop-test` /
`mainloop-test-control-plane` after every lane. Root disk stayed in the 19-27G-free range
throughout (above the plan's 8G in-flight-trial abort threshold at all times); available RAM
stayed above 8G. No Mainloop repository files outside this branch's own commits were changed.
