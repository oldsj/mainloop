"""Native-session transport to a pre-created Substrate actor through its CONNECT router.

The actor-local shim owns the native CLI turn and journal files. Delivery errors after
``POST /turn`` are unknown; callers must reconcile the journal and never replay blindly.
"""

from __future__ import annotations

import asyncio
import base64
import http.client
import json
import logging
import re
import socket
from dataclasses import dataclass
from urllib.parse import urlencode, urlsplit

from kubernetes import client, config
from kubernetes.client.rest import ApiException
from mainloop.config import settings
from mainloop.runtime.credential_broker import CredentialBroker
from mainloop.runtime.substrate import TransportError

logger = logging.getLogger(__name__)


class WorkspaceUnavailable(RuntimeError):
    """The Substrate actor is not ready; no turn was attempted."""


@dataclass(frozen=True, slots=True)
class WorkspaceState:
    name: str
    uid: str | None
    ready: bool


@dataclass(frozen=True, slots=True)
class JournalSlice:
    file: str | None
    total_lines: int
    lines: list[tuple[int, str]]


_AGENT = re.compile(r"^(claude|codex)$")
_DNS_LABEL = re.compile(r"^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$")
_MAX_RESPONSE_BYTES = 2 * 1024 * 1024
_JOURNAL_PAGE_SIZE = 200
_ACTOR_PORT = 8090
_SECRET_API: client.CoreV1Api | None = None


@dataclass(frozen=True, slots=True)
class _Response:
    status: int
    body: str


def _secret_api() -> client.CoreV1Api:
    global _SECRET_API
    if _SECRET_API is None:
        try:
            config.load_incluster_config()
        except config.ConfigException:
            config.load_kube_config()
        _SECRET_API = client.CoreV1Api()
    return _SECRET_API


def _read_secret_token(secret_name: str, namespace: str) -> str:
    try:
        secret = _secret_api().read_namespaced_secret(secret_name, namespace)
    except ApiException as exc:
        if exc.status == 404:
            raise WorkspaceUnavailable(
                "Substrate shim token Secret is unavailable"
            ) from exc
        raise TransportError(
            f"Substrate shim token Secret read failed (status {exc.status})"
        ) from exc
    encoded = (secret.data or {}).get("token")
    if not isinstance(encoded, str):
        raise WorkspaceUnavailable("Substrate shim token Secret has no token key")
    try:
        token = base64.b64decode(encoded, validate=True).decode("utf-8").strip()
    except (ValueError, UnicodeDecodeError) as exc:
        raise WorkspaceUnavailable("Substrate shim token Secret is invalid") from exc
    if not token:
        raise WorkspaceUnavailable("Substrate shim token Secret is empty")
    return token


def _read_headers(reader) -> tuple[int, http.client.HTTPMessage]:
    status_line = reader.readline(8192)
    if not status_line.endswith(b"\r\n"):
        raise ValueError("invalid HTTP status line")
    parts = status_line.decode("latin1").strip().split(" ", 2)
    if len(parts) < 2 or not parts[0].startswith("HTTP/"):
        raise ValueError("invalid HTTP status line")
    status = int(parts[1])
    headers = http.client.parse_headers(reader)
    return status, headers


def _read_body(reader, headers: http.client.HTTPMessage) -> bytes:
    transfer_encoding = headers.get("transfer-encoding", "").lower()
    if "chunked" in transfer_encoding:
        chunks: list[bytes] = []
        size = 0
        while True:
            line = reader.readline(8192)
            if not line:
                raise ValueError("truncated chunked HTTP response")
            chunk_size = int(line.split(b";", 1)[0].strip(), 16)
            if chunk_size == 0:
                while reader.readline(8192) not in (b"\r\n", b"\n", b""):
                    pass
                return b"".join(chunks)
            size += chunk_size
            if size > _MAX_RESPONSE_BYTES:
                raise ValueError("HTTP response exceeded the bounded size")
            chunk = reader.read(chunk_size)
            if len(chunk) != chunk_size or reader.read(2) != b"\r\n":
                raise ValueError("truncated chunked HTTP response")
            chunks.append(chunk)
    length = headers.get("content-length")
    if length is not None:
        size = int(length)
        if size < 0 or size > _MAX_RESPONSE_BYTES:
            raise ValueError("HTTP response exceeded the bounded size")
        body = reader.read(size)
        if len(body) != size:
            raise ValueError("truncated HTTP response")
        return body
    body = reader.read(_MAX_RESPONSE_BYTES + 1)
    if len(body) > _MAX_RESPONSE_BYTES:
        raise ValueError("HTTP response exceeded the bounded size")
    return body


