"""Thin Substrate adapter: drives ``kubectl ate`` (control-plane CLI over gRPC to
``ate-api-server``) to manage per-session actors as Mainloop workspaces.

The actor image is designed for headless, per-turn native CLI invocations. This adapter talks to
Substrate's cluster-level control plane:
actors are created, suspended, resumed, reverted and deleted through ``ateapipb.Control`` (see
the pinned checkout's ``pkg/proto/ateapipb/ateapi.proto`` and
``cmd/kubectl-ate/internal/cmd/actor.go``). ``TransportError`` means the outcome of the call is
unknown; callers must inspect the actor before retrying rather than blindly re-creating it.
"""

from __future__ import annotations

import asyncio
import json
import logging
import shlex
import time
from dataclasses import dataclass
from enum import StrEnum
from typing import Callable

from mainloop.config import settings

logger = logging.getLogger(__name__)


class TransportError(RuntimeError):
    """The ``kubectl ate`` call failed or timed out; whether it took effect is unknown."""


class ActorState(StrEnum):
    """Mirrors ``ateapipb.ActorState``; unrecognized strings map to UNSPECIFIED."""

    UNSPECIFIED = "ACTOR_STATE_UNSPECIFIED"
    RESUMING = "ACTOR_STATE_RESUMING"
    RUNNING = "ACTOR_STATE_RUNNING"
    SUSPENDING = "ACTOR_STATE_SUSPENDING"
    SUSPENDED = "ACTOR_STATE_SUSPENDED"
    PAUSING = "ACTOR_STATE_PAUSING"
    PAUSED = "ACTOR_STATE_PAUSED"
    CRASHED = "ACTOR_STATE_CRASHED"
    DELETING = "ACTOR_STATE_DELETING"
    REVERTING = "ACTOR_STATE_REVERTING"

    @classmethod
    def parse(cls, raw: str | None) -> "ActorState":
        try:
            return cls(raw)
        except ValueError:
            return cls.UNSPECIFIED


# ActorState -> WorkspaceBinding.observed_state (models.native_agent). Only RUNNING is ready;
# terminal/absent states are unavailable; states mid-transition or unrecognized are unknown.
OBSERVED_STATE = {
    ActorState.RUNNING: "ready",
    ActorState.SUSPENDED: "unavailable",
    ActorState.PAUSED: "unavailable",
    ActorState.CRASHED: "unavailable",
    ActorState.DELETING: "unavailable",
    ActorState.RESUMING: "unknown",
    ActorState.SUSPENDING: "unknown",
    ActorState.PAUSING: "unknown",
    ActorState.REVERTING: "unknown",
    ActorState.UNSPECIFIED: "unknown",
}


@dataclass(frozen=True, slots=True)
class ExecResult:
    exit_code: int
    stdout: str
    stderr: str


@dataclass(frozen=True, slots=True)
class ActorRecord:
    """Parsed subset of an ``ateapipb.Actor`` (protojson via ``kubectl ate ... -o json``)."""

    atespace: str
    name: str
    uid: str | None
    state: ActorState
    external_snapshot_uri: str | None
    current_actor_template_uid: str | None
    raw: dict


def _actor_from_json(doc: dict) -> ActorRecord:
    metadata = doc.get("metadata") or {}
    status = doc.get("status") or {}
    snapshot = status.get("externalSnapshot") or {}
    return ActorRecord(
        atespace=metadata.get("atespace", ""),
        name=metadata.get("name", ""),
        uid=metadata.get("uid"),
        state=ActorState.parse(status.get("state")),
        external_snapshot_uri=snapshot.get("snapshotUri"),
        current_actor_template_uid=status.get("currentActorTemplateUid"),
        raw=doc,
    )


class GoldenState(StrEnum):
    """Mirrors the ``ateapipb.GoldenSnapshotStatus`` lifecycle for an ActorTemplate."""

    PENDING = "pending"
    READY = "ready"
    FAILED = "failed"


@dataclass(frozen=True, slots=True)
class ActorTemplateRecord:
    """Parsed subset of an ``ateapipb.ActorTemplate``, including golden-snapshot status."""

    atespace: str
    name: str
    uid: str | None
    golden_state: GoldenState
    golden_tag: str | None
    error_message: str
    raw: dict


