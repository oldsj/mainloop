"""Export real observer/API results for frontend contract/render tests; fake gateway only."""

import asyncio
import json
import os
from pathlib import Path
from unittest.mock import patch

import httpx
from mainloop import api
from mainloop.config import settings
from mainloop.runtime import native_sessions
from mainloop.runtime.hitl_observer import HITLObserver
from mainloop.runtime.kagent_client import Unreachable
from tests.runtime.test_hitl_observer import Gateway, approval, pending
from tests.runtime.test_postgres_ledger import PostgresTestCase


class HITLUITests(PostgresTestCase):
    async def test_observer_api_ui_contract(self):
        gateway = Gateway()
        live, task = gateway.add("ui-session")
        sid, _ = await self.bound_session()
        await native_sessions.ledger.update_binding(sid, kagent_session_id=live.id)
        service = HITLObserver(
            gateway,
            gateway=f"https://gateway/{self.user}",
            owner=self.user,
            creator="gateway-owner",
        )

        async def cycle():
            for _ in range(3):
                await service.once()

        await cycle()
        fixtures = {}
        with patch.object(settings, "api_hosts", "localhost"), patch.object(
            settings, "owner_id", self.user
        ), patch("mainloop.runtime.hitl_continuation.observer", return_value=service):
            async with httpx.AsyncClient(
                transport=httpx.ASGITransport(app=api.app), base_url="http://localhost"
            ) as client:
                ids = (await client.get(f"/sessions/{sid}/hitl")).json()
                self.assertEqual(len(ids), 1)
                request_id = ids[0]
                with patch.dict(
                    "os.environ", {"MAINLOOP_OWNER_HITL_WRITES_ENABLED": "false"}
                ):
                    fixtures["disabled"] = (
                        await client.get(f"/hitl/{request_id}")
                    ).json()
                    self.assertFalse(fixtures["disabled"]["writes_enabled"])
                with patch.dict(
                    "os.environ", {"MAINLOOP_OWNER_HITL_WRITES_ENABLED": "true"}
                ):
                    fixtures["pending"] = (
                        await client.get(f"/hitl/{request_id}")
                    ).json()
                    self.assertTrue(fixtures["pending"]["answerable"])
                    self.assertEqual(fixtures["pending"]["context"]["session_id"], sid)
                    self.assertEqual(
                        fixtures["pending"]["context"]["provider"], "claude"
                    )
                    self.assertIsNone(fixtures["pending"]["merge"])
                    await cycle()
                    self.assertEqual(
                        (await client.get(f"/sessions/{sid}/hitl")).json(), ids
                    )
                    incomplete = await client.post(
                        f"/hitl/{request_id}/respond",
                        json={
                            "action_id": "incomplete",
                            "response": {
                                "type": "tool_approval_response",
                                "approvals": [],
                            },
                        },
                    )
                    self.assertEqual(incomplete.status_code, 422)
                    gateway.failure = Unreachable("temporary")
                    await cycle()
                    fixtures["stale"] = (await client.get(f"/hitl/{request_id}")).json()
                    self.assertFalse(fixtures["stale"]["answerable"])
                    gateway.failure = None
                    await cycle()
                    body = {
                        "action_id": "mobile",
                        "response": {
                            "type": "tool_approval_response",
                            "approvals": [
                                {
                                    "id": "call",
                                    "approved": False,
                                    "rejection_reason": "Please keep the tests.",
                                }
                            ],
                        },
                    }
                    other = {
                        "action_id": "chat",
                        "response": {
                            "type": "tool_approval_response",
                            "approvals": [{"id": "call", "approved": True}],
                        },
                    }
                    results = await asyncio.gather(
                        client.post(f"/hitl/{request_id}/respond", json=body),
                        client.post(f"/hitl/{request_id}/respond", json=other),
                    )
                    self.assertEqual(sorted(r.status_code for r in results), [200, 409])
                    fixtures["answered"] = (
                        await client.get(f"/hitl/{request_id}")
                    ).json()
                    self.assertFalse(fixtures["answered"]["answerable"])
                    self.assertEqual(len(gateway.sent), 1)
                    # Same native task, successive request; old controls cannot answer it.
                    payload = {
                        "type": "ask_user_request",
                        "id": "question-2",
                        "questions": [
                            {
                                "question": "Choose targets",
                                "choices": ["Linux", "macOS"],
                                "multiple": True,
                            },
                            {
                                "question": "What should change?",
                                "choices": None,
                                "multiple": False,
                            },
                        ],
                        "future_metadata": {"safe": "<script>untrusted</script>"},
                    }
                    gateway.tasks[task.id] = pending(live, task.id, payload)
                    await cycle()
                    new_ids = (await client.get(f"/sessions/{sid}/hitl")).json()
                    self.assertEqual(len(new_ids), 1)
                    self.assertNotEqual(new_ids, ids)
                    fixtures["questions"] = (
                        await client.get(f"/hitl/{new_ids[0]}")
                    ).json()
                    stale_submit = await client.post(
                        f"/hitl/{request_id}/respond",
                        json={**other, "action_id": "late"},
                    )
                    self.assertEqual(stale_submit.status_code, 409)
                    answer = await client.post(
                        f"/hitl/{new_ids[0]}/respond",
                        json={
                            "action_id": "answer",
                            "response": {
                                "type": "ask_user_response",
                                "id": "question-2",
                                "answers": [
                                    {"answer": ["Linux", "macOS"]},
                                    {"answer": ["Keep tests"]},
                                ],
                            },
                        },
                    )
                    self.assertEqual(answer.status_code, 200, answer.text)
                    fixtures["question_answered"] = answer.json()
                    gateway.tasks[task.id] = pending(live, task.id, approval("third"))
                    await cycle()
                    newest = (await client.get(f"/sessions/{sid}/hitl")).json()[0]
                    del gateway.sessions[live.id]
                    await cycle()
                    fixtures["unavailable"] = (
                        await client.get(f"/hitl/{newest}")
                    ).json()
                    self.assertFalse(fixtures["unavailable"]["answerable"])
                with patch.object(settings, "owner_id", "someone-else"):
                    self.assertEqual(
                        (await client.get(f"/sessions/{sid}/hitl")).status_code, 404
                    )
                    self.assertEqual(
                        (await client.get(f"/hitl/{request_id}")).status_code, 404
                    )
        if target := os.environ.get("HITL_UI_FIXTURE_PATH"):
            Path(target).write_text(json.dumps(fixtures))
