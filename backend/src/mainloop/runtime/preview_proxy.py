"""Owner-scoped wildcard preview proxy through the Substrate router CONNECT tunnel."""

from __future__ import annotations

import asyncio
import base64
import hashlib
import http.client
import io
import json
import os
import re
import socket
import struct
from dataclasses import dataclass
from urllib.parse import urlsplit

from fastapi import FastAPI, Request, WebSocket
from fastapi.responses import HTMLResponse, Response, StreamingResponse
from mainloop.config import settings
from mainloop.db import db
from mainloop.runtime import workspace_adapter
from mainloop.runtime.substrate_workspace import SubstrateWorkspace

_WORKSPACE_LABEL = re.compile(r"^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$")
_PORT_LABEL = re.compile(
    r"^(?P<port>[1-9][0-9]{0,4})--(?P<workspace>[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)$"
)
_MAX_REQUEST_BYTES = 10 * 1024 * 1024
_MAX_HEADER_BYTES = 64 * 1024
_MAX_WEBSOCKET_MESSAGE_BYTES = 16 * 1024 * 1024
_SHIM_PORT = 8090
_HOP_HEADERS = {
    "connection",
    "keep-alive",
    "proxy-authenticate",
    "proxy-authorization",
    "proxy-connection",
    "te",
    "trailer",
    "transfer-encoding",
    "upgrade",
}
_PRIVATE_HEADERS = {
    "authorization",
    "cookie",
    "cf-access-authenticated-user-email",
    "x-user-id",
}


@dataclass(frozen=True, slots=True)
class PreviewHost:
    port: int
    workspace_id: str


@dataclass(frozen=True, slots=True)
class PreviewTarget:
    workspace_id: str
    user_id: str
    atespace: str
    actor: str
    agent: str
    shim_secret_name: str
    manifest: dict
    observed_state: str = "unknown"


@dataclass(slots=True)
class _UpstreamHTTP:
    sock: socket.socket
    reader: object
    status: int
    headers: http.client.HTTPMessage

    def close(self) -> None:
        try:
            self.reader.close()
        finally:
            self.sock.close()


def parse_preview_host(
    host_header: str, base_url: str | None = None
) -> PreviewHost | None:
    """Parse `<port>--<workspace>.<preview-domain>` without accepting path-like labels."""
    parsed_base = urlsplit(base_url or settings.substrate_preview_base_url)
    domain = parsed_base.hostname
    if not domain:
        return None
    try:
        parsed_host = urlsplit(f"//{host_header}")
    except ValueError:
        return None
    hostname = (parsed_host.hostname or "").lower().rstrip(".")
    suffix = "." + domain.lower().rstrip(".")
    if not hostname.endswith(suffix):
        return None
    label = hostname[: -len(suffix)]
    match = _PORT_LABEL.fullmatch(label)
    if not match:
        return None
    try:
        host_port = parsed_host.port
    except ValueError:
        return None
    if parsed_base.port is not None and host_port != parsed_base.port:
        return None
    port = int(match.group("port"))
    workspace_id = match.group("workspace")
    if port > 65535 or not _WORKSPACE_LABEL.fullmatch(workspace_id):
        return None
    return PreviewHost(port, workspace_id)


def _secret_name_for_workspace(atespace: str, actor: str) -> str:
    for binding in settings.substrate_actor_bindings.values():
        if binding.atespace == atespace and binding.actor == actor:
            return binding.shim_token_secret_name
    return settings.shim_token_secret_name(atespace, actor)