def _actor_template_from_json(doc: dict) -> ActorTemplateRecord:
    metadata = doc.get("metadata") or {}
    status = doc.get("status") or {}
    golden = status.get("goldenSnapshotStatus") or {}
    error_message = golden.get("errorMessage", "")
    golden_tag_ref = golden.get("goldenTag")
    golden_tag = golden_tag_ref.get("name") if golden_tag_ref else None
    if error_message:
        golden_state = GoldenState.FAILED
    elif golden_tag:
        golden_state = GoldenState.READY
    else:
        golden_state = GoldenState.PENDING
    return ActorTemplateRecord(
        atespace=metadata.get("atespace", ""),
        name=metadata.get("name", ""),
        uid=metadata.get("uid"),
        golden_state=golden_state,
        golden_tag=golden_tag,
        error_message=error_message,
        raw=doc,
    )


class WaitTimeout(RuntimeError):
    """A bounded poll reached its deadline without observing a terminal outcome. Callers
    must treat this as failure, never as an implied success."""


class GoldenSnapshotFailed(RuntimeError):
    """The template controller reported an error while building the golden snapshot."""


class GoldenSnapshotTimeout(WaitTimeout):
    """The golden snapshot did not reach a terminal state within the bound. Not success."""


class NoEligibleWorker(WaitTimeout):
    """No worker registered for the atespace within the bound. Not success."""


class IdentityOutcome(StrEnum):
    """Result of reconciling a persisted actor UID against the cluster's current state,
    before a retry decides whether to create, resume, or refuse to touch an actor."""

    ABSENT = "absent"  # no live actor with this name; safe to create fresh
    UNOWNED = "unowned"  # live actor exists but no actor uid was persisted
    MATCHES = "matches"  # live actor's uid matches the persisted identity
    DIVERGED = "diverged"  # live actor exists under this name with a different uid


class IdentityConflict(RuntimeError):
    """A live actor exists under the expected name but with a different uid than the
    identity persisted from a prior run. Recreating or resuming it blindly could operate
    on someone else's actor; this must be surfaced, not silently resolved."""


def reconcile_actor_identity(
    persisted_uid: str | None, live: ActorRecord | None
) -> IdentityOutcome:
    if live is None:
        return IdentityOutcome.ABSENT
    if persisted_uid is None:
        return IdentityOutcome.UNOWNED
    if persisted_uid == live.uid:
        return IdentityOutcome.MATCHES
    return IdentityOutcome.DIVERGED


