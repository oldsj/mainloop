"""PostgreSQL storage seams for verified observations and durable owner decisions.

The caller owns the transaction for observations/checkpoints and merge lookup. No
remote operations occur under these locks. b2 owns fresh gateway verification and
route selection; c claims a receipt with its merge intent in the same transaction.
"""

import json

import asyncpg
from mainloop.runtime.hitl_correlation import (
    task_identity,
    validate_receipt,
    verified_route,
)

from models.hitl import (
    DecisionReceipt,
    HITLProjection,
    MergeReceiptKey,
    ObservedSession,
    ObserverCheckpoint,
    VerifiedAssociation,
    normalized_hash,
)


class HITLConflict(ValueError):
    """A response identity or a verified leaf already has a different decision."""


def _decode(value):
    return json.loads(value) if isinstance(value, str) else value


async def observe_session(conn: asyncpg.Connection, session: ObservedSession) -> None:
    result = await conn.execute(
        """INSERT INTO native_observed_sessions(gateway,runtime_session_id,owner_id,snapshot)
           VALUES($1,$2,$3,$4::jsonb) ON CONFLICT(gateway,runtime_session_id) DO UPDATE
           SET snapshot=excluded.snapshot, observed_at=now()
           WHERE native_observed_sessions.owner_id=excluded.owner_id""",
        session.gateway,
        session.runtime_session_id,
        session.owner_id,
        session.model_dump_json(),
    )
    if result == "INSERT 0 0":
        raise HITLConflict("Observed session owner changed")


async def save_checkpoint(
    conn: asyncpg.Connection,
    gateway: str,
    owner_id: str,
    checkpoint: ObserverCheckpoint,
) -> None:
    """Call in the same transaction as the upserts covered by this cursor."""
    normalized_hash(checkpoint.model_dump(mode="json"))
    await conn.execute(
        """INSERT INTO native_hitl_observer_checkpoints(gateway,owner_id,checkpoint)
           VALUES($1,$2,$3::jsonb) ON CONFLICT(gateway,owner_id) DO UPDATE
           SET checkpoint=excluded.checkpoint,updated_at=now()""",
        gateway,
        owner_id,
        checkpoint.model_dump_json(),
    )


async def load_checkpoint(
    conn: asyncpg.Connection, gateway: str, owner_id: str
) -> ObserverCheckpoint:
    value = await conn.fetchval(
        "SELECT checkpoint FROM native_hitl_observer_checkpoints WHERE gateway=$1 AND owner_id=$2",
        gateway,
        owner_id,
    )
    return (
        ObserverCheckpoint.model_validate(_decode(value))
        if value
        else ObserverCheckpoint()
    )


async def save_association(
    conn: asyncpg.Connection, association: VerifiedAssociation
) -> None:
    """Server-only: gateway payload IDs are not verification evidence."""
    await conn.execute(
        """INSERT INTO native_hitl_associations(identity_hash,owner_id,snapshot)
           VALUES($1,$2,$3::jsonb) ON CONFLICT DO NOTHING""",
        normalized_hash(association.model_dump(mode="json")),
        association.owner_id,
        association.model_dump_json(),
    )


