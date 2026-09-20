# Spike: Herdr-owned arbitrary agents in a Kubernetes workspace

Status: local spike, not a product feature. Implementation lives in `spikes/k8s-herdr-agents/`.

## What it shows

One non-root Kubernetes pod owns a persistent workspace volume. A real Herdr 0.9.0 headless server runs in the pod and owns interactive agent processes. Which agent runs, and with which arguments, is configuration (a ConfigMap binding), and the same supervisor-facing operations (`agentctl start|prompt|stop|identity <binding>`) work for every kind.

## Real versus stand-in

| Layer                                                                      | Status                                                                                 |
| -------------------------------------------------------------------------- | -------------------------------------------------------------------------------------- |
| kind cluster `mainloop-test`, StatefulSet, PVC, pod replacement            | Real                                                                                   |
| Herdr server, panes, agent detection, `agent start`/`prompt --wait`/`read` | Real (Herdr 0.9.0 copied from the host)                                                |
| `pi` and `qwen` executables                                                | **Stand-in**: one deterministic script installed under two Herdr-recognised kind names |
| Provider calls, credentials, model output                                  | None                                                                                   |

Herdr recognises a kind by executable name and bundled screen rules, so the stand-in emits the per-kind "working" signal each rule expects (a spinner line for `pi`, an OSC title for `qwen`). This is a fixture against Herdr's detection, not proof that real `pi` or `qwen` behave the same.

## Run it

```bash
spikes/k8s-herdr-agents/demo.sh
```

The script uses `sudo -n` for Docker and kind (Docker is root-only on the development host; override with `SUDO=`). It creates or reuses only the `mainloop-test` kind cluster, with a run-owned kubeconfig under the evidence directory, and passes `--kubeconfig` and `--context kind-mainloop-test` on every `kubectl` call. Evidence goes to `$EVIDENCE_DIR` (default `.tasknotes/runs/<RUN_ID>/spike-evidence/`). It never deletes the cluster, PVC, images, or evidence.

Journey: build and load the image, deploy, start both bindings, prompt each with a nonce, delete the pod normally, let the StatefulSet recreate it on the same PVC, restart both bindings, and check that each stand-in resumes its native session (turn 2 referencing the earlier nonce).

## Inspect

```bash
K="kubectl --kubeconfig <evidence>/kubeconfig-mainloop-test --context kind-mainloop-test -n herdr-spike"
$K exec -it workspace-0 -- herdr --session mainloop-spike     # attach to the live Herdr TUI
$K exec workspace-0 -- herdr --session mainloop-spike agent list
$K exec workspace-0 -- cat /workspace/repo/.mainloop/alpha.identity.json
$K exec workspace-0 -- ls /workspace/.standin/pi /workspace/.standin/qwen
```

## Observed behaviour

- Pod replacement preserves the PVC, the Herdr `session.json` (workspaces and panes are restored, with new terminal IDs), and the stand-ins' native session files.
- Agent processes do **not** survive pod replacement; after restart no agents are live and `agentctl start` relaunches them. Continuity comes from native session state on the volume, not from Herdr keeping processes alive.
- The pod has no service-account token, no host mounts, no Docker socket, no kubeconfig, and no credentials; the root filesystem is read-only.

## Limits

Not proved: real Codex/Claude/pi/qwen compatibility, provider authentication, hostile-agent isolation, production durability, central delivery semantics, journal normalisation, automatic suspension, off-host recovery, or network isolation. Herdr's detection depends on bundled screen rules that vary by agent and version. Delivery reconciliation after an uncertain prompt is not implemented (the demo never retries).

## Real Claude and Codex through the Mainloop UI (run 20260920T125511Z; measured, local kind only)

Status: **implemented and measured** with real Claude Code 2.1.278 and codex-cli 0.155.1 driven from the real
Mainloop UI. Not production; not multi-user; not a durable delivery ledger across backend restarts.

What ran: the `test` overlay's backend, frontend and Postgres (through `k8s/apps/mainloop/overlays/spike-herdr`,
which also fixes the image remap and scales the Claude Agent SDK controller to zero), plus `workspace-0` from
`spikes/k8s-herdr-agents` built with the real CLIs (`build-real-agents.sh`). Credentials are Kubernetes Secrets
created by path (`claude-oauth`, `codex-auth`; `claude-credentials` and `mainloop-secrets` in `mainloop`).

Design as built:

- **Transport**: Kubernetes API pod-exec from the backend (`backend/src/mainloop/runtime/herdr.py`), with Role
  `workspace-exec` (pods get/list, pods/exec create+get, namespace `herdr-spike` only). The Python client's exec is a
  WebSocket GET, so `get` on `pods/exec` is required. Tradeoff: the backend can run any command in the workspace pod.
- **Pod side**: `agentctl` gained `start --new-id/--resume`, `send` (deliver only), `native-id`, `journal`, `status`.
  Kinds, flags and resume syntax are ConfigMap bindings (`claude.env`, `codex.env`).
- **Replies**: read only from native journals on the PVC (`journal.py`: reply text, receipts, completion and model).
  `NativeEvent` carries no text, and the fixture adapters do not match real journals (measured): real Claude
  transcripts use `sessionId` and end a turn with `system/turn_duration`, and real Codex rollouts use
  `event_msg/task_complete` and `response_item`. `journal.py` translates real records to the adapter shape, so the
  existing adapters classify them (`output`, `completed`); text and receipts come from the raw record.
- **Ledger**: `native_bindings` and `native_deliveries` in Postgres; each prompt is persisted `sending` before the
  transport is touched and sent once; the journal supplies `delivered` and `completed` with an evidence reference.
- **Resume**: after pod replacement the agent is not live; the next delivery restarts it with `claude --resume <id>` or
  `codex resume <id>` on the same native session id.

Findings that cost time (kept because they will recur):

- Claude Code wraps Herdr's terminal paste in `<pasted_content>` and the model may refuse to act on it as untrusted
  data. Fixed by a system-prompt file (ConfigMap `mainloop-system.md`, `--append-system-prompt-file`) stating that
  Mainloop-relayed text is the user's own message.
- Codex shows an "Approaching rate limits, switch model?" modal after a turn that swallows the next paste. The
  entrypoint seeds `[notice] hide_rate_limit_model_nudge = true` (Codex's own dismissal; no model change). The banner
  itself is a usage signal: Codex is near its limit on this account.
- The Kubernetes client's `stream()` is not thread-safe on a shared `ApiClient`; each exec uses its own client.
- `agentctl status` must not pipe `agent get` into `jq` (the pipeline hid a missing agent).
- First-launch dialogs are avoided by seeding `~/.claude.json`, `settings.json` and the Codex `config.toml` trust entry.

Resume outcome (real agents, real UI): follow-up after `kubectl delete pod workspace-0` returned both earlier nonces
for Claude and for Codex, with the pod UID changed, the native session id unchanged and the generation 1 to 2.

Reaching the UI: `kubectl port-forward` (two Herdr panes on `dev`, 127.0.0.1) to `localhost:5173` (frontend) and
`localhost:8081` (backend; the frontend image bakes `VITE_API_URL=http://localhost:8081`).

Limits: no delivery recovery after a backend restart mid-delivery (a `recorded` row stays), no per-session
workspace pods (all sessions share `workspace-0`), the claim "no replies from the terminal" holds for the product
path only (debugging used pane reads), one writer per pod is not enforced, prompt delivery relies on Herdr's paste.