class SubstrateControl:
    """Wraps ``kubectl ate`` for one (kubeconfig, context) pair. No retries, no caching."""

    def __init__(
        self,
        *,
        kubeconfig: str | None = None,
        context: str | None = None,
        cli: str | None = None,
    ):
        self.kubeconfig = (
            kubeconfig if kubeconfig is not None else settings.substrate_kubeconfig
        )
        self.context = context if context is not None else settings.substrate_context
        self.cli = cli or settings.substrate_cli

    def _base_args(self) -> list[str]:
        args = [self.cli]
        if self.kubeconfig:
            args += ["--kubeconfig", self.kubeconfig]
        if self.context:
            args += ["--context", self.context]
        return args

    async def _exec(
        self, args: list[str], timeout: float = 45, stdin: str | None = None
    ) -> ExecResult:
        command = self._base_args() + args
        try:
            proc = await asyncio.create_subprocess_exec(
                *command,
                stdin=asyncio.subprocess.PIPE if stdin is not None else None,
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.PIPE,
            )
            try:
                out, err = await asyncio.wait_for(
                    proc.communicate(stdin.encode() if stdin is not None else None),
                    timeout=timeout,
                )
            except TimeoutError as exc:
                proc.kill()
                await proc.wait()
                raise TransportError(
                    f"{cli_quote(*command)} timed out after {timeout}s"
                ) from exc
        except OSError as exc:
            raise TransportError(f"exec failed: {type(exc).__name__}: {exc}") from exc
        if proc.returncode is None:
            raise TransportError("kubectl ate did not report an exit status")
        return ExecResult(
            proc.returncode, out.decode(errors="replace"), err.decode(errors="replace")
        )

    async def get_actor(self, atespace: str, name: str) -> ActorRecord | None:
        res = await self._exec(
            ["get", "actor", name, "--atespace", atespace, "-o", "json"]
        )
        if res.exit_code != 0:
            if "not found" in res.stderr.lower() or "NotFound" in res.stderr:
                return None
            raise TransportError(
                f"get actor failed (exit {res.exit_code}): {res.stderr.strip()[-300:]}"
            )
        text = res.stdout.strip()
        if not text:
            return None
        return _actor_from_json(json.loads(text))

    async def create_actor(
        self, atespace: str, name: str, *, template: str, tag: str | None = None
    ) -> ActorRecord:
        args = [
            "create",
            "actor",
            name,
            "--atespace",
            atespace,
            "--template",
            template,
            "-o",
            "json",
        ]
        if tag:
            args += ["--tag", tag]
        res = await self._exec(args, timeout=90)
        if res.exit_code != 0:
            raise RuntimeError(
                f"create actor failed (exit {res.exit_code}): {res.stderr.strip()[-300:]}"
            )
        return await self._require(atespace, name, res)

    async def resume_actor(self, atespace: str, name: str) -> ActorRecord:
        res = await self._exec(
            ["resume", "actor", name, "--atespace", atespace, "-o", "json"], timeout=90
        )
        if res.exit_code != 0:
            raise RuntimeError(
                f"resume actor failed (exit {res.exit_code}): {res.stderr.strip()[-300:]}"
            )
        return await self._require(atespace, name, res)

    async def suspend_actor(self, atespace: str, name: str) -> ActorRecord:
        res = await self._exec(
            ["suspend", "actor", name, "--atespace", atespace, "-o", "json"], timeout=90
        )
        if res.exit_code != 0:
            raise RuntimeError(
                f"suspend actor failed (exit {res.exit_code}): {res.stderr.strip()[-300:]}"
            )
        return await self._require(atespace, name, res)

    async def revert_actor(self, atespace: str, name: str) -> ActorRecord:
        """Discard current execution, roll back to the last completed snapshot. Explicit only:
        callers must have surfaced the possible loss of unsnapshotted work before calling this.
        """
        res = await self._exec(
            ["revert", "actor", name, "--atespace", atespace, "-o", "json"], timeout=90
        )
        if res.exit_code != 0:
            raise RuntimeError(
                f"revert actor failed (exit {res.exit_code}): {res.stderr.strip()[-300:]}"
            )
        return await self._require(atespace, name, res)

    async def delete_actor(
        self, atespace: str, name: str, *, any_state: bool = False
    ) -> None:
        args = ["delete", "actor", name, "--atespace", atespace]
        if any_state:
            args.append("--any-state")
        res = await self._exec(args, timeout=60)
        if res.exit_code != 0 and "not found" not in res.stderr.lower():
            raise RuntimeError(
                f"delete actor failed (exit {res.exit_code}): {res.stderr.strip()[-300:]}"
            )

    async def _require(self, atespace: str, name: str, res: ExecResult) -> ActorRecord:
        text = res.stdout.strip()
        if text:
            return _actor_from_json(json.loads(text))
        # Some verbs may not echo the actor; read it back rather than guessing its state.
        actor = await self.get_actor(atespace, name)
        if actor is None:
            raise TransportError(f"actor {atespace}/{name} not found after operation")
        return actor

    async def atespace_exists(self, name: str) -> bool:
        res = await self._exec(["get", "atespaces", name, "-o", "json"])
        if res.exit_code == 0:
            return True
        if "not found" in res.stderr.lower():
            return False
        raise TransportError(
            f"get atespace failed (exit {res.exit_code}): {res.stderr.strip()[-300:]}"
        )

    async def ensure_atespace(self, name: str) -> None:
        """Register the atespace via the control-plane API. Idempotent: a Kubernetes
        Namespace of the same name is a separate, unrelated object and does not register
        an atespace, so this call is required before an ActorTemplate or actor can be
        created in it (``create actor-template``/``create actor`` require it to exist).
        """
        res = await self._exec(["create", "atespace", name, "-o", "json"])
        if res.exit_code == 0:
            return
        if "already exists" in res.stderr.lower() or "AlreadyExists" in res.stderr:
            return
        raise TransportError(
            f"create atespace failed (exit {res.exit_code}): {res.stderr.strip()[-300:]}"
        )

    async def get_actor_template(
        self, atespace: str, name: str
    ) -> ActorTemplateRecord | None:
        res = await self._exec(
            ["get", "actor-template", name, "-a", atespace, "-o", "json"]
        )
        if res.exit_code != 0:
            if "not found" in res.stderr.lower() or "NotFound" in res.stderr:
                return None
            raise TransportError(
                f"get actor-template failed (exit {res.exit_code}): "
                f"{res.stderr.strip()[-300:]}"
            )
        text = res.stdout.strip()
        if not text:
            return None
        return _actor_template_from_json(json.loads(text))

    async def create_actor_template(
        self, atespace: str, name: str, manifest: str
    ) -> ActorTemplateRecord:
        """``manifest`` is the protojson-shaped ActorTemplate document; its own
        ``metadata.atespace``/``metadata.name`` select where it is created. The atespace
        must already exist (``ensure_atespace``); templates are immutable, so a changed
        manifest needs a new name, never a reused one. The pinned CLI prints a table by
        default, so JSON is requested explicitly. If the create result is uncertain, read
        the named template back before reporting failure; a later invocation also starts
        with that read and cannot blindly repeat the create."""
        try:
            res = await self._exec(
                ["create", "actor-template", "-f", "-", "-o", "json"],
                stdin=manifest,
            )
        except TransportError as create_error:
            return await self._read_uncertain_actor_template_create(
                atespace, name, create_error
            )

        text = res.stdout.strip()
        if res.exit_code != 0:
            reason = (
                f"create actor-template failed (exit {res.exit_code}): "
                f"{res.stderr.strip()[-300:]}"
            )
            return await self._read_uncertain_actor_template_create(
                atespace, name, TransportError(reason)
            )
        if not text:
            return await self._read_uncertain_actor_template_create(
                atespace,
                name,
                TransportError("create actor-template returned no output"),
            )
        try:
            return _actor_template_from_json(json.loads(text))
        except (json.JSONDecodeError, TypeError) as parse_error:
            return await self._read_uncertain_actor_template_create(
                atespace,
                name,
                TransportError(
                    f"create actor-template returned invalid JSON: {parse_error}"
                ),
            )

    async def _read_uncertain_actor_template_create(
        self, atespace: str, name: str, create_error: TransportError
    ) -> ActorTemplateRecord:
        try:
            existing = await self.get_actor_template(atespace, name)
        except TransportError as read_error:
            raise TransportError(
                f"actor-template create outcome is uncertain and read-back failed: "
                f"{read_error}"
            ) from create_error
        if existing is not None:
            return existing
        raise create_error

    async def get_eligible_workers(
        self, namespace: str, selector: str, sandbox_class: str
    ) -> int:
        """Count active workers with free actor capacity matching this WorkerPool.

        The pinned CLI's ``-a`` filter means "already hosting an actor in an atespace",
        so it excludes the idle worker needed before the first actor exists. Namespace,
        pool labels, and sandbox class identify the intended pool instead.
        """
        res = await self._exec(
            [
                "get",
                "workers",
                "-n",
                namespace,
                "-l",
                selector,
                "--sandbox-class",
                sandbox_class,
                "-o",
                "json",
            ]
        )
        if res.exit_code != 0:
            raise TransportError(
                f"get workers failed (exit {res.exit_code}): {res.stderr.strip()[-300:]}"
            )
        text = res.stdout.strip()
        if not text:
            return 0
        try:
            doc = json.loads(text)
        except json.JSONDecodeError as exc:
            raise TransportError(f"get workers returned invalid JSON: {exc}") from exc
        if not isinstance(doc, dict):
            raise TransportError("get workers JSON is missing its 'workers' list")
        # The pinned CLI uses protojson's default omission behavior: a valid
        # ListWorkers response with zero matches is `{}`, not `{"workers": []}`.
        # Treat only that empty object as the empty list; a non-empty object
        # without the field is still a contract error.
        if "workers" not in doc:
            if doc:
                raise TransportError("get workers JSON is missing its 'workers' list")
            workers = []
        else:
            workers = doc["workers"]
        if not isinstance(workers, list):
            raise TransportError("get workers JSON field 'workers' is not a list")
        return sum(
            _worker_is_eligible(
                worker,
                namespace=namespace,
                selector=selector,
                sandbox_class=sandbox_class,
            )
            for worker in workers
        )


