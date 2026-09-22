"""Thin Substrate adapter: drives ``kubectl ate`` (control-plane CLI over gRPC to
``ate-api-server``) to manage per-session actors as Mainloop workspaces.

Unlike ``herdr.py`` (pod-exec into an already-running workspace), this adapter talks to
Substrate's cluster-level control plane: actors are created, suspended, resumed, reverted and
deleted through ``ateapipb.Control`` (see the pinned checkout's ``pkg/proto/ateapipb/ateapi.proto``
and ``cmd/kubectl-ate/internal/cmd/actor.go``). ``TransportError`` means the outcome of the call
is unknown; callers must inspect the actor before retrying rather than blindly re-creating it.
"""

from __future__ import annotations

import asyncio
import json
import logging
import shlex
from dataclasses import dataclass
from enum import StrEnum

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

    async def _exec(self, args: list[str], timeout: float = 45) -> ExecResult:
        command = self._base_args() + args
        try:
            proc = await asyncio.create_subprocess_exec(
                *command,
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.PIPE,
            )
            try:
                out, err = await asyncio.wait_for(proc.communicate(), timeout=timeout)
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


def cli_quote(*parts: str) -> str:
    """Only for logging/evidence; commands are passed as argv, never through a shell."""
    return " ".join(shlex.quote(p) for p in parts)
