# Spike: Substrate as Mainloop's Kubernetes workspace runtime

Status: local spike, not a product feature. Adapter code lives in
`backend/src/mainloop/runtime/substrate.py` and `workspace_adapter.py`; the actor manifest
lives in `spikes/substrate-workspace-adapter/`. The earlier live proof used a Herdr-backed
actor; the target described by this closeout has no Herdr server or terminal manager in the
actor and invokes native CLIs headlessly once per turn. See
`docs/spikes/k8s-herdr-agents.md` for the historical native-session/Herdr spike.

## Current status — Phase 5 closeout (2026-09-23)

The Round 3 and Phase 4 runs measured Substrate actors, Cilium-enforced router ingress,
per-actor shim tokens, Envoy hostname egress, and native sessions for both Claude Code and
Codex in the earlier Herdr-backed actor. Both agents completed a native turn and recalled a
nonce in the same session after suspend/resume. Measured suspend/resume times were 393/408 ms
for Claude and 573/789 ms for Codex. Those results do not prove the headless per-turn actor
model. Its rebuilt image is **not yet live-proved**. The Codex file marker was not confirmed in
the recorded run; its follow-up result, Claude recall after worker loss, and Claude history
after revert remain pending in the cluster lane.

The earlier Codex API HTTP 503 came from Envoy selecting unreachable IPv6 upstream addresses
when its DNS caches used `ALL`; `V4_PREFERRED` corrected that path. A separate ChatGPT SAN
failure was traced by reviewer-provided evidence to cross-SNI upstream TLS session reuse in
Envoy 1.39.1. After applying both settings, a credential-free probe returned the expected API
401 and ChatGPT 200 with no TLS verification failures, then the native Codex turn and recall
completed. Native-session support behind the Phase 3 boundary is proved for both agents in the
earlier actor design. The requested headless actor design remains unproved, the broader
continuity gate is partial, and production adoption is deferred pending live proof of its
rebuilt image and the requirements below.

## What it shows

