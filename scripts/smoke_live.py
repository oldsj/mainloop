"""Opt-in disposable-repository smoke. Run with uv run scripts/smoke_live.py."""

import argparse
import json
import math
import re
import signal
import socket
import subprocess  # nosec B404 - operator CLI uses argv, never a shell
import time
import urllib.request
import uuid
from contextlib import contextmanager


@contextmanager
def wall_deadline(seconds):
    """Bound complete operations, including a slowly streaming response body."""
    if seconds <= 0:
        raise RuntimeError("operation deadline")

    def expired(signum, frame):
        raise RuntimeError("operation deadline")

    previous = signal.signal(signal.SIGALRM, expired)
    signal.setitimer(signal.ITIMER_REAL, seconds)
    try:
        yield
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        signal.signal(signal.SIGALRM, previous)


def completed(view, facts, pr, app_login, repo):
    """Bind independent merger evidence to this repository, branch and head."""
    projection = view["projection"]
    head = pr.get("head") or {}
    return (
        view["task"]["status"] == "completed"
        and projection.get("merge_state") == "merged"
        and pr.get("merged") is True
        and (pr.get("merged_by") or {}).get("login") == app_login
        and facts["push_confirmed"]
        and (pr.get("base", {}).get("repo") or {}).get("full_name", "").lower()
        == repo.lower()
        and (head.get("repo") or {}).get("full_name", "").lower() == repo.lower()
        and head.get("ref") == view["task"]["checkout"]["branch"]
        and bool(projection.get("pr_head_sha"))
        and head.get("sha") == projection.get("pr_head_sha")
        and projection.get("ci_head_sha") == head.get("sha")
    )


def stage(view, facts, pr):
    if view is None:
        return "delegation"
    if not facts["push_confirmed"]:
        return "push"
    projection = view["projection"]
    if not projection.get("pr_number"):
        return "pull_request"
    if projection.get("ci_state") != "success":
        return "ci"
    if not pr.get("merged"):
        return "merge"
    return "completion"


def gh(repo, path, timeout):
    result = subprocess.run(  # nosec B603, B607
        ["gh", "api", f"repos/{repo}/{path}"],
        capture_output=True,
        timeout=timeout,
        check=False,
    )
    if result.returncode:
        raise RuntimeError("GitHub read failed")
    return json.loads(result.stdout)