async def save_projection(conn: asyncpg.Connection, projection: HITLProjection) -> str:
    """Atomic with its inbox reference when called inside a transaction.

    Unverified propagated claims must have no leaves and be unavailable. A caller
    must resolve and select the outermost verified route before recording a response.
    """
    if not conn.is_in_transaction():
        raise RuntimeError("Projection and inbox writes require a transaction")
    normalized_hash(projection.model_dump(mode="json"))
    if projection.outer.request_hash != normalized_hash(
        projection.payload.model_dump(mode="json")
    ):
        raise ValueError("Projection request snapshot mismatch")
    if not projection.leaves and projection.availability != "unavailable":
        raise ValueError("Actionable projection needs verified leaves")
    for leaf in projection.leaves:
        if projection.payload.nested and task_identity(
            projection.outer
        ) == task_identity(leaf):
            raise ValueError(
                "Nested payload cannot claim the direct outer response key"
            )
        if (
            not projection.payload.nested
            and leaf.request_hash != projection.outer.request_hash
        ):
            raise ValueError("Direct request hash mismatch")
        if leaf.owner_id != projection.owner_id or not verified_route(
            projection.owner_id, projection.outer, leaf, projection.associations
        ):
            raise ValueError("Unverified alias cannot join a response key")
    # Same locks as response recording; b2 can extend the enclosing transaction
    # to choose a route over all verified aliases before first response recording.
    for key in sorted({leaf.key() for leaf in projection.leaves}):
        await conn.execute("SELECT pg_advisory_xact_lock(hashtextextended($1,0))", key)
    outer_key = normalized_hash(projection.outer.model_dump(mode="json"))
    request_id = await conn.fetchval(
        """INSERT INTO native_hitl_requests(id,owner_id,outer_key,snapshot)
           VALUES($1,$2,$3,$4::jsonb) ON CONFLICT(owner_id,outer_key) DO UPDATE
           SET snapshot=excluded.snapshot,observed_at=now(),superseded=false RETURNING id""",
        projection.id,
        projection.owner_id,
        outer_key,
        projection.model_dump_json(),
    )
    await conn.execute(
        "DELETE FROM native_hitl_aliases WHERE request_id=$1", request_id
    )
    for leaf in projection.leaves:
        await conn.execute(
            "INSERT INTO native_hitl_aliases(request_id,leaf_key) VALUES($1,$2)",
            request_id,
            leaf.key(),
        )
    thread_id = f"mt-{projection.owner_id}"
    await conn.execute(
        "INSERT INTO main_threads(id,user_id) VALUES($1,$2) ON CONFLICT(id) DO NOTHING",
        thread_id,
        projection.owner_id,
    )
    await conn.execute(
        """INSERT INTO queue_items(id,main_thread_id,user_id,item_type,title,content,hitl_request_id)
           VALUES($1,$2,$3,'hitl_request','Session needs input','',$4)
           ON CONFLICT(hitl_request_id) WHERE hitl_request_id IS NOT NULL DO NOTHING""",
        f"hitl-{request_id}",
        thread_id,
        projection.owner_id,
        request_id,
    )
    await refresh_card(conn, request_id)
    await collapse_alias_cards(conn, [leaf.key() for leaf in projection.leaves])
    return request_id


async def supersede_task_projections(conn, projection):
    """Retire historical observations, not unavailable verified parent routes.

    Caller saves the exact freshly observed projection in this same transaction.
    Leaves remain in the old snapshot for receipt lookup/idempotency, but no longer
    participate in route selection or card collapsing.
    """
    if not conn.is_in_transaction():
        raise RuntimeError("Supersession and current projection require a transaction")
    rows = await conn.fetch(
        """SELECT snapshot FROM native_hitl_requests WHERE owner_id=$1
        AND snapshot->'outer'->>'gateway'=$2 AND snapshot->'outer'->>'runtime_session_id'=$3
        AND snapshot->'outer'->>'task_id'=$4 AND id<>$5 AND NOT superseded""",
        projection.owner_id,
        projection.outer.gateway,
        projection.outer.runtime_session_id,
        projection.outer.task_id,
        projection.id,
    )
    previous = [HITLProjection.model_validate(_decode(row["snapshot"])) for row in rows]
    keys = {leaf.key() for item in [projection, *previous] for leaf in item.leaves}
    for key in sorted(keys):
        await conn.execute("SELECT pg_advisory_xact_lock(hashtextextended($1,0))", key)
    for old in previous:
        retired = old.model_copy(
            update={
                "availability": "unavailable",
                "unavailable_reason": "Pending request was superseded",
            }
        )
        await conn.execute(
            "UPDATE native_hitl_requests SET snapshot=$2::jsonb,superseded=true WHERE id=$1",
            old.id,
            retired.model_dump_json(),
        )
        await conn.execute(
            "DELETE FROM native_hitl_aliases WHERE request_id=$1", old.id
        )
        await refresh_card(conn, old.id)
        await conn.execute(
            "UPDATE queue_items SET context=context || jsonb_build_object('route_request_id',$2::text) WHERE hitl_request_id=$1",
            old.id,
            projection.id,
        )


