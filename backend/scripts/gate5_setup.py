#!/usr/bin/env python3
r"""
Gate 5 (native-session continuity, .tasknotes/plan.md) credential-free harness: registers
the atespace, resolves and applies the WorkerPool, creates a *versioned* ActorTemplate and
waits for its golden snapshot, then creates/resumes one actor and waits for it to become
RUNNING with its persistent control service (Herdr) confirmed ready -- all without a
provider credential, a credential server, or a native Claude/Codex session.

Implements recovery step 2 of .tasknotes/gate5-review-and-recovery-plan-2026-09-22.md. That
review found the prior harness applied an unresolved `ko://` worker image, never registered
the atespace at the API level (a Kubernetes Namespace of the same name is not an atespace),
checked for an existing ActorTemplate with the atespace embedded in the name instead of the
CLI's required `-a` flag, and printed success once a log line appeared even when that
happened before the read that mattered -- while the underlying golden-snapshot failure was a
credential fetch built into the shared, immutable template, which this script's manifest no
longer has (see spikes/substrate-workspace-adapter/live-agent-image/entrypoint.sh).

Deliberately out of scope here (recovery plan steps 3-4, a separate task): fetching a real
credential, attaching it to a running actor, and starting a native Claude/Codex session.

Prerequisites:
    kubectl, kubectl-ate, and ko built from the pinned Substrate checkout.
    A running kind-substrate-preview cluster with the ate-system + agentgateway dataplane
    installed (this script accepts only the exact kind-substrate-preview context).
    The live-agent-gate image already built and pushed (see live-agent-image/), its digest
    passed with --image.

Usage:
    cd backend
    uv run python scripts/gate5_setup.py \\
        --context kind-substrate-preview --kubeconfig /tmp/substrate-preview-kubeconfig \\
        --ate-cli /tmp/substrate-preview-src/bin/kubectl-ate \\
        --ko /tmp/substrate-preview-src/bin/ko \\
        --substrate-src /tmp/substrate-preview-src \\
        --atespace live-agent-gate --template-version v1 \\
        --image localhost:5001/live-agent-gate@sha256:... \\
        --manifest ../spikes/substrate-workspace-adapter/k8s/live-agent-gate-template.yaml.tmpl \\
        --state-file /tmp/gate5-run-state.json \\
        --egress-tool /tmp/substrate-preview-src/bin/mainloop-egress-tool \\
        --egress-deny-all

Re-running with the same --state-file reconciles the persisted actor uid against the
cluster's current state rather than blindly creating or resuming; a name collision with a
*different* uid is refused, not silently overwritten.
"""

import argparse
import asyncio
import contextlib
import http.client
import json
import os
import re
import socket
import string
import subprocess  # nosec B404 - drives trusted local kubectl/ko/egress-tool binaries, argv only
import sys
import tempfile
import time
import uuid
from pathlib import Path

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))

from mainloop.runtime.substrate import (  # noqa: E402
    ActorFailedToStart,
    ActorState,
    GoldenState,
    IdentityConflict,
    IdentityOutcome,
    SubstrateControl,
    TransportError,
    reconcile_actor_identity,
    wait_for_actor_health,
    wait_for_actor_running,
    wait_for_eligible_worker,
    wait_for_golden_snapshot,
)

