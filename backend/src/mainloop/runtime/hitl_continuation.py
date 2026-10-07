"""Owner decisions use a task-bound lane independent of ordinary queued turns."""

import asyncio
import json
import uuid

from mainloop.db import db
from mainloop.db import hitl as store
from mainloop.runtime.hitl_correlation import (
    build_decision_receipt,
    task_identity,
    verified_route,
)
from mainloop.runtime.hitl_observer import Unavailable, agent_for, observer
from mainloop.runtime.kagent_client import KagentError, SendNotAccepted, Unreachable

from models.hitl import (
    HITL_EXTENSION,
    DecisionReceipt,
    HITLProjection,
    VerifiedAssociation,
    normalized_hash,
)


async def load_projection(conn, owner, request_id):
    raw = await conn.fetchval(
        "SELECT snapshot FROM native_hitl_requests WHERE owner_id=$1 AND id=$2",
        owner,
        request_id,
    )
    if not raw:
        raise LookupError("HITL request not found")
    return HITLProjection.model_validate(store._decode(raw))


async def routes(conn, projection):
    keys = [leaf.key() for leaf in projection.leaves]
    rows = await conn.fetch(
        """SELECT DISTINCT r.id,r.snapshot FROM native_hitl_requests r
        JOIN native_hitl_aliases a ON a.request_id=r.id WHERE r.owner_id=$1 AND a.leaf_key=ANY($2::text[])""",
        projection.owner_id,
        keys,
    )
    candidates = [
        HITLProjection.model_validate(store._decode(r["snapshot"])) for r in rows
    ]
    # Only trusted records may hold a direct route pending parent discovery.
    links = [
        VerifiedAssociation.model_validate(store._decode(row["snapshot"]))
        for row in await conn.fetch(
            "SELECT snapshot FROM native_hitl_associations WHERE owner_id=$1",
            projection.owner_id,
        )
    ]
    relevant = {task_identity(leaf).model_dump_json() for leaf in projection.leaves}
    for _ in range(len(links) + 1):
        before = len(relevant)
        for link in links:
            if link.leaf.model_dump_json() in relevant:
                relevant.add(link.outer.model_dump_json())
        if len(relevant) == before:
            break
    observed = {
        task_identity(candidate.outer).model_dump_json() for candidate in candidates
    }
    if not relevant.issubset(observed):
        raise store.HITLConflict("Verified parent continuation awaits reconciliation")
    # An unavailable verified parent remains a hold, never a fallback to a child.
    if any({leaf.key() for leaf in p.leaves} != set(keys) for p in candidates):
        raise store.HITLConflict("Overlapping batch aliases require reconciliation")
    outermost = [
        p
        for p in candidates
        if all(
            verified_route(p.owner_id, p.outer, other.outer, p.associations)
            for other in candidates
        )
    ]
    if len(outermost) != 1:
        raise store.HITLConflict("Ambiguous verified continuation route")
    return outermost[0], candidates


