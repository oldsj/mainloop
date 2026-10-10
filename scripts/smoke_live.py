"""Opt-in disposable-repository smoke. Run with uv run scripts/smoke_live.py."""

import argparse
import json
import re
import socket
import subprocess  # nosec B404 - operator CLI uses argv, never a shell
import time
import urllib.request
import uuid
from contextlib import contextmanager


def completed(view, facts, pr, app_login):
    """Require independent publication and merger evidence, never agent claims."""
    return (
        view["task"]["status"] == "completed"
        and view["projection"].get("merge_state") == "merged"
        and pr.get("merged") is True
        and (pr.get("merged_by") or {}).get("login") == app_login
        and any(p["state"] == "confirmed" for p in facts["pushes"])
    )


def stage(view, facts, pr):
    if view is None:
        return "delegation"
    if not any(p["state"] == "confirmed" for p in facts["pushes"]):
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

    def api(path, data=None):
        remaining = end - time.monotonic()
        if remaining <= 0:
            raise RuntimeError("overall deadline")
        request = urllib.request.Request(
            base + path,
            data=None if data is None else json.dumps(data).encode(),
            headers={
                "Host": "mainloop-backend",
                "Accept": "application/json",
                "Content-Type": "application/json",
            },
        )
        with urllib.request.urlopen(
            request, timeout=min(15, remaining)
        ) as response:  # nosec B310 - loopback HTTP base
            return json.load(response)

    api("/health")
    project = api(f"/projects/{args.project_id}")
    if project["full_name"].lower() != args.repo.lower():
        raise RuntimeError("preflight: project repository differs")
    observation_path = f"/projects/{args.project_id}/smoke-observations"
    facts = api(observation_path)
    if facts["deliveries"]:
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
    # Exactly one submission; uncertain responses are never retried.
    receipt = api("/chat", {"message": prompt})
    print("delivery_message_id=" + str(receipt.get("delivery_message_id")), flush=True)
    view, pr, ci = None, {}, {}
    current, since, last, failure = "delegation", time.monotonic(), None, None
    try:
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
                pr = gh(
                    args.repo,
                    f"pulls/{number}",
                    min(15, max(0.1, end - time.monotonic())),
                )
                ci = gh(
                    args.repo,
                    f"commits/{pr['head']['sha']}/check-runs",
                    min(15, max(0.1, end - time.monotonic())),
                )
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
            if view and completed(view, facts, pr, args.app_login):
                print("PASS", flush=True)
                return 0
            next_stage = stage(view, facts, pr)
            if next_stage != current:
                current, since = next_stage, time.monotonic()
            if view and view["task"]["status"] in ("failed", "cancelled", "blocked"):
                raise RuntimeError(current + ": task " + view["task"]["status"])
            if view and view["projection"].get("ci_state") == "failure":
                raise RuntimeError("ci: failed")
            if time.monotonic() - since >= args.step_deadline:
                raise RuntimeError(current + ": step deadline")
            time.sleep(min(args.poll_interval, max(0, end - time.monotonic())))
        failure = current + ": overall deadline"
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as exc:
        failure = (
            str(exc)
            if isinstance(exc, RuntimeError)
            else current + ": " + type(exc).__name__
        )
    # Independent, bounded reads continue after failure, even after overall expiry.
    end = time.monotonic() + 45
    for label, read in (
        ("task", lambda: api("/tasks?project_id=" + args.project_id)),
        ("pushes", lambda: api(observation_path + "?branch=" + branch)),
        (
            "pr",
            lambda: gh(
                args.repo,
                "pulls?state=all&head=" + args.repo.split("/")[0] + ":" + branch,
                10,
            ),
        ),
    ):
        try:
            value = read()
            if label == "task":
                value = [
                    {
                        "task_id": v["task"]["id"],
                        "status": v["task"]["status"],
                        "projection": v["projection"],
                    }
                    for v in value
                    if (v["task"].get("checkout") or {}).get("branch") == branch
                ]
            elif label == "pr":
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
    try:
        if view and view["projection"].get("pr_number"):
            pr = gh(args.repo, f"pulls/{view['projection']['pr_number']}", 10)
            ci = gh(args.repo, f"commits/{pr['head']['sha']}/check-runs", 10)
            print(
                json.dumps(
                    {
                        "pr_merge": {
                            "merged": pr.get("merged"),
                            "merged_by": (pr.get("merged_by") or {}).get("login"),
                        }
                    }
                )
            )
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as exc:
        print("ci/merge: unavailable (" + type(exc).__name__ + ")")
    print(
        json.dumps(
            {
                "ci": [
                    {
                        "name": c["name"],
                        "status": c["status"],
                        "conclusion": c["conclusion"],
                    }
                    for c in ci.get("check_runs", [])
                ]
            }
        )
    )
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
    if not re.fullmatch(r"[\w.-]+/[\w.-]+", args.repo) or not re.fullmatch(
        r"[\w-]+", args.project_id
    ):
        parser.error("invalid repository or project id")
    if min(args.deadline, args.step_deadline, args.poll_interval) <= 0:
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
