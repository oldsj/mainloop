"""kagent client against a fake gateway (fixture-backed; no network, no live kagent)."""

import unittest

import httpx
from mainloop.runtime.kagent_client import (
    A2AError,
    AgentRef,
    KagentClient,
    OutcomeUnknown,
    RuntimeOperation,
    RuntimeState,
    SendNotAccepted,
    SessionError,
    StreamEvent,
    TaskNotFound,
    TaskProjection,
    Unreachable,
    _field_bytes,
    _field_str,
    assistant_message_id,
    decode_agent_response,
    decode_fields,
    is_parked,
    is_terminal,
    normalise_state,
    parse_grpc_web,
)
from tests.runtime.kagent_fake import (
    CONTEXT_ID,
    TASK_ID,
    FakeKagent,
    grpc_response,
    stream_chunks,
)

AGENT = AgentRef("kagent", "claude-subscription")


def make_client(fake: FakeKagent, *, clock=None) -> tuple[KagentClient, list[float]]:
    """Build a client whose sleeps are recorded and, by default, advance a fake clock."""
    sleeps: list[float] = []
    now = [0.0]

    async def sleep(seconds: float) -> None:
        sleeps.append(seconds)
        now[0] += seconds

    http = httpx.AsyncClient(transport=fake.transport(), base_url="http://kagent.test")
    return (
        KagentClient(
            "http://kagent.test",
            user_id="mainloop",
            client=http,
            sleep=sleep,
            clock=clock or (lambda: now[0]),
        ),
        sleeps,
    )


async def collect(stream) -> list[StreamEvent]:
    return [event async for event in stream]


async def send(client: KagentClient, message_id: str = "m-1") -> list[StreamEvent]:
    return await collect(
        client.send_message(
            AGENT, text="hello", message_id=message_id, context_id=CONTEXT_ID
        )
    )


class StateHelperTests(unittest.TestCase):
    def test_state_names_normalise(self):
        self.assertEqual(normalise_state("TASK_STATE_INPUT_REQUIRED"), "input_required")
        self.assertEqual(normalise_state("input-required"), "input_required")
        self.assertEqual(normalise_state(""), "unspecified")

    def test_terminal_and_parked_are_distinct(self):
        self.assertTrue(is_terminal("TASK_STATE_COMPLETED"))
        self.assertTrue(is_terminal("TASK_STATE_CANCELED"))
        self.assertFalse(is_terminal("TASK_STATE_WORKING"))
        self.assertTrue(is_parked("TASK_STATE_INPUT_REQUIRED"))
        self.assertFalse(is_terminal("TASK_STATE_INPUT_REQUIRED"))

    def test_reply_ids_are_deterministic(self):
        self.assertEqual(assistant_message_id("s", "t"), assistant_message_id("s", "t"))
        self.assertNotEqual(
            assistant_message_id("s", "t"), assistant_message_id("s", "u")
        )


class ProjectionTests(unittest.IsolatedAsyncioTestCase):
    async def test_stream_builds_reply_and_terminal_state(self):
        fake = FakeKagent()
        client, _ = make_client(fake)
        proj = TaskProjection()
        for event in await send(client):
            proj.apply(event)
        self.assertEqual(proj.task_id, TASK_ID)
        self.assertTrue(proj.terminal)
        self.assertEqual(proj.text, "ok")
        self.assertEqual(proj.history_message_ids, ["m-1"])

    async def test_replace_supersedes_accumulated_state(self):
        fake = FakeKagent()
        client, _ = make_client(fake)
        events = await send(client)
        proj = TaskProjection()
        for event in events[:3]:
            proj.apply(event)
        self.assertFalse(proj.terminal)
        stale = events[2].artifact_update.artifact
        stale.parts[0].text = "stale"
        proj.artifacts["junk"] = stale
        proj.replace(events[3].task)
        self.assertEqual(proj.text, "ok")
        self.assertNotIn("junk", proj.artifacts)

    async def test_append_extends_artifact(self):
        proj = TaskProjection()
        first = StreamEvent.model_validate(
            {
                "artifactUpdate": {
                    "taskId": "t",
                    "artifact": {"artifactId": "a", "parts": [{"text": "he"}]},
                }
            }
        )
        more = StreamEvent.model_validate(
            {
                "artifactUpdate": {
                    "taskId": "t",
                    "append": True,
                    "artifact": {"artifactId": "a", "parts": [{"text": "llo"}]},
                }
            }
        )
        proj.apply(first)
        proj.apply(more)
        self.assertEqual(proj.text, "hello")

    async def test_input_required_is_parked_not_terminal(self):
        proj = TaskProjection()
        proj.apply(
            StreamEvent.model_validate(
                {
                    "statusUpdate": {
                        "taskId": "t",
                        "status": {"state": "TASK_STATE_INPUT_REQUIRED"},
                    }
                }
            )
        )
        self.assertTrue(proj.parked)
        self.assertFalse(proj.terminal)


