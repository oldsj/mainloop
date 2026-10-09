"""Three fixed GitHub operations. Actor headers and arbitrary fetch URLs are absent."""

import base64
import logging
from collections.abc import Callable
from contextlib import asynccontextmanager
from contextvars import ContextVar
from dataclasses import dataclass
from pathlib import Path
from urllib.parse import urlsplit

import httpx
from mainloop.push_gate.protocol import Limits, TransportError
from mainloop.services.github_auth import GitHubError, app_auth, http_client
from mainloop.services.github_repo import parse_github_repo


@dataclass
class _HttpOperation:
    active: bool = True


_credential_http: ContextVar[_HttpOperation | None] = ContextVar(
    "credential_http", default=None
)


class _HttpLogFactory:
    """Redact transport records before any handler sees headers or exceptions.

    Logger levels are process-wide; the context token isolates concurrent clients.
    Chain the existing record factory and leave unrelated clients/loggers intact.
    """

    def __init__(self, previous):
        self.previous = previous

    def __call__(self, *args, **kwargs):
        record = self.previous(*args, **kwargs)
        operation = _credential_http.get()
        if (
            operation is not None
            and operation.active
            and record.name.split(".", 1)[0] in ("httpcore", "httpx")
        ):
            # Trace arguments may contain headers, request objects or exception
            # text before the caller can check reflection. Retain only provenance
            # and severity, including HTTPX's response reason-phrase log at INFO.
            record.msg = "credentialed_http_transport"
            record.args = ()
            record.exc_info = None
            record.exc_text = None
            record.stack_info = None
        return record


@asynccontextmanager
async def _private_http_logs():
    factory = logging.getLogRecordFactory()
    if not isinstance(factory, _HttpLogFactory):
        logging.setLogRecordFactory(_HttpLogFactory(factory))
    operation = _HttpOperation()
    marker = _credential_http.set(operation)
    try:
        yield
    finally:
        # Descendant tasks copy the context but share this token. End redaction
        # for them too, including on failure/cancellation; reset restores any
        # still-active outer operation in the exiting task's context.
        operation.active = False
        _credential_http.reset(marker)


@dataclass(frozen=True)
class LoopbackFixture:
    """Explicit constructor-only test injection; never accepted from an HTTP request."""

    origin: str
    # Each invocation returns a fresh transport owned by that request's client.
    transport_factory: Callable[[], httpx.AsyncBaseTransport] | None = None

    def __post_init__(self):
        url = urlsplit(self.origin)
        if (
            url.scheme != "http"
            or url.hostname != "127.0.0.1"
            or not url.port
            or url.path
            or url.query
            or url.fragment
            or url.username
            or url.password
        ):
            raise ValueError("loopback_fixture_required")


