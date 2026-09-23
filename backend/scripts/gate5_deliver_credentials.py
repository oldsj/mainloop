#!/usr/bin/env python3
"""Deliver one provider credential from a control-namespace Secret to its final actor.

The source file is passed only as a path. kubectl reads it through an inherited file
descriptor into a Secret in mainloop-control; a short-lived control Job mounts that Secret
and sends its contents in the authenticated exec-shim request body. Nothing reads a Secret
back through the Kubernetes API, and neither secret value is placed in argv, env, or logs.

This is a separate command from gate5_setup.py, so golden creation and credential-free actor
setup never invoke delivery.
"""

import argparse
import json
import os
import re
import secrets
import stat
import subprocess  # nosec B404 - fixed kubectl commands, secret data via file descriptors
import sys
import tempfile
from pathlib import Path
from typing import Callable

CONTROL_NAMESPACE = "mainloop-control"
DEFAULT_CONTEXT = "kind-substrate-preview"
DEFAULT_KUBECONFIG = "/tmp/substrate-preview-kubeconfig"
DEFAULT_IMAGE = (
    "localhost:5001/live-agent-gate@sha256:"
    "8ec007c56b070a2357f20203807197e42ebdb8d0c0e155d57a3bd48fb8d10f57"
)
MAX_SECRET_BYTES = 1024 * 1024
ACTOR_NAMESPACES = {"claude": "native-claude", "codex": "native-codex"}
SHARED_CLUSTER_NOTE = (
    Path(__file__).resolve().parents[2]
    / ".tasknotes"
    / "shared-cluster-2026-09-23.md"
)
DELIVERY_SCRIPT = (
    Path(__file__).resolve().parents[2]
    / "spikes"
    / "substrate-workspace-adapter"
    / "tools"
    / "phase4"
    / "deliver-credentials.cjs"
)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--context", default=DEFAULT_CONTEXT)
    parser.add_argument("--kubeconfig", default=DEFAULT_KUBECONFIG)
    parser.add_argument("--actor-namespace", required=True)
    parser.add_argument("--actor-name", required=True)
    parser.add_argument("--state-file", required=True)
    parser.add_argument("--credential", choices=("claude", "codex"), required=True)
    parser.add_argument(
        "--claude-token-file", default=str(Path.home() / ".claude-token")
    )
    parser.add_argument(
        "--codex-auth-file", default=str(Path.home() / ".codex" / "auth.json")
    )
    parser.add_argument("--image", default=DEFAULT_IMAGE)
    return parser.parse_args()


def read_private_state(path: str, *, actor_namespace: str, actor_name: str) -> dict:
    try:
        with open(path, encoding="utf-8") as stream:
            state = json.load(stream)
    except (OSError, ValueError):
        raise RuntimeError("private actor state is unavailable") from None

    if not isinstance(state, dict) or (
        state.get("context") != DEFAULT_CONTEXT
        or state.get("atespace") != actor_namespace
        or state.get("actor_name") != actor_name
        or not state.get("actor_uid")
    ):
        raise RuntimeError("private actor state does not own the requested final actor")
    token = state.get("shim_token")
    if (
        not isinstance(token, str)
        or not 32 <= len(token) <= 4096
        or re.search(r"\s", token)
    ):
        raise RuntimeError("private actor state has no valid shim token")
    return state


def require_handover(path: Path = SHARED_CLUSTER_NOTE) -> None:
    try:
        note = path.read_text(encoding="utf-8")
    except OSError:
        raise RuntimeError("shared-cluster handover note is unavailable") from None
    if not re.search(r"(?m)^\*\*Handover:\*\*\s+done(?:\s|$)", note):
        raise RuntimeError("shared-cluster handover is not done; no resources were created")


def kubectl_prefix(context: str, kubeconfig: str) -> list[str]:
    if context != DEFAULT_CONTEXT:
        raise RuntimeError(f"refusing context {context!r}; expected {DEFAULT_CONTEXT!r}")
    return ["kubectl", "--context", context, "--kubeconfig", kubeconfig]


def run_kubectl(
    argv: list[str],
    *,
    runner: Callable = subprocess.run,
    input_bytes: bytes | None = None,
    pass_fds: tuple[int, ...] = (),
    timeout: int = 60,
    action: str,
):
    try:
        return runner(
            argv,
            input=input_bytes,
            pass_fds=pass_fds,
            capture_output=True,
            check=True,
            timeout=timeout,
        )
    except (subprocess.CalledProcessError, OSError, subprocess.TimeoutExpired):
        raise RuntimeError(f"{action} failed") from None


