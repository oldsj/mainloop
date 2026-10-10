"""Owner attention against isolated PostgreSQL and sanitized native-input fixtures."""

from contextlib import asynccontextmanager
from unittest.mock import patch

from mainloop import api
from mainloop.attention import read
from mainloop.db import db
from mainloop.db import tasks as store
from mainloop.runtime.kagent_client import TaskStatus
from mainloop.tasks import reports
from mainloop.tasks.principal import TaskPrincipal
from tests.runtime import test_postgres_task_attention as task_setup
from tests.runtime.test_merge import MergeFixture

from models import QueueItem, QueueItemType, SessionStatus
from models.task import TaskReport


class OwnerAttentionTests(MergeFixture):
    enroll = task_setup.TaskAttentionTests.enroll

    async def asyncSetUp(self):
        await super().asyncSetUp()
        await self.pool.execute(
            "TRUNCATE tasks,task_attempts,workspace_writer_claims,task_events,task_operations CASCADE"
        )
        self.task, self.attempt = await self.enroll(self.sid, "feature")

    async def observe_input(self, payload=None):
        runtime = f"runtime-{self.sid}"
        session, native_task = self.gateway.add(runtime, payload)
        async with self.pool.acquire() as conn:
            observed = await self.hitl_service.owned(conn, session)
            async with conn.transaction():
                from mainloop.db.hitl import observe_session

                await observe_session(conn, observed)
            await self.hitl_service.refresh(conn, runtime, native_task.id)
        return await read(db, self.user)

    async def set_task(self, **changes):
        async with self.pool.acquire() as conn:
            task = await store.get_task(conn, self.task.id, TaskPrincipal(self.user))
            task = task.model_copy(update=changes)
            await conn.execute(
                "UPDATE tasks SET snapshot=$2::jsonb,status=$3,current_attempt_id=$4 WHERE id=$1",
                task.id,
                task.model_dump_json(),
                task.status,
                task.current_attempt_id,
            )

    async def test_fyi_is_activity_and_never_badge_attention(self):
        item = QueueItem(
            main_thread_id=await self.thread(),
            user_id=self.user,
            item_type=QueueItemType.NOTIFICATION,
            title="Pull request merged",
            content="Merged PR 17",
            task_id=self.task.id,
        )
        await db.create_queue_item(item)
        projection = await read(db, self.user)
        self.assertEqual(projection.attention.count, 0)
        self.assertEqual(await db.count_unread_queue_items(self.user), 0)
        self.assertIn(item.title, [item.title for item in projection.activity])
        self.assertEqual(projection.sessions[self.sid].status, SessionStatus.IDLE)

    async def test_live_current_input_counts_once_and_links_to_canonical_card(self):
        projection = await self.observe_input()
        self.assertEqual(projection.attention.count, 1)
        item = projection.attention.items[0]
        self.assertEqual(item.session_ids, [self.sid])
        self.assertEqual(item.kind, "approval")
        self.assertIn(item.queue_item.hitl_request_id, item.href)
        self.assertTrue(projection.sessions[self.sid].needs_owner)
        self.assertEqual(projection.attention.sessions, projection.listed_sessions)
        self.assertEqual(
            projection.sessions[self.sid].status, SessionStatus.WAITING_ON_USER
        )
        # Looking at a request cannot hide unfinished work from the owner badge.
        await db.mark_queue_item_read(item.queue_item.id)
        self.assertEqual(await db.count_unread_queue_items(self.user), 1)
        self.assertEqual((await api.owner_attention(self.user)).count, 1)
        self.assertEqual(
            (await api.get_session(self.sid, self.user)).status,
            SessionStatus.WAITING_ON_USER,
        )

    async def test_question_is_actionable_but_idle_session_is_not(self):
        projection = await self.observe_input(
            {
                "type": "ask_user_request",
                "id": "q",
                "questions": [
                    {"question": "Which branch?", "choices": None, "multiple": False}
                ],
            }
        )
        self.assertEqual(projection.attention.items[0].kind, "question")

    async def test_recorded_decision_does_not_prompt_again_while_transport_is_uncertain(
        self,
    ):
        from mainloop.runtime.hitl_continuation import record_response

        from models.hitl import ToolApproval, ToolApprovalResponse

        projection = await self.observe_input()
        item = projection.attention.items[0]
        response = ToolApprovalResponse(
            type="tool_approval_response",
            approvals=(ToolApproval(id="call", approved=True),),
        )
        await record_response(
            self.user,
            item.queue_item.hitl_request_id,
            "owner-action",
            response,
            service=self.hitl_service,
        )
        await self.pool.execute(
            "UPDATE native_hitl_response_transport SET state='uncertain' WHERE owner_id=$1",
            self.user,
        )
        self.assertEqual((await read(db, self.user)).attention.count, 0)
        self.assertEqual(await db.count_unread_queue_items(self.user), 0)

    async def test_rollup_aliases_count_one_canonical_leaf(self):
        await self.observe_input()
        row = await self.pool.fetchrow(
            "SELECT * FROM native_hitl_requests WHERE owner_id=$1", self.user
        )
        snapshot = store.decode(row["snapshot"])
        snapshot["id"] = "alias-request"
        import json

        await self.pool.execute(
            "INSERT INTO native_hitl_requests(id,owner_id,outer_key,snapshot) VALUES($1,$2,$3,$4::jsonb)",
            "alias-request",
            self.user,
            "alias-outer",
            json.dumps(snapshot),
        )
        await db.create_queue_item(
            QueueItem(
                main_thread_id=await self.thread(),
                user_id=self.user,
                item_type=QueueItemType.HITL_REQUEST,
                hitl_request_id="alias-request",
                title="Parent input rollup",
                content="Same canonical leaf",
            )
        )
        self.assertEqual((await read(db, self.user)).attention.count, 1)

    async def test_terminal_task_overrides_stale_session_input_flag(self):
        await self.observe_input()
        for status in ("completed", "failed", "cancelled"):
            with self.subTest(status=status):
                await self.set_task(status=status)
                projection = await read(db, self.user)
                self.assertEqual(projection.attention.count, 0)
                self.assertFalse(projection.sessions[self.sid].needs_owner)
                self.assertEqual(projection.sessions[self.sid].status.value, status)
                self.assertEqual(
                    (await api.get_session(self.sid, self.user)).status.value, status
                )

    async def test_superseded_attempt_and_lost_current_pointer_never_count(self):
        await self.observe_input()
        await self.pool.execute(
            "UPDATE task_attempts SET state='superseded' WHERE id=$1", self.attempt.id
        )
        projection = await read(db, self.user)
        self.assertEqual(projection.attention.count, 0)
        self.assertEqual(projection.sessions[self.sid].status, SessionStatus.SUPERSEDED)
        await self.set_task(current_attempt_id=None)
        self.assertEqual((await read(db, self.user)).attention.count, 0)

    async def test_archived_session_or_attempt_and_deleted_native_are_excluded(self):
        await self.observe_input()
        await self.pool.execute(
            "UPDATE sessions SET archived_at=NOW() WHERE id=$1", self.sid
        )
        self.assertEqual((await read(db, self.user)).attention.count, 0)
        self.assertEqual(await api.list_sessions(self.user), [])
        self.assertEqual(
            (await api.get_session(self.sid, self.user)).status, SessionStatus.IDLE
        )
        await self.pool.execute(
            "UPDATE sessions SET archived_at=NULL WHERE id=$1", self.sid
        )
        await self.pool.execute(
            "UPDATE task_attempts SET snapshot=jsonb_set(snapshot,'{archived_at}',to_jsonb(NOW())) WHERE id=$1",
            self.attempt.id,
        )
        self.assertEqual((await read(db, self.user)).attention.count, 0)
        await self.pool.execute(
            "UPDATE task_attempts SET snapshot=snapshot-'archived_at' WHERE id=$1",
            self.attempt.id,
        )
        await self.pool.execute(
            "UPDATE native_bindings SET kagent_deleted_at=NOW() WHERE session_id=$1",
            self.sid,
        )
        self.assertEqual((await read(db, self.user)).attention.count, 0)

    async def test_replaced_runtime_cannot_reuse_old_input(self):
        await self.observe_input()
        await self.pool.execute(
            "UPDATE native_bindings SET kagent_session_id='replacement' WHERE session_id=$1",
            self.sid,
        )
        self.assertEqual((await read(db, self.user)).attention.count, 0)

    async def test_superseded_request_and_native_task_no_longer_waiting(self):
        await self.observe_input()
        await self.pool.execute(
            "UPDATE native_hitl_requests SET superseded=true WHERE owner_id=$1",
            self.user,
        )
        self.assertEqual((await read(db, self.user)).attention.count, 0)
        await self.pool.execute(
            "UPDATE native_hitl_requests SET superseded=false WHERE owner_id=$1",
            self.user,
        )
        runtime = f"runtime-{self.sid}"
        native_task = self.gateway.tasks[f"task-{runtime}"]
        self.gateway.tasks[native_task.id] = native_task.model_copy(
            update={"status": TaskStatus(state="completed")}
        )
        async with self.pool.acquire() as conn:
            await self.hitl_service.refresh(conn, runtime, native_task.id)
        self.assertEqual((await read(db, self.user)).attention.count, 0)

    async def test_owner_decision_is_explicit_and_current_attempt_only(self):
        from mainloop.runtime.delegation import PgStore

        binding = await PgStore().get_binding(self.sid)
        principal = await PgStore().task_principal(binding)
        request = TaskReport(
            task_id=self.task.id,
            attempt_id=self.attempt.id,
            request_id="decision",
            summary="Two approaches need a choice",
            outcome="blocked",
            owner_action="Choose whether to keep the existing public API.",
        )
        async with self.pool.acquire() as conn, conn.transaction():
            await reports.record(conn, principal, request)
        projection = await read(db, self.user)
        self.assertEqual(projection.attention.count, 1)
        self.assertEqual(projection.attention.items[0].needed, request.owner_action)
        self.assertEqual(projection.attention.items[0].href, f"/tasks/{self.task.id}")
        async with self.pool.acquire() as conn, conn.transaction():
            await reports.record(
                conn,
                principal,
                request.model_copy(
                    update={
                        "request_id": "resumed",
                        "outcome": "progress",
                        "owner_action": None,
                        "summary": "The owner answered; work resumed",
                    }
                ),
            )
        self.assertEqual((await read(db, self.user)).attention.count, 0)
        await self.set_task(reason="ci", status="waiting")
        self.assertEqual((await read(db, self.user)).attention.count, 0)

    async def test_owner_scope_and_main_thread_filter(self):
        await self.observe_input()
        other = await read(db, "other-owner")
        self.assertEqual(other.attention.count, 0)
        self.assertEqual(other.sessions, {})
        await self.pool.execute(
            "UPDATE native_bindings SET role='main' WHERE session_id=$1", self.sid
        )
        self.assertEqual(await api.list_sessions(self.user), [])
        self.assertEqual((await api.owner_attention(self.user)).count, 1)

    async def owner_blocker(self):
        from mainloop.runtime.delegation import PgStore

        binding = await PgStore().get_binding(self.sid)
        principal = await PgStore().task_principal(binding)
        request = TaskReport(
            task_id=self.task.id,
            attempt_id=self.attempt.id,
            request_id="owner-blocker",
            outcome="blocked",
            summary="The API decision remains open.",
            owner_action="Should the existing public API be retained?",
        )
        async with self.pool.acquire() as conn, conn.transaction():
            await reports.record(conn, principal, request)
        return principal, request

    async def test_owner_blocker_survives_unrelated_native_question(self):
        """Reviewer probe: settling a different native question cannot settle a report."""
        from mainloop.runtime.hitl_continuation import record_response

        from models.hitl import AskUserAnswer, AskUserResponse

        principal, request = await self.owner_blocker()
        self.assertEqual((await read(db, self.user)).attention.count, 1)
        during = await self.observe_input(
            {
                "type": "ask_user_request",
                "id": "independent-question",
                "questions": [
                    {
                        "question": "Which test suite?",
                        "choices": None,
                        "multiple": False,
                    }
                ],
            }
        )
        self.assertEqual(during.attention.count, 2)
        self.assertEqual(
            {item.kind for item in during.attention.items},
            {"question", "owner_decision"},
        )
        native_item = next(
            item for item in during.attention.items if item.kind == "question"
        )
        await record_response(
            self.user,
            native_item.queue_item.hitl_request_id,
            "answer-only-test-suite",
            AskUserResponse(
                type="ask_user_response",
                id="independent-question",
                answers=(AskUserAnswer(answer=("Focused runtime tests",)),),
            ),
            service=self.hitl_service,
        )
        after = await read(db, self.user)
        self.assertEqual(
            [item.kind for item in after.attention.items], ["owner_decision"]
        )
        self.assertEqual(after.attention.items[0].needed, request.owner_action)
        self.assertTrue(after.sessions[self.sid].needs_owner)
        # A different diagnostic report also cannot implicitly answer the owner.
        async with self.pool.acquire() as conn, conn.transaction():
            await reports.record(
                conn,
                principal,
                request.model_copy(
                    update={
                        "request_id": "diagnostic",
                        "outcome": "failed",
                        "owner_action": None,
                    }
                ),
            )
        self.assertEqual((await read(db, self.user)).attention.count, 1)
        async with self.pool.acquire() as conn, conn.transaction():
            await reports.record(
                conn,
                principal,
                request.model_copy(
                    update={
                        "request_id": "resumed",
                        "outcome": "progress",
                        "owner_action": None,
                    }
                ),
            )
        self.assertEqual((await read(db, self.user)).attention.count, 0)
        async with self.pool.acquire() as conn:
            task = await store.get_task(conn, self.task.id, principal)
        self.assertIsNone(task.owner_decision)

    async def test_owner_blocker_clears_on_completion_or_attempt_replacement(self):
        principal, request = await self.owner_blocker()
        async with self.pool.acquire() as conn, conn.transaction():
            await reports.record(
                conn,
                principal,
                request.model_copy(
                    update={
                        "request_id": "done",
                        "outcome": "completed",
                        "owner_action": None,
                    }
                ),
            )
        self.assertEqual((await read(db, self.user)).attention.count, 0)
        async with self.pool.acquire() as conn, conn.transaction():
            await reports.record(
                conn,
                principal,
                request.model_copy(update={"request_id": "next-decision"}),
            )
            task = await store.get_task(conn, self.task.id, principal)
            await store.save_task(
                conn,
                task.model_copy(
                    update={
                        "current_attempt_id": None,
                        "version": task.version + 1,
                    }
                ),
                task.version,
                "replace-attempt",
            )
            self.assertIsNone(
                (
                    await store.get_task(conn, task.id, TaskPrincipal(self.user))
                ).owner_decision
            )
        self.assertEqual((await read(db, self.user)).attention.count, 0)

    async def test_resumed_report_clears_only_owner_decision_not_native_input(self):
        principal, request = await self.owner_blocker()
        await self.observe_input()
        async with self.pool.acquire() as conn, conn.transaction():
            await reports.record(
                conn,
                principal,
                request.model_copy(
                    update={
                        "request_id": "resumed-with-native-input",
                        "outcome": "progress",
                        "owner_action": None,
                    }
                ),
            )
            self.assertIsNone(
                (await store.get_task(conn, self.task.id, principal)).owner_decision
            )
        remaining = await read(db, self.user)
        self.assertEqual(
            [item.kind for item in remaining.attention.items], ["approval"]
        )
        self.assertTrue(remaining.sessions[self.sid].needs_owner)

    async def traced_read(self, **options):
        reads = []
        connection = db.connection

        class Trace:
            def __init__(self, conn):
                self.conn = conn

            def __getattr__(self, name):
                return getattr(self.conn, name)

            async def fetch(self, query, *args):
                rows = await self.conn.fetch(query, *args)
                reads.append({"query": " ".join(query.split()), "rows": len(rows)})
                return rows

        @asynccontextmanager
        async def traced_connection():
            async with connection() as conn:
                yield Trace(conn)

        with patch.object(db, "connection", traced_connection):
            result = await read(db, self.user, **options)
        return result, reads

    async def test_task_finality_clears_durable_owner_decision(self):
        await self.owner_blocker()
        principal = TaskPrincipal(self.user)
        async with self.pool.acquire() as conn, conn.transaction():
            task = await store.get_task(conn, self.task.id, principal)
            await store.save_task(
                conn,
                task.model_copy(
                    update={
                        "status": "cancelled",
                        "version": task.version + 1,
                    }
                ),
                task.version,
                "terminal-task",
            )
            self.assertIsNone(
                (await store.get_task(conn, task.id, principal)).owner_decision
            )
        self.assertEqual((await read(db, self.user)).attention.count, 0)

    async def test_history_is_bounded_before_loading_and_attention_retains_old_live_session(
        self,
    ):
        """Reviewer history probe, extended with old sessions and observed runtimes."""
        await self.owner_blocker()
        await self.observe_input()
        report = TaskReport(
            task_id=self.task.id,
            attempt_id=self.attempt.id,
            request_id="history",
            outcome="progress",
            summary="Routine measurement",
        )
        await self.pool.execute(
            """INSERT INTO task_reports(id,task_id,attempt_id,request_id,request_digest,snapshot)
               SELECT 'history-' || n,$1,$2,'history-' || n,'fixture',
                      jsonb_set($3::jsonb,'{request_id}',to_jsonb('history-' || n))
               FROM generate_series(1,1001) n""",
            self.task.id,
            self.attempt.id,
            report.model_dump_json(),
        )
        thread_id = await self.thread()
        await self.pool.execute(
            """INSERT INTO queue_items(id,main_thread_id,user_id,item_type,title,content)
               SELECT 'fyi-' || n,$1,$2,'notification','Routine result','No decision'
               FROM generate_series(1,1001) n""",
            thread_id,
            self.user,
        )
        await self.pool.execute(
            """INSERT INTO sessions(id,user_id,main_thread_id,title,description,prompt,conversation_id,status)
               SELECT 'finished-' || n,user_id,main_thread_id,'Finished','History','Fixture',conversation_id,'completed'
               FROM sessions CROSS JOIN generate_series(1,1001) n WHERE id=$1""",
            self.sid,
        )
        await self.pool.execute(
            """INSERT INTO native_observed_sessions(gateway,runtime_session_id,owner_id,snapshot)
               SELECT 'fixture','old-' || n,$1,'{}'::jsonb FROM generate_series(1,1001) n""",
            self.user,
        )
        # Exercise the indexed observed-session fallback, not only pinned binding IDs.
        await self.pool.execute(
            """UPDATE native_hitl_requests SET snapshot=jsonb_set(snapshot,'{leaves,0,binding_id}','null'::jsonb)
               WHERE owner_id=$1""",
            self.user,
        )
        result, reads = await self.traced_read()
        self.assertEqual(len(result.activity), 100)
        self.assertEqual(result.attention.count, 2)
        self.assertTrue(result.sessions[self.sid].needs_owner)
        history_read = next(
            value for value in reads if "FROM task_reports" in value["query"]
        )
        self.assertEqual(history_read["rows"], 100)
        self.assertTrue(all(value["rows"] <= 101 for value in reads), reads)
        self.assertEqual(
            len(reads), 5
        )  # Fixed batched reads, independent of history size.
        poll, poll_reads = await self.traced_read(include_activity=False)
        self.assertEqual(poll.attention.count, 2)
        self.assertEqual(poll.activity, [])
        self.assertFalse(
            any("FROM task_reports" in value["query"] for value in poll_reads)
        )
        await self.pool.execute(
            "UPDATE native_hitl_requests SET superseded=true WHERE owner_id=$1",
            self.user,
        )
        owner_only = await read(db, self.user, include_activity=False)
        self.assertEqual(
            [item.kind for item in owner_only.attention.items], ["owner_decision"]
        )
        self.assertTrue(owner_only.sessions[self.sid].needs_owner)
        self.assertEqual(
            (await api.get_session("finished-1", self.user)).status,
            SessionStatus.COMPLETED,
        )
        indexes = await self.pool.fetch(
            "SELECT indexname FROM pg_indexes WHERE schemaname=current_schema()"
        )
        self.assertTrue(
            {
                "task_report_history",
                "task_report_recent",
                "native_observed_owner",
                "task_attempt_session",
                "queue_owner_history",
                "session_owner_history",
            }
            <= {row["indexname"] for row in indexes}
        )

    async def test_history_cap_never_truncates_pending_requests_or_owner_blocker(self):
        await self.owner_blocker()
        await self.pool.execute(
            """INSERT INTO queue_items(id,main_thread_id,user_id,item_type,title,content,task_id)
               SELECT 'question-' || n,$1,$2,'question','Owner choice','Which approach?',$3
               FROM generate_series(1,101) n""",
            await self.thread(),
            self.user,
            self.task.id,
        )
        result = await read(db, self.user, include_activity=False)
        self.assertEqual(result.attention.count, 102)
        self.assertEqual(len(result.attention.items), 102)
