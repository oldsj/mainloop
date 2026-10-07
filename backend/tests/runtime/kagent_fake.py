"""Fake kagent gateway for tests: SessionService (grpc-web) and A2A v1 JSON-RPC over httpx.

Fixture-backed: the event shapes come from ``fixtures/kagent`` (sanitized from a live capture).
Nothing here opens a socket.
"""

from __future__ import annotations

import json
from pathlib import Path

import httpx
from mainloop.runtime.kagent_client import (
    RuntimeOperation,
    RuntimeState,
    SessionWorkspace,
    _field_bytes,
    _field_str,
    _varint,
    decode_fields,
    grpc_web_frame,
)

FIXTURES = Path(__file__).parent / "fixtures" / "kagent"
TASK_ID = "task-fixture-1"
CONTEXT_ID = "00000000-0000-4000-8000-000000000001"


def fixture_json(name: str) -> dict:
    return json.loads((FIXTURES / name).read_text())


def stream_chunks(message_id: str, text: str = "hello") -> list[str]:
    """Return the captured turn as SSE event chunks, with the message id and text filled in."""
    raw = (FIXTURES / "turn-stream.sse").read_text()
    escaped = json.dumps(text)[1:-1]  # the template sits inside JSON strings
    raw = raw.replace("{message_id}", json.dumps(message_id)[1:-1]).replace(
        "{text}", escaped
    )
    return [chunk + "\n\n" for chunk in raw.split("\n\n") if chunk.strip()]


def unauthorized_envelope(message: str = "permission denied") -> dict:
    """a2a-go v2.6.0 ErrUnauthorized wire shape, with sanitized test messages."""
    return {
        "jsonrpc": "2.0",
        "id": "fixture-request",
        "error": {
            "code": -31403,
            "message": message,
            "data": [
                {
                    "@type": "type.googleapis.com/google.rpc.ErrorInfo",
                    "domain": "a2a-protocol.org",
                    "reason": "UNAUTHORIZED",
                }
            ],
        },
    }


def session_message(
    session_id: str,
    state: RuntimeState = RuntimeState.READY,
    operation: RuntimeOperation = RuntimeOperation.NONE,
    workspace: bytes = b"",
) -> bytes:
    session = (
        _field_str(1, session_id)
        + _varint(7 << 3)
        + _varint(int(state))
        + _varint(8 << 3)
        + _varint(int(operation))
        + _field_str(14, session_id)
        + (_field_bytes(16, workspace) if workspace else b"")
    )
    return _field_bytes(1, session)


def grpc_response(
    message: bytes | None, *, status: int = 0, detail: str = ""
) -> httpx.Response:
    body = grpc_web_frame(message) if message is not None else b""
    trailer = f"grpc-status: {status}\r\n"
    if detail:
        trailer += f"grpc-message: {detail}\r\n"
    body += b"\x80" + len(trailer.encode()).to_bytes(4, "big") + trailer.encode()
    return httpx.Response(
        200, content=body, headers={"content-type": "application/grpc-web+proto"}
    )


class BreakingStream(httpx.AsyncByteStream):
    """Yields some chunks and then fails like a dropped connection."""

    def __init__(self, chunks: list[str]):
        self.chunks = chunks

    async def __aiter__(self):
        for chunk in self.chunks:
            yield chunk.encode()
        raise httpx.ReadError("connection reset")