def create_secret_from_path(
    *,
    context: str,
    kubeconfig: str,
    namespace: str,
    name: str,
    key: str,
    source_path: str,
    runner: Callable = subprocess.run,
) -> None:
    """Create a Secret without placing the source path or bytes in child argv/env/logs."""
    try:
        source = open(source_path, "rb")
    except OSError:
        raise RuntimeError("credential source file is unavailable") from None
    with source:
        details = os.fstat(source.fileno())
        if (
            not stat.S_ISREG(details.st_mode)
            or details.st_size <= 0
            or details.st_size > MAX_SECRET_BYTES
        ):
            raise RuntimeError("credential source file has an invalid size or type")
        source_fd = source.fileno()
        descriptor_path = f"/proc/self/fd/{source_fd}"
        create = [
            *kubectl_prefix(context, kubeconfig),
            "create",
            "secret",
            "generic",
            name,
            "--namespace",
            namespace,
            f"--from-file={key}={descriptor_path}",
            "--dry-run=client",
            "-o",
            "json",
        ]
        result = run_kubectl(
            create,
            runner=runner,
            pass_fds=(source_fd,),
            action="credential Secret rendering",
        )
        apply = [*kubectl_prefix(context, kubeconfig), "apply", "-f", "-"]
        run_kubectl(
            apply,
            runner=runner,
            input_bytes=result.stdout,
            action="credential Secret creation",
        )


def apply_json(
    *,
    context: str,
    kubeconfig: str,
    manifest: dict,
    runner: Callable = subprocess.run,
    action: str,
) -> None:
    encoded = json.dumps(manifest, separators=(",", ":")).encode("utf-8")
    run_kubectl(
        [*kubectl_prefix(context, kubeconfig), "apply", "-f", "-"],
        runner=runner,
        input_bytes=encoded,
        action=action,
    )


def delete_delivery_resources(
    *, context: str, kubeconfig: str, names: list[str], runner: Callable
) -> None:
    run_kubectl(
        [
            *kubectl_prefix(context, kubeconfig),
            "delete",
            "--namespace",
            CONTROL_NAMESPACE,
            "--ignore-not-found=true",
            "--wait=true",
            "--timeout=15s",
            *names,
        ],
        runner=runner,
        timeout=20,
        action="delivery resource cleanup",
    )


def build_job(
    *,
    name: str,
    image: str,
    credential: str,
    credential_secret: str,
    shim_secret: str,
    actor_namespace: str,
    actor_name: str,
) -> dict:
    return {
        "apiVersion": "batch/v1",
        "kind": "Job",
        "metadata": {"name": name, "namespace": CONTROL_NAMESPACE},
        "spec": {
            "activeDeadlineSeconds": 60,
            "backoffLimit": 0,
            "ttlSecondsAfterFinished": 120,
            "template": {
                "metadata": {"labels": {"mainloop.dev/role": "control"}},
                "spec": {
                    "automountServiceAccountToken": False,
                    "restartPolicy": "Never",
                    "securityContext": {
                        "runAsNonRoot": True,
                        "runAsUser": 10001,
                        "runAsGroup": 10001,
                        "fsGroup": 10001,
                    },
                    "containers": [
                        {
                            "name": "deliver-credential",
                            "image": image,
                            "imagePullPolicy": "IfNotPresent",
                            "command": [
                                "node",
                                "/var/run/phase4-delivery/deliver-credentials.cjs",
                            ],
                            "env": [
                                {"name": "CREDENTIAL_KIND", "value": credential},
                                {"name": "ACTOR_NAMESPACE", "value": actor_namespace},
                                {"name": "ACTOR_NAME", "value": actor_name},
                                {
                                    "name": "ROUTER_HOST",
                                    "value": "atenet-router.ate-system.svc.cluster.local",
                                },
                                {"name": "ROUTER_PORT", "value": "8081"},
                            ],
                            "volumeMounts": [
                                {
                                    "name": "delivery-script",
                                    "mountPath": "/var/run/phase4-delivery",
                                    "readOnly": True,
                                },
                                {
                                    "name": "credential",
                                    "mountPath": "/var/run/phase4-credentials",
                                    "readOnly": True,
                                },
                                {
                                    "name": "shim-auth",
                                    "mountPath": "/var/run/phase4-shim-auth",
                                    "readOnly": True,
                                },
                            ],
                            "securityContext": {
                                "allowPrivilegeEscalation": False,
                                "readOnlyRootFilesystem": True,
                                "capabilities": {"drop": ["ALL"]},
                            },
                        }
                    ],
                    "volumes": [
                        {
                            "name": "delivery-script",
                            "configMap": {
                                "name": name,
                                "items": [
                                    {
                                        "key": "deliver-credentials.cjs",
                                        "path": "deliver-credentials.cjs",
                                    }
                                ],
                            },
                        },
                        {
                            "name": "credential",
                            "secret": {
                                "secretName": credential_secret,
                                "defaultMode": 0o440,
                                "items": [
                                    {"key": "credential", "path": "credential"}
                                ],
                            },
                        },
                        {
                            "name": "shim-auth",
                            "secret": {
                                "secretName": shim_secret,
                                "defaultMode": 0o440,
                                "items": [{"key": "token", "path": "token"}],
                            },
                        },
                    ],
                },
            },
        },
    }


