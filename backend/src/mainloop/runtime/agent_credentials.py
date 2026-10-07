"""Publish per-binding gateway credentials. Never log Secret bodies or API exceptions."""

import asyncio
import base64
import json
import logging
from contextlib import asynccontextmanager
from dataclasses import asdict
from typing import AsyncIterator

from kubernetes import client, config
from kubernetes.client.exceptions import ApiException
from mainloop.config import settings
from mainloop.runtime.agent_identity import hash_token, token_for
from mainloop.runtime.kagent_client import SessionCredential

SECRET_KEY = "authorization"  # nosec B105 - Kubernetes data key, not a credential
SECRET_LABELS = {"mainloop.dev/actor-egress": "true", "mainloop.dev/purpose": "mcp"}
logger = logging.getLogger(__name__)
MCP_ORIGIN = "http://mainloop-mcp.mainloop.svc.cluster.local"
# Pairs only a task attempt can own. Coordination children also exist outside tasks (session
# delegation), so only a ``child``/``workspace`` or ``supervisor`` binding is delegated by its pair.
DELEGATED_PAIRS = frozenset({("supervisor", "workspace"), ("child", "workspace")})
VALID_GRANT_PAIRS = (
    frozenset(
        {
            ("main", "coordination"),
            ("child", "coordination"),
            ("agent", "workspace"),
            ("supervisor", "coordination"),
        }
    )
    | DELEGATED_PAIRS
)


def credential_value(binding_id: str) -> str:
    return "Bearer " + token_for(binding_id)


class CredentialStore:
    def __init__(self, api=None):
        self.api = api

    def _api(self):
        if self.api is None:
            config.load_incluster_config()
            self.api = client.CoreV1Api()
        return self.api

    def _reference(self, binding_id, reference):
        expected = credential_reference(binding_id)
        if reference is not None and reference != expected:
            raise RuntimeError("agent credential reference does not match binding")
        return expected

    def _publish(self, binding_id, reference):
        body = {
            "metadata": {"name": reference.secret_name, "labels": SECRET_LABELS},
            "type": "Opaque",
            "data": {
                SECRET_KEY: base64.b64encode(
                    credential_value(binding_id).encode()
                ).decode()
            },
        }
        api = self._api()
        try:
            api.create_namespaced_secret(
                settings.kagent_namespace, body, _request_timeout=(5, 15)
            )
        except ApiException as exc:
            if exc.status != 409:
                raise RuntimeError(
                    "could not publish Mainloop agent credential"
                ) from None
            # A retry after an uncertain create must observe the existing object.
            # Never overwrite a conflicting Secret or enroll an unrelated object.
            try:
                existing = api.read_namespaced_secret(
                    reference.secret_name,
                    settings.kagent_namespace,
                    _request_timeout=(5, 15),
                )
            except ApiException:
                raise RuntimeError(
                    "could not reconcile Mainloop agent credential"
                ) from None
            if (
                existing.data != body["data"]
                or existing.type != "Opaque"
                or existing.metadata.labels != SECRET_LABELS
            ):
                raise RuntimeError(
                    "Mainloop agent credential Secret conflicts with binding"
                ) from None

    def _remove(self, reference):
        try:
            self._api().delete_namespaced_secret(
                reference.secret_name,
                settings.kagent_namespace,
                _request_timeout=(5, 15),
            )
        except ApiException as exc:
            if exc.status != 404:
                raise RuntimeError(
                    "could not delete Mainloop agent credential"
                ) from None

    async def publish(
        self, binding_id: str, reference: SessionCredential | None = None
    ) -> SessionCredential:
        reference = self._reference(binding_id, reference)
        await asyncio.to_thread(self._publish, binding_id, reference)
        return reference

    async def remove(self, binding_id: str, reference: SessionCredential | None = None):
        reference = self._reference(binding_id, reference)
        await asyncio.to_thread(self._remove, reference)


credentials = CredentialStore()


def credential_reference(binding_id: str) -> SessionCredential:
    """Return the non-secret CreateSession reference for one binding."""
    return SessionCredential(
        origin=MCP_ORIGIN,
        header="Authorization",
        secret_name=f"mainloop-mcp-{binding_id}",
        secret_key=SECRET_KEY,
    )


def reference_data(reference: SessionCredential) -> dict[str, str]:
    return asdict(reference)


def reference_from_data(value) -> SessionCredential | None:
    """Decode a stored credential reference without accepting arbitrary structures."""
    if isinstance(value, str):
        try:
            value = json.loads(value)
        except ValueError:
            return None
    if not isinstance(value, dict):
        return None
    try:
        reference = SessionCredential(
            origin=value["origin"],
            header=value["header"],
            secret_name=value["secret_name"],
            secret_key=value["secret_key"],
        )
    except (KeyError, TypeError):
        return None
    if not all(
        isinstance(part, str) and part
        for part in (
            reference.origin,
            reference.header,
            reference.secret_name,
            reference.secret_key,
        )
    ):
        return None
    return reference


@asynccontextmanager
async def _binding_lock(conn, binding_id: str) -> AsyncIterator[None]:
    """Serialize publication and revocation across all Mainloop processes."""
    key = f"agent-credential:{binding_id}"
    await conn.fetchval("SELECT pg_advisory_lock(hashtextextended($1, 0))", key)
    try:
        yield
    finally:
        await conn.fetchval("SELECT pg_advisory_unlock(hashtextextended($1, 0))", key)


