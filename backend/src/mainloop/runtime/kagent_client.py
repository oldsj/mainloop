"""Client for the kagent gateway: SessionService (Session lifecycle) and A2A v1 (turns).

Mainloop's control plane owns messages, delivery state and attention. kagent owns the native
agent session. This module is the only place that speaks to it:

- **SessionService** is gRPC. Only a handful of tiny messages are needed, so they are hand-encoded
  and sent as ``application/grpc-web+proto`` over HTTP/1.1 instead of pulling in a gRPC stack.
- **A2A v1 JSON-RPC** carries turns. ``SendStreamingMessage`` streams task events as SSE;
  ``GetTask``/``ListTasks``/``CancelTask``/``SubscribeToTask`` observe and cancel.

Delivery rules this client enforces (the caller keeps the ledger):

- A message is identified by its ``messageId`` (the Mainloop message id). It is never sent twice
  by this client except for ``KAGENT_SEND_NOT_ACCEPTED``, which kagent documents as "nothing was
  accepted, retry the same message". The retry reuses the same ``messageId`` and body.
- Any other ambiguous outcome (stream cut, timeout after the request left) raises
  :class:`OutcomeUnknown`. The caller resolves it by observation (``get_task``, ``list_tasks``
  matching ``history[].messageId``, ``subscribe_to_task``), never by re-sending.
- There is no event cursor. After a reconnect the current task replaces the projection
  (:meth:`TaskProjection.replace`); it is not merged with what was seen before.
"""

from __future__ import annotations

import asyncio
import json
import logging
import struct
import time
import uuid
from collections.abc import AsyncIterator, Awaitable, Callable
from dataclasses import dataclass, field
from enum import IntEnum
from typing import Any

import httpx
from pydantic import BaseModel, ConfigDict, Field
from pydantic.alias_generators import to_camel

from models.hitl import HITL_EXTENSION, HITLResponse


@dataclass(frozen=True, slots=True)
class SessionCredential:
    origin: str
    header: str
    secret_name: str
    secret_key: str

    def encode(self) -> bytes:
        secret = _field_str(1, self.secret_name) + _field_str(2, self.secret_key)
        return (
            _field_str(1, self.origin)
            + _field_str(2, self.header)
            + _field_bytes(3, secret)
        )


logger = logging.getLogger(__name__)

# --------------------------------------------------------------------------------------------
# Errors
# --------------------------------------------------------------------------------------------

A2A_ERROR_DOMAIN = "a2a-protocol.org"
SEND_NOT_ACCEPTED = "KAGENT_SEND_NOT_ACCEPTED"
TASK_NOT_FOUND = "TASK_NOT_FOUND"


class KagentError(Exception):
    """A kagent call failed with a definite outcome (the request was rejected or not made)."""


class Unreachable(KagentError):
    """The gateway could not be reached. The request never left, so nothing was sent."""


class OutcomeUnknown(KagentError):
    """The request may have been accepted but its outcome was not observed. Never re-send."""


class A2AError(KagentError):
    """A JSON-RPC error from the A2A endpoint, with its ``google.rpc.ErrorInfo`` if present."""

    def __init__(
        self,
        code: int,
        message: str,
        *,
        domain: str | None = None,
        reason: str | None = None,
        metadata: dict[str, str] | None = None,
    ):
        super().__init__(f"A2A error {code}: {message}")
        self.code = code
        self.message = message
        self.domain = domain
        self.reason = reason
        self.metadata = metadata or {}

    @property
    def retry_after_seconds(self) -> float:
        try:
            return max(0.0, float(self.metadata.get("retryAfterMs", "100")) / 1000.0)
        except ValueError:
            return 0.1


class SendNotAccepted(A2AError):
    """kagent did not accept the message. Retrying the same message is safe."""


class TaskNotFound(A2AError):
    pass


class SessionError(KagentError):
    """A SessionService call failed or the Session is in a state that cannot take a turn."""

    def __init__(self, message: str, *, grpc_status: int | None = None):
        super().__init__(message)
        self.grpc_status = grpc_status