PINNED_SUBSTRATE_COMMIT = "cdac9baef81dd319b46086d695266e6161e9e592"
WORKER_NAMESPACE = "live-agent-gate"
WORKER_SELECTOR = "workload=live-agent-gate"
WORKER_SANDBOX_CLASS = "gvisor"
ACTOR_SHIM_PORT = 8090


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--context", required=True)
    p.add_argument("--kubeconfig", required=True)
    p.add_argument("--ate-cli", default="kubectl-ate")
    p.add_argument("--ko", default="ko")
    p.add_argument("--substrate-src", required=True)
    p.add_argument("--router-port", type=int, default=18091)
    p.add_argument("--atespace", required=True)
    p.add_argument(
        "--template-version",
        required=True,
        help='e.g. "v2" -- never reuse one whose golden snapshot failed',
    )
    p.add_argument(
        "--image",
        required=True,
        help="already-built and pushed image digest, e.g. localhost:5001/live-agent-gate@sha256:...",
    )
    p.add_argument(
        "--manifest", required=True, help="path to live-agent-gate-template.yaml.tmpl"
    )
    p.add_argument("--bucket-name", default="ate-snapshots")
    p.add_argument("--actor-name", default="claude-gate5")
    p.add_argument("--state-file", required=True)
    p.add_argument("--golden-timeout", type=float, default=300)
    p.add_argument("--worker-timeout", type=float, default=120)
    p.add_argument("--actor-timeout", type=float, default=120)
    p.add_argument("--readiness-timeout", type=float, default=60)
    p.add_argument("--egress-tool", required=True)
    egress = p.add_mutually_exclusive_group(required=True)
    egress.add_argument("--egress-cidr", help="CIDR to allow")
    egress.add_argument("--egress-allow-all", action="store_true")
    egress.add_argument("--egress-deny-all", action="store_true")
    return p.parse_args()


def load_state(path: str) -> dict:
    if not os.path.exists(path):
        return {}
    with open(path) as f:
        return json.load(f)


def save_state(path: str, state: dict) -> None:
    tmp = f"{path}.tmp"
    with open(tmp, "w") as f:
        json.dump(state, f, indent=2)
    os.replace(tmp, path)


def render_manifest(
    path: str, *, atespace: str, template_name: str, bucket_name: str, image: str
) -> list[str]:
    """Substitutes the template's ${ATESPACE}/${TEMPLATE_NAME}/${BUCKET_NAME} placeholders
    and the __IMAGE__ marker, then splits the multi-document YAML on its own '---'
    separators. Returns [namespace_and_workerpool_doc, actor_template_doc]."""
    with open(path) as f:
        raw = f.read()
    rendered = (
        string.Template(raw)
        .safe_substitute(
            ATESPACE=atespace, TEMPLATE_NAME=template_name, BUCKET_NAME=bucket_name
        )
        .replace("__IMAGE__", image)
    )
    docs = [d for d in rendered.split("\n---\n") if d.strip()]
    if len(docs) != 3:
        raise RuntimeError(
            f"expected 3 YAML documents (Namespace, WorkerPool, ActorTemplate) in {path}, got {len(docs)}"
        )
    namespace_and_workerpool = f"{docs[0]}\n---\n{docs[1]}\n"
    return [namespace_and_workerpool, docs[2] + "\n"]


def verify_substrate_source(source: str, *, runner=subprocess.run) -> str:
    """Require the exact source checkout that supplies the pinned ``ko`` module."""
    root = Path(source).expanduser().resolve()
    if not (root / ".git").exists():
        raise RuntimeError(f"--substrate-src is not a git checkout: {root}")
    for required in ("go.mod", ".ko.yaml"):
        if not (root / required).is_file():
            raise RuntimeError(f"--substrate-src is missing {required}: {root}")
    result = runner(
        ["git", "-C", str(root), "rev-parse", "HEAD"],
        capture_output=True,
        text=True,
        check=True,
        timeout=15,
    )
    commit = result.stdout.strip()
    if commit != PINNED_SUBSTRATE_COMMIT:
        raise RuntimeError(
            f"--substrate-src must be pinned at {PINNED_SUBSTRATE_COMMIT}; found {commit!r}"
        )
    return str(root)


