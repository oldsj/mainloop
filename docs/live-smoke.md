# Live publication smoke

After deploying, an operator can run the opt-in smoke against a disposable test
repository with an existing Mainloop project, provider profiles, publication
policy, and CI. It creates and merges a documentation PR. It never answers
approval cards; configure the test project's policy beforehand or expect a
bounded failure while approval is pending.

```bash
uv run scripts/smoke_live.py --context <explicit-context> --namespace <namespace> \
  --project-id <project-id> --repo <owner/repository> --provider claude \
  --app-login '<github-app-slug>[bot]' --deadline 1800 --step-deadline 600
```

Requires `uv`, `kubectl`, and authenticated `gh`. GitHub calls are read-only. The
script owns a loopback backend port-forward and closes it on exit. The backend
must include the owner-scoped `/projects/{project_id}/smoke-observations` endpoint.
It exposes only capacity holder IDs, delivery IDs/states and publication
IDs/states/branches, never grants or credentials. Preflight checks repository
identity, owner-main/project deliveries, owner-main parent capacity (including
uncertain attempts), and global capacity. Preflight is a snapshot; admission
remains authoritative if another task starts concurrently.

Each run prints its unique branch and delegation request ID before submitting
one owner-chat message. Never rerun an uncertain submission blindly: reconcile
that branch, delivery and task first. The overall deadline defaults to 30 minutes;
each unfinished step defaults to 10 minutes. Failure diagnostics have a separate
bounded read budget of 45 seconds. The script leaves tasks and branches intact
for inspection, and prints sanitized observations after the first failure.

Success requires a completed task, merged projection, confirmed push ledger
record for the unique branch, and a GitHub PR merged by the explicitly named
Mainloop App bot. CI state changes are reported from the projection, with GitHub
check-run details included on failure. An agent report alone cannot pass.

Offline decision fixtures:

```bash
uv run --no-project --directory scripts -m unittest test_smoke_live
```