async def _resolve_target(workspace_id: str, user_id: str) -> PreviewTarget | None:
    async with db.connection() as conn:
        row = await conn.fetchrow(
            """SELECT b.*,
                      s.user_id, l.manifest, l.observed_state AS lifecycle_state,
                      n.kind AS agent_kind
               FROM workspace_bindings b
               JOIN sessions s ON s.id=b.workspace_id
               LEFT JOIN workspace_lifecycles l ON l.workspace_id=b.workspace_id
               LEFT JOIN native_bindings n ON n.session_id=b.workspace_id
               WHERE b.workspace_id=$1""",
            workspace_id,
        )
    if row is None or row["user_id"] != user_id:
        return None
    record = dict(row)
    manifest = record.get("manifest") or {}
    if isinstance(manifest, str):
        try:
            manifest = json.loads(manifest)
        except json.JSONDecodeError:
            manifest = {}
    if not isinstance(manifest, dict):
        manifest = {}
    atespace = record["atespace"]
    actor = record["actor_name"]
    return PreviewTarget(
        workspace_id=record["workspace_id"],
        user_id=user_id,
        atespace=atespace,
        actor=actor,
        agent=(
            record.get("agent_kind")
            if record.get("agent_kind") in {"claude", "codex"}
            else "claude"
        ),
        shim_secret_name=record.get("shim_token_secret_name")
        or _secret_name_for_workspace(atespace, actor),
        manifest=manifest,
        observed_state=record.get("lifecycle_state")
        or ("running" if record.get("observed_state") == "ready" else "unknown"),
    )


def declared_ports(manifest: dict) -> dict[int, str]:
    """Read only explicit HTTP preview ports from current and dev-section manifest shapes."""
    declared: dict[int, str] = {}
    candidates = [manifest.get("forwardPorts", []), manifest.get("ports", [])]
    dev = manifest.get("dev")
    if isinstance(dev, dict):
        candidates.append(dev.get("ports", []))
    for candidate in candidates:
        if not isinstance(candidate, (list, tuple)):
            continue
        for item in candidate:
            if isinstance(item, bool):
                continue
            if isinstance(item, int):
                port, name = item, f"Port {item}"
            elif isinstance(item, dict):
                port = item.get("number", item.get("port"))
                name = item.get("name")
                if not isinstance(name, str) or not name.strip():
                    name = f"Port {port}"
            else:
                continue
            if (
                isinstance(port, int)
                and not isinstance(port, bool)
                and 1 <= port <= 65535
            ):
                declared[port] = name
    return declared


async def _reported_ports(target: PreviewTarget) -> tuple[int, ...]:
    workspace = SubstrateWorkspace(
        atespace=target.atespace,
        actor=target.actor,
        agent=target.agent,
        shim_token_secret_name=target.shim_secret_name,
        timeout=settings.substrate_preview_connect_timeout_seconds,
    )
    try:
        return tuple(
            port for port in await workspace.listening_ports() if port != _SHIM_PORT
        )
    except Exception:
        # Declared ports remain usable while the shim is waking or port discovery is unavailable.
        return ()


async def workspace_preview_ports(
    workspace_id: str, user_id: str
) -> list[dict[str, object]] | None:
    target = await _resolve_target(workspace_id, user_id)
    if target is None:
        return None
    ports = declared_ports(target.manifest)
    if target.observed_state == "running":
        for port in await _reported_ports(target):
            ports.setdefault(port, f"Port {port}")
    return [
        {"port": port, "name": ports[port], "url": preview_url(workspace_id, port)}
        for port in sorted(ports)
    ]


def preview_url(workspace_id: str, port: int) -> str:
    base = urlsplit(settings.substrate_preview_base_url)
    host = base.hostname or "preview.localhost"
    netloc = f"{port}--{workspace_id}.{host}"
    if base.port is not None:
        netloc += f":{base.port}"
    return f"{base.scheme or 'http'}://{netloc}"


async def _touch_preview(workspace_id: str) -> None:
    await workspace_adapter.touch_workspace(workspace_id, reason="preview")


def _read_head(reader) -> tuple[int, http.client.HTTPMessage]:
    status_line = reader.readline(8192)
    if not status_line.endswith(b"\r\n"):
        raise ValueError("invalid upstream status line")
    parts = status_line.decode("latin1").strip().split(" ", 2)
    if len(parts) < 2 or not parts[0].startswith("HTTP/"):
        raise ValueError("invalid upstream status line")
    status = int(parts[1])
    headers = http.client.parse_headers(reader, _class=http.client.HTTPMessage)
    if sum(len(key) + len(value) for key, value in headers.items()) > _MAX_HEADER_BYTES:
        raise ValueError("upstream headers exceeded the bounded size")
    return status, headers


