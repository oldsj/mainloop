# Live publication smoke

After deploying, an operator can run the opt-in smoke against a disposable test
repository with an existing Mainloop project, provider profiles, publication
policy, and CI. It drives one task from chat to a merged documentation PR and
spends quota. It never answers approval cards; configure the test project's
policy beforehand or expect a bounded failure while approval is pending.

The read-only checks (`mainloop smoke --read-only`) cost no quota and can run after any deploy. See [Development environment](dev-environment.md#staging-instance) for those and for the port-forwards this command needs.

## Run

Run it from an operator machine, not the pod: it reads GitHub through the
operator's `gh auth status` and refuses, spending no budget, when `gh` is
missing or not logged in. Open two port-forwards, each in its own terminal:

```bash
kubectl --context <explicit-context> -n <namespace> port-forward svc/mainloop-backend 18000:8000
kubectl --context <explicit-context> -n <namespace> port-forward svc/mainloop-git-push 18080:80
```

Then, from `server/`:

```bash
go run ./cmd/mainloop smoke --live --budget 1 \
  --base-url http://127.0.0.1:18000 --api-host <API_DOMAIN> \
  --git-push-url http://127.0.0.1:18080 \
  --git-push-host mainloop-git-push.<namespace>.svc.cluster.local \
  --project-id <project-id> --repo <owner/repository> \
  --app-login '<github-app-slug>[bot]' --provider claude --merge-tools <on|off>
```

Every target is named on the command line; nothing defaults. Outside dev mode
each listener refuses any Host it does not serve, and a port-forward sends
`127.0.0.1:<port>`. `--api-host` sets the Host header for the owner API and
`--git-push-host` sets it for the git push probe; omit either to send the URL's
own host. Without `--git-push-host` the probe fails with `403 host_denied` and
every later step is skipped.

`--budget` is a ceiling from 1 to 3, but every run's outcome ends it, so one run
starts. Set `--merge-tools` to match the instance: with `off`, a run stops after
CI and proves no merge. The overall deadline defaults to 30 minutes and each step
to 10 (`--deadline`, `--step-deadline`). `--poll-interval` sets the pause between reads.

## What a run checks

The steps run in order, each one waiting for the one before it: `delegation`,
`push`, `pull-request`, `ci`, `merge` and `completion`. They are listed in
[`server/internal/smoke/live.go`](../server/internal/smoke/live.go). A step that
cannot run is reported as skipped with its reason. A run with skipped steps can
pass every other step and still prove nothing about the merge, so a run counts as
proof only when its completion step passed (`CompletionProven` in
`server/internal/smoke/check.go`). The command prints `live completion proven: yes|no`
and exits 3 when a step was skipped.

Each check is one line: `ok`, `fail` or `skip`, then the check name and a short detail.

Success requires a completed task, a merged projection, a confirmed push ledger
record for the unique branch, and a GitHub PR merged by the named Mainloop App
bot, with repository, head branch and SHA matching the task and its CI
projection. An agent report alone cannot pass.

The read-only run takes the same host flags for a port-forward: `--api-host`,
`--mcp-host`, `--git-read-host` and `--git-push-host`, alongside `--base-url`,
`--mcp-url`, `--git-url` and `--git-push-url`.

## Failure handling

- If a step fails, the later steps are reported as skipped and the run stops. Read the failing line first.
- Each run prints its unique branch and delegation request ID before submitting one owner-chat message. Never rerun an uncertain submission blindly: reconcile that branch, delivery and task first, as [AGENTS.md](../AGENTS.md#reliability) requires. Tasks and branches stay intact for inspection after a failure.
