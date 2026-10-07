"""Control credential admission against in-process HTTP fakes only."""

import io
import json
import logging
import tempfile
import unittest
from pathlib import Path

import httpx
from mainloop.config import Settings
from mainloop.runtime.kagent_client import (
    AgentRef,
    KagentClient,
    ServiceConfigurationError,
    _field_bytes,
    _field_str,
)
from pydantic import ValidationError
from tests.runtime.kagent_fake import FakeKagent, grpc_response, unauthorized_envelope

AGENT = AgentRef("kagent", "claude-subscription")


class ControlCredentialTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.path = Path(self.directory.name) / "token"
        self.path.write_text("a" * 32 + "\n")
        self.fake = FakeKagent()
        self.requests = []
        self.refusal = None

        def handle(request):
            self.requests.append(request)
            if self.refusal is not None:
                return self.refusal()
            if "AgentService" in request.url.path:
                ref = _field_str(1, AGENT.namespace) + _field_str(2, AGENT.name)

                def struct(items):
                    return b"".join(
                        _field_bytes(1, _field_str(1, key) + _field_bytes(2, value))
                        for key, value in items
                    )

                template = struct([("name", _field_str(3, "claude-workspace"))])
                spec = struct([("templateRef", _field_bytes(5, template))])
                resource = struct([("spec", _field_bytes(5, spec))])
                agent = _field_bytes(1, ref) + _field_bytes(
                    2, _field_bytes(3, resource)
                )
                return grpc_response(_field_bytes(1, agent))
            if (
                request.url.path.startswith("/agents/")
                and json.loads(request.content)["method"] == "GetExtendedAgentCard"
            ):
                return httpx.Response(200, json={"result": {"capabilities": {}}})
            return self.fake.handle(request)

        self.http = httpx.AsyncClient(
            transport=httpx.MockTransport(handle), base_url="http://kagent.test"
        )
        self.addAsyncCleanup(self.http.aclose)
        self.client = KagentClient(
            "http://kagent.test",
            user_id="mainloop",
            client=self.http,
            control_token_file=str(self.path),
        )

    async def exercise(self, client):
        session = await client.create_session(AGENT, request_id="persisted-request")
        await client.get_session(session.id)
        await client.list_sessions()
        await client.suspend_session(session.id)
        await client.resume_session(session.id)
        await client.get_agent(AGENT)
        await client.supports_hitl(AGENT)
        events = [
            event
            async for event in client.send_message(
                AGENT,
                text="hello",
                message_id="message",
                context_id=session.id,
            )
        ]
        task_id = next(event.task_id for event in events if event.task_id)
        await client.get_task(AGENT, task_id)
        await client.list_tasks(AGENT, session.id)
        for _ in range(2):
            [event async for event in client.subscribe_to_task(AGENT, task_id)]
        await client.cancel_task(AGENT, task_id)
        await client.delete_session(session.id)

    async def test_all_paths_and_reconnect_use_control_bearer(self):
        await self.exercise(self.client)
        self.assertGreaterEqual(len(self.requests), 14)
        for request in self.requests:
            self.assertEqual(request.headers["authorization"], "Bearer " + "a" * 32)
            self.assertNotIn("x-user-id", request.headers)

    async def test_unset_preserves_legacy_headers(self):
        client = KagentClient("http://kagent.test", user_id="legacy", client=self.http)
        await self.exercise(client)
        for request in self.requests:
            self.assertEqual(request.headers["x-user-id"], "legacy")
            self.assertNotIn("authorization", request.headers)

    async def test_rotation_and_failed_reload_without_fallback(self):
        await self.client.list_sessions()
        replacement = self.path.with_suffix(".next")
        replacement.write_text("b" * 32)
        replacement.replace(self.path)
        await self.client.list_sessions()
        self.assertEqual(
            self.requests[-1].headers["authorization"], "Bearer " + "b" * 32
        )
        self.path.unlink()
        count = len(self.requests)
        with self.assertRaises(ServiceConfigurationError):
            await self.client.list_sessions()
        self.assertEqual(len(self.requests), count)

    async def test_startup_configuration_is_bounded_and_sanitized(self):
        for contents in (None, "", " " * 20, "secret\nheader", "s" * 4097):
            if contents is None:
                self.path.unlink(missing_ok=True)
            else:
                self.path.write_text(contents)
            with self.assertRaises(ValidationError) as error:
                Settings(_env_file=None, kagent_control_token_file=str(self.path))
            self.assertNotIn("s" * 64, str(error.exception))
            with self.assertRaises(ServiceConfigurationError):
                KagentClient(
                    "http://kagent.test",
                    user_id="mainloop",
                    client=self.http,
                    control_token_file=str(self.path),
                )
        self.path.write_text("a" * 32)
        with self.assertRaises(ValidationError):
            Settings(
                _env_file=None,
                kagent_control_token_file=str(self.path),
                kagent_user_id="other",
            )

    async def test_auth_refusals_preempt_not_found_retry_and_untrusted_details(self):
        for status in (401, 403, 7, 16):
            self.refusal = (
                (
                    lambda status=status: httpx.Response(
                        status, json={"error": {"message": "a" * 32}}
                    )
                )
                if status in (401, 403)
                else (
                    lambda status=status: grpc_response(
                        None, status=status, detail="a" * 32
                    )
                )
            )
            calls = [lambda: self.client.get_session("existing")]
            if status in (401, 403):
                calls += [
                    lambda: self.client.get_task(AGENT, "task"),
                    lambda: self.collect_send(),
                    lambda: self.collect_subscribe(),
                ]
            for call in calls:
                count = len(self.requests)
                with self.assertRaises(ServiceConfigurationError) as error:
                    await call()
                self.assertNotIn("a" * 32, str(error.exception))
                self.assertEqual(len(self.requests), count + 1)

    async def test_companion_unauthorized_json_and_sse_are_sanitized(self):
        secret = "a" * 32
        envelope = unauthorized_envelope("remote echoed Bearer " + secret)
        for streaming in (False, True):
            with self.subTest(streaming=streaming):
                if streaming:
                    self.refusal = lambda: httpx.Response(
                        200,
                        headers={"content-type": "text/event-stream"},
                        text="data: " + json.dumps(envelope) + "\n\n",
                    )
                    call = self.collect_send
                else:
                    self.refusal = lambda: httpx.Response(200, json=envelope)

                    def call():
                        return self.client.get_task(AGENT, "task")

                count = len(self.requests)
                with self.assertRaises(ServiceConfigurationError) as error:
                    await call()
                self.assertNotIn(secret, str(error.exception))
                self.assertNotIn("remote echoed", str(error.exception))
                self.assertEqual(len(self.requests), count + 1)

    async def test_refusal_logs_exclude_remote_secret(self):
        output = io.StringIO()
        handler = logging.StreamHandler(output)
        logger = logging.getLogger("httpx")
        previous_level = logger.level
        logger.setLevel(logging.INFO)
        logger.addHandler(handler)
        try:
            self.refusal = lambda: httpx.Response(403, text="Bearer " + "a" * 32)
            with self.assertRaises(ServiceConfigurationError):
                await self.collect_send()
            self.assertIn("403", output.getvalue())
            self.assertNotIn("a" * 32, output.getvalue())
        finally:
            logger.removeHandler(handler)
            logger.setLevel(previous_level)

    async def collect_send(self):
        return [
            event
            async for event in self.client.send_message(
                AGENT, text="hello", message_id="message", context_id="existing"
            )
        ]

    async def collect_subscribe(self):
        return [event async for event in self.client.subscribe_to_task(AGENT, "task")]