def _connect_router(
    target: PreviewTarget,
    port: int,
    timeout: float,
    *,
    method: str,
    path: str,
    headers: list[tuple[str, str]],
    body: bytes,
) -> _UpstreamHTTP:
    router = urlsplit(settings.substrate_router_address)
    if router.scheme != "http" or not router.hostname:
        raise ValueError("preview router address must be an HTTP origin")
    sock = socket.create_connection(
        (router.hostname, router.port or 80), timeout=timeout
    )
    sock.settimeout(timeout)
    reader = sock.makefile("rb")
    try:
        upstream_host = f"actor-upstream:{port}"
        connect_request = (
            f"CONNECT {upstream_host} HTTP/1.1\r\n"
            f"Host: {upstream_host}\r\n"
            f"ate-target-actor: {target.atespace}/{target.actor}\r\n"
            "Connection: keep-alive\r\n\r\n"
        ).encode("ascii")
        sock.sendall(connect_request)
        connect_status, _ = _read_head(reader)
        if connect_status != 200:
            raise ConnectionError(f"router CONNECT returned HTTP {connect_status}")
        request_headers = [
            (name, value)
            for name, value in headers
            if name.lower() not in _HOP_HEADERS | _PRIVATE_HEADERS | {"content-length"}
        ]
        request_headers.extend(
            [
                ("Host", upstream_host),
                ("Connection", "close"),
                ("Content-Length", str(len(body))),
            ]
        )
        wire = (
            f"{method} {path} HTTP/1.1\r\n"
            + "".join(f"{name}: {value}\r\n" for name, value in request_headers)
            + "\r\n"
        )
        sock.sendall(wire.encode("latin1") + body)
        status, response_headers = _read_head(reader)
        sock.settimeout(None)
        return _UpstreamHTTP(sock, reader, status, response_headers)
    except Exception:
        reader.close()
        sock.close()
        raise


def _response_headers(headers: http.client.HTTPMessage) -> list[tuple[str, str]]:
    connection_tokens = {
        part.strip().lower()
        for value in headers.get_all("connection", [])
        for part in value.split(",")
    }
    blocked = _HOP_HEADERS | connection_tokens
    return [
        (name.lower(), value)
        for name, value in headers.items()
        if name.lower() not in blocked
    ]


def _response_body(reader, headers: http.client.HTTPMessage, no_body: bool):
    if no_body:
        return
    transfer = headers.get("transfer-encoding", "").lower()
    if "chunked" in transfer:
        while True:
            line = reader.readline(8192)
            if not line:
                return
            size = int(line.split(b";", 1)[0].strip(), 16)
            if size == 0:
                while reader.readline(8192) not in (b"\r\n", b"\n", b""):
                    pass
                return
            chunk = reader.read(size)
            if len(chunk) != size or reader.read(2) != b"\r\n":
                raise ValueError("truncated upstream response")
            yield chunk
    length = headers.get("content-length")
    if length is not None:
        remaining = int(length)
        while remaining:
            chunk = reader.read(min(64 * 1024, remaining))
            if not chunk:
                raise ValueError("truncated upstream response")
            remaining -= len(chunk)
            yield chunk
        return
    while True:
        chunk = reader.read(64 * 1024)
        if not chunk:
            return
        yield chunk


def _waking_page() -> HTMLResponse:
    return HTMLResponse(
        "<!doctype html><title>Workspace waking up</title>"
        "<main><h1>Workspace waking up</h1>"
        "<p>Reload this preview in a few seconds.</p></main>",
        status_code=503,
        headers={"retry-after": "2", "cache-control": "no-store"},
    )