def a2a_error_from_json(error: dict[str, Any]) -> A2AError:
    """Classify a JSON-RPC error object by its ErrorInfo, never by the JSON-RPC code."""
    code = error.get("code") if isinstance(error.get("code"), int) else -32603
    message = str(error.get("message") or "")
    domain = reason = None
    metadata: dict[str, str] = {}
    details = error.get("data")
    for item in details if isinstance(details, list) else []:
        if isinstance(item, dict) and str(item.get("@type", "")).endswith(
            "google.rpc.ErrorInfo"
        ):
            domain = item.get("domain")
            reason = item.get("reason")
            raw = item.get("metadata")
            if isinstance(raw, dict):
                metadata = {str(k): str(v) for k, v in raw.items()}
            break
    cls: type[A2AError] = A2AError
    # kagent reports the rejection as an UNSUPPORTED_OPERATION whose own reason travels in
    # ``ErrorInfo.metadata.reason``; the top-level ``reason`` is the generic A2A one.
    if domain == A2A_ERROR_DOMAIN and metadata.get("reason") == SEND_NOT_ACCEPTED:
        cls = SendNotAccepted
    elif reason == TASK_NOT_FOUND:
        cls = TaskNotFound
    return cls(code, message, domain=domain, reason=reason, metadata=metadata)


# --------------------------------------------------------------------------------------------
# A2A models. Only what Mainloop reads; unknown fields are ignored.
# --------------------------------------------------------------------------------------------


class _Wire(BaseModel):
    model_config = ConfigDict(
        alias_generator=to_camel, populate_by_name=True, extra="ignore"
    )


class Part(_Wire):
    text: str | None = None
    data: Any = None
    metadata: dict[str, Any] | None = None


class Message(_Wire):
    message_id: str = ""
    context_id: str | None = None
    task_id: str | None = None
    role: str | None = None
    parts: list[Part] = Field(default_factory=list)
    metadata: dict[str, Any] = Field(default_factory=dict)
    extensions: list[str] = Field(default_factory=list)

    @property
    def text(self) -> str:
        return "".join(p.text for p in self.parts if p.text)


class TaskStatus(_Wire):
    state: str = ""
    timestamp: str | None = None
    message: Message | None = None


class Artifact(_Wire):
    artifact_id: str = ""
    name: str | None = None
    parts: list[Part] = Field(default_factory=list)
    metadata: dict[str, Any] | None = None

    @property
    def text(self) -> str:
        return "".join(p.text for p in self.parts if p.text)

    @property
    def position(self) -> str:
        value = (self.metadata or {}).get("kagent.dev/a2a/timeline-position")
        return value if isinstance(value, str) else ""


class Task(_Wire):
    id: str
    context_id: str | None = None
    status: TaskStatus = Field(default_factory=TaskStatus)
    artifacts: list[Artifact] = Field(default_factory=list)
    history: list[Message] = Field(default_factory=list)
    metadata: dict[str, Any] | None = None


class StatusUpdate(_Wire):
    task_id: str
    context_id: str | None = None
    status: TaskStatus = Field(default_factory=TaskStatus)


class ArtifactUpdate(_Wire):
    task_id: str
    context_id: str | None = None
    artifact: Artifact
    append: bool = False
    last_chunk: bool = False


class StreamEvent(_Wire):
    """One JSON-RPC result from a stream: exactly one of the four members is set."""

    task: Task | None = None
    status_update: StatusUpdate | None = None
    artifact_update: ArtifactUpdate | None = None
    message: Message | None = None

    @property
    def task_id(self) -> str | None:
        if self.task:
            return self.task.id
        if self.status_update:
            return self.status_update.task_id
        if self.artifact_update:
            return self.artifact_update.task_id
        if self.message:
            return self.message.task_id
        return None


def normalise_state(state: str) -> str:
    """``TASK_STATE_INPUT_REQUIRED`` / ``input-required`` -> ``input_required``."""
    value = state.removeprefix("TASK_STATE_").lower().replace("-", "_")
    return value or "unspecified"


TERMINAL_STATES = frozenset({"completed", "canceled", "failed", "rejected"})
# Waiting for someone other than the agent. Not terminal: the task is still the session's one
# non-quiescent task, so it blocks new turns until it is answered or cancelled.
PARKED_STATES = frozenset({"input_required", "auth_required"})


def is_terminal(state: str) -> bool:
    return normalise_state(state) in TERMINAL_STATES


def is_parked(state: str) -> bool:
    return normalise_state(state) in PARKED_STATES


_NS = uuid.UUID("0d6a3f3e-5a91-4c0f-9a5e-3a6b1d0c7e42")


def assistant_message_id(session_id: str, task_id: str) -> str:
    """Deterministic id of the mirrored assistant reply for one task."""
    return str(uuid.uuid5(_NS, f"assistant:{session_id}:{task_id}"))


