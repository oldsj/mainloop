"""Control-plane storage for native-agent credentials.

The broker stores owner credentials in Kubernetes Secrets and publishes only the value used by
the egress credential provider. Codex refresh-token exchange is deliberately not implemented:
the checked-in design records the endpoint but has no evidenced request payload or rotation
handling.
"""

from __future__ import annotations

import asyncio
import base64
import json
import re
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from typing import Mapping, Protocol

from kubernetes import client, config
from kubernetes.client.rest import ApiException
from mainloop.config import settings

_PROVIDERS = {"codex", "claude"}
_JWT_EXPIRY_MARGIN_SECONDS = 5 * 60
_MAX_CODEX_AUTH_BYTES = 256 * 1024
_MAX_CLAUDE_TOKEN_BYTES = 16 * 1024
_DNS_LABEL = re.compile(r"^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$")
_SEED_LOCKS: dict[tuple[int, str, str], asyncio.Lock] = {}


class CredentialBrokerError(RuntimeError):
    """Credential state could not be read or safely published."""


class CredentialNeedsSignin(CredentialBrokerError):
    """A provider credential is absent, expiring or rejected."""

    def __init__(self, provider: str):
        if provider not in _PROVIDERS:
            raise ValueError("unsupported credential provider")
        self.provider = provider
        super().__init__(f"{provider.title()} needs sign-in")


class CredentialSecretMissing(CredentialBrokerError):
    """The Secret must be created through GitOps before it can be seeded."""


@dataclass(frozen=True, slots=True)
class CredentialStatus:
    provider: str
    available: bool
    needs_signin: bool
    expires_at: datetime | None = None
    state: str = "needs_signin"


class CredentialSecretStore(Protocol):
    """Read and patch pre-created Secret records without exposing their contents in logs."""

    def read(self, namespace: str, name: str) -> dict[str, str] | None: ...

    def publish(self, namespace: str, name: str, values: Mapping[str, str]) -> None: ...


class KubernetesCredentialSecretStore:
    """Kubernetes Secret adapter. Secrets must be provisioned through GitOps first."""

    def __init__(self, api: client.CoreV1Api | None = None):
        self._api = api

    def _client(self) -> client.CoreV1Api:
        if self._api is None:
            try:
                config.load_incluster_config()
            except config.ConfigException:
                config.load_kube_config()
            self._api = client.CoreV1Api()
        return self._api

    def read(self, namespace: str, name: str) -> dict[str, str] | None:
        try:
            secret = self._client().read_namespaced_secret(name, namespace)
        except ApiException as exc:
            if exc.status == 404:
                return None
            raise CredentialBrokerError(
                f"credential Secret read failed (status {exc.status})"
            ) from exc
        data = secret.data or {}
        try:
            return {
                key: base64.b64decode(value, validate=True).decode("utf-8")
                for key, value in data.items()
                if isinstance(key, str) and isinstance(value, str)
            }
        except (ValueError, UnicodeDecodeError) as exc:
            raise CredentialBrokerError("credential Secret data is invalid") from exc

    def publish(self, namespace: str, name: str, values: Mapping[str, str]) -> None:
        body = {
            "data": {
                key: base64.b64encode(value.encode("utf-8")).decode("ascii")
                for key, value in values.items()
            }
        }
        try:
            self._client().patch_namespaced_secret(name, namespace, body)
        except ApiException as exc:
            raise CredentialBrokerError(
                f"credential Secret update failed (status {exc.status})"
            ) from exc