def cli_quote(*parts: str) -> str:
    """Only for logging/evidence; commands are passed as argv, never through a shell."""
    return " ".join(shlex.quote(p) for p in parts)


def _worker_is_eligible(
    worker: object, *, namespace: str, selector: str, sandbox_class: str
) -> bool:
    if not isinstance(worker, dict):
        return False
    if worker.get("workerNamespace") != namespace:
        return False
    if worker.get("sandboxClass") != sandbox_class:
        return False
    key, separator, value = selector.partition("=")
    if not separator or not key or not value:
        raise ValueError(f"unsupported worker label selector: {selector!r}")
    labels = worker.get("labels") or {}
    if not isinstance(labels, dict) or labels.get(key) != value:
        return False

    status = worker.get("status") or {}
    if not isinstance(status, dict) or status.get("state") != "WORKER_STATE_ACTIVE":
        return False
    capacity = status.get("capacity") or {}
    allocated = status.get("allocated") or {}
    if isinstance(capacity, dict) and capacity.get("actors") is not None:
        allocated_actors = (
            allocated.get("actors", 0) if isinstance(allocated, dict) else 0
        )
        try:
            if int(capacity["actors"]) - int(allocated_actors or 0) <= 0:
                return False
        except (TypeError, ValueError) as exc:
            raise TransportError("worker actor capacity is not an integer") from exc
    return True