# --------------------------------------------------------------------------------------------
# Task projection
# --------------------------------------------------------------------------------------------


@dataclass
class TaskProjection:
    """Mainloop's view of one task, built from stream events and replaced by snapshots.

    ``replace`` is the reconnect path: kagent has no event cursor, so a snapshot (``GetTask`` or
    the first event of ``SubscribeToTask``) supersedes everything accumulated so far.
    """

    task_id: str | None = None
    context_id: str | None = None
    state: str = ""
    failure_text: str = ""
    artifacts: dict[str, Artifact] = field(default_factory=dict)
    history_message_ids: list[str] = field(default_factory=list)
    status_message: Message | None = None

    @property
    def terminal(self) -> bool:
        return bool(self.state) and is_terminal(self.state)

    @property
    def parked(self) -> bool:
        return bool(self.state) and is_parked(self.state)

    @property
    def normalised_state(self) -> str:
        return normalise_state(self.state) if self.state else ""

    @property
    def text(self) -> str:
        """The agent's reply: text parts of the artifacts, in timeline order."""
        ordered = sorted(
            enumerate(self.artifacts.values()), key=lambda i: (i[1].position, i[0])
        )
        return "\n\n".join(t for _, a in ordered if (t := a.text.strip()))

    def replace(self, task: Task) -> None:
        self.task_id = task.id
        self.context_id = task.context_id
        self.status_message = task.status.message
        self.state = task.status.state
        self.failure_text = task.status.message.text if task.status.message else ""
        self.artifacts = {a.artifact_id: a for a in task.artifacts}
        self.history_message_ids = [m.message_id for m in task.history]

    def apply(self, event: StreamEvent) -> bool:
        """Fold one stream event in. Returns whether the projection changed."""
        if event.task is not None:
            before = (
                self.task_id,
                self.state,
                self.text,
                self.failure_text,
                self.status_message,
            )
            self.replace(event.task)
            return before != (
                self.task_id,
                self.state,
                self.text,
                self.failure_text,
                self.status_message,
            )
        if event.status_update is not None:
            update = event.status_update
            self.task_id = self.task_id or update.task_id
            self.context_id = self.context_id or update.context_id
            changed = (
                update.status.state != self.state
                or update.status.message != self.status_message
            )
            self.status_message = update.status.message
            self.state = update.status.state or self.state
            if update.status.message is not None:
                self.failure_text = update.status.message.text
            return changed
        if event.artifact_update is not None:
            update = event.artifact_update
            self.task_id = self.task_id or update.task_id
            artifact = update.artifact
            existing = self.artifacts.get(artifact.artifact_id)
            if update.append and existing is not None:
                existing.parts.extend(artifact.parts)
            else:
                self.artifacts[artifact.artifact_id] = artifact
            return True
        return False


# --------------------------------------------------------------------------------------------
# protobuf / grpc-web (just enough for SessionService)
# --------------------------------------------------------------------------------------------


def _varint(value: int) -> bytes:
    out = bytearray()
    while True:
        bits = value & 0x7F
        value >>= 7
        if value:
            out.append(bits | 0x80)
        else:
            out.append(bits)
            return bytes(out)


def _field_bytes(number: int, payload: bytes) -> bytes:
    return _varint(number << 3 | 2) + _varint(len(payload)) + payload


def _field_varint(number: int, value: int) -> bytes:
    return _varint(number << 3) + _varint(value)


def _field_str(number: int, value: str) -> bytes:
    return _field_bytes(number, value.encode()) if value else b""


def _read_varint(buf: bytes, pos: int) -> tuple[int, int]:
    shift = result = 0
    while True:
        if pos >= len(buf):
            raise ValueError("truncated varint")
        byte = buf[pos]
        pos += 1
        result |= (byte & 0x7F) << shift
        if not byte & 0x80:
            return result, pos
        shift += 7


def decode_fields(buf: bytes) -> dict[int, list[int | bytes]]:
    """Decode a protobuf message into ``{field number: [values]}`` (varints and byte strings)."""
    fields: dict[int, list[int | bytes]] = {}
    pos = 0
    while pos < len(buf):
        key, pos = _read_varint(buf, pos)
        number, wire = key >> 3, key & 7
        value: int | bytes
        if wire == 0:
            value, pos = _read_varint(buf, pos)
        elif wire == 2:
            length, pos = _read_varint(buf, pos)
            if pos + length > len(buf):
                raise ValueError("truncated field")
            value = buf[pos : pos + length]
            pos += length
        elif wire == 1:
            value, pos = buf[pos : pos + 8], pos + 8
        elif wire == 5:
            value, pos = buf[pos : pos + 4], pos + 4
        else:
            raise ValueError(f"unsupported wire type {wire}")
        fields.setdefault(number, []).append(value)
    return fields