def _router_request(
    *,
    host: str,
    port: int,
    actor: str,
    atespace: str,
    actor_port: int,
    timeout: float,
    method: str,
    path: str,
    token: str | None,
    body: dict | None,
) -> _Response:
    target = f"actor-upstream:{actor_port}"
    sock = socket.create_connection((host, port), timeout=timeout)
    sock.settimeout(timeout)
    reader = sock.makefile("rb")
    try:
        connect = (
            f"CONNECT {target} HTTP/1.1\r\n"
            f"Host: {target}\r\n"
            f"ate-target-actor: {atespace}/{actor}\r\n"
            "Connection: keep-alive\r\n\r\n"
        ).encode("ascii")
        sock.sendall(connect)
        connect_status, _ = _read_headers(reader)
        if connect_status != 200:
            return _Response(connect_status, "")

        encoded_body = (
            json.dumps(body, separators=(",", ":")).encode()
            if body is not None
            else b""
        )
        headers = [f"Host: {target}", "Connection: close"]
        if token is not None:
            headers.append(f"Authorization: Bearer {token}")
        if body is not None:
            headers.extend(
                [
                    "Content-Type: application/json",
                    f"Content-Length: {len(encoded_body)}",
                ]
            )
        request = (
            f"{method} {path} HTTP/1.1\r\n" + "\r\n".join(headers) + "\r\n\r\n"
        ).encode("ascii")
        sock.sendall(request + encoded_body)
        status, response_headers = _read_headers(reader)
        response_body = _read_body(reader, response_headers)
        return _Response(status, response_body.decode("utf-8", errors="replace"))
    finally:
        reader.close()
        sock.close()