def deliver_credentials(
    args: argparse.Namespace,
    *,
    runner: Callable = subprocess.run,
    handover_note: Path = SHARED_CLUSTER_NOTE,
) -> None:
    require_handover(handover_note)
    expected_namespace = ACTOR_NAMESPACES[args.credential]
    if args.actor_namespace != expected_namespace:
        raise RuntimeError("credential kind and actor namespace do not match")
    if not re.fullmatch(
        r"localhost:5001/live-agent-gate@sha256:[0-9a-f]{64}", args.image
    ):
        raise RuntimeError("delivery image must be the digest-pinned live-agent-gate image")

    state = read_private_state(
        args.state_file,
        actor_namespace=args.actor_namespace,
        actor_name=args.actor_name,
    )
    credential_path = (
        args.claude_token_file if args.credential == "claude" else args.codex_auth_file
    )
    credential_secret = f"phase4-{args.credential}-{secrets.token_hex(4)}"
    shim_secret = f"phase4-shim-{secrets.token_hex(4)}"
    job_name = f"phase4-delivery-{args.credential}-{secrets.token_hex(4)}"
    resource_names = [
        f"job/{job_name}",
        f"configmap/{job_name}",
        f"secret/{credential_secret}",
        f"secret/{shim_secret}",
    ]
    failed = False
    try:
        create_secret_from_path(
            context=args.context,
            kubeconfig=args.kubeconfig,
            namespace=CONTROL_NAMESPACE,
            name=credential_secret,
            key="credential",
            source_path=credential_path,
            runner=runner,
        )
        with tempfile.TemporaryFile(mode="w+b") as shim_token_file:
            shim_token_file.write(state["shim_token"].encode("utf-8"))
            shim_token_file.flush()
            shim_token_file.seek(0)
            shim_fd = shim_token_file.fileno()
            shim_key_path = f"/proc/self/fd/{shim_fd}"
            create = [
                *kubectl_prefix(args.context, args.kubeconfig),
                "create",
                "secret",
                "generic",
                shim_secret,
                "--namespace",
                CONTROL_NAMESPACE,
                f"--from-file=token={shim_key_path}",
                "--dry-run=client",
                "-o",
                "json",
            ]
            created = run_kubectl(
                create,
                runner=runner,
                pass_fds=(shim_fd,),
                action="shim Secret rendering",
            )
            run_kubectl(
                [*kubectl_prefix(args.context, args.kubeconfig), "apply", "-f", "-"],
                runner=runner,
                input_bytes=created.stdout,
                action="shim Secret creation",
            )

        configmap = {
            "apiVersion": "v1",
            "kind": "ConfigMap",
            "metadata": {"name": job_name, "namespace": CONTROL_NAMESPACE},
            "data": {
                "deliver-credentials.cjs": DELIVERY_SCRIPT.read_text(encoding="utf-8")
            },
        }
        apply_json(
            context=args.context,
            kubeconfig=args.kubeconfig,
            manifest=configmap,
            runner=runner,
            action="delivery ConfigMap creation",
        )
        job = build_job(
            name=job_name,
            image=args.image,
            credential=args.credential,
            credential_secret=credential_secret,
            shim_secret=shim_secret,
            actor_namespace=args.actor_namespace,
            actor_name=args.actor_name,
        )
        apply_json(
            context=args.context,
            kubeconfig=args.kubeconfig,
            manifest=job,
            runner=runner,
            action="delivery Job creation",
        )
        run_kubectl(
            [
                *kubectl_prefix(args.context, args.kubeconfig),
                "wait",
                "--namespace",
                CONTROL_NAMESPACE,
                f"job/{job_name}",
                "--for=condition=complete",
                "--timeout=75s",
            ],
            runner=runner,
            timeout=80,
            action="credential delivery Job",
        )
        print(
            f"credential delivered for {args.credential} to "
            f"{args.actor_namespace}/{args.actor_name}"
        )
    except Exception:
        failed = True
        raise
    finally:
        try:
            delete_delivery_resources(
                context=args.context,
                kubeconfig=args.kubeconfig,
                names=resource_names,
                runner=runner,
            )
        except RuntimeError:
            if not failed:
                raise
            print("credential delivery resource cleanup failed", file=sys.stderr)


def main() -> None:
    args = parse_args()
    try:
        deliver_credentials(args)
    except RuntimeError as exc:
        print(f"gate5_deliver_credentials failed: {exc}", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