class SessionServiceTests(unittest.IsolatedAsyncioTestCase):
    async def test_create_is_idempotent_by_request_id(self):
        fake = FakeKagent()
        client, _ = make_client(fake)
        first = await client.create_session(AGENT, request_id="req-1", name="n")
        second = await client.create_session(AGENT, request_id="req-1", name="n")
        self.assertEqual(first.id, second.id)
        self.assertEqual(first.context_id, first.id)
        self.assertEqual(first.state, RuntimeState.READY)
        fields = decode_fields(fake.session_calls("CreateSession")[0])
        self.assertEqual(fields[3][0], b"req-1")
        agent = decode_fields(fields[5][0])
        self.assertEqual(
            (agent[1][0], agent[2][0]), (b"kagent", b"claude-subscription")
        )

    async def test_identity_header_is_sent(self):
        seen: list[str] = []

        def handler(request: httpx.Request) -> httpx.Response:
            seen.append(request.headers["x-user-id"])
            return httpx.Response(
                200, json={"jsonrpc": "2.0", "id": "x", "result": {"tasks": []}}
            )

        http = httpx.AsyncClient(
            transport=httpx.MockTransport(handler), base_url="http://k.test"
        )
        client = KagentClient("http://k.test", user_id="mainloop", client=http)
        await client.list_tasks(AGENT, CONTEXT_ID)
        self.assertEqual(seen, ["mainloop"])

    async def test_suspend_then_resume(self):
        fake = FakeKagent()
        client, _ = make_client(fake)
        session = await client.create_session(AGENT, request_id="r")
        suspended = await client.suspend_session(session.id)
        self.assertEqual(suspended.state, RuntimeState.SUSPENDED)
        resumed = await client.ensure_ready(suspended)
        self.assertEqual(resumed.state, RuntimeState.READY)
        self.assertEqual(len(fake.session_calls("ResumeSession")), 1)

    async def test_ready_session_is_not_resumed(self):
        fake = FakeKagent()
        client, _ = make_client(fake)
        session = await client.create_session(AGENT, request_id="r")
        await client.ensure_ready(session)
        self.assertEqual(fake.session_calls("ResumeSession"), [])

    async def test_waits_for_busy_session(self):
        fake = FakeKagent()
        client, sleeps = make_client(fake)
        session = await client.create_session(AGENT, request_id="r")
        busy = type(session)(
            id=session.id,
            state=RuntimeState.CREATING,
            operation=RuntimeOperation.CREATE,
            context_id=session.context_id,
        )
        ready = await client.ensure_ready(busy, interval=0.5)
        self.assertEqual(ready.state, RuntimeState.READY)
        self.assertEqual(sleeps, [0.5])

    async def test_failed_session_raises(self):
        fake = FakeKagent()
        client, _ = make_client(fake)
        session = await client.create_session(AGENT, request_id="r")
        failed = type(session)(
            id=session.id,
            state=RuntimeState.FAILED,
            operation=RuntimeOperation.NONE,
            context_id=session.context_id,
            failure_reason="Boom",
        )
        with self.assertRaises(SessionError):
            await client.ensure_ready(failed)

    async def test_grpc_error_status(self):
        fake = FakeKagent()
        client, _ = make_client(fake)
        with self.assertRaises(SessionError) as ctx:
            await client.get_session("00000000-0000-4000-8000-0000000000ff")
        self.assertEqual(ctx.exception.grpc_status, 5)

    async def test_get_agent_reads_named_template_ref(self):
        def field(number, value):
            return _field_bytes(number, value)

        def text(number, value):
            return _field_str(number, value)

        def struct_value(value):
            return field(5, value)

        def struct(items):
            return b"".join(
                field(1, text(1, key) + field(2, value)) for key, value in items
            )

        template_ref = struct([("name", text(3, "claude-workspace"))])
        spec = struct([("templateRef", struct_value(template_ref))])
        resource = struct([("spec", struct_value(spec))])
        ref = text(1, "kagent") + text(2, "claude-subscription")
        structured = (
            text(1, "kagent.dev/v1alpha3") + text(2, "Agent") + field(3, resource)
        )
        agent = field(1, ref) + field(2, structured)

        def handler(request):
            self.assertTrue(
                request.url.path.endswith("AgentService/GetAgent"),
                request.url.path,
            )
            return grpc_response(field(1, agent))

        client = KagentClient(
            "http://k.test",
            user_id="mainloop",
            client=httpx.AsyncClient(
                transport=httpx.MockTransport(handler), base_url="http://k.test"
            ),
        )
        found = await client.get_agent(AGENT)
        self.assertEqual(found.ref, AGENT)
        self.assertEqual(found.template_name, "claude-workspace")
        self.assertFalse(found.inline_template)

    async def test_agent_lookup_rejects_inline_template(self):
        def field(number, value):
            return _field_bytes(number, value)

        def text(number, value):
            return _field_str(number, value)

        def struct_value(value):
            return field(5, value)

        inline = b""
        spec = field(1, text(1, "template") + field(2, struct_value(inline)))
        resource = field(1, text(1, "spec") + field(2, struct_value(spec)))
        ref = text(1, "kagent") + text(2, "claude-subscription")
        structured = (
            text(1, "kagent.dev/v1alpha3") + text(2, "Agent") + field(3, resource)
        )
        response = field(1, field(1, ref) + field(2, structured))
        decoded = decode_agent_response(response)
        self.assertTrue(decoded.inline_template)
        self.assertIsNone(decoded.template_name)

    async def test_ambiguous_session_errors_are_not_definitive_rejections(self):
        from tests.runtime.kagent_fake import grpc_response

        for response in [
            httpx.Response(500),
            *(grpc_response(None, status=status) for status in (4, 10, 13, 14)),
            grpc_response(None),
        ]:
            with self.subTest(status=response.status_code, body=response.content):
                http = httpx.AsyncClient(
                    transport=httpx.MockTransport(
                        lambda request, response=response: response
                    ),
                    base_url="http://k.test",
                )
                client = KagentClient("http://k.test", user_id="fixture", client=http)
                with self.assertRaises(OutcomeUnknown):
                    await client.create_session(AGENT, request_id="fixture-request")
                await http.aclose()

    async def test_grpc_web_frames_round_trip(self):
        from tests.runtime.kagent_fake import grpc_response, session_message

        message = session_message("abc")
        messages, trailers = parse_grpc_web(grpc_response(message).content)
        self.assertEqual(messages, [message])
        self.assertEqual(trailers["grpc-status"], "0")
        with self.assertRaises(ValueError):
            parse_grpc_web(grpc_response(message).content[:-9])


