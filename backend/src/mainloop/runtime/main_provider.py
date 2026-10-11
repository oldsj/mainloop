"""Explicit owner main-provider selection; no prompt replay or automatic fallback.

The product binding stays fixed, including child ownership and report recipients. A
switch retires the idle native identity, rotates MCP authority and commits a new
create contract before any external create. Recovery repeats only that contract.
"""

import json
import uuid

from mainloop.config import settings
from mainloop.db import db
from mainloop.push_gate import lifecycle as push_lifecycle
from mainloop.runtime import native_sessions as ns
from mainloop.runtime.agent_credentials import (
    _binding_lock,
    credential_reference,
    reference_data,
)
from mainloop.runtime.agent_identity import hash_token, token_for
from mainloop.runtime.delegation import ensure_main_session
from mainloop.runtime.kagent_client import (
    OutcomeUnknown,
    RuntimeState,
    SessionError,
    is_terminal,
)
from mainloop.tasks import lifecycle

from models.main_provider import MainProviderInfo, MainProviderSelection


class MainProviderError(ValueError):
    def __init__(self, status: int, detail: str):
        super().__init__(detail)
        self.status = status


async def info(session_id: str, *, conn=None) -> MainProviderInfo:
    if conn is None:
        async with db.connection() as owned:
            return await info(session_id, conn=owned)
    binding = await ns.get_binding(session_id, conn=conn)
    pending = binding["main_provider_pending"]
    history = await conn.fetch(
        "SELECT provider,kagent_session_id FROM main_native_history WHERE session_id=$1 ORDER BY retired_at,id",
        session_id,
    )
    effective = (
        binding["kind"] if binding["kagent_session_id"] and not pending else None
    )
    return MainProviderInfo(
        session_id=session_id,
        desired_provider=binding["kind"],
        effective_provider=effective,
        status=(
            ("failed" if binding["main_provider_error"] else "pending")
            if pending
            else "ready" if effective else "not_started"
        ),
        kagent_session_id=binding["kagent_session_id"],
        error=binding["main_provider_error"],
        history=[dict(row) for row in history],
    )


async def _retire_idle(conn, binding: dict) -> None:
    """Check the ledger and native runtime, then suspend without deleting history.

    The caller holds publication, authority, runtime and delivery locks. kagent's
    suspend is the final idle fence; no native turn is cancelled by this operation.
    """
    sid = binding["session_id"]
    busy = await conn.fetchval(
        """SELECT EXISTS(SELECT 1 FROM native_deliveries WHERE session_id=$1
           AND state IN ('recorded','queued','sending','delivered','uncertain'))""",
        sid,
    )
    status = await conn.fetchval("SELECT status FROM sessions WHERE id=$1", sid)
    if busy or status != "idle":
        raise MainProviderError(409, "main_provider_busy")
    if not binding["kagent_session_id"]:
        if binding["git_create_dispatched"]:
            raise MainProviderError(409, "main_native_creation_unresolved")
        return
    try:
        runtime = await ns.get_client().get_session(binding["kagent_session_id"])
    except SessionError as exc:
        if exc.grpc_status == 5:
            return  # Confirmed absent; retain its stored identity in history.
        raise
    await ns.validate_bound_session(binding, runtime, conn=conn)
    if runtime.state == RuntimeState.DELETED and runtime.settled:
        return
    if not runtime.settled or runtime.state not in (
        RuntimeState.READY,
        RuntimeState.SUSPENDED,
    ):
        raise MainProviderError(409, "main_native_state_unsettled")
    tasks = await ns.get_client().list_tasks(
        await ns.binding_agent_ref(binding, conn=conn), runtime.id
    )
    if any(
        task.context_id != runtime.id or not is_terminal(task.status.state)
        for task in tasks
    ):
        raise MainProviderError(409, "main_native_turn_active")
    if runtime.state == RuntimeState.READY:
        # A lost suspend reply leaves the original binding unchanged. Repeating the
        # owner request observes/joins that same lifecycle operation before retiring it.
        await ns.get_client().suspend_session(runtime.id)
        runtime = await ns.get_client().get_session(runtime.id)
        await ns.validate_bound_session(binding, runtime, conn=conn)
        if runtime.state != RuntimeState.SUSPENDED or not runtime.settled:
            raise MainProviderError(409, "main_native_suspend_unconfirmed")


