"""Anonymous, bounded OCI metadata reads. Image layers are never fetched."""

import asyncio
import hashlib
import json
import re
from contextlib import asynccontextmanager
from typing import Protocol
from urllib.parse import urlsplit

import httpx
from pydantic import BaseModel, ConfigDict, Field

from models.environment import Architecture, Digest, EnvironmentVersion

VALIDATOR_VERSION = "oci-static-v1"
MAX_METADATA_BYTES = 4 * 1024 * 1024
REGISTRY_READ_BUDGET_SECONDS = 30.0
VALIDATION_BUDGET_SECONDS = 90.0
REFRESH_BUDGET_SECONDS = 90.0
REFERENCE = re.compile(r"^([a-z0-9.-]+)/([a-z0-9._/-]+)@(sha256:[0-9a-f]{64})$")
ACCEPT = ", ".join(
    (
        "application/vnd.oci.image.index.v1+json",
        "application/vnd.oci.image.manifest.v1+json",
        "application/vnd.docker.distribution.manifest.list.v2+json",
        "application/vnd.docker.distribution.manifest.v2+json",
    )
)


class RegistryError(ValueError):
    """An unsupported, inaccessible or invalid registry object."""


@asynccontextmanager
async def elapsed_budget(seconds, operation):
    try:
        async with asyncio.timeout(seconds):
            yield
    except TimeoutError as exc:
        raise RegistryError(
            f"Registry {operation} elapsed-time budget exceeded"
        ) from exc


class Descriptor(BaseModel):
    digest: Digest
    size: int = Field(ge=0, le=MAX_METADATA_BYTES)
    platform: dict[str, str] = Field(default_factory=dict)


class Manifest(BaseModel):
    schemaVersion: int
    config: Descriptor | None = None
    manifests: list[Descriptor] | None = None


class ImageConfig(BaseModel):
    model_config = ConfigDict(extra="ignore")
    architecture: Architecture
    os: str
    config: dict = Field(default_factory=dict)


class AnonymousToken(BaseModel):
    token: str | None = None
    access_token: str | None = None


class RegistryClient(Protocol):
    async def read(
        self, registry: str, repository: str, kind: str, reference: str
    ) -> bytes: ...


class AnonymousOCIRegistry:
    """Anonymous bearer exchange stays on the allowlisted registry's HTTPS origin.

    Cross-host auth/metadata redirects are unsupported; no credentials are loaded.
    """

    def __init__(self, transport=None):
        self.transport = transport

    async def read(self, registry, repository, kind, reference):
        async with elapsed_budget(REGISTRY_READ_BUDGET_SECONDS, "read"):
            return await self._read(registry, repository, kind, reference)

    async def _read(self, registry, repository, kind, reference):
        url = f"https://{registry}/v2/{repository}/{kind}/{reference}"
        async with httpx.AsyncClient(
            transport=self.transport,
            timeout=20,
            follow_redirects=False,
            trust_env=False,
        ) as client:

            async def fetch(target, headers=None, params=None):
                async with client.stream(
                    "GET",
                    target,
                    headers={"Accept-Encoding": "identity", **(headers or {})},
                    params=params,
                ) as response:
                    encoding = (
                        response.headers.get("content-encoding", "identity")
                        .strip()
                        .lower()
                    )
                    if encoding != "identity":
                        raise RegistryError(
                            "Registry metadata requires identity Content-Encoding"
                        )
                    # Mock transports may supply already-buffered identity responses.
                    if response.is_stream_consumed:
                        data = response.content
                        if len(data) > MAX_METADATA_BYTES:
                            raise RegistryError("Registry metadata exceeds size limit")
                        return response.status_code, response.headers, data
                    data = bytearray()
                    async for chunk in response.aiter_raw():
                        if len(chunk) > MAX_METADATA_BYTES - len(data):
                            raise RegistryError("Registry metadata exceeds size limit")
                        data.extend(chunk)
                    return response.status_code, response.headers, bytes(data)

            headers = {"Accept": ACCEPT}
            try:
                status, response_headers, data = await fetch(url, headers)
                if status == 401:
                    challenge = response_headers.get("www-authenticate", "")
                    if not challenge.lower().startswith("bearer "):
                        raise RegistryError("private images not supported yet")
                    fields = dict(re.findall(r'(\w+)="([^"\r\n]*)"', challenge))
                    realm = fields.get("realm", "")
                    parsed = urlsplit(realm)
                    if (
                        parsed.scheme != "https"
                        or parsed.netloc != registry
                        or parsed.username
                        or parsed.fragment
                    ):
                        raise RegistryError(
                            "private images not supported yet: unsupported authentication origin"
                        )
                    token_status, _, token_data = await fetch(
                        realm,
                        params={
                            "service": fields.get("service", registry),
                            "scope": f"repository:{repository}:pull",
                        },
                    )
                    if token_status != 200:
                        raise RegistryError("private images not supported yet")
                    response_token = AnonymousToken.model_validate_json(token_data)
                    token = response_token.token or response_token.access_token
                    if not isinstance(token, str) or not token:
                        raise RegistryError("private images not supported yet")
                    status, _, data = await fetch(
                        url, {**headers, "Authorization": f"Bearer {token}"}
                    )
                if status in (401, 403, 404):
                    raise RegistryError(
                        "private images not supported yet (or image not found)"
                    )
                if status != 200:
                    raise RegistryError(
                        f"Registry metadata request failed: HTTP {status}"
                    )
                return data
            except (httpx.HTTPError, ValueError) as exc:
                if isinstance(exc, RegistryError):
                    raise
                raise RegistryError(
                    "Registry metadata unavailable or malformed"
                ) from exc


