"""Credential re-auth job interface and Kubernetes implementation.

The job image must provide ``/usr/local/bin/mainloop-reauth``. It captures provider output,
prints only a validated device challenge, and sends the resulting credential to the callback
over the in-cluster service. The credential never appears in Kubernetes logs or API responses.
"""

from __future__ import annotations

import asyncio
import json
import re
import secrets
from dataclasses import dataclass
from typing import Protocol

from kubernetes import client, config
from kubernetes.client.rest import ApiException
from mainloop.config import settings

_PROVIDERS = {"codex", "claude"}
_CHALLENGE_MARKER = "MAINLOOP_REAUTH_CHALLENGE "
_CHALLENGE_CODE = re.compile(r"^[A-Z0-9-]{4,32}$")
_SAFE_AUTH_HOSTS = {"auth.openai.com", "claude.ai", "console.anthropic.com"}


@dataclass(frozen=True, slots=True)
class ReauthChallenge:
    url: str
    code: str


@dataclass(frozen=True, slots=True)
class ReauthStatus:
    id: str
    provider: str
    state: str
    challenge: ReauthChallenge | None = None


class CredentialReauthRunner(Protocol):
    async def start(
        self, provider: str, *, owner: str | None = None
    ) -> ReauthStatus: ...

    async def status(self, job_id: str) -> ReauthStatus | None: ...

    async def callback_provider(self, job_id: str, token: str) -> str | None: ...

    async def complete(self, job_id: str, token: str) -> None: ...


class FakeCredentialReauthRunner:
    """In-memory runner for API and lifecycle tests. Credential values are never retained."""

    def __init__(self):
        self._jobs: dict[str, tuple[str, str, ReauthStatus]] = {}

    async def start(self, provider: str, *, owner: str | None = None) -> ReauthStatus:
        del owner
        if provider not in _PROVIDERS:
            raise ValueError("unsupported credential provider")
        job_id = f"reauth-{provider}-{secrets.token_hex(6)}"
        token = secrets.token_urlsafe(32)
        result = ReauthStatus(job_id, provider, "running")
        self._jobs[job_id] = (provider, token, result)
        return result

    async def status(self, job_id: str) -> ReauthStatus | None:
        row = self._jobs.get(job_id)
        return row[2] if row else None

    async def callback_provider(self, job_id: str, token: str) -> str | None:
        row = self._jobs.get(job_id)
        return row[0] if row and secrets.compare_digest(row[1], token) else None

    async def complete(self, job_id: str, token: str) -> None:
        row = self._jobs.get(job_id)
        if row and secrets.compare_digest(row[1], token):
            provider, _, result = row
            self._jobs[job_id] = (
                provider,
                "",
                ReauthStatus(job_id, provider, "completed", result.challenge),
            )