def get_cluster_identity(
    *, context: str, kubeconfig: str, runner=subprocess.run
) -> dict[str, str]:
    """Identify the selected cluster by its API URL and kube-system namespace UID."""
    base = ["kubectl", "--context", context, "--kubeconfig", kubeconfig]
    config = runner(
        [*base, "config", "view", "--minify", "-o", "json"],
        capture_output=True,
        text=True,
        check=True,
        timeout=20,
    )
    config_doc = json.loads(config.stdout)
    context_name = (config_doc.get("contexts") or [{}])[0].get("name")
    api_server = ((config_doc.get("clusters") or [{}])[0].get("cluster") or {}).get(
        "server"
    )
    if context_name != context or not api_server:
        raise RuntimeError(
            f"kubeconfig did not resolve the requested context {context!r} to an API server"
        )
    namespace = runner(
        [*base, "get", "namespace", "kube-system", "-o", "json"],
        capture_output=True,
        text=True,
        check=True,
        timeout=20,
    )
    namespace_uid = (json.loads(namespace.stdout).get("metadata") or {}).get("uid")
    if not namespace_uid:
        raise RuntimeError("kube-system namespace response is missing metadata.uid")
    return {"api_server_url": api_server, "kube_system_namespace_uid": namespace_uid}


def prepare_run_state(args: argparse.Namespace, cluster: dict[str, str]) -> dict:
    """Validate or persist the run intent before any cluster create/apply call."""
    if not re.fullmatch(r".+@sha256:[0-9a-fA-F]{64}", args.image):
        raise RuntimeError("--image must be pinned by a full @sha256 digest")
    template_name = f"live-agent-gate-{args.template_version}"
    identity = {
        "context": args.context,
        "cluster_identity": cluster,
        "atespace": args.atespace,
        "template_name": template_name,
        "image_digest": args.image,
        "actor_name": args.actor_name,
    }
    state = load_state(args.state_file)
    if state:
        missing = {"run_id", "template_uid", "actor_uid"} - state.keys()
        mismatch = {
            key: (state.get(key), value)
            for key, value in identity.items()
            if state.get(key) != value
        }
        if missing or mismatch:
            details = []
            if missing:
                details.append(f"missing identity fields {sorted(missing)}")
            if mismatch:
                details.append(f"requested identity differs: {mismatch}")
            raise RuntimeError(
                "state file does not match this run (" + "; ".join(details) + ")"
            )
        return state

    state = {
        **identity,
        "run_id": str(uuid.uuid4()),
        "template_uid": None,
        "actor_uid": None,
    }
    save_state(args.state_file, state)
    return state


def apply_worker_pool(
    doc: str,
    *,
    kubeconfig: str,
    context: str,
    ko: str,
    substrate_src: str,
    runner=subprocess.run,
) -> None:
    """Resolve the WorkerPool's `ko://...` workerImage and apply it (and the Namespace doc
    it's paired with) via `ko resolve | kubectl apply`. An unresolved ko:// reference
    reaches the pod as an InvalidImageName, not a manifest-time error, so this step must not
    be skipped even though `kubectl apply` alone would exit 0."""
    substrate_src = verify_substrate_source(substrate_src, runner=runner)
    ko_docker_repo = os.environ.get("KO_DOCKER_REPO")
    if not ko_docker_repo:
        raise RuntimeError(
            "KO_DOCKER_REPO must be set (e.g. localhost:5001 for kind) to resolve ko:// images"
        )
    with tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as f:
        f.write(doc)
        doc_path = f.name
    try:
        resolved = (
            runner(  # nosec B603 - argv list, ko path is an operator-supplied flag
                [ko, "resolve", "-f", doc_path],
                env={**os.environ, "KO_DOCKER_REPO": ko_docker_repo},
                capture_output=True,
                text=True,
                check=True,
                timeout=180,
                cwd=substrate_src,
            )
        )
        runner(  # nosec - argv list; kubectl is expected on PATH like git/uv
            [
                "kubectl",
                "--context",
                context,
                "--kubeconfig",
                kubeconfig,
                "apply",
                "-f",
                "-",
            ],
            input=resolved.stdout,
            capture_output=True,
            text=True,
            check=True,
            timeout=60,
        )
    finally:
        os.unlink(doc_path)