async def _preview_http(request: Request, parsed: PreviewHost) -> Response:
    user_id = (
        request.headers.get("x-user-id")
        or request.headers.get("cf-access-authenticated-user-email")
        or "local-dev-user"
    )
    target = await _resolve_target(parsed.workspace_id, user_id)
    if target is None:
        return Response("Workspace not found", status_code=404)
    await _touch_preview(parsed.workspace_id)
    allowed = set(declared_ports(target.manifest))
    allowed.update(await _reported_ports(target))
    if parsed.port not in allowed:
        return Response("Preview port is not declared or listening", status_code=403)
    chunks = bytearray()
    async for chunk in request.stream():
        chunks.extend(chunk)
        if len(chunks) > _MAX_REQUEST_BYTES:
            return Response("Preview request is too large", status_code=413)
    body = bytes(chunks)
    raw_path = request.scope.get(
        "raw_path", request.url.path.encode("ascii", errors="ignore")
    )
    path = raw_path.decode("ascii", errors="replace")
    if request.url.query:
        path += "?" + request.url.query
    headers = [
        (key.decode("latin1"), value.decode("latin1"))
        for key, value in request.headers.raw
    ]
    timeout = settings.substrate_preview_connect_timeout_seconds
    upstream = None
    for attempt in range(2):
        try:
            upstream = await asyncio.to_thread(
                _connect_router,
                target,
                parsed.port,
                timeout,
                method=request.method,
                path=path,
                headers=headers,
                body=body,
            )
            if upstream.status not in (502, 503) or attempt == 1:
                break
            upstream.close()
            upstream = None
        except Exception:
            if attempt == 1:
                return _waking_page()
    if upstream is None:
        return _waking_page()
    if upstream.status in (502, 503):
        upstream.close()
        return _waking_page()
    no_body = request.method == "HEAD" or upstream.status in (204, 304)

    def stream_body():
        try:
            yield from _response_body(upstream.reader, upstream.headers, no_body)
        finally:
            upstream.close()

    response = StreamingResponse(
        stream_body(),
        status_code=upstream.status,
        media_type=None,
    )
    response.raw_headers = [
        (key.encode("latin1"), value.encode("latin1"))
        for key, value in _response_headers(upstream.headers)
    ]
    return response


async def _read_async_head(
    reader: asyncio.StreamReader,
) -> tuple[int, http.client.HTTPMessage]:
    raw = await reader.readuntil(b"\r\n\r\n")
    if len(raw) > _MAX_HEADER_BYTES:
        raise ValueError("upstream headers exceeded the bounded size")
    status_line, _, header_bytes = raw.partition(b"\r\n")
    parts = status_line.decode("latin1").split(" ", 2)
    if len(parts) < 2 or not parts[0].startswith("HTTP/"):
        raise ValueError("invalid upstream status line")
    return int(parts[1]), http.client.parse_headers(io.BytesIO(header_bytes))


def _encode_websocket_frame(opcode: int, payload: bytes, *, masked: bool) -> bytes:
    first = 0x80 | opcode
    mask_bit = 0x80 if masked else 0
    size = len(payload)
    if size < 126:
        header = bytes([first, mask_bit | size])
    elif size < 65536:
        header = bytes([first, mask_bit | 126]) + struct.pack("!H", size)
    else:
        header = bytes([first, mask_bit | 127]) + struct.pack("!Q", size)
    if not masked:
        return header + payload
    mask = os.urandom(4)
    masked_payload = bytes(
        value ^ mask[index % 4] for index, value in enumerate(payload)
    )
    return header + mask + masked_payload


async def _read_websocket_frame(
    reader: asyncio.StreamReader,
) -> tuple[bool, int, bytes]:
    first, second = await reader.readexactly(2)
    final = bool(first & 0x80)
    opcode = first & 0x0F
    masked = bool(second & 0x80)
    size = second & 0x7F
    if size == 126:
        size = struct.unpack("!H", await reader.readexactly(2))[0]
    elif size == 127:
        size = struct.unpack("!Q", await reader.readexactly(8))[0]
    if size > _MAX_WEBSOCKET_MESSAGE_BYTES:
        raise ValueError("websocket message exceeded the bounded size")
    mask = await reader.readexactly(4) if masked else b""
    payload = await reader.readexactly(size)
    if masked:
        payload = bytes(value ^ mask[index % 4] for index, value in enumerate(payload))
    return final, opcode, payload