async def publish_for_binding(binding: dict) -> SessionCredential:
    """Publish only an enrolled, still-authorized binding under the shared DB lock."""
    from mainloop.db import db

    binding_id = binding["session_id"]
    async with db.connection() as conn:
        async with _binding_lock(conn, binding_id):
            row = await conn.fetchrow(
                """SELECT b.role,b.mcp_grant_kind,b.token_hash,b.credential_ref,
                          s.status,s.archived_at
                   FROM native_bindings b JOIN sessions s ON s.id=b.session_id
                   WHERE b.session_id=$1""",
                binding_id,
            )
            if not row:
                raise RuntimeError("agent credential binding is unavailable")
            grant = row["mcp_grant_kind"]
            valid_pair = (row["role"], grant) in VALID_GRANT_PAIRS
            attempt = await conn.fetchrow(
                """SELECT a.state,a.writer_generation,t.mode,a.id,t.current_attempt_id,
                          c.held AS claim_held,c.generation AS claim_generation
                   FROM task_attempts a JOIN tasks t ON t.id=a.task_id
                   LEFT JOIN workspace_writer_claims c ON c.attempt_id=a.id
                   WHERE a.binding_id=$1""",
                binding_id,
            )
            if (row["role"], grant) in DELEGATED_PAIRS or row["role"] == "supervisor":
                # These pairs exist only for a task attempt.
                valid_pair = valid_pair and attempt is not None
            if valid_pair and attempt is not None:
                # A delegated bearer exists only for a live attempt that still holds its branch
                # claim at the generation it was admitted with. Coordination tasks have no claim.
                valid_pair = (
                    attempt["id"] == attempt["current_attempt_id"]
                    and attempt["state"] in ("creating", "active")
                    and (
                        attempt["mode"] == "coordination"
                        or (
                            attempt["claim_held"]
                            and attempt["claim_generation"]
                            == attempt["writer_generation"]
                        )
                    )
                )
            if (
                not valid_pair
                or not row["token_hash"]
                or row["archived_at"] is not None
                or row["status"] in {"completed", "failed", "cancelled"}
                or row["token_hash"] != hash_token(token_for(binding_id))
            ):
                raise RuntimeError("agent credential binding is revoked or unavailable")
            reference = reference_from_data(row["credential_ref"])
            if reference is None:
                raise RuntimeError("agent credential reference is unavailable")
            return await credentials.publish(binding_id, reference)


async def revoke(binding_id: str):
    from mainloop.db import db
    from mainloop.push_gate import lifecycle as push_lifecycle

    revoked = False
    async with db.connection() as conn:
        async with push_lifecycle.locked(conn, binding_id, revoke=True), _binding_lock(
            conn, binding_id
        ):
            async with conn.transaction():
                row = await conn.fetchrow(
                    """SELECT mcp_grant_kind,credential_ref,token_hash,
                              credential_cleanup_pending
                       FROM native_bindings WHERE session_id=$1 FOR UPDATE""",
                    binding_id,
                )
                if not row or row["mcp_grant_kind"] not in (
                    "coordination",
                    "workspace",
                ):
                    return
                if not row["token_hash"] and not row["credential_cleanup_pending"]:
                    return
                reference_value = row["credential_ref"]
                if reference_value is None:
                    reference_value = reference_data(credential_reference(binding_id))
                elif not isinstance(reference_value, str):
                    reference_value = json.dumps(reference_value)
                await conn.execute(
                    """UPDATE native_bindings
                       SET token_hash=NULL, credential_cleanup_pending=TRUE
                       WHERE session_id=$1""",
                    binding_id,
                )
                await conn.execute(
                    """INSERT INTO agent_credential_cleanup(session_id,credential_ref)
                       VALUES($1,$2::jsonb)
                       ON CONFLICT(session_id) DO UPDATE
                         SET credential_ref=EXCLUDED.credential_ref""",
                    binding_id,
                    reference_value,
                )
                revoked = True
    if revoked:
        await _cleanup(binding_id)


async def _cleanup(binding_id: str):
    from mainloop.db import db

    async with db.connection() as conn:
        async with _binding_lock(conn, binding_id):
            row = await conn.fetchrow(
                "SELECT credential_ref FROM agent_credential_cleanup WHERE session_id=$1",
                binding_id,
            )
            if row is None:
                return
            reference = reference_from_data(row["credential_ref"])
            if reference is None:
                raise RuntimeError("agent credential cleanup reference is unavailable")
            try:
                await credentials.remove(binding_id, reference)
            except Exception as exc:
                # Auth is already revoked. Do not lose cancellation to a Kubernetes outage.
                logger.warning(
                    "reconcile step failed: step=credential_cleanup session_id=%s error_class=%s",
                    binding_id,
                    type(exc).__name__,
                    extra={
                        "event": "reconcile_step_failed",
                        "step": "credential_cleanup",
                        "session_id": binding_id,
                        "error_class": type(exc).__name__,
                    },
                )
                return
            async with conn.transaction():
                await conn.execute(
                    "DELETE FROM agent_credential_cleanup WHERE session_id=$1",
                    binding_id,
                )
                await conn.execute(
                    "UPDATE native_bindings SET credential_cleanup_pending=FALSE WHERE session_id=$1",
                    binding_id,
                )


async def reconcile_cleanup():
    from mainloop.db import db

    async with db.connection() as conn:
        rows = await conn.fetch(
            "SELECT session_id FROM agent_credential_cleanup ORDER BY created_at"
        )
    for row in rows:
        try:
            await _cleanup(row["session_id"])
        except Exception as exc:
            logger.error(
                "reconcile step failed: step=credential_cleanup session_id=%s error_class=%s",
                row["session_id"],
                type(exc).__name__,
                exc_info=exc,
                extra={
                    "event": "reconcile_step_failed",
                    "step": "credential_cleanup",
                    "session_id": row["session_id"],
                    "error_class": type(exc).__name__,
                },
            )