class SendTests(unittest.IsolatedAsyncioTestCase):
    async def test_send_streams_task_events(self):
        fake = FakeKagent()
        client, _ = make_client(fake)
        events = await send(client)
        self.assertEqual([bool(e.task) for e in events], [True, False, False, True])
        body = fake.rpc_calls("SendStreamingMessage")[0]["params"]["message"]
        self.assertEqual(body["messageId"], "m-1")
        self.assertEqual(body["contextId"], CONTEXT_ID)
        self.assertEqual(body["role"], "ROLE_USER")
        self.assertNotIn("taskId", body)

    async def test_not_accepted_sse_is_retried_with_the_same_message(self):
        fake = FakeKagent()
        fake.send_script = ["not-accepted", "not-accepted", "ok"]
        client, sleeps = make_client(fake)
        events = await send(client)
        self.assertEqual(len(events), 4)
        calls = [c["params"]["message"] for c in fake.rpc_calls("SendStreamingMessage")]
        self.assertEqual(len(calls), 3)
        self.assertEqual({c["messageId"] for c in calls}, {"m-1"})
        # kagent's retryAfterMs, backed off.
        self.assertEqual(sleeps, [0.1, 0.2])

    async def test_not_accepted_plain_json_is_retried(self):
        fake = FakeKagent()
        fake.send_script = ["not-accepted-json", "ok"]
        client, _ = make_client(fake)
        await send(client)
        self.assertEqual(len(fake.rpc_calls("SendStreamingMessage")), 2)

    async def test_not_accepted_gives_up_when_the_30s_budget_runs_out(self):
        fake = FakeKagent()
        fake.send_script = ["not-accepted"] * 100
        client, sleeps = make_client(fake)
        with self.assertRaises(SendNotAccepted):
            await send(client)
        calls = fake.rpc_calls("SendStreamingMessage")
        self.assertEqual(len(calls), len(sleeps) + 1)
        self.assertLessEqual(sum(sleeps), 30.0)
        self.assertGreater(sum(sleeps), 25.0)
        self.assertLessEqual(max(sleeps), 2.0)  # backoff is capped
        self.assertEqual({c["params"]["message"]["messageId"] for c in calls}, {"m-1"})
        self.assertEqual(fake.accepted_message_ids, [])

    async def test_kagents_own_wait_per_attempt_counts_against_the_budget(self):
        # kagent holds each attempt up to 10s while the Session is busy before it says
        # "not accepted"; the client measures wall time, not attempts.
        fake = FakeKagent()
        fake.send_script = ["not-accepted"] * 10
        ticks = iter(range(0, 1000, 10))
        client, _ = make_client(fake, clock=lambda: float(next(ticks)))
        with self.assertRaises(SendNotAccepted):
            await send(client)
        self.assertEqual(len(fake.rpc_calls("SendStreamingMessage")), 3)

    async def test_other_errors_are_not_retried(self):
        fake = FakeKagent()
        fake.send_script = ["other-error", "ok"]
        client, _ = make_client(fake)
        with self.assertRaises(A2AError) as ctx:
            await send(client)
        self.assertNotIsInstance(ctx.exception, SendNotAccepted)
        self.assertEqual(len(fake.rpc_calls("SendStreamingMessage")), 1)

    async def test_not_accepted_needs_the_a2a_domain(self):
        from mainloop.runtime.kagent_client import a2a_error_from_json

        error = a2a_error_from_json(
            {
                "code": -32603,
                "message": "x",
                "data": [
                    {
                        "@type": "type.googleapis.com/google.rpc.ErrorInfo",
                        "reason": "UNSUPPORTED_OPERATION",
                        "domain": "other.example",
                        "metadata": {"reason": "KAGENT_SEND_NOT_ACCEPTED"},
                    }
                ],
            }
        )
        self.assertNotIsInstance(error, SendNotAccepted)

    async def test_not_accepted_is_read_from_the_metadata_reason_not_the_top_level_one(
        self,
    ):
        from mainloop.runtime.kagent_client import a2a_error_from_json

        info = {
            "@type": "type.googleapis.com/google.rpc.ErrorInfo",
            "reason": "KAGENT_SEND_NOT_ACCEPTED",
            "domain": "a2a-protocol.org",
        }
        error = a2a_error_from_json({"code": -32004, "message": "x", "data": [info]})
        self.assertNotIsInstance(error, SendNotAccepted)
        info = {
            **info,
            "reason": "UNSUPPORTED_OPERATION",
            "metadata": {"reason": info["reason"]},
        }
        error = a2a_error_from_json({"code": -32004, "message": "x", "data": [info]})
        self.assertIsInstance(error, SendNotAccepted)

    async def test_unreachable_is_a_definite_non_delivery(self):
        fake = FakeKagent()
        fake.send_script = ["unreachable"]
        client, _ = make_client(fake)
        with self.assertRaises(Unreachable):
            await send(client)

    async def test_lost_request_is_outcome_unknown_and_not_resent(self):
        fake = FakeKagent()
        fake.send_script = ["drop", "ok"]
        client, _ = make_client(fake)
        with self.assertRaises(OutcomeUnknown):
            await send(client)
        self.assertEqual(len(fake.rpc_calls("SendStreamingMessage")), 1)

    async def test_stream_cut_is_outcome_unknown_and_resolvable_without_resend(self):
        fake = FakeKagent()
        fake.send_script = ["cut"]
        client, _ = make_client(fake)
        seen: list[StreamEvent] = []
        with self.assertRaises(OutcomeUnknown):
            async for event in client.send_message(
                AGENT, text="hello", message_id="m-1", context_id=CONTEXT_ID
            ):
                seen.append(event)
        self.assertEqual(len(seen), 2)
        task = await client.find_task_for_message(AGENT, CONTEXT_ID, "m-1")
        self.assertIsNotNone(task)
        self.assertEqual(task.id, TASK_ID)
        self.assertEqual(len(fake.rpc_calls("SendStreamingMessage")), 1)

    async def test_list_tasks_follows_pages(self):
        fake = FakeKagent()
        fake.default_page_size = 50
        for i in range(130):
            fake.tasks[f"t-{i}"] = {
                "id": f"t-{i}",
                "contextId": CONTEXT_ID,
                "status": {"state": "TASK_STATE_COMPLETED"},
                "history": [{"messageId": f"m-{i}", "parts": [{"text": "x"}]}],
            }
        client, _ = make_client(fake)
        tasks = await client.list_tasks(AGENT, CONTEXT_ID)
        self.assertEqual([t.id for t in tasks], [f"t-{i}" for i in range(130)])
        params = [c["params"] for c in fake.rpc_calls("ListTasks")]
        self.assertEqual(len(params), 2)
        self.assertEqual(params[0]["pageSize"], 100)
        self.assertNotIn("pageToken", params[0])
        self.assertEqual(params[1]["pageToken"], "100")

    async def test_found_task_is_reread_because_listed_tasks_have_no_artifacts(self):
        # A long-lived main thread: the message is in a task past the first page, and the
        # reply text only comes back from GetTask.
        fake = FakeKagent()
        for i in range(120):
            fake.tasks[f"t-{i}"] = {
                "id": f"t-{i}",
                "contextId": CONTEXT_ID,
                "status": {"state": "TASK_STATE_COMPLETED"},
                "history": [{"messageId": f"m-{i}", "parts": [{"text": "x"}]}],
                "artifacts": [
                    {"artifactId": f"a-{i}", "parts": [{"text": f"reply {i}"}]}
                ],
            }
        client, _ = make_client(fake)
        listed = await client.list_tasks(AGENT, CONTEXT_ID)
        self.assertTrue(all(t.artifacts == [] for t in listed))
        task = await client.find_task_for_message(AGENT, CONTEXT_ID, "m-117")
        self.assertEqual(task.id, "t-117")
        self.assertEqual(task.artifacts[0].text, "reply 117")
        self.assertEqual(fake.rpc_calls("GetTask")[-1]["params"], {"id": "t-117"})

    async def test_find_task_for_unknown_message_is_none(self):
        fake = FakeKagent()
        client, _ = make_client(fake)
        await send(client)
        self.assertIsNone(
            await client.find_task_for_message(AGENT, CONTEXT_ID, "other")
        )


