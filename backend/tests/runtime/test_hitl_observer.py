"""Real observer/owner API + isolated PostgreSQL; fake gateway, no native agents."""

import asyncio
from dataclasses import replace
from unittest.mock import patch

import httpx
from mainloop import api
from mainloop.config import settings
from mainloop.db import hitl as store
from mainloop.runtime import hitl_continuation as continuation
from mainloop.runtime import native_sessions
from mainloop.runtime.hitl_correlation import task_identity
from mainloop.runtime.hitl_observer import HITLObserver
from mainloop.runtime.kagent_client import (
    AgentRef,
    KagentSession,
    Message,
    OutcomeUnknown,
    RuntimeOperation,
    RuntimeState,
    SessionError,
    StreamEvent,
    Task,
    TaskStatus,
    Unreachable,
)
from tests.runtime.test_postgres_ledger import PostgresTestCase

from models.hitl import (
    HITL_EXTENSION,
    HITLProjection,
    ToolApproval,
    ToolApprovalResponse,
    VerifiedAssociation,
)


def approval(request_id="call"):
    return {
        "type": "tool_approval_request",
        "tools": [
            {
                "id": request_id,
                "call_id": "native-call",
                "name": "ordinary.tool",
                "args": {"value": 1},
            }
        ],
    }


def pending(session, task_id="task", payload=None):
    return Task(
        id=task_id,
        context_id=session.context_id,
        status=TaskStatus(
            state="input-required",
            message=Message(
                message_id=f"status-{task_id}",
                task_id=task_id,
                context_id=session.context_id,
                extensions=[HITL_EXTENSION],
                metadata={
                    HITL_EXTENSION: payload or approval(),
                    "other.extension": {"retained": True},
                },
            ),
        ),
    )


class Gateway:
    def __init__(self):
        self.sessions = {}
        self.tasks = {}
        self.sent = []
        self.reads = []
        self.pages = []
        self.failure = None
        self.send_failure = None
        self.capable = True
        self.page_size = 50

    def add(self, sid, payload=None):
        session = KagentSession(
            id=sid,
            context_id=f"context-{sid}",
            state=RuntimeState.SUSPENDED,
            operation=RuntimeOperation.NONE,
            creator="gateway-owner",
            agent=AgentRef("team", "agent"),
            prepared_revision="unknown-but-pinned",
            a2a_authority=f"session-{sid}.team.actors.resources.substrate.ate.dev",
        )
        self.sessions[sid] = session
        task = pending(session, f"task-{sid}", payload)
        self.tasks[task.id] = task
        return session, task

    async def list_sessions_page(self, cursor="", limit=100):
        self.pages.append(cursor)
        values = list(self.sessions.values())
        start = int(cursor or "0")
        end = start + min(limit, self.page_size)
        return values[start:end], str(end) if end < len(values) else ""

    async def get_session(self, sid):
        if self.failure:
            raise self.failure
        if sid not in self.sessions:
            raise SessionError("deleted", grpc_status=5)
        return self.sessions[sid]

    async def list_tasks_page(self, agent, context, cursor="", limit=100):
        tasks = [t for t in self.tasks.values() if t.context_id == context]
        start = int(cursor or "0")
        return tasks[start : start + limit], (
            str(start + limit) if start + limit < len(tasks) else ""
        )

    async def get_task(self, agent, task_id):
        self.reads.append(task_id)
        if self.failure:
            raise self.failure
        return self.tasks[task_id].model_copy(deep=True)

    async def supports_hitl(self, agent):
        return self.capable

    async def send_hitl_response(self, agent, **kwargs):
        self.sent.append((agent, kwargs))
        if self.send_failure:
            raise self.send_failure
        task = self.tasks[kwargs["task_id"]]
        task.history.append(
            Message(
                message_id=kwargs["message_id"],
                task_id=task.id,
                context_id=task.context_id,
                metadata={
                    HITL_EXTENSION: kwargs["response"].model_dump(
                        mode="json", exclude_none=True
                    )
                },
            )
        )
        task.status = TaskStatus(state="working")
        yield StreamEvent(task=task)