async def wait_for_golden_snapshot(
    control: SubstrateControl,
    atespace: str,
    name: str,
    *,
    timeout_s: float,
    poll_interval_s: float = 3,
    sleep=asyncio.sleep,
    clock=time.monotonic,
) -> ActorTemplateRecord:
    """Poll an ActorTemplate's golden-snapshot status to a terminal outcome. Raises
    ``GoldenSnapshotFailed``/``GoldenSnapshotTimeout`` rather than returning normally on
    anything but a completed golden tag -- a caller must never infer success from a log
    line or from reaching this function without an exception being possible."""
    deadline = clock() + timeout_s
    while True:
        record = await control.get_actor_template(atespace, name)
        if record is None:
            raise TransportError(
                f"actor-template {atespace}/{name} disappeared while awaiting its "
                "golden snapshot"
            )
        if record.golden_state is GoldenState.FAILED:
            raise GoldenSnapshotFailed(
                f"golden snapshot for {atespace}/{name} failed: {record.error_message}"
            )
        if record.golden_state is GoldenState.READY:
            return record
        if clock() >= deadline:
            raise GoldenSnapshotTimeout(
                f"golden snapshot for {atespace}/{name} did not complete within "
                f"{timeout_s}s (last state: {record.golden_state.value})"
            )
        await sleep(poll_interval_s)


async def wait_for_eligible_worker(
    control: SubstrateControl,
    namespace: str,
    selector: str,
    sandbox_class: str,
    *,
    timeout_s: float,
    poll_interval_s: float = 3,
    sleep=asyncio.sleep,
    clock=time.monotonic,
) -> int:
    """Poll for at least one worker matching the intended WorkerPool. Raises on timeout rather
    than proceeding to create an actor-template/actor against a pool with no capacity.
    """
    deadline = clock() + timeout_s
    while True:
        count = await control.get_eligible_workers(namespace, selector, sandbox_class)
        if count > 0:
            return count
        if clock() >= deadline:
            raise NoEligibleWorker(
                f"no eligible worker for namespace={namespace}, selector={selector}, "
                f"sandbox_class={sandbox_class} within {timeout_s}s"
            )
        await sleep(poll_interval_s)


class ActorHealthTimeout(WaitTimeout):
    """The actor route did not return a healthy response within the readiness bound."""


async def wait_for_actor_health(
    health_check: Callable[[], bool],
    *,
    timeout_s: float,
    poll_interval_s: float = 2,
    sleep=asyncio.sleep,
    clock=time.monotonic,
) -> None:
    """Wait for a live actor-route ``/healthz`` check to succeed.

    A restored actor need not replay startup logs, so readiness is based on current service
    health rather than a boot marker.
    """
    deadline = clock() + timeout_s
    while True:
        if health_check():
            return
        if clock() >= deadline:
            raise ActorHealthTimeout(
                f"actor /healthz did not return 200 within {timeout_s}s"
            )
        await sleep(poll_interval_s)


class ActorFailedToStart(RuntimeError):
    """An actor reached a terminal, non-running state (e.g. CRASHED) while awaiting
    readiness. Distinct from ``WaitTimeout``: this is a reported failure, not a bound
    running out."""


_ACTOR_TERMINAL_FAILURE_STATES = frozenset({ActorState.CRASHED, ActorState.DELETING})


async def wait_for_actor_running(
    control: SubstrateControl,
    atespace: str,
    name: str,
    *,
    timeout_s: float,
    poll_interval_s: float = 2,
    sleep=asyncio.sleep,
    clock=time.monotonic,
) -> ActorRecord:
    """Poll an actor to RUNNING. Raises ``ActorFailedToStart`` on a terminal failure state
    and ``WaitTimeout`` on exceeding the bound -- never returns normally except on RUNNING,
    so a caller can never mistake "still starting" for success."""
    deadline = clock() + timeout_s
    while True:
        actor = await control.get_actor(atespace, name)
        if actor is None:
            raise TransportError(
                f"actor {atespace}/{name} disappeared while awaiting readiness"
            )
        if actor.state is ActorState.RUNNING:
            return actor
        if actor.state in _ACTOR_TERMINAL_FAILURE_STATES:
            raise ActorFailedToStart(
                f"actor {atespace}/{name} reached {actor.state.value} while awaiting readiness"
            )
        if clock() >= deadline:
            raise WaitTimeout(
                f"actor {atespace}/{name} did not reach RUNNING within {timeout_s}s "
                f"(last state: {actor.state.value})"
            )
        await sleep(poll_interval_s)