def run_egress_tool(args: argparse.Namespace) -> None:
    cmd = [
        args.egress_tool,
        "--kubeconfig",
        args.kubeconfig,
        "--context",
        args.context,
        "--atespace",
        args.atespace,
        "--actor",
        args.actor_name,
    ]
    if args.egress_deny_all:
        cmd.append("--deny-all")
    elif args.egress_allow_all:
        cmd.append("--allow-all")
    else:
        cmd += ["--cidr", args.egress_cidr]
    subprocess.run(
        cmd, check=True, timeout=60
    )  # nosec B603 - argv list, path is an operator-supplied flag


@contextlib.contextmanager
def actor_router_tunnel(args: argparse.Namespace):
    """Forward the Substrate router through the explicitly selected kube context."""
    command = [
        "kubectl",
        "--context",
        args.context,
        "--kubeconfig",
        args.kubeconfig,
        "port-forward",
        "--address",
        "127.0.0.1",
        "--namespace",
        "ate-system",
        "service/atenet-router",
        f"{args.router_port}:8081",
    ]
    process = (
        subprocess.Popen(  # nosec B603 - fixed kubectl argv, explicit kube context
            command,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
        )
    )
    try:
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            if process.poll() is not None:
                output = process.communicate()[0]
                raise RuntimeError(f"router port-forward exited early: {output[-500:]}")
            try:
                with socket.create_connection(
                    ("127.0.0.1", args.router_port), timeout=0.2
                ):
                    break
            except OSError:
                time.sleep(0.2)
        else:
            raise RuntimeError("router port-forward did not become ready within 20s")
        yield args.router_port
    finally:
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        if process.stdout:
            process.stdout.close()


def actor_health_check(
    *, port: int, atespace: str, actor_name: str, timeout_s: float = 2
) -> bool:
    """Call the actor's non-default shim port through Substrate's HTTP CONNECT route."""
    connection = http.client.HTTPConnection("127.0.0.1", port, timeout=timeout_s)
    connection.set_tunnel(
        f"actor-upstream:{ACTOR_SHIM_PORT}",
        headers={"ate-target-actor": f"{atespace}/{actor_name}"},
    )
    try:
        connection.request("GET", "/healthz")
        response = connection.getresponse()
        response.read()
        return response.status == 200
    except (OSError, http.client.HTTPException):
        return False
    finally:
        connection.close()


async def ensure_golden_template(
    control: SubstrateControl,
    args: argparse.Namespace,
    template_name: str,
    actor_template_doc: str,
    state: dict,
) -> str:
    existing = await control.get_actor_template(args.atespace, template_name)
    if existing is None:
        print(f"-- creating actor-template {args.atespace}/{template_name}")
        existing = await control.create_actor_template(
            args.atespace, template_name, actor_template_doc
        )
    if existing.atespace != args.atespace or existing.name != template_name:
        raise IdentityConflict(
            "actor-template create/read returned a different identity"
        )
    if not existing.uid:
        raise TransportError("actor-template response is missing metadata.uid")
    if state.get("template_uid") and state["template_uid"] != existing.uid:
        raise IdentityConflict(
            f"actor-template {args.atespace}/{template_name} uid changed from "
            f"{state['template_uid']} to {existing.uid}"
        )
    containers = existing.raw.get("containers") or []
    template_images = [
        item.get("image") for item in containers if isinstance(item, dict)
    ]
    if args.image not in template_images:
        raise IdentityConflict(
            f"actor-template {args.atespace}/{template_name} does not use requested "
            "image digest"
        )
    state["template_uid"] = existing.uid
    save_state(args.state_file, state)

    if existing.golden_state is GoldenState.FAILED:
        raise SystemExit(
            f"actor-template {args.atespace}/{template_name} already failed its golden "
            f"snapshot ({existing.error_message}); ActorTemplates are immutable -- pass a "
            "new --template-version rather than reusing this one"
        )
    if existing.golden_state is not GoldenState.PENDING:
        print(
            f"-- actor-template {args.atespace}/{template_name} already exists (state={existing.golden_state.value}), awaiting its golden snapshot"
        )

    record = await wait_for_golden_snapshot(
        control, args.atespace, template_name, timeout_s=args.golden_timeout
    )
    print(f"-- golden snapshot ready: {record.golden_tag}")
    return record.uid or existing.uid