[Substrate](https://github.com/agent-substrate/substrate) can provide the per-session isolated
compute Mainloop's roadmap calls for ("Workspace platform"), while Mainloop stays the durable
owner of session-to-actor mapping, delivery, and audit state. The target actor has no Herdr
server or terminal manager; Mainloop delivers each turn to a headless native CLI process. The
actor holds files between turns, with no attachable TUI; callers watch progress through streamed
events. The rebuilt image for that design is not yet live-proved. Existing live lifecycle and
native-session results below came from the earlier Herdr-backed actor and are historical evidence
for Substrate and the Phase 3 boundary, not proof of the target image.

The headless shim exposes authenticated `POST /turn` and `GET /turn/:id` endpoints. Prompts are
sent to the native CLI on stdin; the shim stores bounded JSONL events per turn and reports the
native session/thread id and final message. It permits one in-flight turn per agent and returns
409 instead of queueing. `POST /run` starts a shell command with a bounded timeout and actor-local
output file; `GET /run/:id` reports its status and bounded output. `/healthz` and `/readyz` check
only the shim and workspace. The actor image and these routes still need live proof.

## Substrate runtime

`WORKSPACE_RUNTIME=substrate` selects the native-session transport; `herdr` remains the default.
`SUBSTRATE_ROUTER_ADDRESS` configures the HTTP CONNECT listener. `SUBSTRATE_ACTOR_BINDINGS` is a
JSON object keyed by `claude` and `codex`; each entry supplies `atespace`, `actor`, and
`shim_token_secret_name`. `SUBSTRATE_SHIM_SECRET_NAMESPACE` selects the Secret namespace and
defaults to `mainloop-control`. The backend reads the Secret's `token` key there and keeps the
value out of logs and config. Actor names and Secret names stay in deployment config; Mainloop
attaches only to the named, pre-created actor. A 401 refreshes the cached token and retries
read/status operations once; a rejected `POST /turn` clears the cache and is never replayed.

The adapter routes turns and status through the authenticated shim, verifies the selected CLI
credential is installed before starting a session, then mirrors replies and completion from native
session journals. The shim's bounded `GET /journal` endpoint pages Claude transcripts and Codex
rollout files. This is fixture-backed transport behavior; it does not add live-proof claims for the
headless image or its current actors.

## Earlier real-versus-stand-in inventory (before Round 3)

| Layer                                                                                                                                                                                                                 | Status                                                                                                                                                                   |
| --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| kind cluster `substrate-preview`, pinned Substrate `cdac9baef8...` (ate-system + agentgateway dataplane)                                                                                                              | Real                                                                                                                                                                     |
| `mainloop-workspace` WorkerPool + ActorTemplate, actor create/get/resume/suspend/revert/delete                                                                                                                        | Real, driven through `backend/src/mainloop/runtime/substrate.py`'s actual code (not a separate probe script's own CLI calls)                                             |
| `preview-gate` WorkerPool + ActorTemplate: real Herdr server, real Vite dev server, real NGINX header-proxy, real browser (`agent-browser`), real WebSocket HMR                                                       | Real                                                                                                                                                                     |
| `dev-service-gate` WorkerPool + ActorTemplate: real `psql`, real external `postgres:16-alpine` StatefulSet, real `EgressPolicy` (CIDR rule, created via a small gRPC tool since `kubectl-ate` has no CLI verb for it) | Real                                                                                                                                                                     |
| Herdr + `agentctl` inside the actor images                                                                                                                                                                            | Real (same image contents as `spikes/k8s-herdr-agents`), without the real Claude/Codex CLIs                                                                              |
| File edits and shell commands run inside actors (a generic `herdr pane run` shim, not a native agent's own Bash tool)                                                                                                 | Stand-in -- see "Why not a real agent" below                                                                                                                             |
| `live-agent-gate` WorkerPool + ActorTemplate: real Claude/Codex CLIs, credential-free boot                                                                                                                            | Golden snapshot READY and actor RUNNING proved live. A later credential-free preview actor hit the pinned gVisor restore error described below. No provider session ran. |
| Claude/Codex agent processes, credentials                                                                                                                                                                             | Not run; the 2026-09-23 live lane stopped before credential work (see "Limits")                                                                                          |
| `workspace_bindings` durable mapping (Postgres)                                                                                                                                                                       | Fixture/unit-tested only; not exercised against a live backend + database in this run                                                                                    |

## Preview-gate edit path and Round 3 credential boundary

Substrate's pinned commit has no generic secret-injection mechanism equivalent to a Kubernetes
Secret volume/env mount. `ActorTemplate` container env values are literal only (no
`envFrom`/`valueFrom`, and the template is immutable, so baking a token in would also mean
storing it permanently in a control-plane object -- unacceptable under this task's "credentials
by path, never by value" rule). `SystemInfo` volumes are limited to actor metadata and an
allowlisted CA bundle. `CredentialProvider` is an egress-gateway plugin, keyed by actor SPIFFE
identity, that injects a credential into an outbound request; it does not hand a token to the
CLI's local environment or filesystem.

The pinned commit also has experimental static-header injection from a Kubernetes Secret URI
into decrypted outbound requests. HTTPS hostname rules require the Envoy sdsmint overlay and
`--experimental-egress-credential-injection`; the plain Envoy overlay has no MITM egress, and
agentgateway does not implement this injection path. An earlier Envoy 1.39.1 router install
crashed, while the later Round 3 Cilium setup ran Envoy egress with sdsmint and the
actor-bound credential provider. Claude's credential was injected on the upstream leg and stayed
out of actor snapshots. Codex instead received `auth.json` through the authenticated shim because
Codex refreshes that file locally; its actor snapshots therefore contain that credential. Moving
Codex credentials out of snapshots remains a production requirement. Neither credential value
was logged or copied into a golden snapshot, and the old unauthenticated relay was not used.

The Round 3 egress run also found that `dns_lookup_family: ALL` selected unreachable IPv6
addresses on the IPv4-only Kind node. Setting the five egress DNS caches to `V4_PREFERRED`
allowed the credential-free API and ChatGPT probes to reach upstreams. A separate
reviewer-provided diagnosis tied the earlier ChatGPT SAN failures to cross-SNI TLS session
resumption; setting `max_session_keys: 0` on the four approved upstream TLS clusters preserved
SAN verification and the later probe passed. These are Envoy-specific settings; they do not make
agentgateway hostname enforcement safe.

The preview/HMR edit itself still uses a generic exec shim
(`spikes/substrate-workspace-adapter/image/exec-shim.js`) that pastes text into a real Herdr shell
pane via `herdr pane run` -- a real shell executing a real command, but not a native agent's own
Bash tool.

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

## Historical limits from the earlier Phase 2 checkpoint

- **Native-session gate, live at that checkpoint**: no Claude or Codex session ran then. The
  owner authorized the work; a tool policy rejected particular actions after the old relay
  returned HTTP 403. The relay was removed. The first Phase 3 preview restore error was later
  diagnosed as a dead app from the old image and missing readiness probe, then corrected with a
  new image and template. Those earlier limits were superseded by the Round 3 and Phase 4 results
  above.
- Preview/HMR under the router policy and actor-to-actor access were unverified at the earlier
  checkpoint. In the current finish run, preview HMR passed, and an unrelated actor's CONNECT
  to the shim was rejected at the actor egress gateway before reaching the router. The
  per-actor shim token passed live checks, including suspend/resume persistence.
- Phase 2's counter/marker persistence and worker-loss revert were not proved in that original
  run. Later Phase 4 checks proved Claude's file marker across suspend/resume; the three pending
  Phase 4 checks are listed in the current results below.
- **`workspace_bindings` orchestration functions** (`ensure_workspace`, `resume_workspace`, ...)
  were not exercised against a live Postgres + running backend; only their extracted pure logic
  (`plan_ensure`, `_binding_from_row`, `is_crashed`) is unit tested, and the transport layer
  they call (`SubstrateControl`) is proved live as described above.

### Native-session gate addendum: harness repair and Phase 2 partial live proof

The original live attempt (outside this doc's own commits) created the live-agent-gate
infrastructure and hit a real failure during golden-snapshot creation, reviewed
in `.tasknotes/gate5-review-and-recovery-plan-2026-09-22.md`: the golden actor's entrypoint
fetched a credential from `cred-server` unconditionally at boot, that fetch was denied (`403`),
and the golden actor exited before its snapshot was captured. A later restore returned
`runsc exit 128`; the original evidence did not establish that the denied fetch caused that
restore error. The harness script driving that attempt (never committed; reviewed from a scratch copy) also applied an
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
rejects non-empty objects missing `workers`. The affected correction run passed 30 tests: 9
worker-discovery tests and 21 contract tests. The live Phase 2 run reached a READY golden and a
RUNNING actor, returned `/healthz` 200 through the CONNECT listener on port 8081, and suspended
then resumed the same actor to RUNNING with `/healthz` 200. Its unchanged rerun did not create a
duplicate actor, but waited for spare worker capacity before checking the persisted actor UID and
failed because the single worker was already occupied. The marker/counter and process PID were
not measured in that run.

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
sandbox before the Phase 2 empty-result correction. The previous Phase 2 run's operator calls
after deny-all egress did not complete. In the Phase 3 run, an in-cluster control namespace POST
to `/run` returned 200 after deny-all egress was attached, so the ingress response path worked
for that trusted request. The focused setup, Substrate, and contract suites now pass 85/85.
The credential-free proof is partial live evidence; native-session continuity remains partial,
and full counter/marker and worker-loss checks are unproved.

### Phase 2b — rerun and router corrections

The harness now forwards to the router CONNECT listener on Service port 8081, checks live actor
identity before waiting for worker capacity, and handles the pinned CLI's `{}` zero-worker result.
The focused 63-test setup/Substrate suite passed before the Phase 3 code; the latest setup,
Substrate, and contract run passed 85/85. Phase 2b was committed locally as `3ca780a`.
The in-sandbox `unittest discover` process IDs 2293648 and 2293673 were still alive but invisible
to the sandbox check; the reviewing session killed them on 2026-09-23. The full suite result
189/189 is attributed to that reviewing session.

### Phase 3–4 finish run — router boundary, preview correction, and egress stop

One fresh `kind-substrate-preview` cluster ran Kind v0.33.0, node image v1.37.0, enforcing
kindnet `v20260820`, and pinned Substrate
`cdac9baef81dd319b46086d695266e6161e9e592` with the agentgateway dataplane. Phase 3a passed:
the client reached an HTTP probe before policy, was blocked by deny-all ingress, then reached it
after a namespace-scoped allow.

The tracked `k8s/router-ingress-policy.yaml` selects router Pods by `app=atenet-router`. It
allows `mainloop-control` on router control ports, the preview proxy on HTTP only, and
`otel-system` on stats port 15020. NetworkPolicy ports target Pod ports; Service port 80 maps
to router container port 8080. A POST to `/run` through port 8081 returned 200 from
`mainloop-control` and timed out from an unrelated Pod in `default`.

The corrected `preview-gate-v2` template used the rebuilt image from
`spikes/substrate-workspace-adapter/image/`, an explicit working directory, and `readyz`. The
live Vite page passed HMR through the NGINX preview proxy: editing `main.js` changed the page
heading in place, preserved the browser marker and time origin, and produced a successful HMR
WebSocket upgrade. An unrelated preview actor's CONNECT attempt to
`live-agent-gate/claude-gate5:8090` returned 405 at the actor egress gateway before reaching the
router. The request was rejected, though that result is an egress-gateway denial rather than a
router NetworkPolicy denial.

The first immutable preview template remains abandoned. Its worker logged `npm error ENOENT`
for `/package.json` before golden checkpoint. `savedMFOwners=[_pause:/]` records the surviving
pause container after the application exited; the template lacked `readyz`, so the dead app was
snapshotted. This is a harness/image/readiness error, not a gVisor restore blocker. Record it as
a candidate upstream issue because Substrate checkpointed an exited container without a clear
error. The corrected template name is `preview-gate-v2`.

On `live-agent-gate/claude-gate5` (actor UID
`6125c129-7312-453b-aea5-a2fae6745c62`), the live shim-token test passed: missing and wrong
tokens returned 401, the correct token authorized `/run`, a second token install returned 409,
and the token continued to authorize requests after suspend/resume with the same actor UID. A
deny-all control check initially returned `/healthz` 200 and authenticated `/run` 200 with the
command marker observed. After a later restore, `/healthz` intermittently timed out at the shim's
serial Herdr status/pane-read check, while direct `herdr status server` and authenticated
`/read` succeeded. The WorkerPool remained Ready; the failure was located at the shim health
handler, not the router or NetworkPolicy.

Provider discovery without credentials exited early (Claude rc 1, Codex rc 2) and emitted no
hostnames. Since those were not CLI-measured destinations, the initial bounded check used
provider/API/auth host candidates, informed by [Claude Code's network requirements](https://code.claude.com/docs/en/corporate-proxy),
the [Codex ChatGPT sign-in flow](https://developers.openai.com/codex/auth), and the Codex file's
`chatgpt` auth mode:
`api.anthropic.com`, `platform.claude.com`, `api.openai.com`, `auth.openai.com`, and
`chatgpt.com`. The live Substrate API confirmed exactly one hostname rule with those entries;
its policy cache is 10 seconds. HTTPS to those hosts reached their public services, but an
unlisted HTTPS request to `example.com` also returned 200. This fails Phase 3d's required
unlisted-host 403 and shows hostname-only egress is not enforced on this agentgateway path.
The actor was restored to a zero-rule deny-all EgressPolicy immediately afterward. No provider
Secret was created or read, no credential was delivered, and no Claude or Codex session was
started. Phase 4 and all snapshot-revert tests were not attempted.

The restore diagnosis is clarified in the Phase 3 stop-condition section of the finish plan.
The Codex sandbox could not see the stalled `unittest discover` processes 2293648 and 2293673;
the reviewing session killed both on 2026-09-23. The full runtime result 189/189 is attributed
to that reviewing session, not to a sandbox process killed by this agent.

## Historical CapabilityResult (before Round 3)

| Capability                   | State   | Scope      | Evidence and limit                                                                                                                                |
| ---------------------------- | ------- | ---------- | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| `workspace_adapter_contract` | partial | fixture    | `SubstrateControl` is exercised live; `workspace_bindings` orchestration is fixture-tested, not run against a live backend and Postgres.          |
| `substrate_actor_lifecycle`  | partial | live       | READY golden creation and actor RUNNING proved live; full marker and worker-loss recovery remain unproved.                                        |
| `preview_hmr`                | proved  | live       | Real Vite HMR edit and WebSocket upgrade succeeded through the preview proxy.                                                                     |
| `dev_service_postgres`       | proved  | live       | Real Postgres query, narrow allow rule, denied destination, and reconnect after wake.                                                             |
| `phase3a_networkpolicy`      | proved  | live       | Kindnet blocked and then allowed the in-cluster HTTP probe according to NetworkPolicy.                                                            |
| `router_ingress_boundary`    | proved  | live       | `default` was blocked; `mainloop-control` was admitted; unrelated actor CONNECT to the shim was rejected at actor egress; preview traffic passed. |
| `shim_token_auth`            | proved  | live       | 401/409 behavior, successful bearer use, and token persistence across suspend/resume passed on the final actor.                                   |
| `provider_egress`            | failed  | live       | Exact hostname policy was confirmed, but HTTPS to unlisted `example.com` returned 200 through agentgateway. Actor returned to deny-all.           |
| `credential_delivery`        | unknown | unverified | No provider Secret was created or read; credentials were not delivered.                                                                           |
| `native_session_continuity`  | partial | unverified | Phase 3d failed before credential delivery; no native session was run.                                                                            |
| `failure_recovery`           | partial | live       | Earlier CRASHED/revert mechanics were proved; worker-loss recovery and the backend-restart `recorded` case remain unproved live.                  |

## Historical recommendation (superseded by the Round 3/Phase 4 result)

The corrected preview image resolves the earlier `npm ENOENT`/dead-checkpoint harness error;
that result does not justify deferring native-session adoption. The current agentgateway run
found a separate security blocker: a hostname-only EgressPolicy allowed HTTPS to an unlisted
host. Do not deliver provider credentials or promote native sessions to production until
hostname enforcement is verified on a supported dataplane or another host-aware egress boundary
is added. Production also needs a GitOps-managed router NetworkPolicy on an enforcing CNI,
shim-token issuance by the real Mainloop backend, and snapshot-bucket access controls. The
credential-bearing snapshot trade-off remains open because no credential entered an actor.

## Historical cleanup note (superseded by the Phase 4 owner-review hold)

Phase 0 removed the approved relay resources and both named Secrets, then deleted the
owner-confirmed failed-trial cluster. Per the owner's 2026-09-23 instruction, cleanup of the
fresh `kind-substrate-preview` cluster and its registry is stopped; both remain up for review.
The actor's EgressPolicy is zero-rule deny-all. `mainloop-test` remains outside scope. No
provider Secret or credential-bearing snapshot was created. Current Kind/Docker inventory and
disk headroom are recorded in the task proof note. The owner handles rotation of credentials
previously served by the removed relay.

## Current CapabilityResult — gates 1–6 and Phase 4

| Gate / capability                               | State   | Scope      | Evidence and limit                                                                                                                                                                                                                                                                    |
| ----------------------------------------------- | ------- | ---------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Gate 1 — native contract                        | proved  | fixture    | Record transitions, ownership fencing, and reconciliation are exercised with `ContractStore` fakes; this is not a database-backed restart proof.                                                                                                                                      |
| Gate 2 — workspace binding adapter              | partial | fixture    | Planning and row-mapping logic use fakes; `workspace_bindings` orchestration was not run against live Postgres and a running backend.                                                                                                                                                 |
| Gate 2 — Substrate control adapter              | proved  | live       | Actor lifecycle operations used the real `kubectl ate` adapter and the Cilium preview cluster.                                                                                                                                                                                        |
| Gate 3 — preview/HMR                            | proved  | live       | Real Vite edits updated the open browser session over WebSocket, including across suspend/resume.                                                                                                                                                                                     |
| Gate 4 — dev-service access                     | proved  | live       | Real PostgreSQL query, narrow CIDR policy, denied destination, and reconnect after wake passed.                                                                                                                                                                                       |
| Gate 5 — native sessions in prior actor design  | proved  | live       | Claude and Codex each completed a native turn and recalled a nonce after suspend/resume in the same Herdr-backed actor. Suspend/resume measured 393/408 ms for Claude and 573/789 ms for Codex.                                                                                       |
| Gate 5 — headless per-turn CLI actor            | unknown | unverified | Target design has no Herdr server or terminal manager in the actor. Its rebuilt image is not yet live-proved; the earlier Phase 4 measurements do not establish this gate.                                                                                                            |
| Gate 5 — complete continuity                    | partial | live       | In the earlier actor design, the Codex file marker was not confirmed. Claude recall after worker loss, Claude history after revert, and the Codex marker follow-up remain pending.                                                                                                    |
| Gate 6 — actor failure recovery                 | partial | live       | CRASHED-to-revert-to-resume and replacement-worker restore were measured; native recall after worker loss and history after revert are not established.                                                                                                                               |
| Phase 4 — Claude native session (prior actor)   | proved  | live       | In the earlier Herdr-backed actor, Claude Code completed a turn and same-session nonce recall after suspend/resume; the post-worker-loss and post-revert history checks remain pending.                                                                                               |
| Phase 4 — Codex native session (prior actor)    | partial | live       | In the earlier Herdr-backed actor, Codex CLI completed a turn and same-session nonce recall after suspend/resume; file-marker continuity remains pending.                                                                                                                             |
| `backend_restart_delivery_reconciliation`       | proved  | fixture    | A new fake-backed test reloads a persisted message and `recorded` attempt into a fresh `ContractStore`; retry is blocked until `not_delivered` evidence, and the same payload reference remains pending. It does not exercise Postgres, a transport, or the production delivery loop. |
| `router_ingress_boundary` and `shim_token_auth` | proved  | live       | The unrelated namespace was denied, control-namespace access succeeded, and missing/wrong/correct token plus one-time install and suspend/resume checks passed.                                                                                                                       |
| `provider_hostname_egress`                      | proved  | live       | Envoy/Cilium actor policies denied unlisted hosts; the earlier agentgateway HTTPS path allowed an unlisted host and is not a supported hostname boundary.                                                                                                                             |
| `credential_delivery`                           | proved  | live       | Claude was injected on the Envoy upstream leg. Codex `auth.json` was installed through the shim and remains in the actor snapshot; production must move Codex credentials to egress injection.                                                                                        |
| `cilium_kube_proxy_replacement`                 | partial | live       | KPR=true CoreDNS Pod-IP queries worked, but kube-dns Service-IP queries timed out. The cause was not isolated; the run continued with KPR=false.                                                                                                                                      |
| `snapshot_bucket_access_control`                | unknown | unverified | Snapshot-bucket read access was not established.                                                                                                                                                                                                                                      |
| `stuck_state_timeouts`                          | unknown | unverified | Production timeouts and surfaced recovery for stuck states remain to be implemented and measured.                                                                                                                                                                                     |

## Current recommendation and production requirements

**Defer production adoption of the headless per-turn actor design.** The earlier Herdr-backed
actor proved that Substrate and the Phase 3 boundary can host native Claude and Codex sessions;
both completed a turn and same-session nonce recall across suspend/resume. This does not prove
the target actor, which has no Herdr server or terminal manager and invokes native CLIs headlessly
per turn. Its rebuilt image is not yet live-proved. Production readiness also remains partial
because the Codex marker and two Claude post-worker-loss/revert-history checks are pending, and
durable backend integration and snapshot access controls are not proved.

Production requirements:

- Isolate worker selection by tenant. Substrate does not scope selection by atespace or
  namespace; require unique per-tenant WorkerPool names/selectors and enforce uniqueness with
  admission policy.
- Manage the router `NetworkPolicy` through GitOps and use a CNI that enforces it.
- Issue and rotate per-actor shim tokens from the real Mainloop backend.
- Enforce snapshot-bucket access controls. The Codex snapshot currently contains `auth.json`.
- Move Codex credentials from the actor snapshot to a supported egress-injection flow.
- Use Envoy sdsmint for experimental credential injection; agentgateway does not implement it.
- Set `dns_lookup_family: V4_PREFERRED` on the egress DNS caches and `max_session_keys: 0` on
  the relevant upstream TLS clusters; retain hostname and SAN verification.
- Resolve KPR=true actor DNS: CoreDNS Pod-IP lookup worked, but kube-dns Service-IP lookup
  timed out and the exact cause remains unknown.
- Add bounded timeouts and visible recovery for actors and deliveries that remain stuck.

## Current cleanup and review hold

The Phase 4 owner-review brief requires both actors to remain SUSPENDED for review and their
credential Secrets to be deleted. The dedicated preview cluster and run-built images remain
available for that review; `mainloop-test` remains untouched. Temporary delivery Jobs,
ConfigMaps, shim-token Secrets, the Claude provider, and the Claude/Codex credential Secrets are
removed. The actor snapshots are intentionally retained for owner inspection, so the Codex
snapshot still contains `auth.json`. The proof note records the final inventory and identifies
this review hold as the reason the older finish-plan teardown was not applied.

The Round 3 helper sources are under `spikes/substrate-workspace-adapter/tools/`; the captured
sources match the pinned Substrate checkout. The Codex file-write route now requires an installed
shim token even on a tokenless golden and validates a JSON object. That source hardening is
fixture-tested, but the retained actor image digest predates the change; rebuild before using
that route in another run.