class CredentialBroker:
    """Seed protected credentials by file path and publish egress values in pre-created Secrets."""

    def __init__(
        self,
        *,
        store: CredentialSecretStore | None = None,
        namespace: str | None = None,
        secret_prefix: str | None = None,
        account: str | None = None,
        codex_auth_path: str | None = None,
        claude_token_path: str | None = None,
    ):
        self.store = store or KubernetesCredentialSecretStore()
        self.namespace = namespace or settings.substrate_credential_secret_namespace
        self.secret_prefix = (
            secret_prefix or settings.substrate_credential_secret_prefix
        )
        self.account = account or settings.substrate_credential_account
        self.codex_auth_path = (
            codex_auth_path
            if codex_auth_path is not None
            else settings.substrate_codex_auth_path
        )
        self.claude_token_path = (
            claude_token_path
            if claude_token_path is not None
            else settings.substrate_claude_token_path
        )
        if not _DNS_LABEL.fullmatch(self.secret_prefix) or not _DNS_LABEL.fullmatch(
            self.account
        ):
            raise ValueError("credential Secret prefix and account must be DNS labels")

    def secret_name(self, provider: str) -> str:
        if provider not in _PROVIDERS:
            raise ValueError("unsupported credential provider")
        name = f"{self.secret_prefix}-{self.account}-{provider}"
        if len(name) > 63 or not _DNS_LABEL.fullmatch(name):
            raise ValueError("credential Secret name must be a DNS label")
        return name

    async def _read(self, provider: str) -> dict[str, str] | None:
        try:
            return await asyncio.to_thread(
                self.store.read, self.namespace, self.secret_name(provider)
            )
        except CredentialBrokerError:
            raise
        except Exception as exc:
            raise CredentialBrokerError("credential Secret read failed") from exc

    async def _publish(self, provider: str, values: Mapping[str, str]) -> None:
        try:
            await asyncio.to_thread(
                self.store.publish,
                self.namespace,
                self.secret_name(provider),
                values,
            )
        except CredentialBrokerError:
            raise
        except Exception as exc:
            raise CredentialBrokerError("credential Secret update failed") from exc

    async def seed_configured(self, provider: str) -> CredentialStatus:
        lock_key = (
            id(asyncio.get_running_loop()),
            self.namespace,
            self.secret_name(provider),
        )
        lock = _SEED_LOCKS.setdefault(lock_key, asyncio.Lock())
        async with lock:
            return await self._seed_configured_once(provider)

    async def _seed_configured_once(self, provider: str) -> CredentialStatus:
        existing = await self._read(provider)
        if existing is None:
            raise CredentialSecretMissing("credential Secret is not pre-created")
        if existing:
            # Never replace an existing rejected, expired or valid credential from a file.
            return self._status_from_values(provider, existing)
        if provider == "codex":
            path = self.codex_auth_path
            if not path:
                raise CredentialNeedsSignin(provider)
            raw = await self._read_path(path, _MAX_CODEX_AUTH_BYTES, provider)
            values = self._codex_auth_values(raw)
        elif provider == "claude":
            path = self.claude_token_path
            if not path:
                raise CredentialNeedsSignin(provider)
            raw = await self._read_path(path, _MAX_CLAUDE_TOKEN_BYTES, provider)
            try:
                values = self._claude_token_values(raw.decode("utf-8", errors="strict"))
            except UnicodeDecodeError as exc:
                raise CredentialBrokerError("Claude token file is invalid") from exc
        else:
            raise ValueError("unsupported credential provider")

        # An empty data map is the sole uninitialized state. A second sequential call sees
        # the published values above and leaves them untouched.
        await self._publish(provider, values)
        return self._status_from_values(provider, values)

    async def _read_path(self, path: str, max_bytes: int, provider: str) -> bytes:
        try:
            return await asyncio.to_thread(_read_bounded_file, path, max_bytes)
        except CredentialBrokerError:
            raise
        except Exception as exc:
            # Keep the path and OS exception out of loggable errors.
            raise CredentialNeedsSignin(provider) from exc

    @classmethod
    def _codex_auth_values(cls, raw: bytes) -> dict[str, str]:
        try:
            auth = json.loads(raw)
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise CredentialBrokerError("Codex auth file is invalid") from exc
        return cls._codex_secret_values(auth, raw)

    @staticmethod
    def _claude_token_values(token: str) -> dict[str, str]:
        token = token.strip()
        if not token:
            raise CredentialBrokerError("Claude token file is empty")
        return {
            "oauth-token": token,
            "injection-value": token,
            "needs-signin": "false",
        }

    async def store_codex_auth_document(self, raw: bytes) -> CredentialStatus:
        if not raw or len(raw) > _MAX_CODEX_AUTH_BYTES:
            raise CredentialBrokerError("Codex auth document size is invalid")
        values = self._codex_auth_values(raw)
        status = self._status_from_values("codex", values)
        if status.needs_signin:
            raise CredentialNeedsSignin("codex")
        await self._publish("codex", values)
        return status

    async def store_claude_token(self, token: str) -> CredentialStatus:
        if (
            not isinstance(token, str)
            or len(token.encode("utf-8")) > _MAX_CLAUDE_TOKEN_BYTES
        ):
            raise CredentialBrokerError("Claude token size is invalid")
        values = self._claude_token_values(token)
        await self._publish("claude", values)
        return self._status_from_values("claude", values)

    @staticmethod
    def _codex_secret_values(auth: object, raw: bytes) -> dict[str, str]:
        if not isinstance(auth, dict) or not isinstance(auth.get("tokens"), dict):
            raise CredentialBrokerError("Codex auth file has an invalid shape")
        tokens = auth["tokens"]
        access_token = tokens.get("access_token")
        id_token = tokens.get("id_token")
        refresh_token = tokens.get("refresh_token")
        account_id = tokens.get("account_id")
        if (
            not isinstance(access_token, str)
            or not isinstance(id_token, str)
            or not isinstance(refresh_token, str)
            or not isinstance(account_id, str)
            or not account_id.strip()
            or not isinstance(auth.get("last_refresh"), (str, int, float))
        ):
            raise CredentialBrokerError("Codex auth file has an invalid shape")
        expiry = _jwt_expiry(access_token)
        needs_signin = (
            expiry <= datetime.now(UTC).timestamp() + _JWT_EXPIRY_MARGIN_SECONDS
        )
        return {
            "auth.json": raw.decode("utf-8"),
            "account-id": account_id,
            "access-token": access_token if not needs_signin else "",
            "injection-value": access_token if not needs_signin else "",
            "expires-at": str(int(expiry)),
            "needs-signin": "true" if needs_signin else "false",
        }

    @staticmethod
    def _status_from_values(
        provider: str, values: Mapping[str, str]
    ) -> CredentialStatus:
        if not values:
            return CredentialStatus(provider, False, True, state="uninitialized")
        available = bool(values.get("injection-value"))
        needs_signin = values.get("needs-signin") == "true" or not available
        expires_at = None
        state = "available" if available else "needs_signin"
        raw_expiry = values.get("expires-at")
        if raw_expiry:
            try:
                expires_at = datetime.fromtimestamp(int(raw_expiry), UTC)
            except (ValueError, OverflowError, OSError):
                needs_signin = True
                state = "rejected"
        if (
            provider == "codex"
            and expires_at is not None
            and expires_at.timestamp()
            <= datetime.now(UTC).timestamp() + _JWT_EXPIRY_MARGIN_SECONDS
        ):
            needs_signin = True
            state = "expired"
        elif values.get("needs-signin") == "true":
            state = "rejected"
        return CredentialStatus(
            provider,
            available and not needs_signin,
            needs_signin,
            expires_at,
            state,
        )

    async def status(self, provider: str) -> CredentialStatus:
        values = await self._read(provider)
        if values is None:
            return CredentialStatus(provider, False, True, state="missing")
        if not values:
            path = (
                self.codex_auth_path if provider == "codex" else self.claude_token_path
            )
            if not path:
                return CredentialStatus(provider, False, True, state="uninitialized")
            return await self.seed_configured(provider)
        status = self._status_from_values(provider, values)
        if provider == "codex" and status.needs_signin:
            await self._disable_expired_codex_injection(values)
        return status

    async def codex_placeholder_auth(self) -> str:
        values = await self._read("codex")
        if values is None:
            raise CredentialNeedsSignin("codex")
        if not values:
            await self.seed_configured("codex")
            values = await self._read("codex")
        if not values:
            raise CredentialNeedsSignin("codex")
        status = self._status_from_values("codex", values)
        if not status.available or status.needs_signin:
            if status.needs_signin:
                await self._disable_expired_codex_injection(values)
            raise CredentialNeedsSignin("codex")
        account_id = values.get("account-id", "")
        if not account_id:
            raise CredentialNeedsSignin("codex")
        expires = int(datetime.now(UTC).timestamp()) + 365 * 24 * 60 * 60
        jwt = _synthetic_jwt(expires)
        placeholder = {
            "auth_mode": "chatgpt",
            "tokens": {
                "id_token": jwt,
                "access_token": jwt,
                "refresh_token": "",
                "account_id": account_id,
            },
            "last_refresh": datetime.now(UTC).isoformat(),
        }
        return json.dumps(placeholder, separators=(",", ":"))

    async def claude_placeholder_token(self) -> str:
        status = await self.status("claude")
        if not status.available or status.needs_signin:
            raise CredentialNeedsSignin("claude")
        return "sk-ant-oat01-mainloop-egress-placeholder"

    async def _disable_expired_codex_injection(self, values: Mapping[str, str]) -> None:
        if not values.get("injection-value"):
            return
        updated = dict(values)
        updated.update(
            {"access-token": "", "injection-value": "", "needs-signin": "true"}
        )
        await self._publish("codex", updated)