class KubernetesCredentialReauthRunner:
    """Run one bounded control-side Job per sign-in attempt (not live-cluster verified)."""

    def __init__(
        self,
        batch_api: client.BatchV1Api | None = None,
        core_api: client.CoreV1Api | None = None,
    ):
        self._batch = batch_api
        self._core = core_api
        self.namespace = settings.substrate_reauth_job_namespace
        self._tokens: dict[str, tuple[str, str]] = {}
        self._completed: set[str] = set()

    def _clients(self) -> tuple[client.BatchV1Api, client.CoreV1Api]:
        if self._batch is None or self._core is None:
            try:
                config.load_incluster_config()
            except config.ConfigException:
                config.load_kube_config()
            self._batch = self._batch or client.BatchV1Api()
            self._core = self._core or client.CoreV1Api()
        return self._batch, self._core  # type: ignore[return-value]

    async def start(self, provider: str, *, owner: str | None = None) -> ReauthStatus:
        del owner
        if provider not in _PROVIDERS:
            raise ValueError("unsupported credential provider")
        image = settings.substrate_reauth_job_image
        if not image:
            raise RuntimeError("credential re-auth job image is not configured")
        job_id = f"reauth-{provider}-{secrets.token_hex(6)}"
        callback_token = secrets.token_urlsafe(32)
        body = client.V1Job(
            metadata=client.V1ObjectMeta(
                name=job_id,
                namespace=self.namespace,
                labels={"app.kubernetes.io/name": "mainloop-credential-reauth"},
            ),
            spec=client.V1JobSpec(
                backoff_limit=0,
                active_deadline_seconds=settings.substrate_reauth_timeout_seconds,
                ttl_seconds_after_finished=3600,
                template=client.V1PodTemplateSpec(
                    metadata=client.V1ObjectMeta(labels={"job-name": job_id}),
                    spec=client.V1PodSpec(
                        restart_policy="Never",
                        automount_service_account_token=False,
                        volumes=[
                            client.V1Volume(
                                name="tmp",
                                empty_dir=client.V1EmptyDirVolumeSource(
                                    medium="Memory", size_limit="64Mi"
                                ),
                            )
                        ],
                        containers=[
                            client.V1Container(
                                name="reauth",
                                image=image,
                                image_pull_policy="IfNotPresent",
                                command=["node", "/usr/local/bin/mainloop-reauth"],
                                args=[provider, job_id],
                                env=[
                                    client.V1EnvVar(
                                        name="MAINLOOP_REAUTH_CALLBACK_URL",
                                        value=settings.substrate_reauth_callback_url,
                                    ),
                                    client.V1EnvVar(
                                        name="MAINLOOP_REAUTH_CALLBACK_TOKEN",
                                        value=callback_token,
                                    ),
                                    client.V1EnvVar(
                                        name="MAINLOOP_REAUTH_TIMEOUT_SECONDS",
                                        value=str(
                                            settings.substrate_reauth_timeout_seconds
                                        ),
                                    ),
                                ],
                                security_context=client.V1SecurityContext(
                                    allow_privilege_escalation=False,
                                    run_as_non_root=True,
                                    run_as_user=10001,
                                    run_as_group=10001,
                                    read_only_root_filesystem=True,
                                    capabilities=client.V1Capabilities(drop=["ALL"]),
                                ),
                                volume_mounts=[
                                    client.V1VolumeMount(
                                        name="tmp",
                                        mount_path="/tmp",  # nosec B108 - memory-backed emptyDir
                                    )
                                ],
                                resources=client.V1ResourceRequirements(
                                    requests={"cpu": "50m", "memory": "128Mi"},
                                    limits={"cpu": "500m", "memory": "512Mi"},
                                ),
                            )
                        ],
                    ),
                ),
            ),
        )
        try:
            batch, _ = self._clients()
            await asyncio.to_thread(batch.create_namespaced_job, self.namespace, body)
        except ApiException as exc:
            raise RuntimeError(
                f"credential re-auth job could not start (status {exc.status})"
            ) from exc
        self._tokens[job_id] = (provider, callback_token)
        return ReauthStatus(job_id, provider, "running")

    async def status(self, job_id: str) -> ReauthStatus | None:
        if not re.fullmatch(r"reauth-(?:codex|claude)-[a-f0-9]{12}", job_id):
            return None
        try:
            batch, core = self._clients()
            job = await asyncio.to_thread(
                batch.read_namespaced_job, job_id, self.namespace
            )
        except ApiException as exc:
            if exc.status == 404:
                return None
            raise RuntimeError(
                f"credential re-auth job status failed (status {exc.status})"
            ) from exc
        provider = "codex" if "-codex-" in job_id else "claude"
        challenge = None
        pods = await asyncio.to_thread(
            core.list_namespaced_pod,
            self.namespace,
            label_selector=f"job-name={job_id}",
        )
        for pod in pods.items:
            if not pod.metadata or not pod.metadata.name:
                continue
            try:
                output = await asyncio.to_thread(
                    core.read_namespaced_pod_log,
                    pod.metadata.name,
                    self.namespace,
                    tail_lines=100,
                )
            except ApiException:
                continue
            challenge = _challenge_from_logs(output)
            if challenge:
                break
        if job.status and job.status.succeeded:
            state = "completed" if job_id in self._completed else "failed"
        elif job.status and job.status.failed:
            state = "failed"
            self._tokens.pop(job_id, None)
        else:
            state = "running"
        return ReauthStatus(job_id, provider, state, challenge)

    async def callback_provider(self, job_id: str, token: str) -> str | None:
        row = self._tokens.get(job_id)
        if row is None or not token or not secrets.compare_digest(row[1], token):
            return None
        return row[0]

    async def complete(self, job_id: str, token: str) -> None:
        row = self._tokens.get(job_id)
        if row and secrets.compare_digest(row[1], token):
            self._tokens.pop(job_id, None)
            self._completed.add(job_id)


def _challenge_from_logs(output: str) -> ReauthChallenge | None:
    for line in output.splitlines()[-100:]:
        if not line.startswith(_CHALLENGE_MARKER):
            continue
        try:
            value = json.loads(line[len(_CHALLENGE_MARKER) :])
            url = value["url"]
            code = value["code"]
            from urllib.parse import urlsplit

            parsed = urlsplit(url)
        except (KeyError, TypeError, ValueError, json.JSONDecodeError):
            return None
        if (
            parsed.scheme == "https"
            and parsed.hostname in _SAFE_AUTH_HOSTS
            and isinstance(code, str)
            and _CHALLENGE_CODE.fullmatch(code)
        ):
            return ReauthChallenge(url, code)
    return None