class ReconnectAndCancelTests(unittest.IsolatedAsyncioTestCase):
    async def test_subscribe_first_event_replaces_the_projection(self):
        fake = FakeKagent()
        fake.subscribe_events = stream_chunks("m-1")[-1:]
        client, _ = make_client(fake)
        proj = TaskProjection(task_id=TASK_ID, state="TASK_STATE_WORKING")
        events = await collect(client.subscribe_to_task(AGENT, TASK_ID))
        proj.replace(events[0].task)
        self.assertTrue(proj.terminal)
        self.assertEqual(proj.text, "ok")
        self.assertEqual(
            fake.rpc_calls("SubscribeToTask")[0]["params"], {"id": TASK_ID}
        )

    async def test_get_task_and_missing_task(self):
        fake = FakeKagent()
        client, _ = make_client(fake)
        await send(client)
        self.assertEqual((await client.get_task(AGENT, TASK_ID)).id, TASK_ID)
        with self.assertRaises(TaskNotFound):
            await client.get_task(AGENT, "nope")

    async def test_cancel_running_task(self):
        fake = FakeKagent()
        fake.send_script = ["cut"]
        client, _ = make_client(fake)
        with self.assertRaises(OutcomeUnknown):
            await send(client)
        task = await client.cancel_task(AGENT, TASK_ID)
        self.assertEqual(normalise_state(task.status.state), "canceled")

    async def test_cancel_completed_task_returns_it_unchanged(self):
        fake = FakeKagent()
        client, _ = make_client(fake)
        await send(client)
        task = await client.cancel_task(AGENT, TASK_ID)
        self.assertEqual(normalise_state(task.status.state), "completed")