async def _relay_websocket(
    websocket: WebSocket,
    reader: asyncio.StreamReader,
    writer: asyncio.StreamWriter,
) -> None:
    async def downstream_to_actor() -> None:
        while True:
            message = await websocket.receive()
            if message["type"] == "websocket.disconnect":
                code = message.get("code", 1000)
                writer.write(
                    _encode_websocket_frame(8, struct.pack("!H", code), masked=True)
                )
                await writer.drain()
                return
            text = message.get("text")
            data = message.get("bytes")
            if text is not None:
                opcode, payload = 1, text.encode("utf-8")
            elif data is not None:
                opcode, payload = 2, data
            else:
                continue
            writer.write(_encode_websocket_frame(opcode, payload, masked=True))
            await writer.drain()

    async def actor_to_downstream() -> None:
        fragments = bytearray()
        message_opcode: int | None = None
        while True:
            final, opcode, payload = await _read_websocket_frame(reader)
            if opcode == 8:
                code = (
                    struct.unpack("!H", payload[:2])[0] if len(payload) >= 2 else 1000
                )
                await websocket.close(code=code)
                return
            if opcode == 9:
                writer.write(_encode_websocket_frame(10, payload, masked=True))
                await writer.drain()
                continue
            if opcode == 10:
                continue
            if opcode in (1, 2):
                fragments.clear()
                message_opcode = opcode
                fragments.extend(payload)
            elif opcode == 0 and message_opcode is not None:
                fragments.extend(payload)
            else:
                raise ValueError("invalid upstream websocket frame")
            if len(fragments) > _MAX_WEBSOCKET_MESSAGE_BYTES:
                raise ValueError("websocket message exceeded the bounded size")
            if final and message_opcode is not None:
                completed = bytes(fragments)
                if message_opcode == 1:
                    await websocket.send_text(completed.decode("utf-8"))
                else:
                    await websocket.send_bytes(completed)
                fragments.clear()
                message_opcode = None

    tasks = {
        asyncio.create_task(downstream_to_actor()),
        asyncio.create_task(actor_to_downstream()),
    }
    try:
        done, pending = await asyncio.wait(tasks, return_when=asyncio.FIRST_COMPLETED)
        for task in done:
            task.result()
        for task in pending:
            task.cancel()
        if pending:
            await asyncio.gather(*pending, return_exceptions=True)
    finally:
        writer.close()
        try:
            await writer.wait_closed()
        except OSError:
            pass


