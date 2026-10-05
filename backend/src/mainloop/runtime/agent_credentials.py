"""Publish per-binding gateway credentials. Never log Secret bodies or API exceptions."""

import asyncio
import base64
import logging

from kubernetes import client, config
from kubernetes.client.exceptions import ApiException
from mainloop.config import settings
from mainloop.runtime.agent_identity import token_for
from mainloop.runtime.kagent_client import SessionCredential

SECRET_NAME = (
    "mainloop-agent-tokens"  # nosec B105 - Kubernetes object name, not a credential
)
logger = logging.getLogger(__name__)
MCP_ORIGIN = "http://mainloop-mcp.mainloop.svc.cluster.local"


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

    def _write(self, binding_id: str, value: str | None):
        # A merge patch touches one key only, so concurrent bindings cannot overwrite others.
        data = base64.b64encode(value.encode()).decode() if value is not None else None
        try:
            self._api().patch_namespaced_secret(
                SECRET_NAME,
                settings.kagent_namespace,
                {"data": {binding_id: data}},
                _request_timeout=(5, 15),
            )
        except ApiException:
            raise RuntimeError("could not update Mainloop agent credential") from None

    async def publish(self, binding_id: str) -> SessionCredential:
        await asyncio.to_thread(self._write, binding_id, credential_value(binding_id))
        return SessionCredential(
            origin=MCP_ORIGIN,
            header="Authorization",
            secret_name=SECRET_NAME,
            secret_key=binding_id,
        )

    async def remove(self, binding_id: str):
        await asyncio.to_thread(self._write, binding_id, None)


credentials = CredentialStore()


async def revoke(binding_id: str):
    from mainloop.db import db

    async with db.connection() as conn:
        revoked = await conn.fetchval(
            "UPDATE native_bindings SET token_hash=NULL, credential_cleanup_pending=TRUE WHERE session_id=$1 AND role IN ('main','child') RETURNING session_id",
            binding_id,
        )
    if revoked:
        await _cleanup(binding_id)


async def _cleanup(binding_id: str):
    from mainloop.db import db

    try:
        await credentials.remove(binding_id)
    except Exception as exc:
        # Auth is already revoked. Do not lose a report or cancellation to a Kubernetes outage.
        logger.warning(
            "reconcile step failed: step=credential_cleanup session_id=%s error_class=%s",
            binding_id,
            type(exc).__name__,
            extra={"event": "reconcile_step_failed", "step": "credential_cleanup",
                   "session_id": binding_id, "error_class": type(exc).__name__},
        )
        return
    async with db.connection() as conn:
        await conn.execute(
            "UPDATE native_bindings SET credential_cleanup_pending=FALSE WHERE session_id=$1",
            binding_id,
        )


async def reconcile_cleanup():
    from mainloop.db import db

    async with db.connection() as conn:
        rows = await conn.fetch(
            "SELECT session_id FROM native_bindings WHERE credential_cleanup_pending=TRUE"
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
                extra={"event": "reconcile_step_failed", "step": "credential_cleanup",
                       "session_id": row["session_id"], "error_class": type(exc).__name__},
            )