async def submit(owner, request_id, action_id, response, *, service=None):
    service = service or observer()
    if owner != service.owner:
        raise LookupError("HITL request not found")
    async with db.connection() as conn:
        projection = await load_projection(conn, owner, request_id)
        existing = await conn.fetchval(
            "SELECT snapshot FROM native_hitl_responses WHERE owner_id=$1 AND action_id=$2",
            owner,
            action_id,
        )
        if existing:
            receipt = DecisionReceipt.model_validate_json(
                json.dumps(store._decode(existing))
            )
            if receipt.response != response or {
                c.leaf.key() for c in receipt.calls
            } != {leaf.key() for leaf in projection.leaves}:
                raise store.HITLConflict("Action ID already has another decision")
            return await view(conn, projection, receipt)
        if projection.availability != "pending":
            raise Unavailable(projection.unavailable_reason or "Request is unavailable")
        if not projection.leaves:
            raise Unavailable(
                projection.unavailable_reason or "Unverified continuation"
            )
        selected, candidates = await routes(conn, projection)
        if selected.availability != "pending":
            raise Unavailable(selected.unavailable_reason or "Request is unavailable")
        # Fresh remote reads occur before locks. Under locks, require the same
        # route/projection and association snapshots before recording consent.
        async with asyncio.timeout(10):
            session = await service.fresh_session(
                conn, selected.outer.runtime_session_id
            )
            task = await service.client.get_task(
                agent_for(session), selected.outer.task_id
            )
            if task.id != selected.outer.task_id:
                raise Unavailable("Task identity mismatch")
            resolved = await service.resolve(conn, session, task)
        fresh = service.projection(resolved)
        if (
            fresh.outer != selected.outer
            or fresh.leaves != selected.leaves
            or fresh.associations != selected.associations
        ):
            raise store.HITLConflict("Pending request or verified relationship changed")
        outer, request, leaf, leaf_request, binding, associations = resolved
        from mainloop.runtime.policy import PolicyError
        from mainloop.services.merge_authorization import decision_inputs, lock_decision

        try:
            async with asyncio.timeout(60):
                configuration, proposals = await decision_inputs(
                    conn, owner, leaf, leaf_request, binding, response
                )
        except PolicyError as exc:
            raise ValueError(exc.message) from None
        receipt_arguments = dict(
            action_id=action_id,
            owner_id=owner,
            outbound_message_id=str(
                uuid.uuid5(uuid.NAMESPACE_URL, f"hitl:{owner}:{action_id}")
            ),
            outer=outer,
            request=request,
            leaf_task=leaf,
            leaf_request=leaf_request,
            leaf_binding_id=binding,
            response=response,
            associations=associations,
        )
        async with conn.transaction():
            validate_proposal = await lock_decision(conn, owner, proposals)
            receipt = build_decision_receipt(
                **receipt_arguments,
                configuration=configuration,
                validate_proposal=validate_proposal,
            )
            for key in sorted(leaf.key() for leaf in projection.leaves):
                await conn.execute(
                    "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", key
                )
            current, current_candidates = await routes(conn, projection)
            if current != selected or sorted(
                p.model_dump_json() for p in candidates
            ) != sorted(p.model_dump_json() for p in current_candidates):
                raise store.HITLConflict("Continuation changed; refresh the request")
            # Association records are immutable server-owned evidence; verify they still exist.
            for association in associations:
                if not await conn.fetchval(
                    "SELECT 1 FROM native_hitl_associations WHERE identity_hash=$1",
                    normalized_hash(association.model_dump(mode="json")),
                ):
                    raise store.HITLConflict("Continuation evidence was removed")
            receipt = await store.record_response(conn, receipt)
    await dispatch(receipt, service=service)
    async with db.connection() as conn:
        return await view(conn, projection, receipt)


async def view(conn, projection, receipt=None):
    if receipt is None:
        raw = await conn.fetchval(
            """SELECT r.snapshot FROM native_hitl_responses r
            WHERE r.owner_id=$1 AND (r.snapshot->'outer'=$3::jsonb OR EXISTS (
                SELECT 1 FROM native_hitl_response_members m WHERE m.owner_id=r.owner_id
                AND m.action_id=r.action_id AND m.leaf_key=ANY($2::text[]))) LIMIT 1""",
            projection.owner_id,
            [leaf.key() for leaf in projection.leaves],
            projection.outer.model_dump_json(),
        )
        if raw:
            receipt = DecisionReceipt.model_validate_json(
                json.dumps(store._decode(raw))
            )
    state = None
    route_id = projection.id
    reason = projection.unavailable_reason
    if receipt:
        state = await conn.fetchval(
            "SELECT state FROM native_hitl_response_transport WHERE owner_id=$1 AND action_id=$2",
            receipt.owner_id,
            receipt.action_id,
        )
    elif projection.leaves:
        try:
            selected, _ = await routes(conn, projection)
            route_id = selected.id
            reason = selected.unavailable_reason
        except ValueError as exc:
            reason = str(exc)
    from mainloop.services.merge_authorization import enrichment

    return {
        "merge_enrichment": await enrichment(conn, projection),
        "request": projection.model_dump(mode="json"),
        "response": receipt.model_dump(mode="json") if receipt else None,
        "transport_state": state,
        "route_request_id": route_id,
        "answerable": receipt is None
        and projection.availability == "pending"
        and reason is None
        and route_id == projection.id,
        "unavailable_reason": reason,
    }


async def transport_state(conn, receipt, state):
    await conn.execute(
        "UPDATE native_hitl_response_transport SET state=$3,updated_at=now() WHERE owner_id=$1 AND action_id=$2",
        receipt.owner_id,
        receipt.action_id,
        state,
    )
    await store.refresh_receipt_cards(conn, receipt)