@contextmanager
def forward(args):
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    process = subprocess.Popen(  # nosec B603, B607
        [
            "kubectl",
            "--context",
            args.context,
            "--namespace",
            args.namespace,
            "port-forward",
            "--address=127.0.0.1",
            "service/mainloop-backend",
            f"{port}:8000",
        ],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    try:
        end = time.monotonic() + 20
        while time.monotonic() < end:
            if process.poll() is not None:
                raise RuntimeError("port-forward exited")
            try:
                with socket.create_connection(("127.0.0.1", port), timeout=0.2):
                    break
            except OSError:
                time.sleep(0.1)
        else:
            raise RuntimeError("port-forward deadline")
        yield f"http://127.0.0.1:{port}"
    finally:
        if process.poll() is None:
            process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()


def run(args, base):
    end = time.monotonic() + args.deadline

    operation_end = end

    def remaining():
        value = min(end, operation_end) - time.monotonic()
        if value <= 0:
            raise RuntimeError("operation deadline")
        return min(15, value)

    def github(path):
        with wall_deadline(remaining()):
            return gh(args.repo, path, remaining())

    def api(path, data=None):
        budget = remaining()
        request = urllib.request.Request(
            base + path,
            data=None if data is None else json.dumps(data).encode(),
            headers={
                "Host": "mainloop-backend",
                "Accept": "application/json",
                "Content-Type": "application/json",
            },
        )
        with wall_deadline(budget):
            with urllib.request.urlopen(
                request, timeout=budget
            ) as response:  # nosec B310
                return json.load(response)

    api("/health")
    project = api(f"/projects/{args.project_id}")
    if project["full_name"].lower() != args.repo.lower():
        raise RuntimeError("preflight: project repository differs")
    observation_path = f"/projects/{args.project_id}/smoke-observations"
    facts = api(observation_path)
    if facts["deliveries_busy"]:
        raise RuntimeError(
            "preflight: deliveries in flight " + json.dumps(facts["deliveries"])
        )
    if len(facts["capacity_holders"]) >= facts["parent_capacity"]:
        raise RuntimeError(
            "preflight: parent capacity held by " + ",".join(facts["capacity_holders"])
        )
    if not facts["global_capacity_available"]:
        raise RuntimeError("preflight: global capacity unavailable")
    request_id = uuid.uuid4().hex
    branch = f"smoke/{args.provider}-{request_id}"
    print(f"request_id={request_id} branch={branch}", flush=True)
    prompt = (
        f"Delegate exactly one code task on project {args.project_id}, repository {args.repo}, "
        f"provider_profile_id {args.provider}, checkout.branch {branch}, request_id {request_id}. "
        f"Create only docs/smoke-{request_id}.md containing a short smoke verification note. "
        "Commit the file, git push through the publication gate, open_pull_request, "
        "wait for exact-head CI using foreground waits, merge_pull_request, then report completed. "
        "Never bypass policy or answer approval cards. Do not delegate additional tasks."
    )
    view, pr = None, {}
    current, since, last, failure = "delegation", time.monotonic(), None, None
    try:
        operation_end = min(end, since + args.step_deadline)
        # Exactly one submission, including uncertain accepted-but-timeout outcomes.
        receipt = api("/chat", {"message": prompt})
        print(
            "delivery_message_id=" + str(receipt.get("delivery_message_id")), flush=True
        )
        while time.monotonic() < end:
            views = api("/tasks?project_id=" + args.project_id)
            matches = [
                v
                for v in views
                if (v["task"].get("checkout") or {}).get("branch") == branch
            ]
            if len(matches) > 1:
                raise RuntimeError("delegation: multiple matching tasks")
            view = matches[0] if matches else None
            facts = api(observation_path + "?branch=" + branch)
            if view and view["projection"].get("pr_number"):
                number = view["projection"]["pr_number"]
                pr = github(f"pulls/{number}")
                github(f"commits/{pr['head']['sha']}/check-runs")
            observed = (
                None
                if view is None
                else {
                    "task_id": view["task"]["id"],
                    "status": view["task"]["status"],
                    "pr_number": view["projection"].get("pr_number"),
                    "ci_state": view["projection"].get("ci_state"),
                    "merge_state": view["projection"].get("merge_state"),
                }
            )
            if observed != last:
                print(json.dumps(observed), flush=True)
                last = observed
            if view and completed(view, facts, pr, args.app_login, args.repo):
                print("PASS", flush=True)
                return 0
            next_stage = stage(view, facts, pr)
            if next_stage != current:
                current, since = next_stage, time.monotonic()
                operation_end = min(end, since + args.step_deadline)
            if view and view["task"]["status"] in ("failed", "cancelled", "blocked"):
                raise RuntimeError(current + ": task " + view["task"]["status"])
            if view and view["projection"].get("ci_state") == "failure":
                raise RuntimeError("ci: failed")
            if time.monotonic() - since >= args.step_deadline:
                raise RuntimeError(current + ": step deadline")
            time.sleep(
                min(
                    args.poll_interval,
                    max(0, min(end, operation_end) - time.monotonic()),
                )
            )
        failure = current + ": overall deadline"
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as exc:
        failure = (
            current + ": " + str(exc)
            if isinstance(exc, RuntimeError)
            else current + ": " + type(exc).__name__
        )
    # Fresh, read-only reconciliation after any submission or polling failure.
    end = operation_end = time.monotonic() + 45
    discoveries = []
    for label, read in (
        ("task", lambda: api("/tasks?project_id=" + args.project_id)),
        ("pushes", lambda: api(observation_path + "?branch=" + branch)),
        (
            "pr",
            lambda: github(
                "pulls?state=all&head=" + args.repo.split("/")[0] + ":" + branch
            ),
        ),
    ):
        try:
            value = read()
            if label == "task":
                matches = [
                    v
                    for v in value
                    if (v["task"].get("checkout") or {}).get("branch") == branch
                ]
                discoveries.extend(
                    v["projection"]["pr_number"]
                    for v in matches
                    if v["projection"].get("pr_number")
                )
                value = [
                    {
                        "task_id": v["task"]["id"],
                        "status": v["task"]["status"],
                        "projection": v["projection"],
                    }
                    for v in matches
                ]
            elif label == "pr":
                discoveries.extend(p["number"] for p in value)
                value = [
                    {
                        "number": p["number"],
                        "state": p["state"],
                        "merged_at": p.get("merged_at"),
                    }
                    for p in value
                ]
            print(json.dumps({label: value}), flush=True)
        except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as exc:
            print(f"{label}: unavailable ({type(exc).__name__})", flush=True)
    for number in sorted(set(discoveries)):
        for label in ("pr_merge", "ci"):
            try:
                value = github(
                    f"pulls/{number}"
                    if label == "pr_merge"
                    else f"commits/{pr['head']['sha']}/check-runs"
                )
                if label == "pr_merge":
                    pr = value
                    value = {
                        "number": number,
                        "merged": pr.get("merged"),
                        "merged_by": (pr.get("merged_by") or {}).get("login"),
                    }
                else:
                    value = [
                        {
                            "name": c["name"],
                            "status": c["status"],
                            "conclusion": c["conclusion"],
                        }
                        for c in value.get("check_runs", [])
                    ]
                print(json.dumps({label: value}), flush=True)
            except (
                OSError,
                ValueError,
                RuntimeError,
                subprocess.SubprocessError,
            ) as exc:
                print(f"{label}: unavailable ({type(exc).__name__})")
                if label == "pr_merge":
                    break
    print(f"FAIL {failure}; last_state={json.dumps(last)}", flush=True)
    return 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("context", "namespace", "project-id", "repo", "app-login"):
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--provider", required=True, choices=("claude", "codex"))
    parser.add_argument("--deadline", type=float, default=1800, help="overall seconds")
    parser.add_argument(
        "--step-deadline", type=float, default=600, help="seconds per step"
    )
    parser.add_argument("--poll-interval", type=float, default=5)
    args = parser.parse_args()
    if not args.context.strip():
        parser.error("context must be nonblank")
    if not re.fullmatch(r"[\w.-]+/[\w.-]+", args.repo) or not re.fullmatch(
        r"[\w-]+", args.project_id
    ):
        parser.error("invalid repository or project id")
    if any(
        not math.isfinite(v) or v <= 0
        for v in (args.deadline, args.step_deadline, args.poll_interval)
    ):
        parser.error("deadlines and poll interval must be positive")
    try:
        with forward(args) as base:
            return run(args, base)
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as exc:
        print(
            "FAIL preflight/submission: "
            + (str(exc) if isinstance(exc, RuntimeError) else type(exc).__name__)
        )
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