def _read_bounded_file(path: str, max_bytes: int) -> bytes:
    with Path(path).open("rb") as source:
        data = source.read(max_bytes + 1)
    if not data or len(data) > max_bytes:
        raise CredentialBrokerError("credential file size is invalid")
    return data


def _jwt_expiry(token: str) -> float:
    pieces = token.split(".")
    if len(pieces) != 3:
        raise CredentialBrokerError("Codex access token is not a JWT")
    try:
        payload = pieces[1] + "=" * (-len(pieces[1]) % 4)
        claims = json.loads(base64.urlsafe_b64decode(payload).decode("utf-8"))
        expiry = claims["exp"]
    except (
        ValueError,
        UnicodeDecodeError,
        json.JSONDecodeError,
        KeyError,
        TypeError,
    ) as exc:
        raise CredentialBrokerError("Codex access token expiry is unavailable") from exc
    if isinstance(expiry, bool) or not isinstance(expiry, (int, float)):
        raise CredentialBrokerError("Codex access token expiry is invalid")
    return float(expiry)


def _synthetic_jwt(expiry: int) -> str:
    def encode(value: object) -> str:
        raw = json.dumps(value, separators=(",", ":")).encode("utf-8")
        return base64.urlsafe_b64encode(raw).decode("ascii").rstrip("=")

    return f"{encode({'alg': 'none'})}.{encode({'exp': expiry})}.synthetic"
