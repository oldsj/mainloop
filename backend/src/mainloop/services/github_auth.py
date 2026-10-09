"""Repository-scoped GitHub App authentication and bounded fixed-origin HTTP.

Credentials and installation tokens stay in this process. No auth or write retries:
uncertain product writes still belong to their existing durable intent ledgers.
"""

from __future__ import annotations

import asyncio
import base64
import binascii
import re
import time
from dataclasses import dataclass, field
from datetime import datetime
from weakref import WeakKeyDictionary

import httpx
import jwt
from cryptography.exceptions import UnsupportedAlgorithm
from cryptography.hazmat.primitives.asymmetric.rsa import RSAPrivateKey
from cryptography.hazmat.primitives.serialization import load_pem_private_key
from mainloop.config import settings
from mainloop.runtime.policy import PolicyError
from mainloop.services.github_repo import InvalidGithubRepo, parse_github_repo
from pydantic import BaseModel, ConfigDict, Field, SecretStr, ValidationError

REQUEST_TIMEOUT_SECONDS = 15
TOKEN_MARGIN_SECONDS = 60
API_HEADERS = {
    "Accept": "application/vnd.github+json",
    "X-GitHub-Api-Version": "2026-03-10",
}


class GitHubError(Exception):
    """Opaque upstream failure, without response/request/credential text."""


class GitHubNotFound(GitHubError):
    """An endpoint-specific 404; installation absence is a PolicyError instead."""


def repository_name(value: str) -> str:
    try:
        return parse_github_repo(value).full_name.lower()
    except (InvalidGithubRepo, ValueError, TypeError):
        raise PolicyError("ownership", "invalid GitHub repository") from None


def endpoint(repository: str, method: str, path: str) -> dict[str, str]:
    """Refuse other origins/repos and select the endpoint's minimum permissions."""
    if not isinstance(path, str) or not path.startswith("/repos/"):
        raise PolicyError("ownership", "GitHub client repository scope mismatch")
    url = httpx.URL("https://api.github.com" + path)
    prefix = f"/repos/{repository}"
    if (
        url.query
        or url.fragment
        or "\\" in url.path
        or any(part in (".", "..") for part in url.path.split("/"))
        or not (url.path.lower() == prefix or url.path.lower().startswith(prefix + "/"))
    ):
        raise PolicyError("ownership", "GitHub client repository scope mismatch")
    suffix = url.path[len(prefix) :]
    if re.fullmatch(r"/issues/comments/[^/]+/reactions", suffix):
        raise PolicyError(
            "github",
            "GitHub App issue-comment reactions require Issues permissions, which are not granted",
        )
    if method == "GET":
        if not suffix:
            return {"metadata": "read"}
        if suffix.startswith("/pulls"):
            return {"pull_requests": "read"}
        if suffix.startswith("/branches/") and suffix.endswith("/protection"):
            return {"administration": "read"}
        if suffix.startswith("/rules/branches/"):
            return {"metadata": "read"}
        if suffix.startswith("/branches/"):
            return {"contents": "read"}
        if suffix.startswith("/commits"):
            if suffix.endswith(("/check-runs", "/check-suites")):
                return {"checks": "read"}
            if suffix.endswith("/statuses"):
                return {"statuses": "read"}
            return {"contents": "read"}
        if suffix.startswith("/issues/comments/"):
            return {"pull_requests": "read"}
        if suffix.startswith("/issues/") and suffix.endswith("/comments"):
            return {"pull_requests": "read"}
        if re.fullmatch(r"/issues/[0-9]+", suffix):
            return {"issues": "read"}
    elif method == "POST":
        if suffix == "/pulls" or suffix.startswith("/pulls/"):
            return {"pull_requests": "write"}
        if suffix.startswith("/issues/"):
            return {"pull_requests": "write"}
        if suffix == "/issues":
            return {"issues": "write"}
    elif method == "PATCH" and re.fullmatch(r"/issues/[0-9]+", suffix):
        return {"pull_requests": "write"}
    elif method == "PUT" and re.fullmatch(r"/pulls/[0-9]+/merge", suffix):
        return {"contents": "write"}
    raise PolicyError("github", "unsupported GitHub endpoint")


def http_client(transport=None) -> httpx.AsyncClient:
    return httpx.AsyncClient(
        base_url="https://api.github.com",
        headers=API_HEADERS,
        timeout=httpx.Timeout(10.0),
        follow_redirects=False,
        trust_env=False,
        transport=transport,
    )


async def bounded_response(client, method, path, *, accepted=(200, 201), **kwargs):
    try:
        async with asyncio.timeout(REQUEST_TIMEOUT_SECONDS), client.stream(
            method, path, **kwargs
        ) as response:
            if response.status_code == 404:
                raise GitHubNotFound
            if response.status_code not in accepted:
                raise GitHubError
            data = bytearray()
            async for chunk in response.aiter_bytes(chunk_size=65536):
                data.extend(chunk)
                if len(data) > 2_000_000:
                    raise GitHubError
            # aiter_bytes() decoded the wire body. Drop headers describing its
            # encoding/framing before constructing a new response; HTTPX supplies
            # the decoded content length. Don't retain credential-bearing requests.
            headers = {
                name: value
                for name, value in response.headers.items()
                if name.lower()
                not in ("content-encoding", "content-length", "transfer-encoding")
            }
            return httpx.Response(
                response.status_code, headers=headers, content=bytes(data)
            )
    except (httpx.HTTPError, ValueError, TimeoutError):
        raise GitHubError from None