async def refresh_card(conn, request_id):
    """Derive inbox presentation from projection and durable delivery, never vice versa."""
    await conn.execute(
        """UPDATE queue_items q SET
        title=CASE WHEN t.state='uncertain' OR t.state='sending' THEN 'Decision delivery uncertain'
            WHEN t.state='accepted' THEN 'Decision delivered'
            WHEN t.state='rejected_transport' THEN 'Decision destination unavailable'
            WHEN t.state='recorded' THEN 'Decision recorded'
            WHEN r.snapshot->>'availability'='pending' THEN 'Session needs input'
            ELSE 'Session input unavailable' END,
        status=CASE WHEN r.superseded THEN 'expired' WHEN t.state='accepted' THEN 'responded' ELSE 'pending' END,
        context=jsonb_build_object('hitl_request_id',r.id,'availability',r.snapshot->>'availability',
            'unavailable_reason',r.snapshot->>'unavailable_reason','transport_state',t.state,
            'observed_at',r.observed_at)
        FROM native_hitl_requests r
        LEFT JOIN LATERAL (
            SELECT transport.state FROM native_hitl_aliases a
            JOIN native_hitl_response_members m ON m.leaf_key=a.leaf_key
            JOIN native_hitl_response_transport transport USING(owner_id,action_id)
            WHERE a.request_id=r.id LIMIT 1
        ) t ON true WHERE q.hitl_request_id=r.id AND r.id=$1""",
        request_id,
    )


async def collapse_alias_cards(conn, keys):
    """One inbox entry for verified aliases; other session views link to that entry."""
    if not keys:
        return
    rows = await conn.fetch(
        """SELECT DISTINCT r.id,r.snapshot FROM native_hitl_requests r
        JOIN native_hitl_aliases a ON a.request_id=r.id WHERE a.leaf_key=ANY($1::text[]) ORDER BY r.id""",
        keys,
    )
    projections = [
        HITLProjection.model_validate(_decode(row["snapshot"])) for row in rows
    ]
    if not projections:
        return
    raw = await conn.fetchval(
        """SELECT r.snapshot FROM native_hitl_responses r
        JOIN native_hitl_response_members m USING(owner_id,action_id) WHERE m.leaf_key=ANY($1::text[]) LIMIT 1""",
        keys,
    )
    if raw:
        receipt = DecisionReceipt.model_validate_json(json.dumps(_decode(raw)))
        chosen = next(
            (p for p in projections if p.outer == receipt.outer), projections[0]
        )
    else:
        roots = [
            p
            for p in projections
            if all(
                verified_route(p.owner_id, p.outer, other.outer, p.associations)
                for other in projections
            )
        ]
        if len(roots) != 1:
            return
        chosen = roots[0]
    await refresh_card(conn, chosen.id)
    for projection in projections:
        await conn.execute(
            """UPDATE queue_items SET context=context || jsonb_build_object('route_request_id',$2::text),
            status=CASE WHEN hitl_request_id=$2 THEN status ELSE 'expired' END WHERE hitl_request_id=$1""",
            projection.id,
            chosen.id,
        )


async def refresh_receipt_cards(conn, receipt):
    rows = await conn.fetch(
        "SELECT DISTINCT request_id FROM native_hitl_aliases WHERE leaf_key=ANY($1::text[])",
        [c.leaf.key() for c in receipt.calls],
    )
    for row in rows:
        await refresh_card(conn, row["request_id"])
    await collapse_alias_cards(conn, [c.leaf.key() for c in receipt.calls])