class FakeKagent:
    """Records every request and answers from scripted behaviour.

    ``send_script`` is a list consumed one entry per SendStreamingMessage request:
    ``"ok"``, ``"not-accepted"`` (SSE error event), ``"not-accepted-json"`` (plain body),
    ``"other-error"``, ``"cut"`` (stream breaks after the first events), ``"drop"`` (the
    request is lost before any response), ``"lost-response"`` (kagent accepted and completed the
    task but the response never arrives), or ``"unreachable"``. ``list_tasks_fails`` makes
    ``ListTasks`` answer HTTP 503.
    """

    def __init__(self):
        self.requests: list[tuple[str, str, dict | bytes]] = []
        self.send_script: list[str] = []
        self.sessions: dict[str, tuple[RuntimeState, RuntimeOperation]] = {}
        self.created_request_ids: dict[str, str] = {}
        # Workspace bytes (CreateSession field 6) persisted per Session id, as kagent does.
        self.workspaces: dict[str, bytes] = {}
        self.tasks: dict[str, dict] = {}
        self.accepted_message_ids: list[str] = []
        self.subscribe_events: list[str] | None = None
        self.cut_after = 2
        self.list_tasks_fails = False
        # CancelTask answers HTTP 503, or returns the task still running.
        self.cancel_task_fails = False
        self.cancel_task_ignored = False
        self.default_page_size = 50
        # ListSessions page size (0 = everything in one page).
        self.list_page_size = 0
        # Ids handed to the next CreateSession calls with a new request id (then CONTEXT_ID).
        self.next_session_ids: list[str] = []

    # ---- helpers for tests -------------------------------------------------------------
    def rpc_calls(self, method: str) -> list[dict]:
        return [
            body
            for _, path, body in self.requests
            if path.startswith("/agents/")
            and isinstance(body, dict)
            and body["method"] == method
        ]

    def session_calls(self, method: str) -> list[bytes]:
        return [
            body
            for _, path, body in self.requests
            if path.endswith("/" + method) and isinstance(body, bytes)
        ]

    def created_workspaces(self) -> list[SessionWorkspace | None]:
        """Return the workspace carried by each CreateSession call, in order."""
        out: list[SessionWorkspace | None] = []
        for message in self.session_calls("CreateSession"):
            raw = decode_fields(message).get(6, [b""])[0]
            out.append(SessionWorkspace.decode(raw) if raw else None)
        return out

    def transport(self) -> httpx.MockTransport:
        return httpx.MockTransport(self.handle)

    # ---- handler -----------------------------------------------------------------------
    def handle(self, request: httpx.Request) -> httpx.Response:
        path = request.url.path
        if "SessionService" in path:
            return self._session(request, path)
        body = json.loads(request.content)
        self.requests.append((request.method, path, body))
        method = body["method"]
        if method == "SendStreamingMessage":
            return self._send(body)
        if method == "SubscribeToTask":
            chunks = self.subscribe_events or []
            return httpx.Response(
                200,
                headers={"content-type": "text/event-stream"},
                content="".join(chunks),
            )
        if method == "GetTask":
            task = self.tasks.get(body["params"]["id"])
            if task is None:
                return httpx.Response(200, json=fixture_json("task-not-found.json"))
            return httpx.Response(
                200, json={"jsonrpc": "2.0", "id": body["id"], "result": task}
            )
        if method == "ListTasks":
            if self.list_tasks_fails:
                return httpx.Response(503)
            return httpx.Response(
                200,
                json={
                    "jsonrpc": "2.0",
                    "id": body["id"],
                    "result": self._list(body["params"]),
                },
            )
        if method == "CancelTask":
            if self.cancel_task_fails:
                return httpx.Response(503)
            task = self.tasks.get(body["params"]["id"])
            if task is None:
                return httpx.Response(200, json=fixture_json("task-not-found.json"))
            if not self.cancel_task_ignored and not task["status"]["state"].endswith(
                ("COMPLETED", "CANCELED", "FAILED")
            ):
                task["status"]["state"] = "TASK_STATE_CANCELED"
            return httpx.Response(
                200, json={"jsonrpc": "2.0", "id": body["id"], "result": task}
            )
        return httpx.Response(404)

    def _list(self, params: dict) -> dict:
        """ListTasks as kagent serves it: oldest first, paged (50 by default), artifacts left out unless
        ``includeArtifacts`` (``interactions.go`` ``ListTasks``/``shapeTask``)."""
        tasks = [
            t for t in self.tasks.values() if t.get("contextId") == params["contextId"]
        ]
        start = int(params.get("pageToken") or 0)
        size = params.get("pageSize") or self.default_page_size
        page = tasks[start : start + size]
        if not params.get("includeArtifacts"):
            page = [{k: v for k, v in t.items() if k != "artifacts"} for t in page]
        more = start + size < len(tasks)
        return {
            "tasks": page,
            "totalSize": len(tasks),
            "pageSize": size,
            "nextPageToken": str(start + size) if more else "",
        }

    def _session(self, request: httpx.Request, path: str) -> httpx.Response:
        method = path.rsplit("/", 1)[1]
        frame = request.content
        message = frame[5:]
        self.requests.append((request.method, path, message))
        fields = decode_fields(message)
        if method == "CreateSession":
            request_id = fields[3][0].decode()
            known = self.created_request_ids.get(request_id)
            if known is not None and self.sessions.get(known, (None,))[0] in (
                RuntimeState.DELETED,
                None,
            ):
                return grpc_response(
                    None, status=9, detail="request_id belongs to a deleted Session"
                )
            if known is None:
                known = (
                    self.next_session_ids.pop(0)
                    if self.next_session_ids
                    else CONTEXT_ID
                )
            workspace = fields.get(6, [b""])[0]
            session_id = self.created_request_ids.setdefault(request_id, known)
            if (
                session_id in self.workspaces
                and self.workspaces[session_id] != workspace
            ):
                return grpc_response(
                    None, status=6, detail="workspace differs for this request_id"
                )
            self.workspaces.setdefault(session_id, workspace)
            self.sessions.setdefault(
                session_id, (RuntimeState.READY, RuntimeOperation.NONE)
            )
            return grpc_response(
                session_message(
                    session_id,
                    *self.sessions[session_id],
                    workspace=self.workspaces[session_id],
                )
            )
        if method == "ListSessions":
            return self._list_sessions(fields)
        session_id = fields[1][0].decode()
        if session_id not in self.sessions:
            return grpc_response(None, status=5, detail="session not found")
        state, op = self.sessions[session_id]
        if method == "SuspendSession":
            self.sessions[session_id] = (RuntimeState.SUSPENDED, RuntimeOperation.NONE)
        elif method == "ResumeSession":
            self.sessions[session_id] = (RuntimeState.READY, RuntimeOperation.NONE)
        elif method == "DeleteSession":
            self.sessions[session_id] = (RuntimeState.DELETED, RuntimeOperation.NONE)
        state, op = self.sessions[session_id]
        return grpc_response(
            session_message(
                session_id, state, op, workspace=self.workspaces.get(session_id, b"")
            )
        )

    def _list_sessions(self, fields: dict) -> httpx.Response:
        """ListSessions as kagent serves it: ids in order, ``list_page_size`` per page, and a
        ``next_page_token`` (the offset) while more remain."""
        page = decode_fields(fields[3][0]) if 3 in fields else {}
        start = int(page[2][0].decode()) if 2 in page else 0
        ids = sorted(self.sessions)
        size = self.list_page_size or len(ids) or 1
        chosen = ids[start : start + size]
        body = b"".join(
            session_message(
                sid, *self.sessions[sid], workspace=self.workspaces.get(sid, b"")
            )
            for sid in chosen
        )
        if start + size < len(ids):
            body += _field_bytes(2, _field_str(1, str(start + size)))
        return grpc_response(body or None)

    def _send(self, body: dict) -> httpx.Response:
        message = body["params"]["message"]
        step = self.send_script.pop(0) if self.send_script else "ok"
        if step == "unreachable":
            raise httpx.ConnectError("no route")
        if step == "drop":
            raise httpx.ReadTimeout("lost")
        if step == "lost-response":
            self.accepted_message_ids.append(message["messageId"])
            self._record_task(message, completed=True)
            raise httpx.ReadTimeout("response lost after kagent accepted the message")
        if step == "not-accepted":
            chunk = (
                "id: e1\ndata: "
                + json.dumps(fixture_json("send-not-accepted.json"))
                + "\n\n"
            )
            return httpx.Response(
                200, headers={"content-type": "text/event-stream"}, content=chunk
            )
        if step == "not-accepted-json":
            return httpx.Response(200, json=fixture_json("send-not-accepted.json"))
        if step == "other-error":
            return httpx.Response(200, json=fixture_json("other-error.json"))
        chunks = stream_chunks(message["messageId"], message["parts"][0]["text"])
        self.accepted_message_ids.append(message["messageId"])
        self._record_task(message, completed=step != "cut")
        headers = {"content-type": "text/event-stream"}
        if step == "cut":
            return httpx.Response(
                200, headers=headers, stream=BreakingStream(chunks[: self.cut_after])
            )
        return httpx.Response(200, headers=headers, content="".join(chunks))

    def _record_task(self, message: dict, *, completed: bool) -> None:
        final = json.loads(
            stream_chunks(message["messageId"], message["parts"][0]["text"])[-1].split(
                "data: ", 1
            )[1]
        )
        task = final["result"]["task"]
        if not completed:
            task["status"] = {"state": "TASK_STATE_WORKING"}
        task["contextId"] = message["contextId"]
        self.tasks[task["id"]] = task
