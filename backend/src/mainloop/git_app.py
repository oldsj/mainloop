"""Dedicated Git read/push sidecar. Run with ``python -m mainloop.git_app``."""

import asyncio
import signal
import tempfile
from contextlib import ExitStack, asynccontextmanager, nullcontext
from dataclasses import replace
from pathlib import Path

import uvicorn
from mainloop.config import settings
from mainloop.db import db
from mainloop.push_gate.protocol import DEFAULT_LIMITS, TransportError
from mainloop.push_gate.transport import create_git_applications
from mainloop.push_gate.transport_authority import PostgresTransportAuthority
from mainloop.push_gate.upstream import GitHubAppUpstream
from mainloop.runtime.agent_identity import require_token_key
from mainloop.runtime.native_sessions import close_client, get_client
from mainloop.services.github_auth import app_auth
from mainloop.services.github_pr import get_repo_metadata
from starlette.responses import Response

SPOOL_ROOT = Path("/var/lib/mainloop/git")
READ_PORT = 8003
PUSH_PORT = 8004


class GitListener:
    """Keep repository upstreams request-local and all pack validation serialized."""

    def __init__(self, template, common, slot, upstream_factory):
        self.template = template
        self.common = common
        self.slot = slot
        self.upstream_factory = upstream_factory

    async def __call__(self, scope, receive, send):
        if scope["type"] != "http":
            return
        if not settings.git_transport_enabled or (
            self.template.purpose == "git-push" and not settings.push_gate_enabled
        ):
            return await Response("git_transport_disabled\n", status_code=403)(
                scope, receive, send
            )
        try:
            repository, *_ = self.template.request(scope)
        except (TransportError, ValueError):
            return await Response("route_denied\n", status_code=403)(
                scope, receive, send
            )
        # Parsing a route grants no authority. The core authenticates, checks the
        # bound repository and revalidates dispatch before any token mint or Git I/O.
        upstream = self.upstream_factory(repository, self.common["limits"])
        read, push = create_git_applications(upstream=upstream, **self.common)
        app = read if self.template.purpose == "git-read" else push
        app.slot = self.slot
        await app(scope, receive, send)


def create_applications(
    authority,
    spool_root: Path,
    *,
    limits=DEFAULT_LIMITS,
    upstream_factory=GitHubAppUpstream,
):
    common = dict(
        authority=authority,
        spool_root=spool_root,
        limits=limits,
        seed_upstream_factory=authority.seed_upstream if authority else None,
    )
    templates = create_git_applications(**common)
    slot = asyncio.Semaphore(1)
    return tuple(
        GitListener(template, common, slot, upstream_factory) for template in templates
    )


@asynccontextmanager
async def production_applications(spool_root: Path = SPOOL_ROOT):
    """One database pool and kagent client for both ports; no reconciliation loops."""
    authority = None
    enabled = settings.git_transport_enabled
    limits = replace(
        DEFAULT_LIMITS,
        command_bytes=settings.push_gate_max_command_bytes,
        body_bytes=settings.push_gate_max_body_bytes,
    )
    with ExitStack() as resources:
        try:
            if enabled:
                require_token_key()
                app_auth()  # Validate App configuration before accepting enabled traffic.
                spool_root.mkdir(parents=True, exist_ok=True)
                # The fsGroup-accessible mount belongs to Kubernetes. Own a unique
                # private child rather than changing its ownership or permissions.
                private = resources.enter_context(
                    tempfile.TemporaryDirectory(prefix="process-", dir=spool_root)
                )
                spool_root = Path(private)
                spool_root.chmod(0o700)  # Also clear an inherited setgid bit.
                await db.connect()
                authority = PostgresTransportAuthority(
                    db, get_client(), get_repo_metadata
                )
            yield create_applications(authority, spool_root, limits=limits)
        finally:
            if enabled:
                try:
                    await close_client()
                finally:
                    await db.disconnect()


class ListenerServer(uvicorn.Server):
    def capture_signals(self):
        # The sidecar handles shutdown once for both listeners.
        return nullcontext()


async def serve():
    async with production_applications() as applications:
        servers = [
            ListenerServer(
                uvicorn.Config(
                    app,
                    host="0.0.0.0",  # nosec B104 - private, NetworkPolicy-bound ports
                    port=port,
                    lifespan="off",  # Resources belong to production_applications.
                    access_log=False,
                    proxy_headers=False,
                )
            )
            for app, port in zip(applications, (READ_PORT, PUSH_PORT), strict=True)
        ]

        def stop():
            for server in servers:
                server.should_exit = True

        loop = asyncio.get_running_loop()
        signals = (signal.SIGINT, signal.SIGTERM)
        for sig in signals:
            loop.add_signal_handler(sig, stop)
        tasks = [asyncio.create_task(server.serve()) for server in servers]
        try:
            # Failure or completion of either server stops the paired listener.
            await asyncio.wait(tasks, return_when=asyncio.FIRST_COMPLETED)
        finally:
            stop()
            try:
                await asyncio.gather(*tasks)
            finally:
                for sig in signals:
                    loop.remove_signal_handler(sig)


if __name__ == "__main__":
    asyncio.run(serve())