class SubstrateWorkspace:
    """Drive one native agent in a pre-created Substrate actor."""

    def __init__(
        self,
        *,
        atespace: str,
        actor: str,
        agent: str,
        shim_token_secret_name: str,
        native_session_id: str | None = None,
        router_address: str | None = None,
        timeout: float | None = None,
        credential_broker: CredentialBroker | None = None,
    ):
        if not _DNS_LABEL.fullmatch(atespace) or not _DNS_LABEL.fullmatch(actor):
            raise ValueError("Substrate atespace and actor must be DNS labels")
        if not _AGENT.fullmatch(agent):
            raise ValueError("Substrate native agent must be claude or codex")
        self.atespace = atespace
        self.actor = actor
        self.workspace_name = actor
        self.agent = agent
        self.native_session_id = native_session_id
        address = urlsplit(router_address or settings.substrate_router_address)
        if (
            address.scheme != "http"
            or not address.hostname
            or address.username is not None
            or address.password is not None
            or address.path not in ("", "/")
            or address.query
            or address.fragment
        ):
            raise ValueError("Substrate router address must be an HTTP origin")
        self.router_host = address.hostname
        self.router_port = address.port or 80
        self.actor_port = _ACTOR_PORT
        self.timeout = timeout or settings.substrate_resume_timeout_seconds
        self.secret_namespace = settings.substrate_shim_secret_namespace
        self.secret_name = shim_token_secret_name
        if not _DNS_LABEL.fullmatch(self.secret_name):
            raise ValueError("Substrate shim Secret name must be a DNS label")
        self._token_value: str | None = None
        self.credential_broker = credential_broker or CredentialBroker()

    def set_native_session_id(self, native_session_id: str | None) -> None:
        self.native_session_id = native_session_id

    async def _token(self) -> str:
        if self._token_value is None:
            self._token_value = await asyncio.to_thread(
                _read_secret_token, self.secret_name, self.secret_namespace
            )
        return self._token_value

    async def _request(
        self,
        method: str,
        path: str,
        *,
        body: dict | None = None,
        authenticated: bool = True,
    ) -> _Response:
        token = await self._token() if authenticated else None
        response = await self._exchange(method, path, token=token, body=body)
        if response.status == 401 and authenticated:
            self._token_value = None
            if method == "POST" and path == "/turn":
                raise RuntimeError(
                    "Substrate shim rejected its bearer token (HTTP 401); prompt was not retried"
                )
            token = await self._token()
            response = await self._exchange(method, path, token=token, body=body)
            if response.status == 401:
                self._token_value = None
        if response.status == 503:
            raise WorkspaceUnavailable(
                "Substrate router or actor capacity is unavailable"
            )
        return response

    async def _exchange(
        self, method: str, path: str, *, token: str | None, body: dict | None
    ) -> _Response:
        try:
            response = await asyncio.to_thread(
                _router_request,
                host=self.router_host,
                port=self.router_port,
                actor=self.actor,
                atespace=self.atespace,
                actor_port=self.actor_port,
                timeout=self.timeout,
                method=method,
                path=path,
                token=token,
                body=body,
            )
        except (OSError, TimeoutError, ValueError, http.client.HTTPException) as exc:
            raise TransportError(
                f"Substrate router request failed: {type(exc).__name__}"
            ) from exc
        return response

    def _json(self, response: _Response, *, method: str) -> dict:
        if response.status == 401:
            raise RuntimeError("Substrate shim rejected its bearer token (HTTP 401)")
        if response.status == 409:
            raise RuntimeError("Substrate shim rejected the concurrent turn (HTTP 409)")
        if not 200 <= response.status < 300:
            raise RuntimeError(
                f"Substrate shim {method} failed (HTTP {response.status})"
            )
        try:
            document = json.loads(response.body)
        except json.JSONDecodeError as exc:
            raise TransportError("Substrate shim returned invalid JSON") from exc
        if not isinstance(document, dict):
            raise TransportError("Substrate shim returned an invalid response")
        return document

    async def workspace_state(self) -> WorkspaceState:
        try:
            response = await self._request("GET", "/healthz", authenticated=False)
        except WorkspaceUnavailable:
            return WorkspaceState(self.actor, None, False)
        if response.status == 503:
            return WorkspaceState(self.actor, None, False)
        if response.status != 200:
            raise TransportError(
                f"Substrate health check failed (HTTP {response.status})"
            )
        return WorkspaceState(self.actor, None, True)

    async def require_ready(self) -> WorkspaceState:
        state = await self.workspace_state()
        if not state.ready:
            raise WorkspaceUnavailable(
                f"Substrate actor {self.atespace}/{self.actor} is not ready"
            )
        return state

    async def prepare_credentials(self) -> None:
        """Install provider-shaped placeholders; the real credential stays in its Secret."""
        if self.agent == "codex":
            name = "codex-auth"
            placeholder = await self.credential_broker.codex_placeholder_auth()
        else:
            name = "claude-token"
            placeholder = await self.credential_broker.claude_placeholder_token()
        response = await self._request(
            "PUT",
            "/credential",
            body={"name": name, "contents": placeholder},
        )
        if response.status not in (200, 201):
            raise RuntimeError(
                f"Substrate {self.agent.title()} placeholder delivery failed (HTTP {response.status})"
            )

    async def agent_status(self, name: str) -> dict | None:
        if not name:
            raise ValueError("native agent name is required")
        query = urlencode({"agent": self.agent})
        response = await self._request("GET", f"/turn/status?{query}")
        if response.status == 404:
            return None
        return self._json(response, method="GET /turn/status")

    async def start(
        self,
        binding: str,
        name: str,
        *,
        native_id: str | None,
        resume: bool,
        extra: dict[str, str] | None = None,
    ) -> dict:
        del binding, name, resume, extra
        self.native_session_id = native_id
        await self.require_ready()
        await self.prepare_credentials()
        query = urlencode({"agent": self.agent})
        response = await self._request("GET", f"/agent/ready?{query}")
        self._json(response, method="GET /agent/ready")
        # The actor-local file is synthetic; the egress provider injects the current token.
        # Starting the native CLI remains the shim's turn API's responsibility.
        return {"actor": self.actor}

    async def send(self, name: str, text: str) -> None:
        if not name:
            raise ValueError("native agent name is required")
        payload = {"agent": self.agent, "prompt": text}
        if self.native_session_id:
            payload["session_id"] = self.native_session_id
        response = await self._request("POST", "/turn", body=payload)
        self._json(response, method="POST /turn")

    async def stop(self, name: str) -> None:
        if not name:
            raise ValueError("native agent name is required")
        response = await self._request("POST", "/turn/stop", body={"agent": self.agent})
        self._json(response, method="POST /turn/stop")

    async def _latest_turn(self) -> dict | None:
        query = urlencode({"agent": self.agent})
        response = await self._request("GET", f"/turn/status?{query}")
        if response.status == 404:
            return None
        return self._json(response, method="GET /turn/status")

    async def credential_rejected(self) -> bool:
        """Return only the shim's sanitized provider-auth rejection signal."""
        turn = await self._latest_turn()
        return bool(turn and turn.get("credential_rejected") is True)

    async def native_id(self, name: str) -> str | None:
        if not name:
            raise ValueError("native agent name is required")
        turn = await self._latest_turn()
        native_id = turn.get("native_session_id") if turn else None
        if isinstance(native_id, str) and native_id:
            self.native_session_id = native_id
            return native_id
        return None

    async def journal(self, name: str, native_id: str, from_line: int) -> JournalSlice:
        if not name:
            raise ValueError("native agent name is required")
        query = urlencode(
            {
                "agent": self.agent,
                "id": native_id,
                "from": max(0, from_line),
                "limit": _JOURNAL_PAGE_SIZE,
            }
        )
        response = await self._request("GET", f"/journal?{query}")
        if response.status == 404:
            return JournalSlice(None, 0, [])
        document = self._json(response, method="GET /journal")
        file = document.get("file")
        total = document.get("total_lines")
        raw_lines = document.get("lines")
        if (file is not None and not isinstance(file, str)) or not isinstance(
            total, int
        ):
            raise TransportError("Substrate journal response has invalid metadata")
        if not isinstance(raw_lines, list):
            raise TransportError("Substrate journal response has invalid lines")
        lines: list[tuple[int, str]] = []
        for item in raw_lines:
            if (
                not isinstance(item, dict)
                or not isinstance(item.get("line"), int)
                or not isinstance(item.get("text"), str)
            ):
                raise TransportError("Substrate journal response has an invalid line")
            lines.append((item["line"], item["text"]))
        return JournalSlice(file, total, lines)