async def _prepare(conn, binding: dict) -> None:
    """Recover a committed switch without changing its native create identity."""
    sid = binding["session_id"]
    await conn.execute(
        "UPDATE native_bindings SET main_provider_error=NULL WHERE session_id=$1", sid
    )
    try:
        if binding["kagent_session_id"] is None:
            runtime = await ns._create_bound_session_guarded(binding, conn=conn)
            await ns.ledger.update_binding(sid, conn=conn, kagent_session_id=runtime.id)
            binding["kagent_session_id"] = runtime.id
        else:
            runtime = await ns.get_client().get_session(binding["kagent_session_id"])
            await ns.validate_bound_session(binding, runtime, conn=conn)
        runtime = await ns.get_client().ensure_ready(
            runtime, timeout=settings.kagent_session_ready_timeout_seconds
        )
        await ns.validate_bound_session(binding, runtime, conn=conn)
        if runtime.state != RuntimeState.READY or not runtime.settled:
            raise OutcomeUnknown("main native readiness is unconfirmed")
        await conn.execute(
            "UPDATE native_bindings SET main_provider_pending=FALSE,main_provider_error=NULL,updated_at=NOW() WHERE session_id=$1",
            sid,
        )
    except Exception as exc:
        # No prompt was sent. Leave the same contract pending, including after a
        # lost CreateSession reply or a failed local write following native creation.
        await conn.execute(
            "UPDATE native_bindings SET main_provider_error=$2,updated_at=NOW() WHERE session_id=$1",
            sid,
            ns.safe_detail(ns.describe_error(exc)),
        )


async def select(user_id: str, provider: str) -> MainProviderInfo:
    # Validate authority and selection before first-use creates any rows.
    if user_id != settings.owner_id:
        raise MainProviderError(403, "main_provider_owner_required")
    provider = MainProviderSelection(provider=provider).provider
    from mainloop.providers import registry

    profile = registry().resolve(provider, "main", selecting=True)
    if profile.native_provider != provider:
        raise MainProviderError(400, "main_provider_profile_mismatch")
    opposite = "claude" if provider == "codex" else "codex"
    if profile.agents["main"] == registry().resolve(opposite, "main").agents["main"]:
        raise MainProviderError(400, "main_provider_agent_conflict")
    binding = await ensure_main_session(user_id)
    sid = binding["session_id"]
    async with (
        db.connection() as conn,
        push_lifecycle.locked(conn, sid),
        lifecycle.locked(conn, sid),
    ):
        binding = await ns.get_binding(sid, conn=conn)
        await lifecycle.check(conn, sid, "create")
        if binding["main_provider_pending"] and binding["kind"] != provider:
            raise MainProviderError(409, "main_provider_selection_pending")
        if binding["kind"] != provider:
            current_ref = await ns.binding_agent_ref(binding, conn=conn)
            target_ref = profile.agents["main"]
            if (current_ref.namespace, current_ref.name) == (
                target_ref.namespace,
                target_ref.name,
            ):
                raise MainProviderError(400, "main_provider_agent_conflict")
            # This session-level lock also orders task-report transactions, which
            # use the matching transaction-level advisory lock before taking rows.
            key = f"native-deliveries:{sid}"
            await conn.execute("SELECT pg_advisory_lock(hashtext($1))", key)
            try:
                await _retire_idle(conn, binding)
                identity = str(uuid.uuid4())
                old_ref = await ns.binding_agent_ref(binding, conn=conn)
                snapshot = {
                    "create_request_id": ns._request_id(binding),
                    "agent": {"namespace": old_ref.namespace, "name": old_ref.name},
                    "credential_identity": binding.get("credential_identity") or sid,
                    "credential_ref": binding["credential_ref"],
                    "model": binding["model"],
                    "turns": binding["turns"],
                    "standing_hash": binding["standing_hash"],
                }
                async with _binding_lock(conn, sid), conn.transaction():
                    await conn.execute(
                        "INSERT INTO main_native_history(id,session_id,provider,kagent_session_id,snapshot) VALUES($1,$2,$3,$4,$5::jsonb)",
                        identity,
                        sid,
                        binding["kind"],
                        binding["kagent_session_id"],
                        json.dumps(snapshot),
                    )
                    await conn.execute(
                        """UPDATE native_bindings SET kind=$2,main_agent_ref=$3::jsonb,
                           kagent_session_id=NULL,kagent_request_id=$4,credential_identity=$4,
                           credential_ref=$5::jsonb,token_hash=$6,standing_hash=NULL,turns=0,model=NULL,
                           git_create_dispatched=FALSE,main_provider_pending=TRUE,main_provider_error=NULL,
                           updated_at=NOW() WHERE session_id=$1""",
                        sid,
                        provider,
                        json.dumps(profile.agents["main"].model_dump()),
                        identity,
                        json.dumps(reference_data(credential_reference(identity))),
                        hash_token(token_for(identity)),
                    )
                    await conn.execute(
                        "UPDATE sessions SET description=$2 WHERE id=$1",
                        sid,
                        f"Native {provider} main thread",
                    )
                binding = await ns.get_binding(sid, conn=conn)
            finally:
                await conn.execute("SELECT pg_advisory_unlock(hashtext($1))", key)
        if binding["main_provider_pending"]:
            await _prepare(conn, binding)
        return await info(sid, conn=conn)