async def _preview_websocket(websocket: WebSocket, parsed: PreviewHost) -> None:
    user_id = (
        websocket.headers.get("x-user-id")
        or websocket.headers.get("cf-access-authenticated-user-email")
        or "local-dev-user"
    )
    target = await _resolve_target(parsed.workspace_id, user_id)
    if target is None:
        await websocket.close(code=4404, reason="Workspace not found")
        return
    await _touch_preview(parsed.workspace_id)
    allowed = set(declared_ports(target.manifest))
    allowed.update(await _reported_ports(target))
    if parsed.port not in allowed:
        await websocket.close(code=4403, reason="Preview port is not available")
        return

    router = urlsplit(settings.substrate_router_address)
    if router.scheme != "http" or not router.hostname:
        await websocket.close(code=1011, reason="Preview router is unavailable")
        return
    raw_path = websocket.scope.get(
        "raw_path", websocket.url.path.encode("ascii", errors="ignore")
    )
    path = raw_path.decode("ascii", errors="replace")
    if websocket.url.query:
        path += "?" + websocket.url.query
    key = websocket.headers.get("sec-websocket-key")
    if not key:
        await websocket.close(code=1002, reason="Invalid websocket handshake")
        return
    offered_protocols = [
        part.strip()
        for part in websocket.headers.get("sec-websocket-protocol", "").split(",")
        if part.strip()
    ]
    timeout = settings.substrate_preview_connect_timeout_seconds

    for attempt in range(2):
        writer = None
        try:
            reader, writer = await asyncio.wait_for(
                asyncio.open_connection(router.hostname, router.port or 80), timeout
            )
            writer.write(
                (
                    f"CONNECT actor-upstream:{parsed.port} HTTP/1.1\r\n"
                    f"Host: actor-upstream:{parsed.port}\r\n"
                    f"ate-target-actor: {target.atespace}/{target.actor}\r\n"
                    "Connection: keep-alive\r\n\r\n"
                ).encode("ascii")
            )
            await writer.drain()
            connect_status, _ = await asyncio.wait_for(
                _read_async_head(reader), timeout
            )
            if connect_status != 200:
                if attempt == 0:
                    writer.close()
                    await writer.wait_closed()
                    continue
                break

            upstream_host = f"actor-upstream:{parsed.port}"
            handshake_headers = [
                ("Host", upstream_host),
                ("Upgrade", "websocket"),
                ("Connection", "Upgrade"),
                ("Sec-WebSocket-Key", key),
                ("Sec-WebSocket-Version", "13"),
            ]
            origin = websocket.headers.get("origin")
            if origin:
                handshake_headers.append(("Origin", origin))
            if offered_protocols:
                handshake_headers.append(
                    ("Sec-WebSocket-Protocol", ", ".join(offered_protocols))
                )
            excluded = (
                _HOP_HEADERS
                | _PRIVATE_HEADERS
                | {
                    "host",
                    "origin",
                    "sec-websocket-key",
                    "sec-websocket-version",
                    "sec-websocket-protocol",
                    "sec-websocket-extensions",
                }
            )
            for name, value in websocket.headers.items():
                if name.lower() not in excluded:
                    handshake_headers.append((name, value))
            wire = (
                f"GET {path} HTTP/1.1\r\n"
                + "".join(f"{name}: {value}\r\n" for name, value in handshake_headers)
                + "\r\n"
            )
            writer.write(wire.encode("latin1"))
            await writer.drain()
            status, headers = await asyncio.wait_for(_read_async_head(reader), timeout)
            if status != 101:
                writer.close()
                await writer.wait_closed()
                if attempt == 0 and status in (502, 503):
                    continue
                break
            expected = base64.b64encode(
                hashlib.sha1(
                    (key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode(),
                    usedforsecurity=False,
                ).digest()
            ).decode("ascii")
            if headers.get("sec-websocket-accept") != expected:
                raise ValueError("upstream websocket handshake was invalid")
            protocol = headers.get("sec-websocket-protocol")
            if protocol and protocol not in offered_protocols:
                raise ValueError("upstream selected an unoffered websocket protocol")
            await websocket.accept(subprotocol=protocol)
            await _relay_websocket(websocket, reader, writer)
            return
        except Exception:
            if writer is not None:
                writer.close()
                try:
                    await writer.wait_closed()
                except OSError:
                    pass
            if attempt == 1:
                break
    await websocket.close(code=1013, reason="Workspace waking up; retry shortly")


def register_preview_proxy(app: FastAPI) -> None:
    """Install wildcard host handling without changing ordinary Mainloop API routing."""

    @app.middleware("http")
    async def preview_http_middleware(request: Request, call_next):
        parsed = parse_preview_host(request.headers.get("host", ""))
        if parsed is None:
            return await call_next(request)
        return await _preview_http(request, parsed)

    @app.websocket("/{path:path}")
    async def preview_websocket_route(websocket: WebSocket, path: str):
        del path
        parsed = parse_preview_host(websocket.headers.get("host", ""))
        if parsed is None:
            await websocket.close(code=4404, reason="Preview route not found")
            return
        await _preview_websocket(websocket, parsed)
