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

| Layer                                                                                                    | Status                                                                                                                       |
| -------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------- |
| kind cluster `substrate-preview`, pinned Substrate `cdac9baef8...` (ate-system + agentgateway dataplane) | Real                                                                                                                         |
| `mainloop-workspace` WorkerPool + ActorTemplate, actor create/get/resume/suspend/revert/delete           | Real, driven through `backend/src/mainloop/runtime/substrate.py`'s actual code (not a separate probe script's own CLI calls) |
| Herdr + `agentctl` inside the actor image                                                                | Real (same image contents as `spikes/k8s-herdr-agents`), without the real Claude/Codex CLIs                                  |
| Claude/Codex agent processes, credentials                                                                | Not run in this spike (see "Not attempted")                                                                                  |
| `workspace_bindings` durable mapping (Postgres)                                                          | Fixture/unit-tested only; not exercised against a live backend + database in this run                                        |

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
# build kubectl-ate, build+push the actor image, apply spikes/substrate-workspace-adapter/k8s/actor-template.yaml.tmpl
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

## Limits / not attempted in this run

- **Preview/HMR gate**: no Vite actor, no authenticated fixed-header proxy, no `agent-browser`
  trial. The prior `substrate-kind-preview-proof` note already showed this works and does not
  always work reliably (10-30s stalls with an HMR socket open); this run did not repeat or
  extend that measurement against Mainloop's own actor template.
- **Dev-service gate**: no Postgres actor, no egress policy, no reconnect-after-wake trial against
  our template.
- **Native-session gate, live**: no real Claude/Codex session was started inside a Substrate
  actor; the credential wiring authorized by `.tasknotes/plan.md` was not used. Only the
  fixture-level contract logic (`test_workspace_adapter.py`) and the generic
  restore-vs-reboot log evidence above are available.
- **`workspace_bindings` orchestration functions** (`ensure_workspace`, `resume_workspace`, ...)
  were not exercised against a live Postgres + running backend; only their extracted pure logic
  (`plan_ensure`, `_binding_from_row`, `is_crashed`) is unit tested, and the transport layer
  they call (`SubstrateControl`) is proved live as described above.

## Cleanup

All test actors deleted, then the `substrate-preview` cluster and its `kind-registry` deleted
(`hack/delete-kind-cluster.sh`). Final `kind get clusters` / `docker ps` showed only
`mainloop-test` / `mainloop-test-control-plane`. Root disk free was unchanged (~26G) before and
after. No Mainloop repository files outside this branch's own commits were changed.