async def wait_for_worker_if_actor_is_absent(
    control: SubstrateControl, args: argparse.Namespace, state: dict
) -> None:
    """Reconcile actor ownership before waiting for capacity needed by a new actor."""
    live = await control.get_actor(args.atespace, args.actor_name)
    outcome = reconcile_actor_identity(state.get("actor_uid"), live)
    if outcome is IdentityOutcome.UNOWNED:
        raise IdentityConflict(
            f"actor {args.atespace}/{args.actor_name} already exists with uid {live.uid}, "
            f"but {args.state_file} has no actor uid; refusing to adopt it. Use a new "
            "--actor-name or reconcile the state file explicitly"
        )
    if outcome is IdentityOutcome.DIVERGED:
        raise IdentityConflict(
            f"actor {args.atespace}/{args.actor_name} exists with uid {live.uid}, but "
            f"{args.state_file} recorded {state.get('actor_uid')} from a prior run -- "
            "refusing to resume or recreate it; reconcile manually or use a different "
            "--actor-name"
        )
    if outcome is IdentityOutcome.MATCHES:
        if (
            state.get("template_uid")
            and live.current_actor_template_uid != state["template_uid"]
        ):
            raise IdentityConflict(
                f"actor {args.atespace}/{args.actor_name} references template uid "
                f"{live.current_actor_template_uid!r}, but {args.state_file} recorded "
                f"{state['template_uid']!r}"
            )
        if live.state in {ActorState.CRASHED, ActorState.DELETING}:
            raise ActorFailedToStart(
                f"actor {args.atespace}/{args.actor_name} is {live.state.value}; "
                "choose an explicit revert or a new --actor-name before retrying"
            )
        return

    print(
        f"-- waiting for an eligible worker in namespace={WORKER_NAMESPACE}, "
        f"selector={WORKER_SELECTOR}, sandbox={WORKER_SANDBOX_CLASS}"
    )
    await wait_for_eligible_worker(
        control,
        WORKER_NAMESPACE,
        WORKER_SELECTOR,
        WORKER_SANDBOX_CLASS,
        timeout_s=args.worker_timeout,
    )