async def dispatch(receipt, *, service):
    # A session-level PG lock prevents a second dispatcher observing an in-flight
    # attempt. On process death it releases; persisted sending then means uncertain.
    async with db.connection() as conn:
        lock = f"hitl-send:{receipt.owner_id}:{receipt.action_id}"
        if not await conn.fetchval(
            "SELECT pg_try_advisory_lock(hashtextextended($1,0))", lock
        ):
            return
        try:
            state = await conn.fetchval(
                "SELECT state FROM native_hitl_response_transport WHERE owner_id=$1 AND action_id=$2",
                receipt.owner_id,
                receipt.action_id,
            )
            if state in ("accepted", "rejected_transport"):
                return
            try:
                async with asyncio.timeout(10):
                    session = await service.fresh_session(
                        conn, receipt.outer.runtime_session_id
                    )
                    if (
                        session.endpoint != receipt.outer.endpoint
                        or session.context_id != receipt.outer.context_id
                    ):
                        raise Unavailable("Original continuation identity changed")
                    task = await service.client.get_task(
                        agent_for(session), receipt.outer.task_id
                    )
                    if (
                        task.id != receipt.outer.task_id
                        or task.context_id != receipt.outer.context_id
                    ):
                        raise Unavailable("Original task identity changed")
                    if any(
                        m.message_id == receipt.outbound_message_id
                        and m.task_id in (None, receipt.outer.task_id)
                        and m.context_id in (None, receipt.outer.context_id)
                        and m.metadata.get(HITL_EXTENSION)
                        == receipt.response.model_dump(mode="json", exclude_none=True)
                        for m in task.history
                    ):
                        await transport_state(conn, receipt, "accepted")
                        return
                    if state in ("sending", "uncertain"):
                        await transport_state(conn, receipt, "uncertain")
                        return
                    resolved = await service.resolve(conn, session, task)
                    fresh = service.projection(resolved)
                    # Status-message refreshes do not change the pending operation
                    # or the immutable destination of already-recorded consent.
                    if (
                        task_identity(fresh.outer) != task_identity(receipt.outer)
                        or fresh.outer.request_hash != receipt.outer.request_hash
                    ) or {leaf.key() for leaf in fresh.leaves} != {
                        c.leaf.key() for c in receipt.calls
                    }:
                        raise Unavailable("Original pending request changed")
            except (ValueError, KagentError, TimeoutError) as exc:
                if state in ("sending", "uncertain"):
                    await transport_state(conn, receipt, "uncertain")
                elif (
                    isinstance(exc, ValueError)
                    or getattr(exc, "grpc_status", None) == 5
                ):
                    await transport_state(conn, receipt, "rejected_transport")
                else:
                    await transport_state(conn, receipt, "recorded")
                # Recorded decisions stay recorded on transient reads; observer will retry.
                return
            await transport_state(conn, receipt, "sending")
            observed_event = False
            try:
                async with asyncio.timeout(10):
                    async for _ in service.client.send_hitl_response(
                        agent_for(session),
                        response=receipt.response,
                        message_id=receipt.outbound_message_id,
                        context_id=receipt.outer.context_id,
                        task_id=receipt.outer.task_id,
                    ):
                        observed_event = True
                observed_event = True
                # Streaming completion alone does not prove our exact message was accepted.
                async with asyncio.timeout(10):
                    task = await service.client.get_task(
                        agent_for(session), receipt.outer.task_id
                    )
                accepted = (
                    task.id == receipt.outer.task_id
                    and task.context_id == receipt.outer.context_id
                    and any(
                        m.message_id == receipt.outbound_message_id
                        and m.metadata.get(HITL_EXTENSION)
                        == receipt.response.model_dump(mode="json", exclude_none=True)
                        for m in task.history
                    )
                )
                await transport_state(
                    conn, receipt, "accepted" if accepted else "uncertain"
                )
            except (SendNotAccepted, Unreachable):
                # The client exhausted its bounded safe retry budget, or sent no bytes.
                await transport_state(
                    conn, receipt, "uncertain" if observed_event else "recorded"
                )
            except (KagentError, TimeoutError):
                await transport_state(conn, receipt, "uncertain")
        finally:
            await conn.execute(
                "SELECT pg_advisory_unlock(hashtextextended($1,0))", lock
            )


RESPONSE_RECOVERY_BUDGET_SECONDS = 2.0


async def reconcile_hitl_responses():
    service = observer()
    try:
        # A separate scheduling share from inventory observation. Cancellation
        # after the durable send claim leaves `sending`, never replayable work.
        async with asyncio.timeout(RESPONSE_RECOVERY_BUDGET_SECONDS):
            async with db.connection() as conn:
                rows = await conn.fetch(
                    """SELECT r.snapshot FROM native_hitl_responses r JOIN native_hitl_response_transport t USING(owner_id,action_id)
                    WHERE r.owner_id=$1 AND r.snapshot->'outer'->>'gateway'=$2
                        AND t.state IN ('recorded','sending','uncertain')
                    ORDER BY t.updated_at,t.action_id LIMIT 10""",
                    service.owner,
                    service.gateway,
                )
            for row in rows:
                receipt = DecisionReceipt.model_validate_json(
                    json.dumps(store._decode(row["snapshot"]))
                )
                # Touch before remote work so even an interrupted attempt rotates
                # behind untouched receipts on the next pass (including restart).
                async with db.connection() as conn:
                    await conn.execute(
                        "UPDATE native_hitl_response_transport SET updated_at=now() WHERE owner_id=$1 AND action_id=$2",
                        receipt.owner_id,
                        receipt.action_id,
                    )
                await dispatch(receipt, service=service)
    except TimeoutError:
        return
