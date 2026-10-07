"""Bounded, read-only HITL discovery from owned gateway inventory.

The only nested authority is the server-owned association store. Payload IDs never
create associations. Unknown prepared revisions are usable for generic questions,
but this module deliberately imports no merge configuration/authority.
"""

import asyncio
import logging
import re
import time
import uuid

from mainloop.config import settings
from mainloop.db import db
from mainloop.db import hitl as store
from mainloop.runtime.hitl_correlation import build_decision_receipt, task_identity
from mainloop.runtime.kagent_client import (
    AgentRef,
    KagentError,
    RuntimeState,
    SessionError,
    TaskNotFound,
    normalise_state,
)

from models.hitl import (
    AskUserAnswer,
    AskUserResponse,
    ContinuationIdentity,
    HITLMessageMetadata,
    HITLProjection,
    ObservedSession,
    ToolApproval,
    ToolApprovalRequest,
    ToolApprovalResponse,
    VerifiedAssociation,
    normalized_hash,
)

logger = logging.getLogger(__name__)


class Unavailable(ValueError):
    pass


def agent_for(session: ObservedSession) -> AgentRef:
    parts = session.endpoint.split("/")
    if len(parts) != 4 or parts[:2] != ["", "agents"]:
        raise Unavailable("Invalid stored endpoint")
    return AgentRef(parts[2], parts[3])