class HITLObserverTests(PostgresTestCase):
    async def asyncSetUp(self):
        await super().asyncSetUp()
        self.gateway = Gateway()
        self.service = HITLObserver(
            self.gateway,
            gateway=f"https://gateway/{self.user}",
            owner=self.user,
            creator="gateway-owner",
        )

    async def projections(self):
        rows = await self.pool.fetch(
            "SELECT snapshot FROM native_hitl_requests WHERE owner_id=$1 ORDER BY id",
            self.user,
        )
        return [
            HITLProjection.model_validate(store._decode(r["snapshot"])) for r in rows
        ]

    async def cycle(self, count=1):
        for _ in range(count):
            await self.service.once()

    def response(self, approved=True):
        return ToolApprovalResponse(
            type="tool_approval_response",
            approvals=(ToolApproval(id="call", approved=approved),),
        )

    async def test_startup_observer_standalone_restart_and_owner_api(self):
        session, task = self.gateway.add("standalone")
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_deliveries d JOIN sessions s ON s.id=d.session_id WHERE s.user_id=$1",
                self.user,
            ),
            0,
        )
        self.assertEqual(await self.projections(), [])
        # The same entry point lifespan's reconciliation loop invokes; no browser.
        with patch(
            "mainloop.runtime.hitl_observer.observer", return_value=self.service
        ), patch(
            "mainloop.runtime.hitl_continuation.observer", return_value=self.service
        ):
            await native_sessions.reconcile_once(sweep=False)
        self.service = HITLObserver(
            self.gateway,
            gateway=f"https://gateway/{self.user}",
            owner=self.user,
            creator="gateway-owner",
        )
        await self.cycle(2)
        projections = await self.projections()
        self.assertEqual(len(projections), 1)
        projection = projections[0]
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM queue_items WHERE user_id=$1", self.user
            ),
            1,
        )
        self.assertIsNone(projection.leaves[0].binding_id)
        with patch.object(settings, "api_hosts", "localhost"), patch.object(
            settings, "owner_id", self.user
        ), patch(
            "mainloop.runtime.hitl_continuation.observer", return_value=self.service
        ):
            async with httpx.AsyncClient(
                transport=httpx.ASGITransport(app=api.app), base_url="http://localhost"
            ) as client:
                body = {
                    "action_id": "owner-action",
                    "response": self.response().model_dump(mode="json"),
                }
                with patch.dict(
                    "os.environ", {"MAINLOOP_OWNER_HITL_WRITES_ENABLED": "false"}
                ):
                    result = await client.post(
                        f"/hitl/{projection.id}/respond", json=body
                    )
                    self.assertEqual(result.status_code, 503, result.text)
                self.assertEqual(self.gateway.sent, [])
                with patch.dict(
                    "os.environ", {"MAINLOOP_OWNER_HITL_WRITES_ENABLED": "true"}
                ):
                    result = await client.post(
                        f"/hitl/{projection.id}/respond", json=body
                    )
                    self.assertEqual(result.status_code, 200, result.text)
                    self.assertEqual(result.json()["transport_state"], "accepted")
                    duplicate = await client.post(
                        f"/hitl/{projection.id}/respond", json=body
                    )
                    self.assertEqual(duplicate.status_code, 200, duplicate.text)
                generic = await client.post(
                    f"/queue/hitl-{projection.id}/respond", json={"response": "yes"}
                )
                self.assertEqual(generic.status_code, 409, generic.text)
        self.assertEqual(len(self.gateway.sent), 1)
        agent, sent = self.gateway.sent[0]
        self.assertEqual(
            (agent, sent["task_id"], sent["context_id"]),
            (session.agent, task.id, session.context_id),
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_deliveries d JOIN sessions s ON s.id=d.session_id WHERE s.user_id=$1",
                self.user,
            ),
            0,
        )

    async def test_denied_creator_no_task_contents_and_pagination_restart(self):
        self.gateway.page_size = 1
        self.gateway.add("bad")
        self.gateway.sessions["bad"] = replace(
            self.gateway.sessions["bad"], creator="stranger"
        )
        self.gateway.add("good")
        await self.cycle()
        self.assertEqual(self.gateway.reads, [])
        self.service = HITLObserver(
            self.gateway,
            gateway=f"https://gateway/{self.user}",
            owner=self.user,
            creator="gateway-owner",
        )
        await self.cycle(2)
        self.assertEqual(self.gateway.pages, ["", "1"])
        self.assertEqual(len(await self.projections()), 1)
        self.assertNotIn("task-bad", self.gateway.reads)

    async def nested(self, verified=False):
        child, child_task = self.gateway.add("child")
        outer_payload = approval("parent-call")
        outer_payload["nested"] = {
            "subagent_name": "untrusted",
            "task_id": child_task.id,
            "context_id": child.context_id,
            "tools": approval()["tools"],
        }
        parent, parent_task = self.gateway.add("parent", outer_payload)
        await self.cycle(3)
        if verified:
            await self.verify(parent_task, child_task)
            await self.cycle(2)
        return parent, parent_task, child, child_task

    async def verify(self, parent_task, child_task):
        projections = await self.projections()
        outer = next(p for p in projections if p.outer.task_id == parent_task.id)
        leaf = next(p for p in projections if p.outer.task_id == child_task.id)
        association = VerifiedAssociation(
            owner_id=self.user,
            outer=task_identity(outer.outer),
            leaf=task_identity(leaf.outer),
            evidence_source="gateway_continuation",
            evidence_reference="fixture:trusted-gateway-record",
        )
        async with self.pool.acquire() as conn:
            await store.save_association(conn, association)

    async def test_unverified_claim_isolated_then_verified_parent_single_route(self):
        _, parent_task, _, child_task = await self.nested()
        projections = await self.projections()
        child = next(p for p in projections if p.outer.task_id == child_task.id)
        parent = next(p for p in projections if p.outer.task_id == parent_task.id)
        self.assertEqual(parent.leaves, ())
        async with self.pool.acquire() as conn:
            self.assertTrue((await continuation.view(conn, child))["answerable"])
            self.assertFalse((await continuation.view(conn, parent))["answerable"])
        await self.verify(parent_task, child_task)
        await self.cycle(2)
        # Restart the observer while both projections and trusted association survive.
        self.service = HITLObserver(
            self.gateway,
            gateway=self.service.gateway,
            owner=self.user,
            creator="gateway-owner",
        )
        await self.cycle()
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM queue_items WHERE user_id=$1 AND status='pending'",
                self.user,
            ),
            1,
        )
        results = await asyncio.gather(
            continuation.submit(
                self.user,
                child.id,
                "child-click",
                self.response(),
                service=self.service,
            ),
            continuation.submit(
                self.user,
                parent.id,
                "parent-click",
                self.response(False),
                service=self.service,
            ),
            return_exceptions=True,
        )
        self.assertEqual(sum(isinstance(r, dict) for r in results), 1, results)
        self.assertEqual(len(self.gateway.sent), 1)
        self.assertEqual(self.gateway.sent[0][1]["task_id"], parent_task.id)
        self.assertEqual(self.gateway.sent[0][1]["response"].approvals[0].id, "call")
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_hitl_responses WHERE owner_id=$1",
                self.user,
            ),
            1,
        )

    async def test_recorded_direct_then_verified_alias_never_reroutes_or_replays(self):
        _, parent_task, _, child_task = await self.nested()
        child = next(
            p for p in await self.projections() if p.outer.task_id == child_task.id
        )
        self.gateway.send_failure = OutcomeUnknown("connection lost after send")
        result = await continuation.submit(
            self.user, child.id, "direct", self.response(), service=self.service
        )
        self.assertEqual(result["transport_state"], "uncertain")
        before = await self.pool.fetchval(
            "SELECT snapshot FROM native_hitl_responses WHERE owner_id=$1", self.user
        )
        await self.verify(parent_task, child_task)
        await self.cycle(2)
        parent = next(
            p for p in await self.projections() if p.outer.task_id == parent_task.id
        )
        async with self.pool.acquire() as conn:
            self.assertFalse((await continuation.view(conn, parent))["answerable"])
            await conn.execute(
                "DELETE FROM native_hitl_requests WHERE owner_id=$1", self.user
            )
        await self.cycle(2)
        with patch(
            "mainloop.runtime.hitl_continuation.observer", return_value=self.service
        ):
            await continuation.reconcile_hitl_responses()
        self.assertEqual(len(self.gateway.sent), 1)
        self.assertEqual(
            before,
            await self.pool.fetchval(
                "SELECT snapshot FROM native_hitl_responses WHERE owner_id=$1",
                self.user,
            ),
        )
        self.assertEqual(self.gateway.sent[0][1]["task_id"], child_task.id)

    async def test_temporary_failure_stale_confirmed_delete_unavailable(self):
        self.gateway.add("one")
        await self.cycle()
        self.gateway.failure = Unreachable("temporary")
        await self.cycle()
        self.assertEqual((await self.projections())[0].availability, "stale")
        self.gateway.failure = None
        await self.cycle()
        self.assertEqual((await self.projections())[0].availability, "pending")
        del self.gateway.sessions["one"]
        await self.cycle()
        self.assertEqual((await self.projections())[0].availability, "unavailable")

    async def test_crash_before_dispatch_recovers_recorded_after_claim_never_replays(
        self,
    ):
        self.gateway.add("one")
        await self.cycle()
        projection = (await self.projections())[0]
        with patch(
            "mainloop.runtime.hitl_continuation.dispatch",
            side_effect=RuntimeError("crash before dispatch"),
        ):
            with self.assertRaises(RuntimeError):
                await continuation.submit(
                    self.user,
                    projection.id,
                    "crash",
                    self.response(),
                    service=self.service,
                )
        self.assertEqual(self.gateway.sent, [])
        with patch(
            "mainloop.runtime.hitl_continuation.observer", return_value=self.service
        ):
            await continuation.reconcile_hitl_responses()
        self.assertEqual(len(self.gateway.sent), 1)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT state FROM native_hitl_response_transport WHERE owner_id=$1",
                self.user,
            ),
            "accepted",
        )

    async def test_response_without_merge_configuration_never_mints_merge_authority(
        self,
    ):
        payload = approval()
        payload["tools"][0][
            "name"
        ] = "mcp__mainloop-merge-approval__merge_pull_request_with_approval"
        payload["tools"][0]["args"] = {
            "proposal_id": "proposal",
            "request_id": "invocation",
        }
        self.gateway.add("one", payload)
        await self.cycle()
        projection = (await self.projections())[0]
        result = await continuation.submit(
            self.user,
            projection.id,
            "unknown-config",
            self.response(),
            service=self.service,
        )
        self.assertIsNone(result["response"]["calls"][0]["merge_key"])
        self.assertIsNone(result["response"]["calls"][0]["mapping"])

    async def test_child_binding_queues_ordinary_turns_but_continuation_bypasses(self):
        parent, _ = await self.bound_session()
        sid, cid = await self.bound_session(role="child", parent_session_id=parent)
        session, task = self.gateway.add("bound-child")
        await native_sessions.ledger.update_binding(sid, kagent_session_id=session.id)
        waiting = await self.delivery(sid, cid, "delivered")
        queued = await self.delivery(sid, cid, "queued")
        await self.cycle()
        projection = (await self.projections())[0]
        self.assertEqual(projection.leaves[0].binding_id, sid)
        self.assertEqual(projection.associations, ())
        result = await continuation.submit(
            self.user, projection.id, "child", self.response(), service=self.service
        )
        self.assertEqual(result["transport_state"], "accepted")
        self.assertEqual(self.gateway.sent[0][1]["task_id"], task.id)
        self.assertEqual(await self.state_of(waiting), "delivered")
        self.assertEqual(await self.state_of(queued), "queued")

    async def test_bound_replacement_archive_and_wrong_owner_fail_before_task_read(
        self,
    ):
        sid, _ = await self.bound_session()
        session, _ = self.gateway.add("bound")
        await native_sessions.ledger.update_binding(sid, kagent_session_id=session.id)
        await self.cycle()
        projection = (await self.projections())[0]
        for change in ("archived", "owner", "replaced"):
            with self.subTest(change=change):
                if change == "archived":
                    await self.pool.execute(
                        "UPDATE sessions SET archived_at=now() WHERE id=$1", sid
                    )
                elif change == "owner":
                    await self.pool.execute(
                        "UPDATE sessions SET archived_at=NULL,user_id=$2 WHERE id=$1",
                        sid,
                        "stranger",
                    )
                else:
                    await self.pool.execute(
                        "UPDATE sessions SET user_id=$2 WHERE id=$1", sid, self.user
                    )
                    await native_sessions.ledger.update_binding(
                        sid, kagent_session_id="replacement"
                    )
                self.gateway.reads.clear()
                with self.assertRaises(ValueError):
                    await continuation.submit(
                        self.user,
                        projection.id,
                        change,
                        self.response(),
                        service=self.service,
                    )
                self.assertEqual(self.gateway.reads, [])
                self.assertEqual(self.gateway.sent, [])
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_hitl_responses WHERE owner_id=$1",
                self.user,
            ),
            0,
        )

    async def test_binding_attaches_without_duplicate_card(self):
        session, _ = self.gateway.add("attach")
        await self.cycle()
        before = (await self.projections())[0]
        sid, _ = await self.bound_session()
        await native_sessions.ledger.update_binding(sid, kagent_session_id=session.id)
        await self.cycle()
        after = (await self.projections())[0]
        self.assertEqual(after.id, before.id)
        self.assertEqual(after.leaves[0].binding_id, sid)

    async def test_cursor_rollback_and_fair_rotation_snapshot_bound(self):
        self.gateway.page_size = 2
        for i in range(5):
            self.gateway.add(f"session-{i}")
        with patch(
            "mainloop.runtime.hitl_observer.store.save_checkpoint",
            side_effect=RuntimeError("interrupted transaction"),
        ):
            with self.assertRaises(RuntimeError):
                await self.cycle()
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_observed_sessions WHERE owner_id=$1",
                self.user,
            ),
            0,
        )
        for _ in range(7):
            self.gateway.reads.clear()
            await self.cycle()
            self.assertLessEqual(len(self.gateway.reads), 10)
        self.assertEqual(len(await self.projections()), 5)
        self.assertEqual(self.gateway.pages[:2], ["", ""])

    async def test_changed_request_same_state_stale_click_and_cancel(self):
        session, task = self.gateway.add("successive")
        await self.cycle()
        old = (await self.projections())[0]
        task.status.message.metadata[HITL_EXTENSION] = approval("new-call")
        await self.cycle()
        current = next(
            p for p in await self.projections() if p.availability == "pending"
        )
        self.assertNotEqual(old.id, current.id)
        with self.assertRaises(ValueError):
            await continuation.submit(
                self.user, old.id, "stale", self.response(), service=self.service
            )
        task.status.state = "canceled"
        await self.cycle()
        with self.assertRaises(ValueError):
            await continuation.submit(
                self.user,
                current.id,
                "cancelled",
                self.response(),
                service=self.service,
            )
        self.assertEqual(self.gateway.sent, [])

    async def test_question_choices_null_and_nested_parent_child_ids(self):
        from models.hitl import AskUserAnswer, AskUserResponse

        payload = {
            "type": "ask_user_request",
            "id": "child-question",
            "questions": [{"question": "Explain", "choices": None, "multiple": False}],
        }
        child, child_task = self.gateway.add("question-child", payload)
        parent_payload = {
            **payload,
            "id": "parent-question",
            "nested": {
                "task_id": child_task.id,
                "context_id": child.context_id,
                "tools": [
                    {
                        "id": "child-question",
                        "call_id": "child-question",
                        "name": "ask_user",
                        "args": {},
                    }
                ],
            },
        }
        _, parent_task = self.gateway.add("question-parent", parent_payload)
        await self.cycle(3)
        await self.verify(parent_task, child_task)
        await self.cycle(2)
        parent = next(
            p for p in await self.projections() if p.outer.task_id == parent_task.id
        )
        response = AskUserResponse(
            type="ask_user_response",
            id="child-question",
            answers=(AskUserAnswer(answer=("Free text",)),),
        )
        result = await continuation.submit(
            self.user, parent.id, "answer", response, service=self.service
        )
        self.assertEqual(result["transport_state"], "accepted")
        self.assertEqual(self.gateway.sent[0][1]["response"].id, "child-question")
        self.assertEqual(self.gateway.sent[0][1]["task_id"], parent_task.id)
        self.assertEqual(result["response"]["request"]["id"], "parent-question")
        self.assertIsNone(result["response"]["calls"][0]["merge_key"])

    async def test_sending_crash_and_lost_acceptance_reconcile_only_exact_message(self):
        self.gateway.add("send-crash")
        await self.cycle()
        projection = (await self.projections())[0]
        with patch(
            "mainloop.runtime.hitl_continuation.dispatch",
            side_effect=RuntimeError("crash"),
        ):
            with self.assertRaises(RuntimeError):
                await continuation.submit(
                    self.user,
                    projection.id,
                    "crash",
                    self.response(),
                    service=self.service,
                )
        await self.pool.execute(
            "UPDATE native_hitl_response_transport SET state='sending' WHERE owner_id=$1",
            self.user,
        )
        with patch(
            "mainloop.runtime.hitl_continuation.observer", return_value=self.service
        ):
            await continuation.reconcile_hitl_responses()
            self.assertEqual(self.gateway.sent, [])
            self.assertEqual(
                await self.pool.fetchval(
                    "SELECT state FROM native_hitl_response_transport WHERE owner_id=$1",
                    self.user,
                ),
                "uncertain",
            )
            task = self.gateway.tasks["task-send-crash"]
            task.status.state = "working"
            await continuation.reconcile_hitl_responses()
            self.assertEqual(
                await self.pool.fetchval(
                    "SELECT state FROM native_hitl_response_transport WHERE owner_id=$1",
                    self.user,
                ),
                "uncertain",
            )
            raw = await self.pool.fetchval(
                "SELECT snapshot FROM native_hitl_responses WHERE owner_id=$1",
                self.user,
            )
            receipt = store._decode(raw)
            task.history.append(
                Message(
                    message_id=receipt["outbound_message_id"],
                    task_id=task.id,
                    context_id=task.context_id,
                    metadata={
                        HITL_EXTENSION: self.response().model_dump(
                            mode="json", exclude_none=True
                        )
                    },
                )
            )
            await continuation.reconcile_hitl_responses()
            self.assertEqual(
                await self.pool.fetchval(
                    "SELECT state FROM native_hitl_response_transport WHERE owner_id=$1",
                    self.user,
                ),
                "accepted",
            )
            self.assertEqual(self.gateway.sent, [])

    async def test_unsupported_auth_and_malformed_requests_visible_without_controls(
        self,
    ):
        session, task = self.gateway.add("unsupported")
        task.status.message.metadata[HITL_EXTENSION] = {"type": "new-unsupported-type"}
        task.status.state = "auth-required"
        await self.cycle(2)
        self.assertEqual(await self.projections(), [])
        cards = await self.pool.fetch(
            "SELECT * FROM queue_items WHERE user_id=$1", self.user
        )
        self.assertEqual(len(cards), 1)
        self.assertEqual(cards[0]["title"], "Session input unavailable")
        self.assertIsNone(cards[0]["hitl_request_id"])

    async def test_ambiguous_verified_relationship_holds_direct_but_untrusted_does_not(
        self,
    ):
        _, parent_task, _, child_task = await self.nested()
        await self.verify(parent_task, child_task)
        # Trusted parent exists but is not yet resolved; direct must not slip through.
        child = next(
            p for p in await self.projections() if p.outer.task_id == child_task.id
        )
        with self.assertRaises(ValueError):
            await continuation.submit(
                self.user, child.id, "premature", self.response(), service=self.service
            )
        await self.cycle(2)
        parent = next(
            p for p in await self.projections() if p.outer.task_id == parent_task.id
        )
        association = VerifiedAssociation(
            owner_id=self.user,
            outer=task_identity(parent.outer),
            leaf=task_identity(child.outer),
            evidence_source="control_plane_creation",
            evidence_reference="conflicting-second-evidence",
        )
        async with self.pool.acquire() as conn:
            await store.save_association(conn, association)
        await self.cycle(2)
        with self.assertRaises(ValueError):
            await continuation.submit(
                self.user, child.id, "ambiguous", self.response(), service=self.service
            )
        self.assertEqual(self.gateway.sent, [])

    async def test_post_send_read_failure_does_not_reset_receipt_for_replay(self):
        self.gateway.add("post-send")
        await self.cycle()
        projection = (await self.projections())[0]
        original = self.gateway.send_hitl_response

        async def send_then_disconnect(*args, **kwargs):
            async for event in original(*args, **kwargs):
                yield event
            self.gateway.failure = Unreachable("history read unavailable after send")

        with patch.object(self.gateway, "send_hitl_response", new=send_then_disconnect):
            result = await continuation.submit(
                self.user, projection.id, "sent", self.response(), service=self.service
            )
        self.assertEqual(result["transport_state"], "uncertain")
        with patch(
            "mainloop.runtime.hitl_continuation.observer", return_value=self.service
        ):
            await continuation.reconcile_hitl_responses()
        self.assertEqual(len(self.gateway.sent), 1)

    async def test_identity_and_capability_fail_closed(self):
        for sid, changes in (
            ("missing-creator", {"creator": ""}),
            ("missing-agent", {"agent": None}),
            ("bad-endpoint", {"agent": AgentRef("team", "../evil")}),
            ("bad-authority", {"a2a_authority": "https://attacker.example"}),
            ("missing-revision", {"prepared_revision": ""}),
        ):
            session, _ = self.gateway.add(sid)
            self.gateway.sessions[sid] = replace(session, **changes)
        await self.cycle(2)
        self.assertEqual(self.gateway.reads, [])
        self.assertEqual(await self.projections(), [])
        self.gateway.add("no-capability")
        await self.pool.execute(
            "UPDATE native_hitl_inventory_state SET next_sweep='epoch' WHERE owner_id=$1",
            self.user,
        )
        self.gateway.capable = False
        await self.cycle(2)
        projection = (await self.projections())[0]
        self.assertEqual(projection.availability, "unavailable")
        self.assertEqual(projection.leaves, ())
        with self.assertRaises(ValueError):
            await continuation.submit(
                self.user, projection.id, "no", self.response(), service=self.service
            )

    async def test_batch_incomplete_duplicate_and_alias_conflict_are_atomic(self):
        payload = approval()
        payload["tools"].append({**payload["tools"][0], "id": "second"})
        child, child_task = self.gateway.add("batch-child", payload)
        parent_payload = approval("wrapper")
        parent_payload["nested"] = {
            "task_id": child_task.id,
            "context_id": child.context_id,
            "tools": payload["tools"],
        }
        _, parent_task = self.gateway.add("batch-parent", parent_payload)
        await self.cycle(3)
        await self.verify(parent_task, child_task)
        await self.cycle(2)
        projections = await self.projections()
        parent = next(p for p in projections if p.outer.task_id == parent_task.id)
        child_projection = next(
            p for p in projections if p.outer.task_id == child_task.id
        )
        for response in (
            self.response(),
            ToolApprovalResponse(
                type="tool_approval_response",
                approvals=(
                    ToolApproval(id="call", approved=True),
                    ToolApproval(id="call", approved=False),
                ),
            ),
        ):
            with self.assertRaises(ValueError):
                await continuation.submit(
                    self.user, parent.id, "incomplete", response, service=self.service
                )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_hitl_response_members WHERE owner_id=$1",
                self.user,
            ),
            0,
        )
        response = ToolApprovalResponse(
            type="tool_approval_response",
            approvals=(
                ToolApproval(id="call", approved=True),
                ToolApproval(id="second", approved=False),
            ),
        )
        result = await continuation.submit(
            self.user, parent.id, "batch", response, service=self.service
        )
        self.assertEqual(result["transport_state"], "accepted")
        with self.assertRaises(ValueError):
            await continuation.submit(
                self.user, child_projection.id, "other", response, service=self.service
            )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_hitl_response_members WHERE owner_id=$1",
                self.user,
            ),
            2,
        )
        self.assertEqual(len(self.gateway.sent), 1)

    async def test_invalid_inventory_cursor_restarts_idempotently(self):
        self.gateway.page_size = 1
        self.gateway.add("first")
        self.gateway.add("second")
        await self.cycle()
        original = self.gateway.list_sessions_page

        async def invalid(cursor="", limit=100):
            if cursor:
                raise SessionError("expired cursor", grpc_status=3)
            return await original(cursor, limit)

        with patch.object(self.gateway, "list_sessions_page", new=invalid):
            await self.cycle()
        async with self.pool.acquire() as conn:
            checkpoint = await store.load_checkpoint(
                conn, self.service.gateway, self.user
            )
        self.assertIsNone(checkpoint.inventory_cursor)
        await self.pool.execute(
            "UPDATE native_hitl_inventory_state SET next_sweep='epoch' WHERE owner_id=$1",
            self.user,
        )
        await self.cycle(3)
        self.assertEqual(len(await self.projections()), 2)

    async def test_ambiguous_bound_identity_denies_contents(self):
        first, _ = await self.bound_session()
        second, _ = await self.bound_session()
        session, _ = self.gateway.add("ambiguous")
        await native_sessions.ledger.update_binding(first, kagent_session_id=session.id)
        await native_sessions.ledger.update_binding(
            second, kagent_session_id=session.id
        )
        await self.cycle()
        self.assertEqual(self.gateway.reads, [])
        self.assertEqual(await self.projections(), [])

    async def test_legacy_storage_mutation_cannot_dismiss_hitl(self):
        from mainloop.db import db

        self.gateway.add("generic")
        await self.cycle()
        projection = (await self.projections())[0]
        with self.assertRaises(ValueError):
            await db.update_queue_item(
                f"hitl-{projection.id}", status="cancelled", response="approved"
            )
        self.assertEqual(
            (await db.get_queue_item(f"hitl-{projection.id}")).status, "pending"
        )

    async def record_without_dispatch(self, count):
        from unittest.mock import AsyncMock

        for index in range(count):
            self.gateway.add(f"recovery-{index}")
        await self.cycle(count)
        with patch.object(continuation, "dispatch", new=AsyncMock()):
            for index, projection in enumerate(await self.projections()):
                await continuation.submit(
                    self.user,
                    projection.id,
                    f"recovery-{index}",
                    self.response(),
                    service=self.service,
                )

    async def test_slow_response_recovery_is_bounded_and_rotates_after_restart(self):
        from unittest.mock import AsyncMock

        await self.record_without_dispatch(3)
        original = self.gateway.get_session
        attempted = []

        async def slow(sid):
            attempted.append(sid)
            await asyncio.sleep(1.1)
            raise Unreachable("gateway stalled before sending")

        list_open = AsyncMock(return_value=["ordinary-session"])
        sync = AsyncMock()
        budget = continuation.RESPONSE_RECOVERY_BUDGET_SECONDS
        started = asyncio.get_running_loop().time()
        with (
            patch.object(self.gateway, "get_session", new=slow),
            patch("mainloop.runtime.hitl_observer.observe_hitl_once", new=AsyncMock()),
            patch.object(continuation, "observer", return_value=self.service),
            patch.object(
                native_sessions.ledger, "sessions_with_open_work", new=list_open
            ),
            patch.object(native_sessions, "sync", new=sync),
        ):
            await asyncio.wait_for(
                native_sessions.reconcile_once(sweep=False), budget + 0.75
            )
        self.assertLess(asyncio.get_running_loop().time() - started, budget + 0.75)
        list_open.assert_awaited_once()
        sync.assert_awaited_once_with("ordinary-session")
        self.assertTrue(attempted)
        self.assertLess(len(attempted), 3)
        self.assertEqual(self.gateway.sent, [])
        states = await self.pool.fetch(
            "SELECT state FROM native_hitl_response_transport WHERE owner_id=$1",
            self.user,
        )
        self.assertEqual([row["state"] for row in states], ["recorded"] * 3)
        self.service = HITLObserver(
            self.gateway,
            gateway=self.service.gateway,
            owner=self.user,
            creator="gateway-owner",
        )
        recovered = []

        async def healthy(sid):
            recovered.append(sid)
            return await original(sid)

        with patch.object(self.gateway, "get_session", new=healthy), patch.object(
            continuation, "observer", return_value=self.service
        ):
            await continuation.reconcile_hitl_responses()
        self.assertNotIn(recovered[0], attempted)
        self.assertEqual(len(self.gateway.sent), 3)
        states = await self.pool.fetch(
            "SELECT state FROM native_hitl_response_transport WHERE owner_id=$1",
            self.user,
        )
        self.assertEqual([row["state"] for row in states], ["accepted"] * 3)

    async def test_recovery_budget_during_send_preserves_uncertainty_without_replay(
        self,
    ):
        from unittest.mock import AsyncMock

        await self.record_without_dispatch(1)
        sends = []

        async def hung_send(agent, **kwargs):
            sends.append(kwargs)
            await asyncio.Event().wait()
            yield StreamEvent()

        list_open = AsyncMock(return_value=[])
        with (
            patch.object(self.gateway, "send_hitl_response", new=hung_send),
            patch("mainloop.runtime.hitl_observer.observe_hitl_once", new=AsyncMock()),
            patch.object(continuation, "observer", return_value=self.service),
            patch.object(
                native_sessions.ledger, "sessions_with_open_work", new=list_open
            ),
        ):
            await asyncio.wait_for(
                native_sessions.reconcile_once(sweep=False),
                continuation.RESPONSE_RECOVERY_BUDGET_SECONDS + 0.75,
            )
        list_open.assert_awaited_once()
        self.assertEqual(len(sends), 1)
        state = await self.pool.fetchval(
            "SELECT state FROM native_hitl_response_transport WHERE owner_id=$1",
            self.user,
        )
        self.assertEqual(state, "sending")
        with patch.object(continuation, "observer", return_value=self.service):
            await continuation.reconcile_hitl_responses()
            self.assertEqual(
                await self.pool.fetchval(
                    "SELECT state FROM native_hitl_response_transport WHERE owner_id=$1",
                    self.user,
                ),
                "uncertain",
            )
            self.assertEqual(self.gateway.sent, [])
            sent = sends[0]
            task = self.gateway.tasks[sent["task_id"]]
            task.history.append(
                Message(
                    message_id=sent["message_id"],
                    task_id=task.id,
                    context_id=task.context_id,
                    metadata={
                        HITL_EXTENSION: sent["response"].model_dump(
                            mode="json", exclude_none=True
                        )
                    },
                )
            )
            await continuation.reconcile_hitl_responses()
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT state FROM native_hitl_response_transport WHERE owner_id=$1",
                self.user,
            ),
            "accepted",
        )
        self.assertEqual(self.gateway.sent, [])

    async def test_status_message_refresh_retires_stale_controls_and_survives_restart(
        self,
    ):
        _, task = self.gateway.add("status-refresh")
        await self.cycle()
        old = (await self.projections())[0]
        task.status.message.message_id = "new-status-same-operation"
        await self.cycle()
        self.service = HITLObserver(
            self.gateway,
            gateway=self.service.gateway,
            owner=self.user,
            creator="gateway-owner",
        )
        # Temporary failure must not restore historical aliases or cards.
        self.gateway.failure = Unreachable("temporary")
        await self.cycle()
        self.gateway.failure = None
        await self.cycle(2)
        projections = await self.projections()
        current = next(
            p
            for p in projections
            if p.outer.status_message_id == task.status.message.message_id
        )
        retired = next(p for p in projections if p.id == old.id)
        self.assertEqual(current.leaves, old.leaves)
        async with self.pool.acquire() as conn:
            self.assertFalse((await continuation.view(conn, retired))["answerable"])
            self.assertTrue((await continuation.view(conn, current))["answerable"])
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM native_hitl_aliases WHERE request_id=$1", old.id
            ),
            0,
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT status FROM queue_items WHERE hitl_request_id=$1", old.id
            ),
            "expired",
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT count(*) FROM queue_items WHERE user_id=$1 AND status='pending'",
                self.user,
            ),
            1,
        )
        with self.assertRaisesRegex(ValueError, "superseded"):
            await continuation.submit(
                self.user,
                old.id,
                "stale-control",
                self.response(),
                service=self.service,
            )
        result = await continuation.submit(
            self.user,
            current.id,
            "current-control",
            self.response(),
            service=self.service,
        )
        self.assertEqual(result["transport_state"], "accepted")
        self.assertEqual(len(self.gateway.sent), 1)
        self.assertEqual(
            result["response"]["outer"]["status_message_id"],
            "new-status-same-operation",
        )

    async def test_refreshed_parent_route_keeps_real_unavailable_parent_hold(self):
        parent_session, parent_task, _, child_task = await self.nested(verified=True)
        parent_task.status.message.message_id = "refreshed-parent-status"
        await self.cycle(2)
        projections = await self.projections()
        current = next(
            p
            for p in projections
            if p.outer.status_message_id == "refreshed-parent-status"
        )
        child = next(p for p in projections if p.outer.task_id == child_task.id)
        async with self.pool.acquire() as conn:
            self.assertTrue((await continuation.view(conn, current))["answerable"])
            self.assertEqual(
                (await continuation.view(conn, child))["route_request_id"], current.id
            )
        del self.gateway.sessions[parent_session.id]
        await self.cycle(2)
        child = next(p for p in await self.projections() if p.id == child.id)
        async with self.pool.acquire() as conn:
            self.assertFalse((await continuation.view(conn, child))["answerable"])
        with self.assertRaises(ValueError):
            await continuation.submit(
                self.user,
                child.id,
                "no-fallback",
                self.response(),
                service=self.service,
            )
        self.assertEqual(self.gateway.sent, [])

    async def test_status_refresh_retains_recorded_receipt_and_original_destination(
        self,
    ):
        await self.record_without_dispatch(1)
        before = await self.pool.fetchval(
            "SELECT snapshot FROM native_hitl_responses WHERE owner_id=$1", self.user
        )
        task = self.gateway.tasks["task-recovery-0"]
        task.status.message.message_id = "refreshed-after-consent"
        await self.cycle()
        async with self.pool.acquire() as conn:
            current = next(
                p
                for p in await self.projections()
                if p.outer.status_message_id == "refreshed-after-consent"
            )
            view = await continuation.view(conn, current)
            self.assertFalse(view["answerable"])
            self.assertEqual(view["transport_state"], "recorded")
            await conn.execute(
                "DELETE FROM native_hitl_requests WHERE owner_id=$1", self.user
            )
        self.service = HITLObserver(
            self.gateway,
            gateway=self.service.gateway,
            owner=self.user,
            creator="gateway-owner",
        )
        await self.cycle()
        with patch.object(continuation, "observer", return_value=self.service):
            await continuation.reconcile_hitl_responses()
            await continuation.reconcile_hitl_responses()
        self.assertEqual(len(self.gateway.sent), 1)
        self.assertEqual(self.gateway.sent[0][1]["task_id"], task.id)
        self.assertEqual(self.gateway.sent[0][1]["context_id"], task.context_id)
        self.assertEqual(
            before,
            await self.pool.fetchval(
                "SELECT snapshot FROM native_hitl_responses WHERE owner_id=$1",
                self.user,
            ),
        )
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT state FROM native_hitl_response_transport WHERE owner_id=$1",
                self.user,
            ),
            "accepted",
        )

    async def test_status_refresh_and_rebuild_keep_uncertain_receipt_without_replay(
        self,
    ):
        _, task = self.gateway.add("uncertain-refresh")
        await self.cycle()
        original = (await self.projections())[0]
        self.gateway.send_failure = OutcomeUnknown("send outcome unknown")
        await continuation.submit(
            self.user, original.id, "uncertain", self.response(), service=self.service
        )
        before = await self.pool.fetchval(
            "SELECT snapshot FROM native_hitl_responses WHERE owner_id=$1", self.user
        )
        task.status.message.message_id = "refreshed-uncertain-status"
        await self.cycle()
        current = next(
            p
            for p in await self.projections()
            if p.outer.status_message_id == task.status.message.message_id
        )
        async with self.pool.acquire() as conn:
            view = await continuation.view(conn, current)
            self.assertFalse(view["answerable"])
            self.assertEqual(view["transport_state"], "uncertain")
            await conn.execute(
                "DELETE FROM native_hitl_requests WHERE owner_id=$1", self.user
            )
        self.service = HITLObserver(
            self.gateway,
            gateway=self.service.gateway,
            owner=self.user,
            creator="gateway-owner",
        )
        await self.cycle()
        with patch.object(continuation, "observer", return_value=self.service):
            await continuation.reconcile_hitl_responses()
        rebuilt = (await self.projections())[0]
        async with self.pool.acquire() as conn:
            view = await continuation.view(conn, rebuilt)
        self.assertFalse(view["answerable"])
        self.assertEqual(view["transport_state"], "uncertain")
        self.assertEqual(
            before,
            await self.pool.fetchval(
                "SELECT snapshot FROM native_hitl_responses WHERE owner_id=$1",
                self.user,
            ),
        )
        self.assertEqual(len(self.gateway.sent), 1)
