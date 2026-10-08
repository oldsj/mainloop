"""Standalone injectable Git ASGI listeners. Production authority is deliberately absent.

P2 must implement purpose-bound authentication, attested runtime freshness, ordered
policy/publication locks (no network transaction), and a durable unresolved-write fence.
Nothing here imports the owner API, publishes credentials, or enables a production route.
"""

import asyncio
import base64
import re
import uuid
from collections.abc import AsyncIterator
from contextlib import AbstractAsyncContextManager
from dataclasses import dataclass
from pathlib import Path
from typing import Literal, Protocol

from mainloop.push_gate.authorization import ZERO_OID, authorize
from mainloop.push_gate.protocol import (
    DEFAULT_LIMITS,
    Limits,
    TransportError,
    advertised_refs,
    push_advertisement,
    receipt,
    report,
)
from mainloop.push_gate.quarantine import PreparedPush, prepare_receive, receive_spool
from mainloop.push_gate.upstream import FixedGitUpstream
from mainloop.services.github_repo import parse_github_repo
from models.push_gate import (
    ProtectedBranchPolicy,
    PublicationAttempt,
    PublicationState,
    PushGrant,
)
from pydantic import BaseModel, ConfigDict, Field


class RuntimeAssociation(BaseModel):
    model_config = ConfigDict(extra="forbid", frozen=True)
    session_id: str = Field(min_length=1)
    generation_id: str = Field(min_length=1)
    atespace: str = Field(min_length=1)
    actor_name: str = Field(min_length=1)
    actor_uid: str = Field(min_length=1)
    revision: str = Field(min_length=1)


class TrustedBinding(BaseModel):
    model_config = ConfigDict(extra="forbid", frozen=True)
    purpose: Literal["git-read", "git-push"]
    repository: str = Field(min_length=1)
    association: RuntimeAssociation
    grant: PushGrant | None = None
    policy: ProtectedBranchPolicy | None = None


@dataclass(frozen=True)
class DispatchProof:
    """Yielded under dedicated ordered locks, after fresh metadata/runtime resolution."""

    binding: TrustedBinding


@dataclass(frozen=True)
class AttemptEvidence:
    """P2 must store these immutable facts alongside the existing PublicationAttempt."""

    attempt: PublicationAttempt
    body_sha256: str
    body_bytes: int
    association: RuntimeAssociation
    incoming_objects: int
    expanded_bytes: int
    validated_objects: int
    disk_bytes: int


class TransportAuthority(Protocol):
    async def authenticate(self, capability: str, purpose: str) -> TrustedBinding:
        """Revalidate owned canonical binding and actual current runtime association."""
        ...

    def authorize_dispatch(
        self, binding: TrustedBinding, prepared: PreparedPush | None
    ) -> AbstractAsyncContextManager[DispatchProof]:
        """Refresh authority; retain ordered advisory locks through dispatch/outcome.

        Reject unresolved attempts across bearer rotations. Reads/discovery also require
        current authority. Default/repository metadata must be refreshed here; P1 never
        interprets a header or locator as runtime identity. No DB transaction spans I/O.
        """
        ...

    async def record(self, evidence: AttemptEvidence) -> None:
        """Durably record PENDING and all immutable evidence before DISPATCHING."""
        ...

    async def transition(
        self, evidence: AttemptEvidence, state: PublicationState
    ) -> None:
        """Commit before returning. Failure after dispatch retains DISPATCHING/UNKNOWN."""
        ...