if __name__ == "__main__":
    unittest.main()


class HITLWireTests(unittest.IsolatedAsyncioTestCase):
    async def test_identity_decoder_and_message_projection_preserve_extension(self):
        from mainloop.runtime.kagent_client import (
            Message,
            Task,
            TaskStatus,
            _field_bytes,
            _field_str,
            decode_session_response,
        )

        from models.hitl import HITL_EXTENSION

        raw = (
            _field_str(1, "session")
            + _field_str(2, "creator")
            + _field_str(5, "revision")
            + _field_str(6, "session-session.team.actors.resources.substrate.ate.dev")
            + _field_str(14, "context")
            + _field_bytes(15, AgentRef("team", "agent").encode())
        )
        decoded = decode_session_response(_field_bytes(1, raw))
        self.assertEqual(
            (
                decoded.creator,
                decoded.prepared_revision,
                decoded.agent,
                decoded.context_id,
            ),
            ("creator", "revision", AgentRef("team", "agent"), "context"),
        )
        projection = TaskProjection()
        message = Message(
            message_id="one",
            extensions=[HITL_EXTENSION],
            metadata={
                "unknown": {"keep": True},
                HITL_EXTENSION: {"type": "ask_user_request", "id": "a"},
            },
        )
        task = Task(
            id="task",
            context_id="context",
            status=TaskStatus(state="input-required", message=message),
        )
        self.assertTrue(projection.apply(StreamEvent(task=task)))
        self.assertEqual(projection.status_message.metadata["unknown"], {"keep": True})
        changed = task.model_copy(deep=True)
        changed.status.message.metadata[HITL_EXTENSION]["id"] = "b"
        self.assertTrue(projection.apply(StreamEvent(task=changed)))
        self.assertFalse(projection.apply(StreamEvent(task=changed)))

    async def test_structured_continuation_uses_exact_task_and_activates_extension(
        self,
    ):
        import json

        from models.hitl import HITL_EXTENSION, AskUserAnswer, AskUserResponse

        requests = []

        async def handler(request):
            requests.append(request)
            body = json.loads(request.content)
            if body["method"] == "GetExtendedAgentCard":
                result = {"capabilities": {"extensions": [{"uri": HITL_EXTENSION}]}}
            elif body["method"] == "ListTasks":
                result = {"tasks": [], "nextPageToken": "next"}
            else:
                result = {
                    "task": {
                        "id": "original-task",
                        "contextId": "original-context",
                        "status": {"state": "working"},
                    }
                }
            return httpx.Response(200, json={"jsonrpc": "2.0", "result": result})

        async with httpx.AsyncClient(
            transport=httpx.MockTransport(handler), base_url="http://fake"
        ) as http:
            client = KagentClient("http://fake", user_id="configured", client=http)
            self.assertTrue(await client.supports_hitl(AGENT))
            self.assertEqual(
                await client.list_tasks_page(AGENT, "original-context", "cursor", 7),
                ([], "next"),
            )
            response = AskUserResponse(
                type="ask_user_response",
                id="child-question",
                answers=(AskUserAnswer(answer=("answer",)),),
            )
            events = [
                e
                async for e in client.send_hitl_response(
                    AGENT,
                    response=response,
                    message_id="stable-message",
                    task_id="original-task",
                    context_id="original-context",
                )
            ]
            self.assertEqual(events[0].task.id, "original-task")
        for request in requests:
            self.assertEqual(request.headers["A2A-Extensions"], HITL_EXTENSION)
            self.assertEqual(request.headers["x-user-id"], "configured")
            self.assertEqual(request.url.path, AGENT.path)
        wire = json.loads(requests[-1].content)["params"]["message"]
        self.assertEqual(
            (wire["taskId"], wire["contextId"], wire["messageId"]),
            ("original-task", "original-context", "stable-message"),
        )
        self.assertEqual(wire["metadata"][HITL_EXTENSION]["id"], "child-question")
        self.assertEqual(wire["parts"], [])
        self.assertEqual(
            json.loads(requests[1].content)["params"]["pageToken"], "cursor"
        )
