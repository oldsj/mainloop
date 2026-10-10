"""Retire task-owned inbox presentation without changing native decisions."""

# Shared by expiry, observer refresh and pending list/count queries. `r` always
# denotes a server-verified native request, never a client-supplied task ID.
TERMINAL_SOURCES = """NOT EXISTS (
    SELECT 1 FROM jsonb_array_elements(
      CASE WHEN jsonb_array_length(r.snapshot->'leaves') > 0
        THEN r.snapshot->'leaves'
        ELSE jsonb_build_array(r.snapshot->'outer') END
    ) source
    WHERE NOT EXISTS (
      SELECT 1 FROM task_attempts a JOIN tasks t ON t.id=a.task_id
      WHERE t.owner_id=r.owner_id AND t.status IN ('completed','failed','cancelled')
        AND a.binding_id=COALESCE(source->>'binding_id', (
          SELECT s.snapshot->>'binding_id' FROM native_observed_sessions s
          WHERE s.owner_id=r.owner_id AND s.gateway=source->>'gateway'
            AND s.runtime_session_id=source->>'runtime_session_id'
        ))
    )
)"""

PENDING_CARD_VISIBLE = f"""NOT EXISTS (
    SELECT 1 FROM native_hitl_requests r
    WHERE q.item_type='hitl_request' AND r.id=q.hitl_request_id
      AND r.owner_id=q.user_id AND {TERMINAL_SOURCES}
)"""  # nosec B608 - interpolated SQL is a code-owned constant, never input


async def dismiss_diagnostic(database, owner_id, item_id):
    async with database.connection() as conn:
        return await conn.fetchval(
            """UPDATE queue_items SET status='expired'
               WHERE id=$1 AND user_id=$2 AND item_type='hitl_request'
                 AND hitl_request_id IS NULL RETURNING id""",
            item_id,
            owner_id,
        )


async def retire_terminal_cards(database, owner_id):
    """Backfill old cards on inbox reads, including after a missed terminal event.

    Every verified leaf must belong to a terminal task. Unknown or mixed active
    sources remain visible. Unavailable projections without leaves use the
    observer's verified outer-session binding, never a client-supplied task ID.
    Only queue status changes; snapshots, aliases and delivery receipts survive.
    """
    if not database._pool:
        return
    async with database.connection() as conn:
        cards = await conn.fetch(
            """SELECT q.id,q.hitl_request_id
               FROM queue_items q JOIN native_hitl_requests r ON r.id=q.hitl_request_id
               WHERE q.user_id=$1 AND r.owner_id=$1 AND q.item_type='hitl_request'
                 AND q.status='pending' ORDER BY r.id""",
            owner_id,
        )
        for card in cards:
            async with conn.transaction():
                # Match save_projection's request-then-card order. Each card has
                # its own transaction so expiry never holds a set of card locks
                # while acquiring another request lock.
                await conn.fetchval(
                    """SELECT id FROM native_hitl_requests
                       WHERE id=$1 AND owner_id=$2 FOR UPDATE""",
                    card["hitl_request_id"],
                    owner_id,
                )
                await conn.fetchval(
                    """SELECT id FROM queue_items
                       WHERE id=$1 AND user_id=$2 FOR UPDATE""",
                    card["id"],
                    owner_id,
                )
                # A separate statement after both locks sees the observer's
                # committed sources and the current task states after any wait.
                await conn.execute(
                    f"""UPDATE queue_items q SET status='expired'
                        FROM native_hitl_requests r
                        WHERE q.id=$1 AND q.user_id=$2 AND r.owner_id=$2
                          AND q.hitl_request_id=r.id AND r.id=$3
                          AND q.item_type='hitl_request' AND q.status='pending'
                          AND {TERMINAL_SOURCES}""",  # nosec B608 - constant SQL fragment; all values are bound
                    card["id"],
                    owner_id,
                    card["hitl_request_id"],
                )