async def ensure_actor(
    control: SubstrateControl,
    args: argparse.Namespace,
    template_name: str,
    template_uid: str,
    state: dict,
) -> str:
    """Reconciles any persisted identity against the cluster's current state before
    deciding whether to create or resume, then waits for RUNNING. Returns the actor uid.
    """
    persisted_uid = state.get("actor_uid")
    live = await control.get_actor(args.atespace, args.actor_name)
    outcome = reconcile_actor_identity(persisted_uid, live)

    if outcome is IdentityOutcome.UNOWNED:
        raise IdentityConflict(
            f"actor {args.atespace}/{args.actor_name} already exists with uid {live.uid}, "
            f"but {args.state_file} has no actor uid; refusing to adopt it. Use a new "
            "--actor-name or reconcile the state file explicitly"
        )
    if outcome is IdentityOutcome.DIVERGED:
        raise IdentityConflict(
            f"actor {args.atespace}/{args.actor_name} exists with uid {live.uid}, but "
            f"{args.state_file} recorded {persisted_uid} from a prior run -- refusing to "
            "resume or recreate it; reconcile manually or use a different --actor-name"
        )

    if outcome is IdentityOutcome.ABSENT:
        print(f"-- creating actor {args.atespace}/{args.actor_name}")
        actor = await control.create_actor(
            args.atespace, args.actor_name, template=template_name
        )
        if actor.current_actor_template_uid != template_uid:
            raise IdentityConflict(
                f"created actor {args.atespace}/{args.actor_name} references template uid "
                f"{actor.current_actor_template_uid!r}, expected {template_uid!r}"
            )
        state["actor_uid"] = actor.uid
        save_state(args.state_file, state)
    else:
        actor = live
        if actor.current_actor_template_uid != template_uid:
            raise IdentityConflict(
                f"actor {args.atespace}/{args.actor_name} references template uid "
                f"{actor.current_actor_template_uid!r}, requested {template_uid!r}; "
                "refusing to resume it"
            )
        if actor.state in {ActorState.CRASHED, ActorState.DELETING}:
            raise ActorFailedToStart(
                f"actor {args.atespace}/{args.actor_name} is {actor.state.value}; "
                "choose an explicit revert or a new --actor-name before retrying"
            )
        print(
            f"-- actor {args.atespace}/{args.actor_name} already exists (uid matches persisted identity), state={actor.state.value}"
        )

    if actor.state in {ActorState.SUSPENDED, ActorState.PAUSED}:
        print(f"-- resuming actor {args.atespace}/{args.actor_name}")
        await control.resume_actor(args.atespace, args.actor_name)

    actor = await wait_for_actor_running(
        control, args.atespace, args.actor_name, timeout_s=args.actor_timeout
    )
    print(f"-- actor RUNNING: uid={actor.uid}")
    return actor.uid


async def async_main(args: argparse.Namespace) -> None:
    substrate_src = verify_substrate_source(args.substrate_src)
    cluster = get_cluster_identity(context=args.context, kubeconfig=args.kubeconfig)
    state = prepare_run_state(args, cluster)
    control = SubstrateControl(
        kubeconfig=args.kubeconfig, context=args.context, cli=args.ate_cli
    )
    template_name = f"live-agent-gate-{args.template_version}"

    print(f"-- registering atespace {args.atespace}")
    await control.ensure_atespace(args.atespace)

    namespace_and_workerpool_doc, actor_template_doc = render_manifest(
        args.manifest,
        atespace=args.atespace,
        template_name=template_name,
        bucket_name=args.bucket_name,
        image=args.image,
    )
    print("-- resolving and applying the Namespace + WorkerPool")
    apply_worker_pool(
        namespace_and_workerpool_doc,
        kubeconfig=args.kubeconfig,
        context=args.context,
        ko=args.ko,
        substrate_src=substrate_src,
    )

    await wait_for_worker_if_actor_is_absent(control, args, state)
    template_uid = await ensure_golden_template(
        control, args, template_name, actor_template_doc, state
    )
    await ensure_actor(control, args, template_name, template_uid, state)

    print("-- confirming current control-service health through the actor route")
    with actor_router_tunnel(args) as route_port:
        await wait_for_actor_health(
            lambda: actor_health_check(
                port=route_port,
                atespace=args.atespace,
                actor_name=args.actor_name,
            ),
            timeout_s=args.readiness_timeout,
        )

    print("-- applying the actor's EgressPolicy")
    run_egress_tool(args)

    print()
    print(f"== actor {args.atespace}/{args.actor_name} is RUNNING, credential-free ==")
    print(
        "Credential injection and native-agent session launch are separate, still-gated steps"
    )
    print("(recovery plan steps 3-4) -- not performed by this script.")


def main() -> None:
    args = parse_args()
    if args.context != "kind-substrate-preview":
        raise SystemExit(
            f"refusing to target context {args.context!r}: expected the dedicated "
            "kind-substrate-preview context"
        )
    try:
        asyncio.run(async_main(args))
    except (subprocess.CalledProcessError, RuntimeError) as exc:
        print(f"gate5_setup failed: {exc}", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
