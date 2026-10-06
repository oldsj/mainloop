"""Dedicated, stateless Mainloop MCP listener; it exposes no REST routes."""

from contextlib import asynccontextmanager
from contextvars import ContextVar

from fastapi import HTTPException
from mainloop.db import db
from mainloop.runtime.agent_tools import AgentService, Ctx
from mainloop.runtime.policy import PolicyError, may_call, tools_for
from mcp.server.fastmcp import FastMCP
from mcp.server.transport_security import TransportSecuritySettings
from mcp.types import CallToolResult, TextContent, Tool
from pydantic import ValidationError
from starlette.responses import Response

from models.agent_tools import (
    Delegate,
    OptionalSession,
    PendingDone,
    Read,
    Record,
    Report,
    RequiredSession,
    ToolInput,
    TopicOpen,
)

_context: ContextVar[Ctx] = ContextVar("mainloop_agent")

# Registry is shared by discovery and invocation; future approval groups can select subsets.
TOOLS = {
    "whoami": (ToolInput, "Show your binding identity and role."),
    "topics": (ToolInput, "List topics and pending counts."),
    "topic_open": (TopicOpen, "Create or select a topic and set its status."),
    "note": (Record, "Write a durable topic note."),
    "decide": (Record, "Record a topic decision."),
    "pending_add": (Record, "Record pending user intent."),
    "pending_done": (PendingDone, "Close a pending item by id or unique prefix."),
    "delegate": (Delegate, "Start a child agent for a task."),
    "report": (Report, "Report your result to your parent exactly once."),
    "status": (OptionalSession, "Read child state without sending a turn."),
    "read": (Read, "Read stored child messages."),
    "cancel": (RequiredSession, "Stop a child in your tree."),
    "clear": (OptionalSession, "Archive finished children in your tree."),
}


async def invoke(
    service: AgentService, ctx: Ctx, name: str, arguments: dict
) -> CallToolResult:
    try:
        may_call(ctx.actor, name)
        body = TOOLS[name][0].model_validate(arguments)
        if name == "whoami":
            b = ctx.binding
            result = {
                "text": f"{b['role']} {b['kind']} session={b['session_id'][:8]} depth={ctx.actor.depth}",
                "session_id": b["session_id"],
                "role": b["role"],
                "depth": ctx.actor.depth,
            }
        elif name in ("note", "decide", "pending_add"):
            result = await service.record(
                ctx,
                {"note": "note", "decide": "decision", "pending_add": "pending"}[name],
                body.text,
                body.topic,
            )
        elif name == "pending_done":
            result = await service.done(ctx, body.id)
        else:
            args = body.model_dump()
            if "session" in args:
                args["session_id"] = args.pop("session")
            result = await getattr(service, name)(ctx, **args)
        return CallToolResult(
            content=[TextContent(type="text", text=result["text"])],
            structuredContent=result,
        )
    except PolicyError as exc:
        error = f"[{exc.code}] {exc.message}"
    except ValidationError:
        error = "[validation] invalid tool arguments"
    except HTTPException as exc:
        error = str(exc.detail)
        if not error.startswith("["):
            error = f"[{exc.status_code}] {error}"
    return CallToolResult(content=[TextContent(type="text", text=error)], isError=True)


class AgentAuth:
    def __init__(self, app, service):
        self.app, self.service = app, service

    async def __call__(self, scope, receive, send):
        if scope["type"] != "http":
            return await self.app(scope, receive, send)
        if scope["path"] != "/mcp":
            return await Response(status_code=404)(scope, receive, send)
        headers = dict(scope["headers"])
        scheme, _, token = (
            headers.get(b"authorization", b"").decode("latin1").partition(" ")
        )
        try:
            if scheme.lower() != "bearer" or not token:
                raise HTTPException(401)
            ctx = await self.service.authenticate(token)
        except HTTPException:
            return await Response(status_code=401)(scope, receive, send)
        marker = _context.set(ctx)
        try:
            await self.app(scope, receive, send)
        finally:
            _context.reset(marker)


def create_app(service: AgentService | None = None):
    managed = service is None
    if service is None:
        from mainloop.runtime.delegation import PgStore

        service = AgentService(PgStore())
    server = FastMCP(
        "mainloop",
        stateless_http=True,
        json_response=True,
        transport_security=TransportSecuritySettings(
            allowed_hosts=[
                "mainloop-mcp.mainloop.svc.cluster.local",
                "mainloop-mcp.mainloop.svc.cluster.local:*",
                "localhost:*",
                "127.0.0.1:*",
                "testserver",
            ],
            allowed_origins=[],
        ),
    )

    @server._mcp_server.list_tools()
    async def list_tools():
        return [
            Tool(
                name=name,
                description=description,
                inputSchema=model.model_json_schema(),
            )
            for name, (model, description) in TOOLS.items()
            if name in tools_for(_context.get().actor)
        ]

    @server._mcp_server.call_tool(validate_input=False)
    async def call_tool(name, arguments):
        return await invoke(service, _context.get(), name, arguments)

    http_app = server.streamable_http_app()

    @asynccontextmanager
    async def lifespan(app):
        if managed:
            from mainloop.runtime.agent_identity import require_token_key

            require_token_key()
            await db.connect()
        try:
            async with server.session_manager.run():
                yield
        finally:
            if managed:
                from mainloop.runtime.native_sessions import close_client

                await close_client()
                await db.disconnect()

    http_app.router.lifespan_context = lifespan
    return AgentAuth(http_app, service)


app = create_app()
