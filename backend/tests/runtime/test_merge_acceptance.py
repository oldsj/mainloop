"""Integrated b+c acceptance: real observer/API/PG, synthetic A2A and fake GitHub."""

import asyncio
import json
import os
from datetime import UTC, datetime, timedelta
from pathlib import Path
from unittest.mock import patch

import httpx
from mainloop import api
from mainloop.config import settings
from mainloop.db import db
from mainloop.db import hitl as store
from mainloop.runtime import native_sessions
from mainloop.runtime.hitl_correlation import task_identity
from mainloop.runtime.hitl_observer import HITLObserver
from mainloop.runtime.kagent_client import RuntimeState, Task
from mainloop.services import merge
from mainloop.services.github_repo import parse_github_repo
from tests.runtime import test_merge as support
from tests.runtime.test_hitl_observer import Gateway

from models.hitl import (
    HITL_EXTENSION,
    MERGE_OPERATION,
    HITLProjection,
    VerifiedAssociation,
)

FIXTURES = Path(__file__).parent / "fixtures" / "merge_policy"


class MergeAcceptanceTests(support.MergeFixture):
    async def projections(self):
        return [
            HITLProjection.model_validate(merge.decode(row["snapshot"]))
            for row in await self.pool.fetch(
                "SELECT snapshot FROM native_hitl_requests WHERE owner_id=$1", self.user
            )
        ]

    async def observe(self, observer):
        with (
            patch("mainloop.runtime.hitl_observer.observer", return_value=observer),
            patch("mainloop.runtime.hitl_continuation.observer", return_value=observer),
        ):
            await native_sessions.reconcile_once(sweep=False)

    def observer(self, gateway):
        return HITLObserver(
            gateway,
            gateway=f"https://gateway/{self.user}",
            owner=self.user,
            creator="gateway-owner",
        )

    async def inventory(self, proposal, provider="claude"):
        await self.pool.execute(
            "UPDATE native_bindings SET kind=$2 WHERE session_id=$1", self.sid, provider
        )
        gateway = Gateway()
        session, _ = gateway.add(self.binding["kagent_session_id"])
        wire = json.loads((FIXTURES / f"{provider}.json").read_text())
        wire["id"] = f"task-{session.id}"
        wire["contextId"] = session.context_id
        message = wire["status"]["message"]
        message.update(taskId=wire["id"], contextId=session.context_id)
        message["metadata"][HITL_EXTENSION]["tools"][0]["args"]["proposal_id"] = (
            proposal["proposal_id"]
        )
        task = Task.model_validate(wire)
        gateway.tasks[task.id] = task
        self.assertEqual(session.state, RuntimeState.SUSPENDED)
        self.assertEqual(task.status.state, "input-required")
        config = {
            "owner_id": self.user,
            "binding_id": self.sid,
            "runtime_session_id": session.id,
            "provider": provider,
            "prepared_revision": session.prepared_revision,
            "evidence_reference": "fixture://operator-config",
            "mappings": [
                {
                    "provider": provider,
                    "prepared_revision": session.prepared_revision,
                    "compiled_alias": "mainloop-merge-approval",
                    "remote_server_id": "kagent/mainloop-merge-approval",
                    "endpoint": "http://mainloop-mcp.mainloop.svc.cluster.local/mcp/merge-approval",
                    "tool": "merge_pull_request_with_approval",
                    "require_approval": True,
                }
            ],
        }
        p = patch.dict(os.environ, MAINLOOP_MERGE_CONFIGURATIONS=json.dumps([config]))
        p.start()
        self.addCleanup(p.stop)
        self.assertEqual(
            await self.pool.fetchval("SELECT count(*) FROM native_deliveries"), 0
        )
        self.assertEqual(await self.projections(), [])
        observer = self.observer(gateway)
        await self.observe(observer)
        return gateway, (await self.projections())[0]

    async def http(self, gateway, method, path, body=None):
        with (
            patch.object(settings, "api_hosts", "localhost"),
            patch.object(settings, "owner_id", self.user),
            patch.dict(os.environ, MAINLOOP_OWNER_HITL_WRITES_ENABLED="true"),
            patch(
                "mainloop.runtime.hitl_continuation.observer",
                return_value=self.observer(gateway),
            ),
        ):
            async with httpx.AsyncClient(
                transport=httpx.ASGITransport(app=api.app), base_url="http://localhost"
            ) as client:
                return await client.request(method, path, json=body)

    async def answer(self, gateway, projection, *, approved=True, action="desktop"):
        return await self.http(
            gateway,
            "POST",
            f"/hitl/{projection.id}/respond",
            {
                "action_id": action,
                "response": {
                    "type": "tool_approval_response",
                    "approvals": [
                        {
                            "id": "call",
                            "approved": approved,
                            **(
                                {}
                                if approved
                                else {"rejection_reason": "Keep the protected tests."}
                            ),
                        }
                    ],
                },
            },
        )

    async def assert_receipt(
        self, response, projection, proposal, invocation="invoke-1"
    ):
        self.assertEqual(response.status_code, 200, response.text)
        receipt = response.json()["response"]
        self.assertEqual(receipt["outer"], projection.outer.model_dump(mode="json"))
        call = receipt["calls"][0]
        self.assertEqual(call["leaf"], projection.leaves[0].model_dump(mode="json"))
        self.assertEqual(call["mapping"]["operation"], MERGE_OPERATION)
        self.assertEqual(
            call["merge_key"],
            {
                "owner_id": self.user,
                "leaf_binding_id": self.sid,
                "leaf_runtime_session_id": self.binding["kagent_session_id"],
                "operation": MERGE_OPERATION,
                "proposal_id": proposal["proposal_id"],
                "invocation_request_id": invocation,
            },
        )
        facts = merge.decode(
            await self.pool.fetchval(
                "SELECT facts FROM merge_proposals WHERE id=$1", proposal["proposal_id"]
            )
        )
        self.assertEqual(
            (facts["head_sha"], facts["base_sha"], facts["pr_number"]),
            (support.SHA, support.BASE, 17),
        )
        return receipt

    async def approved_flow(self, provider, *, nested=False):
        p = await self.prepare()
        gateway, direct = await self.inventory(p, provider)
        projection = direct
        if nested:
            leaf_task = gateway.tasks[direct.outer.task_id]
            _, outer_task = gateway.add(
                "outer-parent",
                {
                    "type": "tool_approval_request",
                    "tools": [
                        {
                            "id": "parent",
                            "call_id": "parent-native",
                            "name": "delegate",
                            "args": {},
                        }
                    ],
                    "nested": {
                        "subagent_name": "untrusted-label",
                        "task_id": leaf_task.id,
                        "context_id": leaf_task.context_id,
                        "tools": leaf_task.status.message.metadata[HITL_EXTENSION][
                            "tools"
                        ],
                    },
                },
            )
            await self.pool.execute(
                "UPDATE native_hitl_inventory_state SET next_sweep=now() WHERE owner_id=$1",
                self.user,
            )
            await self.observe(self.observer(gateway))
            projection = next(
                p for p in await self.projections() if p.outer.task_id == outer_task.id
            )
            unavailable = (
                await self.http(gateway, "GET", f"/hitl/{projection.id}")
            ).json()
            self.assertFalse(unavailable["answerable"])
            self.assertTrue(unavailable["unavailable_reason"])
            self.assertEqual((await self.answer(gateway, projection)).status_code, 409)
            self.assertEqual(gateway.sent, [])
            async with self.pool.acquire() as conn:
                await store.save_association(
                    conn,
                    VerifiedAssociation(
                        owner_id=self.user,
                        outer=task_identity(projection.outer),
                        leaf=task_identity(direct.outer),
                        evidence_source="gateway_continuation",
                        evidence_reference="fixture://verified-outer-leaf",
                    ),
                )
        # Recreate the observer against suspended gateway snapshots and persisted state.
        await self.observe(self.observer(gateway))
        await self.observe(self.observer(gateway))
        projection = next(p for p in await self.projections() if p.id == projection.id)
        inbox = await self.http(gateway, "GET", "/queue")
        self.assertEqual(inbox.status_code, 200)
        self.assertEqual(
            sum(
                x["item_type"] == "hitl_request" and x["status"] == "pending"
                for x in inbox.json()
            ),
            1,
        )
        view = (await self.http(gateway, "GET", f"/hitl/{projection.id}")).json()
        self.assertEqual(view["merge_enrichment"][0]["proposal_id"], p["proposal_id"])
        self.assertEqual(view["merge_enrichment"][0]["head_sha"], support.SHA)
        self.assertEqual(view["merge"], view["merge_enrichment"])
        self.assertEqual(
            view["merge"][0]["tool_id"], projection.leaves[0].pending_request_id
        )
        with patch.dict(os.environ, MAINLOOP_MERGE_CONFIGURATIONS="[]"):
            unavailable = (
                await self.http(gateway, "GET", f"/hitl/{projection.id}")
            ).json()
        self.assertIsNone(unavailable["merge"])
        self.assertEqual(unavailable["merge_enrichment"], [])
        if provider == "codex" and not nested:
            if target := os.environ.get("MERGE_UI_FIXTURE_PATH"):
                Path(target).write_text(json.dumps(view))
        with patch.object(merge, "datetime") as clock:
            clock.now.return_value = datetime.now(UTC) + timedelta(hours=16)
            result = await self.answer(gateway, projection)
        await self.assert_receipt(result, projection, p)
        self.assertIsNone(
            await self.pool.fetchval(
                "SELECT deadline FROM merge_requests WHERE owner_id=$1", self.user
            )
        )
        self.assertEqual(
            (gateway.sent[0][1]["task_id"], gateway.sent[0][1]["context_id"]),
            (projection.outer.task_id, projection.outer.context_id),
        )
        if nested:
            self.assertNotEqual(
                projection.outer.runtime_session_id,
                projection.leaves[0].runtime_session_id,
            )
            self.assertEqual(
                (await self.answer(gateway, direct, action="mobile")).status_code, 409
            )
        self.assertEqual((await self.execute(p, approved=True))["state"], "merged")
        self.assertEqual((await self.execute(p, approved=True))["state"], "merged")
        self.assertEqual(len(gateway.sent), 1)
        self.assertEqual(len(self.fake.puts), 1)
        candidate = await self.pool.fetchrow(
            "SELECT r.* FROM merge_requests r JOIN merge_proposals p ON p.candidate_id=r.id WHERE p.id=$1",
            p["proposal_id"],
        )
        self.assertEqual(candidate["active_proposal_id"], p["proposal_id"])
        self.assertEqual(candidate["head_sha"], support.SHA)
        self.assertEqual(candidate["repository_id"], self.fake.repo["id"])
        self.assertEqual(candidate["pr_number"], 17)
        self.assertEqual(candidate["receipt_action_id"], "desktop")
        self.assertEqual(candidate["intent_invocation_id"], "invoke-1")
        self.assertIsNotNone(candidate["intent_id"])
        self.assertEqual(candidate["state"], "merged")
        self.assertEqual(
            await self.pool.fetchval("SELECT count(*) FROM native_deliveries"), 0
        )

    async def test_claude_explicit_approval_inventory_restart_overnight(self):
        await self.pool.execute(
            "UPDATE projects SET merge_policy='approval',merge_policy_version=2 WHERE id=$1",
            self.project.id,
        )
        await self.approved_flow("claude")

    async def test_codex_protected_rename_inventory_restart(self):
        self.fake.files = [
            {
                "filename": "src/moved.py",
                "previous_filename": "k8s/protected.yaml",
                "status": "renamed",
            }
        ]
        await self.approved_flow("codex")

    async def test_claude_protected_delete_nested_duplicate_observation(self):
        self.fake.files = [{"filename": "migrations/001.sql", "status": "removed"}]
        await self.approved_flow("claude", nested=True)

    async def test_codex_nested_duplicate_observation(self):
        self.fake.files = [{"filename": "k8s/protected.yaml", "status": "removed"}]
        await self.approved_flow("codex", nested=True)

    async def test_denial_reason_and_simultaneous_decisions(self):
        self.fake.files[0]["filename"] = "k8s/app.yaml"
        p = await self.prepare()
        gateway, projection = await self.inventory(p, "codex")
        results = await asyncio.gather(
            *(
                self.answer(gateway, projection, approved=False, action=a)
                for a in ("desktop", "mobile")
            )
        )
        self.assertEqual(sorted(r.status_code for r in results), [200, 409])
        result = next(r for r in results if r.status_code == 200)
        await self.assert_receipt(result, projection, p)
        self.assertEqual(
            result.json()["response"]["response"]["approvals"][0]["rejection_reason"],
            "Keep the protected tests.",
        )
        self.assertEqual(len(gateway.sent), 1)
        with self.assertRaises(support.PolicyError):
            await self.execute(p, approved=True)
        self.assertEqual(self.fake.puts, [])

    async def default_auto(self, name):
        self.fake.repo["full_name"] = f"owner/{name}"
        self.project = await db.get_or_create_project(
            self.user, parse_github_repo(f"owner/{name}")
        )
        self.args["project_id"] = self.project.id
        self.assertEqual(self.project.merge_policy, "auto")
        result = await merge.auto_merge(self.binding, self.args)
        self.assertEqual(result["state"], "merged")
        self.assertEqual(await self.projections(), [])
        self.assertEqual(len(self.fake.puts), 1)

    async def test_default_testrepo_auto(self):
        await self.default_auto("testrepo")

    async def test_default_mainloop_auto(self):
        await self.default_auto("mainloop")

    async def test_stale_proposal_cannot_answer_or_merge(self):
        self.fake.files[0]["filename"] = "k8s/app.yaml"
        p = await self.prepare()
        gateway, projection = await self.inventory(p)
        newer = await self.prepare(request_id="replacement")
        self.assertNotEqual(p["proposal_id"], newer["proposal_id"])
        view = (await self.http(gateway, "GET", f"/hitl/{projection.id}")).json()
        self.assertTrue(view["merge_enrichment"][0]["stale"])
        self.assertEqual((await self.answer(gateway, projection)).status_code, 409)
        self.assertEqual(gateway.sent, [])
        self.assertEqual(self.fake.puts, [])

    async def test_approved_same_head_retry_needs_fresh_native_decision(self):
        self.fake.files[0]["filename"] = "k8s/app.yaml"
        p = await self.prepare()
        gateway, projection = await self.inventory(p)
        await self.assert_receipt(await self.answer(gateway, projection), projection, p)
        self.fake.runs[0]["conclusion"] = "failure"
        self.assertEqual((await self.execute(p, approved=True))["state"], "blocked")
        self.fake.runs[0]["conclusion"] = "success"
        newer = await self.prepare(request_id="retry")
        with self.assertRaises(support.PolicyError):
            await self.execute(newer, request="retry-invoke", approved=True)
        task = gateway.tasks[projection.outer.task_id]
        wire = json.loads((FIXTURES / "claude.json").read_text())
        wire.update(id=task.id, contextId=task.context_id)
        message = wire["status"]["message"]
        message.update(
            taskId=task.id, contextId=task.context_id, messageId="retry-status"
        )
        message["metadata"][HITL_EXTENSION]["tools"][0]["args"].update(
            proposal_id=newer["proposal_id"], request_id="retry-invoke"
        )
        gateway.tasks[task.id] = Task.model_validate(wire)
        await self.observe(self.observer(gateway))
        fresh = next(p for p in await self.projections() if p.availability == "pending")
        self.assertNotEqual(fresh.id, projection.id)
        await self.assert_receipt(
            await self.answer(gateway, fresh, action="retry"),
            fresh,
            newer,
            "retry-invoke",
        )
        self.assertEqual(
            (await self.execute(newer, request="retry-invoke", approved=True))["state"],
            "merged",
        )
        self.assertEqual((await self.execute(p, approved=True))["state"], "blocked")
        self.assertEqual(len(self.fake.puts), 1)

    async def test_uncertain_merge_after_owner_approval_never_replays_put(self):
        self.fake.files[0]["filename"] = "k8s/app.yaml"
        p = await self.prepare()
        gateway, projection = await self.inventory(p, "codex")
        await self.assert_receipt(await self.answer(gateway, projection), projection, p)
        self.fake.lose = True
        self.assertEqual((await self.execute(p, approved=True))["state"], "uncertain")
        await self.observe(self.observer(gateway))
        self.assertEqual((await self.execute(p, approved=True))["state"], "merged")
        self.assertEqual(len(self.fake.puts), 1)
        self.assertEqual(len(gateway.sent), 1)

    async def test_multiple_inventory_question_sessions_keep_original_destinations(
        self,
    ):
        gateway = Gateway()
        for name in ("main-question", "standalone-question"):
            gateway.add(
                name,
                {
                    "type": "ask_user_request",
                    "id": "same-question-id",
                    "questions": [
                        {
                            "question": "Which platform?",
                            "choices": ["Linux", "macOS"],
                            "multiple": False,
                        }
                    ],
                },
            )
        await self.observe(self.observer(gateway))
        await self.observe(self.observer(gateway))
        projections = await self.projections()
        self.assertEqual(len(projections), 2)
        for projection in projections:
            response = await self.http(
                gateway,
                "POST",
                f"/hitl/{projection.id}/respond",
                {
                    "action_id": projection.id,
                    "response": {
                        "type": "ask_user_response",
                        "id": "same-question-id",
                        "answers": [{"answer": ["Linux"]}],
                    },
                },
            )
            self.assertEqual(response.status_code, 200, response.text)
            receipt = response.json()["response"]
            self.assertEqual(receipt["outer"], projection.outer.model_dump(mode="json"))
            self.assertEqual(
                receipt["calls"][0]["leaf"],
                projection.leaves[0].model_dump(mode="json"),
            )
            self.assertIsNone(receipt["calls"][0]["merge_key"])
        self.assertEqual(
            {s[1]["task_id"] for s in gateway.sent},
            {p.outer.task_id for p in projections},
        )
        self.assertEqual(
            {s[1]["context_id"] for s in gateway.sent},
            {p.outer.context_id for p in projections},
        )
        self.assertEqual(
            await self.pool.fetchval("SELECT count(*) FROM native_deliveries"), 0
        )