def _text(fields: dict[int, list[int | bytes]], number: int) -> str:
    value = fields.get(number, [b""])[0]
    return value.decode() if isinstance(value, bytes) else ""


def _number(fields: dict[int, list[int | bytes]], number: int) -> int:
    value = fields.get(number, [0])[0]
    return value if isinstance(value, int) else 0


def grpc_web_frame(message: bytes) -> bytes:
    return b"\x00" + struct.pack(">I", len(message)) + message


def parse_grpc_web(body: bytes) -> tuple[list[bytes], dict[str, str]]:
    """Split a grpc-web response into data messages and the trailer headers."""
    messages: list[bytes] = []
    trailers: dict[str, str] = {}
    pos = 0
    while pos + 5 <= len(body):
        flag = body[pos]
        (length,) = struct.unpack(">I", body[pos + 1 : pos + 5])
        payload = body[pos + 5 : pos + 5 + length]
        if len(payload) != length:
            raise ValueError("truncated grpc-web frame")
        pos += 5 + length
        if flag & 0x80:
            for line in payload.decode(errors="replace").split("\r\n"):
                name, sep, value = line.partition(":")
                if sep:
                    trailers[name.strip().lower()] = value.strip()
        else:
            messages.append(payload)
    return messages, trailers


class RuntimeState(IntEnum):
    UNSPECIFIED = 0
    CREATING = 1
    READY = 2
    SUSPENDED = 3
    FAILED = 4
    DELETING = 5
    DELETED = 6


class RuntimeOperation(IntEnum):
    UNSPECIFIED = 0
    CREATE = 1
    SUSPEND = 2
    RESUME = 3
    DELETE = 4
    NONE = 5


@dataclass(frozen=True)
class AgentRef:
    namespace: str
    name: str

    def encode(self) -> bytes:
        return _field_str(1, self.namespace) + _field_str(2, self.name)

    @property
    def path(self) -> str:
        return f"/agents/{self.namespace}/{self.name}"


@dataclass(frozen=True)
class SessionWorkspace:
    """The repository kagent clones into the harness before the first turn.

    ``repo`` is an https URL without credentials whose host the Agent's harness allows
    (``spec.git.origins``). ``ref`` is the branch, tag or commit to check out; ``branch`` is
    the local branch to create or switch to; ``depth`` 0 means the server default. kagent
    persists it on the Session and compares it when a create is retried under the same
    request id, so a replacement Session must resend exactly this value.
    """

    repo: str
    ref: str = ""
    branch: str = ""
    depth: int = 0

    def encode(self) -> bytes:
        message = (
            _field_str(1, self.repo)
            + _field_str(2, self.ref)
            + _field_str(3, self.branch)
        )
        if self.depth:
            message += _varint(4 << 3) + _varint(self.depth)
        return message

    @classmethod
    def decode(cls, raw: bytes) -> "SessionWorkspace":
        fields = decode_fields(raw)
        return cls(
            repo=_text(fields, 1),
            ref=_text(fields, 2),
            branch=_text(fields, 3),
            depth=_number(fields, 4),
        )


@dataclass(frozen=True)
class KagentSession:
    """The Session fields Mainloop uses. ``id`` is also the A2A ``contextId``."""

    id: str
    state: RuntimeState
    operation: RuntimeOperation
    context_id: str
    failure_reason: str = ""
    failure_message: str = ""
    name: str = ""
    workspace: SessionWorkspace | None = None
    creator: str = ""
    agent: AgentRef | None = None
    prepared_revision: str = ""
    a2a_authority: str = ""

    @property
    def settled(self) -> bool:
        return self.operation in (RuntimeOperation.NONE, RuntimeOperation.UNSPECIFIED)


def _enum(cls: type[IntEnum], value: int) -> Any:
    try:
        return cls(value)
    except ValueError:
        return cls(0)


def decode_session_response(message: bytes) -> KagentSession:
    """Decode ``*SessionResponse{session = 1}``."""
    outer = decode_fields(message)
    raw = outer.get(1, [b""])[0]
    if not isinstance(raw, bytes) or not raw:
        raise SessionError("SessionService response carried no session")
    return _decode_session(raw)


