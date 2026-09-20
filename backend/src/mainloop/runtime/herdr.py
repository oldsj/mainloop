"""Thin Herdr adapter: drives ``agentctl`` in the workspace pod over Kubernetes pod-exec.

Herdr owns liveness, naming and delivery of input. This adapter never reads a reply from a
terminal; replies, receipts and completion come from the native journals (``journal.py``),
which ``agentctl journal`` prints from the PVC.

Transport: Kubernetes API pod-exec with a Role limited to pods get/list + pods/exec create in
the workspace namespace. ``TransportError`` means the outcome of the call is unknown.
"""

from __future__ import annotations

import asyncio
import json
import logging
import shlex
from dataclasses import dataclass

from kubernetes import client, config
from kubernetes.client.rest import ApiException
from kubernetes.stream import stream
from mainloop.config import settings

logger = logging.getLogger(__name__)


class TransportError(RuntimeError):
    """The exec channel failed; whether the command ran is unknown."""


class WorkspaceUnavailable(RuntimeError):
    """The workspace pod is not Ready; nothing was attempted."""


@dataclass(frozen=True, slots=True)
class ExecResult:
    exit_code: int
    stdout: str
    stderr: str


@dataclass(frozen=True, slots=True)
class PodState:
    name: str
    uid: str | None
    ready: bool


@dataclass(frozen=True, slots=True)
class JournalSlice:
    file: str | None
    total_lines: int
    lines: list[tuple[int, str]]


_api: client.CoreV1Api | None = None


def _core() -> client.CoreV1Api:
    global _api
    if _api is None:
        try:
            config.load_incluster_config()
        except config.ConfigException:
            config.load_kube_config()
        _api = client.CoreV1Api()
    return _api


class HerdrWorkspace:
    """One workspace pod running a Herdr server and ``agentctl``."""

    def __init__(
        self,
        namespace: str | None = None,
        pod: str | None = None,
        container: str = "workspace",
    ):
        self.namespace = namespace or settings.workspace_namespace
        self.pod = pod or settings.workspace_pod
        self.container = container

    # -- transport -------------------------------------------------------------------------
    def _exec_sync(self, command: list[str], timeout: float) -> ExecResult:
        try:
            _core()  # loads the cluster config once
            # stream() swaps the ApiClient request function while it runs, which is not
            # thread-safe: each exec gets its own client so concurrent polls cannot clash.
            resp = stream(
                client.CoreV1Api(client.ApiClient()).connect_get_namespaced_pod_exec,
                self.pod,
                self.namespace,
                container=self.container,
                command=command,
                stderr=True,
                stdin=False,
                stdout=True,
                tty=False,
                _preload_content=False,
            )
            resp.run_forever(timeout=timeout)
            out, err = resp.read_stdout(), resp.read_stderr()
            code = resp.returncode
            resp.close()
        except (
            ApiException,
            OSError,
            RuntimeError,
        ) as exc:  # websocket errors are OSError/Runtime
            raise TransportError(f"exec failed: {type(exc).__name__}") from exc
        if code is None:
            raise TransportError("exec did not report an exit status")
        return ExecResult(int(code), out or "", err or "")

    async def _exec(self, command: list[str], timeout: float = 45) -> ExecResult:
        return await asyncio.to_thread(self._exec_sync, command, timeout)

    async def pod_state(self) -> PodState:
        try:
            pod = await asyncio.to_thread(
                _core().read_namespaced_pod, self.pod, self.namespace
            )
        except ApiException as exc:
            if exc.status == 404:
                return PodState(self.pod, None, False)
            raise TransportError(f"pod read failed: {exc.status}") from exc
        ready = any(
            c.type == "Ready" and c.status == "True"
            for c in (pod.status.conditions or [])
        )
        if pod.metadata.deletion_timestamp is not None:
            ready = False
        return PodState(self.pod, pod.metadata.uid, ready)

    async def require_ready(self) -> PodState:
        state = await self.pod_state()
        if not state.ready:
            raise WorkspaceUnavailable(f"workspace pod {self.pod} is not Ready")
        return state

    # -- agentctl verbs --------------------------------------------------------------------
    async def agent_status(self, name: str) -> dict | None:
        """Herdr liveness hint; ``None`` when Herdr has no such agent."""
        res = await self._exec(["agentctl", "status", name])
        text = res.stdout.strip()
        if res.exit_code != 0 or not text:
            return None
        return json.loads(text.splitlines()[-1])

    async def start(
        self,
        binding: str,
        name: str,
        *,
        native_id: str | None,
        resume: bool,
        extra: dict[str, str] | None = None,
    ) -> dict:
        """Start (or resume) an agent. ``extra`` maps agentctl options (``--cwd-rel``, ``--model``,
        ``--effort``, ``--standing-b64``, ``--token``) to values; secrets travel as argv over the
        authenticated exec channel and are written to 0600 files on the PVC by agentctl.
        """
        args = ["agentctl", "start", binding, "--name", name]
        if native_id:
            args += ["--resume" if resume else "--new-id", native_id]
        for opt, value in (extra or {}).items():
            args += [opt, value]
        res = await self._exec(args, timeout=100)
        if res.exit_code != 0:
            raise RuntimeError(
                f"agent start failed (exit {res.exit_code}): {res.stderr.strip()[-200:]}"
            )
        last = res.stdout.strip().splitlines()[-1]
        return json.loads(last) if last.startswith("{") else {"note": last}

    async def send(self, name: str, text: str) -> None:
        """Deliver one prompt. Raises TransportError if the outcome is unknown; never retries."""
        res = await self._exec(["agentctl", "send", name, text])
        if res.exit_code != 0:
            raise RuntimeError(
                f"send failed (exit {res.exit_code}): {res.stderr.strip()[-200:]}"
            )

    async def stop(self, name: str) -> None:
        res = await self._exec(["agentctl", "stop", name], timeout=60)
        if res.exit_code != 0:
            raise RuntimeError(
                f"agent stop failed (exit {res.exit_code}): {res.stderr.strip()[-200:]}"
            )

    async def native_id(self, name: str) -> str | None:
        res = await self._exec(["agentctl", "native-id", name])
        return res.stdout.strip() or None if res.exit_code == 0 else None

    async def journal(self, name: str, native_id: str, from_line: int) -> JournalSlice:
        res = await self._exec(["agentctl", "journal", name, native_id, str(from_line)])
        if res.exit_code != 0:
            raise TransportError(f"journal read failed (exit {res.exit_code})")
        file = None
        total = from_line
        lines: list[tuple[int, str]] = []
        for raw in res.stdout.split("\n"):
            if not raw:
                continue
            if raw.startswith("#nofile"):
                return JournalSlice(None, 0, [])
            if raw.startswith("#file\t"):
                _, file, count = raw.split("\t")
                total = int(count)
                continue
            num, _, rest = raw.partition("\t")
            if num.isdigit():
                lines.append((int(num), rest))
        return JournalSlice(file, total, lines)


def agentctl_quote(*parts: str) -> str:
    """Only for logging/evidence; commands are passed as argv, never through a shell."""
    return " ".join(shlex.quote(p) for p in parts)