class HITLObserver:
    def __init__(self, client, *, gateway: str, owner: str, creator: str):
        self.client, self.gateway, self.owner, self.creator = (
            client,
            gateway,
            owner,
            creator,
        )

    async def read_task(self, agent, task_id):
        remaining = getattr(self, "snapshot_budget", None)
        if remaining is not None:
            if remaining <= 0:
                raise TimeoutError("Snapshot budget exhausted")
            self.snapshot_budget -= 1
        return await self.client.get_task(agent, task_id)

    async def owned(self, conn, live, previous: ObservedSession | None = None):
        if live.creator != self.creator or not self.creator:
            raise Unavailable(
                "Gateway creator is missing or does not match configured owner"
            )
        agent = live.agent
        label = r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?"
        if not agent or not all(
            re.fullmatch(label, v) for v in (agent.namespace, agent.name)
        ):
            raise Unavailable("Gateway agent identity is missing or invalid")
        if not live.prepared_revision or not live.context_id:
            raise Unavailable("Gateway revision/context is missing")
        # This is a logical private actor identity, NEVER a URL to fetch.
        if live.a2a_authority and not re.fullmatch(
            rf"session-{re.escape(live.id)}\.{label}\.actors\.resources\.substrate\.ate\.dev",
            live.a2a_authority,
        ):
            raise Unavailable("Unexpected gateway runtime authority")
        rows = await conn.fetch(
            """SELECT b.session_id,s.user_id,s.archived_at,b.kagent_deleted_at
            FROM native_bindings b JOIN sessions s ON s.id=b.session_id
            WHERE b.kagent_session_id=$1""",
            live.id,
        )
        if len(rows) > 1 or any(r["user_id"] != self.owner for r in rows):
            raise Unavailable("Conflicting session binding ownership")
        if rows and (rows[0]["archived_at"] or rows[0]["kagent_deleted_at"]):
            raise Unavailable("Original session is archived or deleted")
        binding = rows[0]["session_id"] if rows else None
        if previous and previous.binding_id and binding != previous.binding_id:
            raise Unavailable("Original binding was replaced")
        result = ObservedSession(
            gateway=self.gateway,
            runtime_session_id=live.id,
            owner_id=self.owner,
            verified_creator=live.creator,
            agent_id=f"{agent.namespace}/{agent.name}",
            endpoint=agent.path,
            context_id=live.context_id,
            prepared_revision=live.prepared_revision,
            binding_id=binding,
            lifecycle=live.state.name.lower(),
        )
        if previous and any(
            getattr(previous, k) != getattr(result, k)
            for k in (
                "gateway",
                "runtime_session_id",
                "owner_id",
                "verified_creator",
                "endpoint",
                "context_id",
                "prepared_revision",
            )
        ):
            raise Unavailable("Stored session identity changed")
        if live.state in (
            RuntimeState.DELETED,
            RuntimeState.DELETING,
            RuntimeState.FAILED,
        ):
            raise Unavailable("Original runtime is unavailable")
        return result

    async def fresh_session(self, conn, runtime_id):
        raw = await conn.fetchval(
            "SELECT snapshot FROM native_observed_sessions WHERE gateway=$1 AND runtime_session_id=$2 AND owner_id=$3",
            self.gateway,
            runtime_id,
            self.owner,
        )
        if not raw:
            raise Unavailable("Session has no verified observation")
        previous = ObservedSession.model_validate(store._decode(raw))
        live = await self.client.get_session(runtime_id)
        if live.id != runtime_id:
            raise Unavailable("Gateway returned a different session")
        return await self.owned(conn, live, previous)

    async def resolve(self, conn, session, task, visited=()):
        if task.context_id != session.context_id or not task.id:
            raise Unavailable("Task/context mismatch")
        if normalise_state(task.status.state) != "input_required":
            raise Unavailable("Task is no longer awaiting input")
        message = task.status.message
        if not message or not message.message_id:
            raise Unavailable("Pending status identity is missing")
        if message.task_id not in (None, task.id) or message.context_id not in (
            None,
            session.context_id,
        ):
            raise Unavailable("Status message identity mismatch")
        request = HITLMessageMetadata(
            metadata=message.metadata, extensions=message.extensions
        ).request()
        outer = ContinuationIdentity(
            gateway=self.gateway,
            endpoint=session.endpoint,
            runtime_session_id=session.runtime_session_id,
            context_id=session.context_id,
            task_id=task.id,
            status_message_id=message.message_id,
            request_hash=normalized_hash(request.model_dump(mode="json")),
        )
        identity = task_identity(outer)
        if identity in visited or len(visited) >= 10:
            raise Unavailable("Cyclic or excessive nested continuation")
        if not await self.client.supports_hitl(agent_for(session)):
            raise Unavailable("Agent does not advertise HITL")
        if not request.nested:
            return outer, request, identity, request, session.binding_id, ()
        records = await conn.fetch(
            "SELECT snapshot FROM native_hitl_associations WHERE owner_id=$1",
            self.owner,
        )
        links = [
            VerifiedAssociation.model_validate(store._decode(r["snapshot"]))
            for r in records
        ]
        matches = [a for a in links if a.outer == identity]
        if len(matches) != 1:
            raise Unavailable(
                "Nested continuation mapping unavailable (including provider-local subagents)"
            )
        link = matches[0]
        if link.leaf.gateway != self.gateway:
            raise Unavailable("Cross-gateway continuation is unsupported")
        leaf_session = await self.fresh_session(conn, link.leaf.runtime_session_id)
        if (
            leaf_session.endpoint != link.leaf.endpoint
            or leaf_session.context_id != link.leaf.context_id
        ):
            raise Unavailable("Nested association identity changed")
        leaf_task = await self.read_task(agent_for(leaf_session), link.leaf.task_id)
        if leaf_task.id != link.leaf.task_id:
            raise Unavailable("Nested task identity changed")
        resolved = await self.resolve(
            conn, leaf_session, leaf_task, (*visited, identity)
        )
        _, _, leaf, leaf_request, binding, associations = resolved
        return outer, request, leaf, leaf_request, binding, (link, *associations)

    def projection(self, resolved):
        outer, request, leaf, leaf_request, binding, associations = resolved
        # A neutral denial/answer validates correlation without recording consent.
        if isinstance(leaf_request, ToolApprovalRequest):
            response = ToolApprovalResponse(
                type="tool_approval_response",
                approvals=tuple(
                    ToolApproval(id=t.id, approved=False) for t in leaf_request.tools
                ),
            )
        else:
            response = AskUserResponse(
                type="ask_user_response",
                id=leaf_request.id,
                answers=tuple(
                    AskUserAnswer(answer=("validation",))
                    for _ in leaf_request.questions
                ),
            )
        receipt = build_decision_receipt(
            action_id="validate",
            owner_id=self.owner,
            outbound_message_id="validate",
            outer=outer,
            request=request,
            leaf_task=leaf,
            leaf_request=leaf_request,
            leaf_binding_id=binding,
            response=response,
            associations=associations,
        )
        return HITLProjection(
            id=str(uuid.uuid5(uuid.NAMESPACE_URL, outer.model_dump_json())),
            owner_id=self.owner,
            outer=outer,
            payload=request,
            leaves=tuple(c.leaf for c in receipt.calls),
            associations=associations,
            availability="pending",
        )

    async def diagnostic(self, conn, runtime_id, detail):
        await conn.execute(
            """INSERT INTO native_hitl_diagnostics(gateway,runtime_session_id,owner_id,detail)
            VALUES($1,$2,$3,$4) ON CONFLICT(gateway,runtime_session_id,owner_id)
            DO UPDATE SET detail=excluded.detail,observed_at=now()""",
            self.gateway,
            runtime_id,
            self.owner,
            detail,
        )

    async def mark_task(self, conn, runtime_id, task_id, availability, reason):
        rows = await conn.fetch(
            """SELECT snapshot FROM native_hitl_requests WHERE owner_id=$1
            AND snapshot->'outer'->>'gateway'=$2 AND snapshot->'outer'->>'runtime_session_id'=$3
            AND snapshot->'outer'->>'task_id'=$4 AND NOT superseded""",
            self.owner,
            self.gateway,
            runtime_id,
            task_id,
        )
        for row in rows:
            projection = HITLProjection.model_validate(store._decode(row["snapshot"]))
            if not projection.leaves:
                availability = "unavailable"
            await store.save_projection(
                conn,
                projection.model_copy(
                    update={"availability": availability, "unavailable_reason": reason}
                ),
            )

    async def refresh(self, conn, runtime_id, task_id):
        session = await self.fresh_session(conn, runtime_id)
        task = await self.read_task(agent_for(session), task_id)
        if task.id != task_id:
            raise Unavailable("Task identity mismatch")
        if normalise_state(task.status.state) not in (
            "input_required",
            "auth_required",
        ):
            async with conn.transaction():
                await self.mark_task(
                    conn,
                    runtime_id,
                    task_id,
                    "unavailable",
                    "Task is no longer awaiting input",
                )
                await conn.execute(
                    "UPDATE native_hitl_tasks SET pending=false WHERE gateway=$1 AND runtime_session_id=$2 AND task_id=$3",
                    self.gateway,
                    runtime_id,
                    task_id,
                )
            return
        try:
            resolved = await self.resolve(conn, session, task)
            projection = self.projection(resolved)
        except ValueError as exc:
            # Valid propagated payloads get their own unavailable card, with NO leaf keys.
            message = task.status.message
            try:
                request = (
                    HITLMessageMetadata(
                        metadata=message.metadata, extensions=message.extensions
                    ).request()
                    if message
                    else None
                )
            except ValueError:
                request = None
            if request and message and message.message_id:
                outer = ContinuationIdentity(
                    gateway=self.gateway,
                    endpoint=session.endpoint,
                    runtime_session_id=runtime_id,
                    context_id=session.context_id,
                    task_id=task_id,
                    status_message_id=message.message_id,
                    request_hash=normalized_hash(request.model_dump(mode="json")),
                )
                projection = HITLProjection(
                    id=str(uuid.uuid5(uuid.NAMESPACE_URL, outer.model_dump_json())),
                    owner_id=self.owner,
                    outer=outer,
                    payload=request,
                    availability="unavailable",
                    unavailable_reason=str(exc),
                )
            else:
                await self.diagnostic(
                    conn,
                    runtime_id,
                    "Authentication required or unsupported/malformed HITL request",
                )
                async with conn.transaction():
                    await self.mark_task(
                        conn,
                        runtime_id,
                        task_id,
                        "unavailable",
                        "Authentication required or unsupported/malformed HITL request",
                    )
                await self.attention(conn, runtime_id, task_id)
                return
        async with conn.transaction():
            await store.supersede_task_projections(conn, projection)
            await store.observe_session(conn, session)
            await store.save_projection(conn, projection)

    async def attention(self, conn, runtime_id, task_id):
        key = normalized_hash([self.gateway, runtime_id, task_id])
        thread = f"mt-{self.owner}"
        async with conn.transaction():
            await conn.execute(
                "INSERT INTO main_threads(id,user_id) VALUES($1,$2) ON CONFLICT DO NOTHING",
                thread,
                self.owner,
            )
            await conn.execute(
                """INSERT INTO queue_items(id,main_thread_id,user_id,item_type,title,content)
                VALUES($1,$2,$3,'hitl_request','Session input unavailable','Authentication required or unsupported/malformed HITL request')
                ON CONFLICT(id) DO NOTHING""",
                f"hitl-diagnostic-{key}",
                thread,
                self.owner,
            )

    async def once(self):
        self.snapshot_budget = 10
        deadline = time.monotonic() + 2
        async with db.connection() as conn:
            lock = f"hitl-observer:{self.gateway}:{self.owner}"
            if not await conn.fetchval(
                "SELECT pg_try_advisory_lock(hashtextextended($1,0))", lock
            ):
                return
            try:
                await self._pass(conn, deadline)
            finally:
                self.snapshot_budget = None
                await conn.execute(
                    "SELECT pg_advisory_unlock(hashtextextended($1,0))", lock
                )

    async def _pass(self, conn, deadline):
        checkpoint = await store.load_checkpoint(conn, self.gateway, self.owner)
        due = await conn.fetchval(
            "SELECT next_sweep <= now() FROM native_hitl_inventory_state WHERE gateway=$1 AND owner_id=$2",
            self.gateway,
            self.owner,
        )
        if checkpoint.inventory_cursor or due is not False:
            try:
                async with asyncio.timeout(
                    min(0.5, max(0.01, deadline - time.monotonic()))
                ):
                    sessions, cursor = await self.client.list_sessions_page(
                        checkpoint.inventory_cursor or "", 50
                    )
                    if (
                        len(sessions) > 50
                        or cursor
                        and cursor == checkpoint.inventory_cursor
                    ):
                        raise SessionError("Invalid inventory page", grpc_status=3)
                    async with conn.transaction():
                        for live in sessions:
                            try:
                                previous = await conn.fetchval(
                                    "SELECT snapshot FROM native_observed_sessions WHERE gateway=$1 AND runtime_session_id=$2",
                                    self.gateway,
                                    live.id,
                                )
                                observed = await self.owned(
                                    conn,
                                    live,
                                    (
                                        ObservedSession.model_validate(
                                            store._decode(previous)
                                        )
                                        if previous
                                        else None
                                    ),
                                )
                                await store.observe_session(conn, observed)
                                await conn.execute(
                                    """INSERT INTO native_hitl_task_scan(gateway,runtime_session_id,owner_id)
                                    VALUES($1,$2,$3) ON CONFLICT DO NOTHING""",
                                    self.gateway,
                                    live.id,
                                    self.owner,
                                )
                            except ValueError as exc:
                                await self.diagnostic(conn, live.id, str(exc))
                        await store.save_checkpoint(
                            conn,
                            self.gateway,
                            self.owner,
                            checkpoint.model_copy(
                                update={"inventory_cursor": cursor or None}
                            ),
                        )
                        await conn.execute(
                            """INSERT INTO native_hitl_inventory_state(gateway,owner_id,next_sweep)
                            VALUES($1,$2,now()+interval '30 seconds') ON CONFLICT(gateway,owner_id)
                            DO UPDATE SET next_sweep=excluded.next_sweep""",
                            self.gateway,
                            self.owner,
                        )
            except SessionError as exc:
                if exc.grpc_status == 3 and checkpoint.inventory_cursor:
                    async with conn.transaction():
                        await store.save_checkpoint(
                            conn,
                            self.gateway,
                            self.owner,
                            checkpoint.model_copy(update={"inventory_cursor": None}),
                        )
                else:
                    logger.warning("HITL inventory failed: %s", type(exc).__name__)
            except (KagentError, TimeoutError):
                logger.warning("HITL inventory temporarily unavailable")
        if time.monotonic() >= deadline:
            return
        # A durable least-recently-scanned queue rotates even after failures/restarts.
        scan = await conn.fetchrow(
            """SELECT * FROM native_hitl_task_scan WHERE gateway=$1 AND owner_id=$2
            ORDER BY scanned_at,runtime_session_id LIMIT 1""",
            self.gateway,
            self.owner,
        )
        if scan:
            try:
                async with asyncio.timeout(
                    min(0.5, max(0.01, deadline - time.monotonic()))
                ):
                    session = await self.fresh_session(conn, scan["runtime_session_id"])
                    tasks, cursor = await self.client.list_tasks_page(
                        agent_for(session), session.context_id, scan["cursor"], 50
                    )
                    if len(tasks) > 50 or cursor and cursor == scan["cursor"]:
                        from mainloop.runtime.kagent_client import A2AError

                        raise A2AError(-32602, "Invalid task page")
                    async with conn.transaction():
                        for task in tasks:
                            if task.context_id != session.context_id:
                                continue
                            await conn.execute(
                                """INSERT INTO native_hitl_tasks(gateway,runtime_session_id,task_id,owner_id)
                                VALUES($1,$2,$3,$4) ON CONFLICT(gateway,runtime_session_id,task_id)
                                DO UPDATE SET pending=true""",
                                self.gateway,
                                session.runtime_session_id,
                                task.id,
                                self.owner,
                            )
                        await conn.execute(
                            "UPDATE native_hitl_task_scan SET cursor=$3 WHERE gateway=$1 AND runtime_session_id=$2",
                            self.gateway,
                            session.runtime_session_id,
                            cursor,
                        )
            except (KagentError, ValueError, TimeoutError) as exc:
                await self.diagnostic(
                    conn, scan["runtime_session_id"], type(exc).__name__
                )
                # Invalid page tokens restart this session, never imply deletion.
                if getattr(exc, "code", None) == -32602:
                    await conn.execute(
                        "UPDATE native_hitl_task_scan SET cursor='' WHERE gateway=$1 AND runtime_session_id=$2",
                        self.gateway,
                        scan["runtime_session_id"],
                    )
            finally:
                await conn.execute(
                    "UPDATE native_hitl_task_scan SET scanned_at=now() WHERE gateway=$1 AND runtime_session_id=$2",
                    self.gateway,
                    scan["runtime_session_id"],
                )
        tasks = await conn.fetch(
            """SELECT * FROM native_hitl_tasks WHERE gateway=$1 AND owner_id=$2 AND pending
            ORDER BY checked_at,runtime_session_id,task_id LIMIT 10""",
            self.gateway,
            self.owner,
        )
        for row in tasks:
            if time.monotonic() >= deadline:
                break
            try:
                async with asyncio.timeout(max(0.01, deadline - time.monotonic())):
                    await self.refresh(conn, row["runtime_session_id"], row["task_id"])
            except (KagentError, ValueError, TimeoutError) as exc:
                gone = (
                    isinstance(exc, (Unavailable, TaskNotFound))
                    or isinstance(exc, SessionError)
                    and exc.grpc_status == 5
                )
                async with conn.transaction():
                    await self.mark_task(
                        conn,
                        row["runtime_session_id"],
                        row["task_id"],
                        "unavailable" if gone else "stale",
                        type(exc).__name__,
                    )
                    await self.diagnostic(
                        conn, row["runtime_session_id"], type(exc).__name__
                    )
            finally:
                await conn.execute(
                    "UPDATE native_hitl_tasks SET checked_at=now() WHERE gateway=$1 AND runtime_session_id=$2 AND task_id=$3",
                    self.gateway,
                    row["runtime_session_id"],
                    row["task_id"],
                )


def observer():
    from mainloop.runtime.native_sessions import get_client

    return HITLObserver(
        get_client(),
        gateway=settings.kagent_gateway_url.rstrip("/"),
        owner=settings.owner_id,
        creator=settings.kagent_user_id,
    )


async def observe_hitl_once():
    await observer().once()