class FakeRegistry:
    """In-memory OCI bytes for tests; missing fixtures never fall through to network."""

    def __init__(self, objects):
        self.objects = objects
        self.calls = []

    async def read(self, registry, repository, kind, reference):
        key = (registry, repository, kind, reference)
        self.calls.append(key)
        if key not in self.objects:
            raise RegistryError("private images not supported yet")
        return self.objects[key]


def parse_reference(reference, allowlist):
    match = REFERENCE.fullmatch(reference)
    if match is None or any(part in ("", ".", "..") for part in match[2].split("/")):
        raise RegistryError(
            "Register a registry/repository@sha256 digest reference, not a mutable tag"
        )
    registry, repository, digest = match.groups()
    if registry not in allowlist:
        raise RegistryError(f"Registry {registry} is not allowlisted")
    return registry, repository, digest


def checked_json(data, digest=None):
    if len(data) > MAX_METADATA_BYTES:
        raise RegistryError("Registry metadata exceeds size limit")
    if digest and "sha256:" + hashlib.sha256(data).hexdigest() != digest:
        raise RegistryError("Registry content digest mismatch")
    try:
        return json.loads(data)
    except (ValueError, UnicodeError) as exc:
        raise RegistryError("Invalid registry JSON") from exc


async def validate(
    client, reference, architecture, allowlist, environment_id, version_id
):
    async with elapsed_budget(VALIDATION_BUDGET_SECONDS, "validation"):
        return await _validate(
            client, reference, architecture, allowlist, environment_id, version_id
        )


async def _validate(
    client, reference, architecture, allowlist, environment_id, version_id
):
    registry, repository, digest = parse_reference(reference, allowlist)
    try:
        raw = await client.read(registry, repository, "manifests", digest)
        manifest = Manifest.model_validate(checked_json(raw, digest))
        if manifest.schemaVersion != 2:
            raise RegistryError("Unsupported image manifest schema")
        index_digest = None
        platform_digest = digest
        if manifest.manifests is not None:
            index_digest = digest
            matches = [
                d
                for d in manifest.manifests
                if d.platform.get("os") == "linux"
                and d.platform.get("architecture") == architecture
            ]
            if len(matches) != 1:
                raise RegistryError(
                    f"Expected one linux/{architecture} platform manifest"
                )
            platform_digest = matches[0].digest
            raw = await client.read(registry, repository, "manifests", platform_digest)
            if len(raw) != matches[0].size:
                raise RegistryError("Platform manifest size mismatch")
            manifest = Manifest.model_validate(checked_json(raw, platform_digest))
        if (
            manifest.schemaVersion != 2
            or manifest.config is None
            or manifest.manifests is not None
        ):
            raise RegistryError("Expected an image platform manifest with config")
        config_raw = await client.read(
            registry, repository, "blobs", manifest.config.digest
        )
        if len(config_raw) != manifest.config.size:
            raise RegistryError("Config size mismatch")
        config = ImageConfig.model_validate(
            checked_json(config_raw, manifest.config.digest)
        )
        user = config.config.get("User")
        if user != "65532:65532":
            raise RegistryError(
                f"Declared USER must be exactly 65532:65532; got {user!r}"
            )
        if config.os != "linux" or config.architecture != architecture:
            raise RegistryError(
                "Config platform does not match selected linux architecture"
            )
        return EnvironmentVersion(
            id=version_id,
            environment_id=environment_id,
            registry=registry,
            repository=repository,
            index_digest=index_digest,
            platform_manifest_digest=platform_digest,
            config_digest=manifest.config.digest,
            declared_user=user,
            architecture=architecture,
            validation_status="static_validated",
            validation_result={
                "user": "65532:65532",
                "platform": f"linux/{architecture}",
                "probes": "not_run",
            },
            validator_version=VALIDATOR_VERSION,
            provenance_kind="user_pushed",
        )
    except ValueError as exc:
        if isinstance(exc, RegistryError):
            raise
        raise RegistryError(f"Invalid OCI metadata: {exc}") from exc


async def refresh(client, env, previous, allowlist, version_id):
    async with elapsed_budget(REFRESH_BUDGET_SECONDS, "refresh"):
        return await _refresh(client, env, previous, allowlist, version_id)


async def _refresh(client, env, previous, allowlist, version_id):
    if not env.watched_tag or not previous.registry or not previous.repository:
        raise RegistryError("Environment has no watched image tag")
    # Validate the registry before even resolving mutable metadata.
    parse_reference(
        f"{previous.registry}/{previous.repository}@{previous.platform_manifest_digest}",
        allowlist,
    )
    raw = await client.read(
        previous.registry, previous.repository, "manifests", env.watched_tag
    )
    digest = "sha256:" + hashlib.sha256(raw).hexdigest()
    return await validate(
        client,
        f"{previous.registry}/{previous.repository}@{digest}",
        previous.architecture,
        allowlist,
        env.id,
        version_id,
    )