async def record_response(
    conn: asyncpg.Connection, receipt: DecisionReceipt
) -> DecisionReceipt:
    """Persist once, including every member, before transport. Transaction required.

    This internal seam accepts server-validated receipts only. In b2 route selection
    and fresh verification occur in the enclosing transaction, under these leaf locks.
    """
    if not conn.is_in_transaction():
        raise RuntimeError("Decision recording requires a transaction")
    validate_receipt(receipt)
    body_hash = normalized_hash(receipt.model_dump(mode="json"))
    # Same leaf-first order as alias selection in the enclosing transaction.
    locks = {call.leaf.key() for call in receipt.calls}
    for key in sorted(locks):
        await conn.execute("SELECT pg_advisory_xact_lock(hashtextextended($1,0))", key)
    await conn.execute(
        "SELECT pg_advisory_xact_lock(hashtextextended($1,0))",
        f"action:{receipt.owner_id}:{receipt.action_id}",
    )
    existing = await conn.fetchrow(
        "SELECT body_hash,snapshot FROM native_hitl_responses WHERE owner_id=$1 AND action_id=$2",
        receipt.owner_id,
        receipt.action_id,
    )
    if existing:
        if existing["body_hash"] != body_hash:
            raise HITLConflict("Action ID already has a different body")
        return DecisionReceipt.model_validate_json(
            json.dumps(_decode(existing["snapshot"]))
        )
    members = [call.leaf.key() for call in receipt.calls]
    if await conn.fetchval(
        "SELECT 1 FROM native_hitl_response_members WHERE leaf_key=ANY($1::text[])",
        members,
    ):
        raise HITLConflict(
            "Leaf already has a recorded response; reconcile its original destination"
        )
    await conn.execute(
        """INSERT INTO native_hitl_responses(owner_id,action_id,body_hash,snapshot,outbound_message_id)
           VALUES($1,$2,$3,$4::jsonb,$5)""",
        receipt.owner_id,
        receipt.action_id,
        body_hash,
        receipt.model_dump_json(),
        receipt.outbound_message_id,
    )
    for call in receipt.calls:
        await conn.execute(
            """INSERT INTO native_hitl_response_members(leaf_key,owner_id,action_id,call_snapshot)
               VALUES($1,$2,$3,$4::jsonb)""",
            call.leaf.key(),
            receipt.owner_id,
            receipt.action_id,
            call.model_dump_json(),
        )
    await conn.execute(
        "INSERT INTO native_hitl_response_transport(owner_id,action_id) VALUES($1,$2)",
        receipt.owner_id,
        receipt.action_id,
    )
    await refresh_receipt_cards(conn, receipt)
    return receipt


async def lookup_merge_receipt(
    conn: asyncpg.Connection, key: MergeReceiptKey, approved_arguments_hash: str
) -> DecisionReceipt | None:
    """Exact consent lookup, NOT proof that this invocation traversed native approval.

    The handler supplies its own operation constant and authenticated live binding /
    runtime identity. c must validate current authority and claim this with one intent.
    Transport acceptance is deliberately not a condition of recorded owner consent.
    """
    rows = await conn.fetch(
        """SELECT r.snapshot,m.call_snapshot FROM native_hitl_response_members m
           JOIN native_hitl_responses r USING(owner_id,action_id)
           WHERE m.owner_id=$1 AND m.call_snapshot->'merge_key'=$2::jsonb""",
        key.owner_id,
        key.model_dump_json(),
    )
    if len(rows) != 1:
        return None
    row = rows[0]
    receipt = DecisionReceipt.model_validate_json(json.dumps(_decode(row["snapshot"])))
    validate_receipt(receipt)
    call = _decode(row["call_snapshot"])
    if (
        call["approved"] is not True
        or call["arguments_hash"] != approved_arguments_hash
    ):
        return None
    return receipt