class FixedGitUpstream:
    def __init__(
        self,
        repository: str,
        token: str,
        limits: Limits,
        *,
        fixture: LoopbackFixture | None = None,
    ):
        self.repository = parse_github_repo(repository).full_name
        if not token or any(char in token for char in "\r\n"):
            raise ValueError("upstream_credential_required")
        self._authorization = (
            "Basic " + base64.b64encode(f"x-access-token:{token}".encode()).decode()
        )
        self._secrets = (
            token.encode(),
            self._authorization.encode(),
            self._authorization[6:].encode(),
            base64.b64encode(token.encode()),
        )
        self._origin = fixture.origin if fixture else "https://github.com"
        self._transport_factory = fixture.transport_factory if fixture else None
        self.limits = limits

    def check_output(self, data: bytes, additional: tuple[bytes, ...] = ()):
        extra = tuple(
            variant
            for secret in additional
            if secret
            for variant in (secret, base64.b64encode(secret))
        )
        if any(secret and secret in data for secret in (*self._secrets, *extra)):
            raise TransportError("upstream_reflection")

    async def _request(
        self,
        service: str,
        *,
        discovery: bool,
        protocol: str | None = None,
        body: bytes | Path = b"",
        secrets: tuple[bytes, ...] = (),
    ) -> bytes:
        headers = {
            "Authorization": self._authorization,
            "Accept": f"application/x-{service}-{'advertisement' if discovery else 'result'}",
            "Accept-Encoding": "identity",
        }
        if protocol is not None:
            if (
                protocol not in ("version=0", "version=1", "version=2")
                or service != "git-upload-pack"
            ):
                raise TransportError("git_protocol")
            headers["Git-Protocol"] = protocol
        if discovery:
            suffix = f"info/refs?service={service}"
        else:
            suffix = service
            headers["Content-Type"] = f"application/x-{service}-request"
        if isinstance(body, Path):

            async def content():
                with body.open("rb") as source:
                    while chunk := source.read(64 * 1024):
                        yield chunk

            data = content()
            headers["Content-Length"] = str(body.stat().st_size)
        else:
            data = body
        # AsyncClient closes its transport on exit, including cancellation. A pool
        # must therefore belong to this request, never another concurrent request.
        transport = (
            self._transport_factory()
            if self._transport_factory
            else httpx.AsyncHTTPTransport(retries=0, verify=True, trust_env=False)
        )
        async with _private_http_logs(), httpx.AsyncClient(
            transport=transport,
            trust_env=False,
            follow_redirects=False,
            timeout=httpx.Timeout(self.limits.dispatch_seconds, connect=10),
        ) as client:
            async with client.stream(
                "GET" if discovery else "POST",
                f"{self._origin}/{self.repository}.git/{suffix}",
                headers=headers,
                content=data,
            ) as response:
                self.check_output(
                    b"\n".join(
                        name + b":" + value for name, value in response.headers.raw
                    ),
                    secrets,
                )
                if response.status_code != 200:
                    raise TransportError("upstream_http")
                response_headers = {}
                for name, value in response.headers.raw:
                    name = name.lower()
                    if name in response_headers and name in (
                        b"content-type",
                        b"content-encoding",
                        b"content-length",
                        b"transfer-encoding",
                    ):
                        raise TransportError("upstream_framing")
                    response_headers[name] = value
                if b"transfer-encoding" in response_headers and (
                    b"content-length" in response_headers
                    or response_headers[b"transfer-encoding"].lower() != b"chunked"
                ):
                    raise TransportError("upstream_framing")
                expected = headers["Accept"]
                if (
                    response.headers.get("content-type", "").lower() != expected
                    or response.headers.get("content-encoding", "identity").lower()
                    != "identity"
                ):
                    raise TransportError("upstream_framing")
                output = bytearray()
                async for chunk in response.aiter_raw():
                    if len(output) + len(chunk) >= self.limits.response_bytes:
                        raise TransportError("response_limit")
                    output.extend(chunk)
                result = bytes(output)
                self.check_output(result, secrets)
                return result

    async def discovery(
        self,
        service: str,
        *,
        protocol: str | None = None,
        secrets: tuple[bytes, ...] = (),
    ) -> bytes:
        if service not in ("git-upload-pack", "git-receive-pack"):
            raise TransportError("upstream_service")
        return await self._request(
            service, discovery=True, protocol=protocol, secrets=secrets
        )

    async def upload_pack(
        self,
        body: bytes,
        *,
        protocol: str | None = None,
        secrets: tuple[bytes, ...] = (),
    ) -> bytes:
        if len(body) >= self.limits.body_bytes:
            raise TransportError("body_limit")
        return await self._request(
            "git-upload-pack",
            discovery=False,
            protocol=protocol,
            body=body,
            secrets=secrets,
        )

    async def receive_pack(
        self, body: Path, *, secrets: tuple[bytes, ...] = ()
    ) -> bytes:
        return await self._request(
            "git-receive-pack", discovery=False, body=body, secrets=secrets
        )


class GitHubAppUpstream(FixedGitUpstream):
    """Request-local Git upstream using the shared repository/permission token cache.

    Tokens are acquired only when dispatch authority calls an operation. Quarantine
    reads use contents:read even on a push request; receive operations use write.
    Each operation retains its own fixed client and authorization header.
    """

    def __init__(
        self,
        repository: str,
        limits: Limits,
        *,
        auth=None,
        fixture: LoopbackFixture | None = None,
    ):
        self.repository = parse_github_repo(repository).full_name.lower()
        self.limits = limits
        self._auth = auth
        self._fixture = fixture
        self._secrets = ()

    async def _request(
        self,
        service: str,
        *,
        discovery: bool,
        protocol: str | None = None,
        body: bytes | Path = b"",
        secrets: tuple[bytes, ...] = (),
    ) -> bytes:
        if service not in ("git-upload-pack", "git-receive-pack"):
            raise TransportError("upstream_service")
        permissions = {"contents": "read" if service == "git-upload-pack" else "write"}
        try:
            async with _private_http_logs(), http_client() as client:
                token = await (self._auth or app_auth()).token(
                    client, self.repository, permissions
                )
        except GitHubError:
            raise TransportError("upstream_credential_unavailable") from None
        # PolicyError retains the authenticator's safe missing-installation refusal.
        upstream = FixedGitUpstream(
            self.repository, token, self.limits, fixture=self._fixture
        )
        self._secrets = tuple(set((*self._secrets, *upstream._secrets)))
        try:
            result = await upstream._request(
                service,
                discovery=discovery,
                protocol=protocol,
                body=body,
                secrets=secrets,
            )
            self.check_output(result, secrets)
            return result
        except httpx.HTTPError:
            # HTTPX exceptions may retain credential-bearing requests.
            raise TransportError("upstream_unavailable") from None