def decode_session_list(message: bytes) -> tuple[list[KagentSession], str]:
    """Decode ``ListSessionsResponse{sessions = 1; page = 2{next_page_token = 1}}``."""
    outer = decode_fields(message)
    sessions = [
        _decode_session(raw) for raw in outer.get(1, []) if isinstance(raw, bytes)
    ]
    page = outer.get(2, [b""])[0]
    token = _text(decode_fields(page), 1) if isinstance(page, bytes) and page else ""
    return sessions, token


def _decode_session(raw: bytes) -> KagentSession:
    fields = decode_fields(raw)
    if any(len(fields.get(number, [])) > 1 for number in (1, 2, 5, 6, 14, 15)):
        raise SessionError("Ambiguous gateway session identity")
    failure = fields.get(9, [b""])[0]
    failure_fields = decode_fields(failure) if isinstance(failure, bytes) else {}
    session_id = _text(fields, 1)
    workspace = fields.get(16, [b""])[0]
    agent_raw = fields.get(15, [b""])[0]
    agent_fields = decode_fields(agent_raw) if isinstance(agent_raw, bytes) else {}
    if any(len(agent_fields.get(number, [])) > 1 for number in (1, 2)):
        raise SessionError("Ambiguous gateway agent identity")
    return KagentSession(
        id=session_id,
        state=_enum(RuntimeState, _number(fields, 7)),
        operation=_enum(RuntimeOperation, _number(fields, 8)),
        context_id=_text(fields, 14) or session_id,
        failure_reason=_text(failure_fields, 1),
        failure_message=_text(failure_fields, 2),
        name=_text(fields, 13),
        creator=_text(fields, 2),
        agent=(
            AgentRef(_text(agent_fields, 1), _text(agent_fields, 2))
            if agent_fields
            else None
        ),
        prepared_revision=_text(fields, 5),
        a2a_authority=_text(fields, 6),
        workspace=(
            SessionWorkspace.decode(workspace)
            if isinstance(workspace, bytes) and workspace
            else None
        ),
    )


# --------------------------------------------------------------------------------------------
# Client
# --------------------------------------------------------------------------------------------

_SESSION_SERVICE = "/kagent.api.v1alpha1.SessionService"
# How long "send not accepted" is retried with the identical message before it counts as a
# definite non-delivery. kagent itself holds each attempt for up to 10s while the Session is busy.
SEND_RETRY_BUDGET = 30.0
_SEND_BACKOFF_CAP = 2.0
# A transport failure of these kinds happened before any byte of the request was delivered.
_NOT_SENT = (httpx.ConnectError, httpx.ConnectTimeout, httpx.PoolTimeout)

Sleep = Callable[[float], Awaitable[None]]
Clock = Callable[[], float]