class GitApplication:
    def __init__(
        self,
        purpose: Literal["git-read", "git-push"],
        *,
        authority: TransportAuthority | None = None,
        upstream: FixedGitUpstream | None = None,
        spool_root: Path | None = None,
        limits: Limits = DEFAULT_LIMITS,
        hosts: tuple[str, ...] = (),
        validation_slot: asyncio.Semaphore | None = None,
        response_secrets: tuple[bytes, ...] = (),
    ):
        self.purpose = purpose
        self.authority = authority
        self.upstream = upstream
        self.spool_root = spool_root
        self.limits = limits
        self.hosts = hosts or (f"mainloop-{purpose}.mainloop.svc.cluster.local",)
        self.slot = validation_slot or asyncio.Semaphore(1)
        self.response_secrets = response_secrets

    async def __call__(self, scope, receive, send):
        if scope["type"] == "lifespan":
            while True:
                message = await receive()
                if message["type"] == "lifespan.startup":
                    if (
                        self.authority is None
                        or self.upstream is None
                        or self.spool_root is None
                    ):
                        await send(
                            {
                                "type": "lifespan.startup.failed",
                                "message": "production_authority_absent",
                            }
                        )
                        return
                    await send({"type": "lifespan.startup.complete"})
                elif message["type"] == "lifespan.shutdown":
                    await send({"type": "lifespan.shutdown.complete"})
                    return
            return
        if scope["type"] != "http":
            return
        request_id = uuid.uuid4().hex
        try:
            if (
                self.authority is None
                or self.upstream is None
                or self.spool_root is None
            ):
                raise TransportError("production_authority_absent")
            async with asyncio.timeout(
                self.limits.request_seconds
                + self.limits.validation_seconds
                + self.limits.dispatch_seconds
            ):
                result, content_type = await self.handle(scope, receive, request_id)
            status = 200
        except asyncio.CancelledError:
            raise
        except Exception as exc:
            # Never format upstream/subprocess/adapter exceptions or packet payloads.
            code = (
                exc.code if isinstance(exc, TransportError) else "transport_unavailable"
            )
            result = f"{code} {request_id}\n".encode()
            content_type = "text/plain"
            status = (
                503
                if code
                in (
                    "production_authority_absent",
                    "publication_unknown",
                    "transport_unavailable",
                )
                else 403
            )
        if self.upstream is not None:
            known = tuple(
                value.removeprefix(b"Bearer ")
                for key, value in scope.get("headers", [])
                if key.lower() == b"authorization"
            )
            try:
                self.upstream.check_output(result, (*self.response_secrets, *known))
            except TransportError:
                result, status = b"", 503
        await send(
            {
                "type": "http.response.start",
                "status": status,
                "headers": [
                    (b"content-type", content_type.encode()),
                    (b"content-length", str(len(result)).encode()),
                    (b"cache-control", b"no-store"),
                    (b"x-mainloop-request-id", request_id.encode()),
                ],
            }
        )
        await send({"type": "http.response.body", "body": result})

    def request(self, scope):
        raw = scope.get("raw_path", b"")
        try:
            path = raw.decode("ascii")
        except UnicodeError:
            raise TransportError("path_invalid") from None
        match = re.fullmatch(
            r"/([A-Za-z0-9-]+)/([A-Za-z0-9._-]+)\.git/(info/refs|git-upload-pack|git-receive-pack)",
            path,
        )
        if not match or "%" in path or "\\" in path:
            raise TransportError("path_invalid")
        repo = parse_github_repo(f"{match[1]}/{match[2]}").full_name
        if f"/{repo}.git/{match[3]}".lower() != path.lower():
            raise TransportError("path_invalid")
        service = (
            "git-upload-pack" if self.purpose == "git-read" else "git-receive-pack"
        )
        discovery = match[3] == "info/refs"
        if (scope["method"], match[3], scope.get("query_string", b"")) != (
            "GET" if discovery else "POST",
            "info/refs" if discovery else service,
            f"service={service}".encode() if discovery else b"",
        ):
            raise TransportError("route_denied")
        headers = {}
        for key, value in scope.get("headers", []):
            key = key.lower()
            if key in headers:
                raise TransportError("duplicate_header")
            headers[key] = value
        if (
            headers.get(b"host", b"").decode("ascii", errors="replace")
            not in self.hosts
        ):
            raise TransportError("host_denied")
        if any(
            key in headers
            for key in (
                b"upgrade",
                b"proxy-authorization",
                b"x-http-method-override",
                b"x-original-url",
                b"x-rewrite-url",
                b"forwarded",
            )
        ) or any(
            key.startswith((b"x-forwarded-", b"x-actor", b"x-session"))
            for key in headers
        ):
            raise TransportError("routing_header")
        if headers.get(b"content-encoding", b"identity").lower() != b"identity":
            raise TransportError("http_compression")
        if b"transfer-encoding" in headers and (
            b"content-length" in headers
            or headers[b"transfer-encoding"].lower() != b"chunked"
        ):
            raise TransportError("http_framing")
        if b"content-length" in headers and not re.fullmatch(
            b"[0-9]{1,10}", headers[b"content-length"]
        ):
            raise TransportError("http_framing")
        length = (
            int(headers[b"content-length"]) if b"content-length" in headers else None
        )
        if length is not None and length >= self.limits.body_bytes:
            raise TransportError("body_limit")
        if (
            not discovery
            and headers.get(b"content-type")
            != f"application/x-{service}-request".encode()
        ):
            raise TransportError("content_type")
        protocol = headers.get(b"git-protocol")
        if protocol is not None and (
            service != "git-upload-pack"
            or protocol not in (b"version=0", b"version=1", b"version=2")
        ):
            raise TransportError("git_protocol")
        bearer = headers.get(b"authorization", b"")
        if not re.fullmatch(b"Bearer [A-Za-z0-9_-]{16,256}", bearer):
            raise TransportError("authentication_required")
        token = bearer[7:].decode()
        return (
            repo,
            service,
            discovery,
            protocol.decode() if protocol else None,
            token,
            length,
        )

    async def chunks(self, receive, length) -> AsyncIterator[bytes]:
        size = 0
        while True:
            message = await receive()
            if message["type"] != "http.request":
                raise TransportError("http_disconnected")
            chunk = message.get("body", b"")
            size += len(chunk)
            if size >= self.limits.body_bytes or length is not None and size > length:
                raise TransportError("http_body_length")
            if chunk:
                yield chunk
            if not message.get("more_body", False):
                break
        if length is not None and length != size:
            raise TransportError("http_body_length")

    def binding(self, binding: TrustedBinding, repository: str):
        if (
            binding.purpose != self.purpose
            or parse_github_repo(binding.repository).full_name.lower()
            != repository.lower()
            or self.upstream.repository.lower() != repository.lower()
        ):
            raise TransportError("binding_scope")
        if self.purpose == "git-push":
            grant, policy = binding.grant, binding.policy
            if (
                grant is None
                or policy is None
                or grant.runtime_identity != binding.association.session_id
            ):
                raise TransportError("binding_scope")
            # Existing role/scope semantics, with a synthetic creation update solely for
            # discovery. Real receive commands and trusted ancestry are checked below.
            from models.push_gate import RefUpdate

            reason = authorize(
                grant,
                policy,
                repository,
                [
                    RefUpdate(
                        ref=f"refs/heads/{grant.branch}",
                        old_oid=ZERO_OID,
                        new_oid="1" * 40,
                    )
                ],
                lambda old, new: None,
            )
            if reason:
                raise TransportError(reason)

    async def handle(self, scope, receive, request_id):
        repository, service, discovery, protocol, token, length = self.request(scope)
        binding = await self.authority.authenticate(token, self.purpose)
        self.binding(binding, repository)
        secrets = (
            *self.response_secrets,
            token.encode(),
            base64.b64encode(token.encode()),
            b"Bearer " + token.encode(),
        )
        if discovery or self.purpose == "git-read":
            body = bytearray()
            async for chunk in self.chunks(receive, length):
                body.extend(chunk)
            if discovery and body:
                raise TransportError("discovery_body")
            async with self.authority.authorize_dispatch(binding, None) as proof:
                self.binding(proof.binding, repository)
                if proof.binding.association != binding.association:
                    raise TransportError("runtime_changed")
                if discovery:
                    data = await self.upstream.discovery(
                        service, protocol=protocol, secrets=secrets
                    )
                    if self.purpose == "git-push":
                        refs = advertised_refs(data, service)
                        data = push_advertisement(
                            proof.binding.grant.branch,
                            refs.get(
                                f"refs/heads/{proof.binding.grant.branch}", ZERO_OID
                            ),
                        )
                else:
                    data = await self.upstream.upload_pack(
                        bytes(body), protocol=protocol, secrets=secrets
                    )
            return (
                data,
                f"application/x-{service}-{'advertisement' if discovery else 'result'}",
            )
        async with self.slot:
            async with receive_spool(
                self.chunks(receive, length), self.spool_root, self.limits
            ) as (work, body, digest, size):
                # Parse/scope denial precedes even trusted read/seed traffic.
                from mainloop.push_gate.protocol import receive_commands

                with body.open("rb") as source:
                    commands = receive_commands(source, self.limits)
                reason = authorize(
                    binding.grant,
                    binding.policy,
                    repository,
                    [commands.update],
                    lambda old, new: True,
                )
                if reason:
                    raise TransportError(reason)
                async with asyncio.timeout(self.limits.validation_seconds):
                    refs = advertised_refs(
                        await self.upstream.discovery(
                            "git-upload-pack", secrets=secrets
                        ),
                        "git-upload-pack",
                    )
                    prepared = await prepare_receive(
                        work,
                        body,
                        digest,
                        size,
                        refs,
                        binding.policy.default_branch,
                        self.upstream,
                        self.limits,
                        secrets=secrets,
                    )
                async with self.authority.authorize_dispatch(
                    binding, prepared
                ) as proof:
                    current = proof.binding
                    self.binding(current, repository)
                    if (
                        current.association != binding.association
                        or current.grant != binding.grant
                    ):
                        raise TransportError("runtime_changed")
                    reason = authorize(
                        current.grant,
                        current.policy,
                        repository,
                        [prepared.commands.update],
                        lambda old, new: prepared.ancestor,
                    )
                    if reason:
                        raise TransportError(reason)
                    actual = advertised_refs(
                        await self.upstream.discovery(
                            "git-receive-pack", secrets=secrets
                        ),
                        "git-receive-pack",
                    )
                    if (
                        actual.get(prepared.commands.update.ref, ZERO_OID)
                        != prepared.commands.update.old_oid
                    ):
                        raise TransportError("remote_ref_changed")
                    prepared.verify_identity()
                    attempt = PublicationAttempt(
                        request_id=request_id,
                        grant_id=current.grant.id,
                        repository=self.upstream.repository,
                        update=prepared.commands.update,
                        grant_version=current.grant.version,
                        policy_version=current.policy.version,
                    )
                    evidence = AttemptEvidence(
                        attempt,
                        prepared.body_sha256,
                        prepared.body_bytes,
                        current.association,
                        prepared.incoming.objects,
                        prepared.validated_expanded_bytes,
                        prepared.validated_objects,
                        prepared.disk_bytes,
                    )
                    await self.authority.record(evidence)
                    await self.authority.transition(
                        evidence, PublicationState.DISPATCHING
                    )
                    # Shield a finite dispatch/outcome task from request cancellation.
                    # Retain the authority context until its durable classification ends.
                    cancelled = asyncio.Event()
                    outcome = asyncio.create_task(
                        self.dispatch(evidence, prepared, secrets, cancelled)
                    )
                    try:
                        accepted = await asyncio.shield(outcome)
                    except asyncio.CancelledError:
                        cancelled.set()
                        while not outcome.done():
                            try:
                                await asyncio.shield(outcome)
                            except asyncio.CancelledError:
                                continue
                            except Exception:
                                break
                        if outcome.done() and not outcome.cancelled():
                            outcome.exception()
                        raise
                    return (
                        report(
                            prepared.commands.update.ref,
                            accepted=accepted,
                            sideband=prepared.commands.sideband,
                        ),
                        "application/x-git-receive-pack-result",
                    )

    async def dispatch(self, evidence, prepared, secrets, cancelled):
        try:
            async with asyncio.timeout(self.limits.dispatch_seconds):
                data = await self.upstream.receive_pack(prepared.body, secrets=secrets)
                accepted = receipt(
                    data,
                    prepared.commands.update.ref,
                    sideband=prepared.commands.sideband,
                )
                if cancelled.is_set():
                    raise TransportError("publication_cancelled")
                await self.authority.transition(
                    evidence,
                    (
                        PublicationState.CONFIRMED
                        if accepted
                        else PublicationState.REJECTED
                    ),
                )
                return accepted
        except BaseException:
            try:
                async with asyncio.timeout(self.limits.dispatch_seconds):
                    await self.authority.transition(evidence, PublicationState.UNKNOWN)
            except Exception:
                # Retained DISPATCHING must fence future requests even if the store is down.
                raise TransportError("publication_unknown") from None
            raise TransportError("publication_unknown") from None


def create_git_applications(
    *,
    authority: TransportAuthority | None = None,
    upstream: FixedGitUpstream | None = None,
    spool_root: Path | None = None,
    limits: Limits = DEFAULT_LIMITS,
    read_hosts: tuple[str, ...] = (),
    push_hosts: tuple[str, ...] = (),
    response_secrets: tuple[bytes, ...] = (),
) -> tuple[GitApplication, GitApplication]:
    slot = asyncio.Semaphore(1)
    common = dict(
        authority=authority,
        upstream=upstream,
        spool_root=spool_root,
        limits=limits,
        validation_slot=slot,
        response_secrets=response_secrets,
    )
    return GitApplication("git-read", hosts=read_hosts, **common), GitApplication(
        "git-push", hosts=push_hosts, **common
    )