class Installation(BaseModel):
    model_config = ConfigDict(strict=True)
    id: int = Field(gt=0)


class InstallationToken(BaseModel):
    token: SecretStr = Field(repr=False, min_length=1)
    expires_at: datetime


@dataclass(repr=False)
class CachedToken:
    token: str
    expires_at: float


@dataclass
class LoopCache:
    locks: dict[str, asyncio.Lock] = field(default_factory=dict)
    installations: dict[str, int] = field(default_factory=dict)
    tokens: dict[tuple, CachedToken] = field(default_factory=dict, repr=False)


class GitHubAppAuth:
    def __init__(self, app_id: str, private_key: SecretStr):
        if not re.fullmatch(r"[1-9][0-9]*", app_id):
            raise PolicyError(
                "configuration", "GITHUB_APP_ID must be a positive numeric App ID"
            )
        try:
            encoded = private_key.get_secret_value()
            pem = base64.b64decode(encoded, validate=True)
            key = load_pem_private_key(pem, password=None)
            if not isinstance(key, RSAPrivateKey) or key.key_size < 2048:
                raise ValueError
        except (
            ValueError,
            TypeError,
            binascii.Error,
            AttributeError,
            UnsupportedAlgorithm,
        ):
            raise PolicyError(
                "configuration",
                "GITHUB_APP_PRIVATE_KEY must be single-line base64 of an unencrypted RSA PEM (at least 2048 bits)",
            ) from None
        self.app_id = app_id
        self._key = key
        self._loops = WeakKeyDictionary()

    def _jwt(self) -> str:
        now = int(time.time())
        try:
            return jwt.encode(
                {"iss": self.app_id, "iat": now - 60, "exp": now + 540},
                self._key,
                algorithm="RS256",
            )
        except (jwt.PyJWTError, ValueError, UnsupportedAlgorithm):
            raise GitHubError from None

    async def token(self, client, repository: str, permissions: dict[str, str]) -> str:
        repository = repository_name(repository)
        loop = asyncio.get_running_loop()
        cache = self._loops.setdefault(loop, LoopCache())
        # Serialize discovery and all permission sets for this repo; different repos
        # remain independent. Check freshness again after acquiring the lock.
        async with cache.locks.setdefault(repository, asyncio.Lock()):
            permission_key = tuple(sorted(permissions.items()))
            installation = cache.installations.get(repository)
            key = (installation, repository, permission_key)
            cached = cache.tokens.get(key)
            if cached and time.time() < cached.expires_at - TOKEN_MARGIN_SECONDS:
                return cached.token
            headers = {"Authorization": f"Bearer {self._jwt()}"}
            try:
                response = await bounded_response(
                    client,
                    "GET",
                    f"/repos/{repository}/installation",
                    headers=headers,
                    accepted=(200,),
                )
                installation = Installation.model_validate(response.json()).id
            except GitHubNotFound:
                cache.installations.pop(repository, None)
                cache.tokens = {
                    key: value
                    for key, value in cache.tokens.items()
                    if key[1] != repository
                }
                raise PolicyError(
                    "github", f"GitHub App not installed on {repository}"
                ) from None
            except (ValidationError, ValueError):
                raise GitHubError from None
            cache.installations[repository] = installation
            key = (installation, repository, permission_key)
            try:
                response = await bounded_response(
                    client,
                    "POST",
                    f"/app/installations/{installation}/access_tokens",
                    headers=headers,
                    accepted=(201,),
                    json={
                        "repositories": [repository.split("/")[1]],
                        "permissions": permissions,
                    },
                )
                value = InstallationToken.model_validate(response.json())
                if (
                    value.expires_at.tzinfo is None
                    or value.expires_at.timestamp()
                    <= time.time() + TOKEN_MARGIN_SECONDS
                ):
                    raise ValueError
            except (GitHubError, ValidationError, ValueError):
                # Auth endpoint absence is never product endpoint absence. In
                # particular it cannot attest that classic protection is absent.
                raise GitHubError from None
            token = value.token.get_secret_value()
            if not token or any(c.isspace() for c in token):
                raise GitHubError
            cache.tokens[key] = CachedToken(token, value.expires_at.timestamp())
            return token


_auth: GitHubAppAuth | None = None
_auth_credentials: tuple[str, SecretStr] | None = None


def app_auth() -> GitHubAppAuth:
    global _auth, _auth_credentials
    credentials = (settings.github_app_id, settings.github_app_private_key)
    if _auth is None or credentials != _auth_credentials:
        _auth = GitHubAppAuth(*credentials)
        _auth_credentials = credentials
    return _auth