class KagentClient:
    """Async client for one kagent gateway, acting as a fixed service identity."""

    def __init__(
        self,
        base_url: str,
        *,
        user_id: str,
        client: httpx.AsyncClient | None = None,
        request_timeout: float = 30.0,
        stream_timeout: float = 900.0,
        send_retry_budget: float = SEND_RETRY_BUDGET,
        sleep: Sleep = asyncio.sleep,
        clock: Clock = time.monotonic,
    ):
        self._owns_client = client is None
        self._client = client or httpx.AsyncClient(base_url=base_url)
        self._user_id = user_id
        self._request_timeout = request_timeout
        self._stream_timeout = stream_timeout
        self._send_retry_budget = send_retry_budget
        self._sleep = sleep
        self._clock = clock

    async def aclose(self) -> None:
        if self._owns_client:
            await self._client.aclose()

    def _headers(self, extra: dict[str, str] | None = None) -> dict[str, str]:
        return {
            "x-user-id": self._user_id,
            "A2A-Extensions": HITL_EXTENSION,
            **(extra or {}),
        }

    # ---- SessionService ----------------------------------------------------------------

    async def _session_call(self, method: str, message: bytes) -> KagentSession:
        messages = await self._session_frames(method, message)
        if not messages:
            raise OutcomeUnknown(f"SessionService {method} returned no message")
        return decode_session_response(messages[0])

    async def _session_frames(self, method: str, message: bytes) -> list[bytes]:
        try:
            response = await self._client.post(
                f"{_SESSION_SERVICE}/{method}",
                content=grpc_web_frame(message),
                headers=self._headers(
                    {
                        "content-type": "application/grpc-web+proto",
                        "accept": "application/grpc-web+proto",
                        "x-grpc-web": "1",
                    }
                ),
                timeout=self._request_timeout,
            )
        except _NOT_SENT as exc:
            raise Unreachable(f"kagent gateway unreachable: {exc}") from exc
        except httpx.HTTPError as exc:
            raise OutcomeUnknown(
                f"SessionService {method} outcome unknown: {type(exc).__name__}"
            ) from exc
        if response.status_code >= 500:
            raise OutcomeUnknown(
                f"SessionService {method} outcome unknown (HTTP {response.status_code})"
            )
        if response.status_code != 200:
            raise SessionError(
                f"SessionService {method} failed (HTTP {response.status_code})"
            )
        try:
            messages, trailers = parse_grpc_web(response.content)
        except ValueError as exc:
            raise OutcomeUnknown(f"SessionService {method} sent a bad frame") from exc
        status = trailers.get("grpc-status", response.headers.get("grpc-status", "0"))
        # The companion can return Aborted after reserving a Session, when its
        # lifecycle workflow contends. It does not prove that nothing was admitted.
        if status in ("4", "10", "13", "14"):
            raise OutcomeUnknown(
                f"SessionService {method} outcome unknown (grpc {status})"
            )
        if status != "0":
            detail = trailers.get("grpc-message", response.headers.get("grpc-message"))
            raise SessionError(
                f"SessionService {method} failed (grpc {status}): {detail or ''}".strip(),
                grpc_status=int(status) if status.isdigit() else None,
            )
        return messages

    async def create_session(
        self,
        agent: AgentRef,
        *,
        request_id: str,
        name: str = "",
        credentials: tuple[SessionCredential, ...] = (),
        workspace: SessionWorkspace | None = None,
    ) -> KagentSession:
        """Create a Session. Retrying with the same ``request_id`` returns the same Session."""
        message = (
            _field_bytes(5, agent.encode())
            + _field_str(3, request_id)
            + _field_str(4, name)
        )
        if workspace is not None:
            message += _field_bytes(6, workspace.encode())
        message += b"".join(
            _field_bytes(7, credential.encode()) for credential in credentials
        )
        return await self._session_call("CreateSession", message)

    async def list_sessions_page(
        self, cursor: str = "", limit: int = 100
    ) -> tuple[list[KagentSession], str]:
        page = _field_varint(1, min(max(limit, 1), 100)) + _field_str(2, cursor)
        frames = await self._session_frames("ListSessions", _field_bytes(3, page))
        return decode_session_list(frames[0]) if frames else ([], "")

    async def list_sessions(self) -> list[KagentSession]:
        sessions: list[KagentSession] = []
        cursor = ""
        while True:
            batch, cursor = await self.list_sessions_page(cursor)
            sessions.extend(batch)
            if not cursor:
                return sessions

    async def get_session(self, session_id: str) -> KagentSession:
        return await self._session_call("GetSession", _field_str(1, session_id))

    async def suspend_session(self, session_id: str) -> KagentSession:
        return await self._session_call("SuspendSession", _field_str(1, session_id))

    async def resume_session(self, session_id: str) -> KagentSession:
        return await self._session_call("ResumeSession", _field_str(1, session_id))

    async def delete_session(self, session_id: str) -> KagentSession:
        return await self._session_call("DeleteSession", _field_str(1, session_id))

    async def ensure_ready(
        self, session: KagentSession, *, timeout: float = 120.0, interval: float = 1.0
    ) -> KagentSession:
        """Return the Session once it can take a turn: resume it if suspended, wait if busy.

        A Session that is Ready is trusted as-is. Waking a Ready-but-quiesced actor is kagent's
        job when the turn arrives.
        """
        deadline = asyncio.get_running_loop().time() + timeout
        resumed = False
        while True:
            if session.state == RuntimeState.READY and session.settled:
                return session
            if session.state in (
                RuntimeState.FAILED,
                RuntimeState.DELETING,
                RuntimeState.DELETED,
            ):
                raise SessionError(
                    f"kagent Session {session.id} is {session.state.name.lower()}: "
                    f"{session.failure_reason} {session.failure_message}".strip()
                )
            if session.state == RuntimeState.SUSPENDED and session.settled:
                if resumed:
                    raise SessionError(
                        f"kagent Session {session.id} stayed suspended after resume"
                    )
                session = await self.resume_session(session.id)
                resumed = True
                continue
            if asyncio.get_running_loop().time() >= deadline:
                raise SessionError(
                    f"kagent Session {session.id} not ready after {timeout:.0f}s "
                    f"(state {session.state.name.lower()})"
                )
            await self._sleep(interval)
            session = await self.get_session(session.id)

    # ---- A2A -----------------------------------------------------------------------------

    def _rpc_body(self, method: str, params: dict[str, Any]) -> dict[str, Any]:
        return {
            "jsonrpc": "2.0",
            "id": str(uuid.uuid4()),
            "method": method,
            "params": params,
        }

    async def _rpc(
        self, agent: AgentRef, method: str, params: dict[str, Any]
    ) -> dict[str, Any]:
        try:
            response = await self._client.post(
                agent.path,
                json=self._rpc_body(method, params),
                headers=self._headers(),
                timeout=self._request_timeout,
            )
        except _NOT_SENT as exc:
            raise Unreachable(f"kagent gateway unreachable: {exc}") from exc
        except httpx.HTTPError as exc:
            raise OutcomeUnknown(
                f"A2A {method} outcome unknown: {type(exc).__name__}"
            ) from exc
        envelope = self._envelope(response, method)
        if "error" in envelope:
            raise a2a_error_from_json(envelope["error"])
        result = envelope.get("result")
        if not isinstance(result, dict):
            raise OutcomeUnknown(f"A2A {method} returned no result")
        return result

    @staticmethod
    def _envelope(response: httpx.Response, method: str) -> dict[str, Any]:
        try:
            document = response.json()
        except ValueError:
            document = None
        if isinstance(document, dict) and ("result" in document or "error" in document):
            return document
        if response.status_code in (502, 503, 504):
            raise OutcomeUnknown(
                f"A2A {method} gateway error (HTTP {response.status_code})"
            )
        raise KagentError(f"A2A {method} failed (HTTP {response.status_code})")

    async def get_task(self, agent: AgentRef, task_id: str) -> Task:
        return Task.model_validate(await self._rpc(agent, "GetTask", {"id": task_id}))

    async def list_tasks_page(
        self, agent: AgentRef, context_id: str, cursor: str = "", limit: int = 100
    ) -> tuple[list[Task], str]:
        result = await self._rpc(
            agent,
            "ListTasks",
            {
                "contextId": context_id,
                "pageSize": min(max(limit, 1), 100),
                **({"pageToken": cursor} if cursor else {}),
            },
        )
        return [Task.model_validate(t) for t in result.get("tasks") or []], result.get(
            "nextPageToken"
        ) or ""

    async def list_tasks(self, agent: AgentRef, context_id: str) -> list[Task]:
        tasks: list[Task] = []
        cursor = ""
        while True:
            batch, cursor = await self.list_tasks_page(agent, context_id, cursor)
            tasks.extend(batch)
            if not cursor:
                return tasks

    async def supports_hitl(self, agent: AgentRef) -> bool:
        card = await self._rpc(agent, "GetExtendedAgentCard", {})
        return any(
            e.get("uri") == HITL_EXTENSION
            for e in card.get("capabilities", {}).get("extensions", [])
            if isinstance(e, dict)
        )

    def send_hitl_response(
        self,
        agent: AgentRef,
        *,
        response: HITLResponse,
        message_id: str,
        context_id: str,
        task_id: str,
    ) -> AsyncIterator[StreamEvent]:
        if not task_id or not context_id or not message_id:
            raise ValueError(
                "Structured continuation requires exact task/context/message identity"
            )
        return self._send_with_retry(
            agent,
            {
                "message": {
                    "messageId": message_id,
                    "contextId": context_id,
                    "taskId": task_id,
                    "role": "ROLE_USER",
                    "parts": [],
                    "extensions": [HITL_EXTENSION],
                    "metadata": {
                        HITL_EXTENSION: response.model_dump(
                            mode="json", exclude_none=True
                        )
                    },
                }
            },
        )

    async def cancel_task(self, agent: AgentRef, task_id: str) -> Task:
        """Cancel a task. On a task that is already terminal, kagent returns it unchanged."""
        return Task.model_validate(
            await self._rpc(agent, "CancelTask", {"id": task_id})
        )

    async def find_task_for_message(
        self, agent: AgentRef, context_id: str, message_id: str
    ) -> Task | None:
        """Resolve an ambiguous send: the task whose history holds this ``messageId``, if any.

        The match is re-read with ``GetTask``, because a listed task has no artifacts.
        """
        for task in await self.list_tasks(agent, context_id):
            if any(m.message_id == message_id for m in task.history):
                return await self.get_task(agent, task.id)
        return None

    def send_message(
        self,
        agent: AgentRef,
        *,
        text: str,
        message_id: str,
        context_id: str,
        task_id: str | None = None,
    ) -> AsyncIterator[StreamEvent]:
        """Send one user message and stream the task's events.

        ``KAGENT_SEND_NOT_ACCEPTED`` before the first event is retried with the identical
        message until the send retry budget (30s by default) runs out. Anything else that goes wrong after the request left raises
        :class:`OutcomeUnknown` (or the definite error), and the message is not re-sent.

        Do not rely on kagent to dedupe a re-send: measured live, re-sending a ``messageId``
        whose task had already completed started a second task.
        """
        message: dict[str, Any] = {
            "messageId": message_id,
            "contextId": context_id,
            "role": "ROLE_USER",
            "parts": [{"text": text}],
        }
        if task_id:
            message["taskId"] = task_id
        return self._send_with_retry(agent, {"message": message})

    async def _send_with_retry(
        self, agent: AgentRef, params: dict[str, Any]
    ) -> AsyncIterator[StreamEvent]:
        deadline = self._clock() + self._send_retry_budget
        attempt = 0
        while True:
            attempt += 1
            started = False
            try:
                async for event in self._stream(agent, "SendStreamingMessage", params):
                    started = True
                    yield event
                return
            except SendNotAccepted as exc:
                # kagent's hint, backed off so fast rejections cannot spin through the budget.
                delay = max(
                    exc.retry_after_seconds,
                    min(0.1 * 2 ** (attempt - 1), _SEND_BACKOFF_CAP),
                )
                if started or self._clock() + delay >= deadline:
                    raise
                logger.info(
                    "kagent did not accept message (attempt %d); retrying the same message",
                    attempt,
                )
                await self._sleep(delay)

    def subscribe_to_task(
        self, agent: AgentRef, task_id: str
    ) -> AsyncIterator[StreamEvent]:
        """Reconnect to a task. The first event is the current task: use ``replace``."""
        return self._stream(agent, "SubscribeToTask", {"id": task_id})

    async def _stream(
        self, agent: AgentRef, method: str, params: dict[str, Any]
    ) -> AsyncIterator[StreamEvent]:
        request = self._client.build_request(
            "POST",
            agent.path,
            json=self._rpc_body(method, params),
            headers=self._headers({"accept": "text/event-stream, application/json"}),
            timeout=httpx.Timeout(self._request_timeout, read=self._stream_timeout),
        )
        try:
            response = await self._client.send(request, stream=True)
        except _NOT_SENT as exc:
            raise Unreachable(f"kagent gateway unreachable: {exc}") from exc
        except httpx.HTTPError as exc:
            raise OutcomeUnknown(
                f"A2A {method} outcome unknown: {type(exc).__name__}"
            ) from exc
        try:
            content_type = response.headers.get("content-type", "")
            if "text/event-stream" not in content_type:
                # A rejection arrives as a plain JSON-RPC body rather than an SSE error event.
                await response.aread()
                envelope = self._envelope(response, method)
                if "error" in envelope:
                    raise a2a_error_from_json(envelope["error"])
                yield self._event(envelope, method)
                return
            data: list[str] = []
            async for line in response.aiter_lines():
                if line.startswith("data:"):
                    data.append(line[5:].removeprefix(" "))
                elif line == "" and data:
                    envelope = json.loads("\n".join(data))
                    data = []
                    if "error" in envelope:
                        raise a2a_error_from_json(envelope["error"])
                    yield self._event(envelope, method)
            if data:
                envelope = json.loads("\n".join(data))
                if "error" in envelope:
                    raise a2a_error_from_json(envelope["error"])
                yield self._event(envelope, method)
        except (httpx.HTTPError, json.JSONDecodeError) as exc:
            raise OutcomeUnknown(
                f"A2A {method} stream broke: {type(exc).__name__}"
            ) from exc
        finally:
            await response.aclose()

    @staticmethod
    def _event(envelope: dict[str, Any], method: str) -> StreamEvent:
        result = envelope.get("result")
        if not isinstance(result, dict):
            raise OutcomeUnknown(f"A2A {method} sent an event without a result")
        # GetTask-shaped results (a bare task) are accepted as a task event.
        if "id" in result and "status" in result and "task" not in result:
            result = {"task": result}
        return StreamEvent.model_validate(result)
