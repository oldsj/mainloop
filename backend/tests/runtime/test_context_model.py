"""Context model (plan r7) with fakes only: no cluster, no agents, no Postgres, no credentials."""

import json
import unittest

from fastapi import FastAPI
from fastapi.testclient import TestClient
from mainloop.runtime import agent_api, policy
from mainloop.runtime.agent_api import AgentService, hash_token
from mainloop.runtime.journal import parse_claude
from mainloop.runtime.native_sessions import config_name, rotation_due
from mainloop.runtime.policy import Actor, PolicyError
from mainloop.runtime.standing import (
    RecentMessage,
    StandingInputs,
    TopicLine,
    content_hash,
    render_standing,
)

KINDS = frozenset({"claude", "codex"})


def spawn(actor, own=0, glob=0, kind="claude"):
    policy.check_spawn(
        actor,
        kind=kind,
        allowed_kinds=KINDS,
        live_children_of_actor=own,
        live_children_global=glob,
    )


class PolicyTests(unittest.TestCase):
    def test_main_may_spawn_up_to_three_concurrent_children(self):
        for own in (0, 1, 2):
            spawn(Actor("main", 0), own=own)
        with self.assertRaises(PolicyError) as cm:
            spawn(Actor("main", 0), own=3)  # the fourth concurrent child
        self.assertEqual(cm.exception.code, "concurrency")

    def test_third_level_spawn_is_refused_by_depth(self):
        with self.assertRaises(PolicyError) as cm:
            spawn(Actor("supervisor", 2))  # depth-2 agent creating a depth-3 agent
        self.assertEqual(cm.exception.code, "depth")

    def test_child_may_not_spawn_until_supervisors_exist(self):
        with self.assertRaises(PolicyError) as cm:
            spawn(Actor("child", 1))
        self.assertEqual(cm.exception.code, "role")

    def test_unknown_kind_and_global_limit(self):
        with self.assertRaises(PolicyError):
            spawn(Actor("main", 0), kind="rm")
        with self.assertRaises(PolicyError) as cm:
            spawn(Actor("main", 0), glob=policy.MAX_CHILDREN_GLOBAL)
        self.assertEqual(cm.exception.code, "global-concurrency")

    def test_only_children_report(self):
        policy.may_report(Actor("child", 1))
        with self.assertRaises(PolicyError):
            policy.may_report(Actor("main", 0))


class RotationTests(unittest.TestCase):
    def test_tokens_are_measured_above_the_lineage_baseline(self):
        kw = dict(turns=1, budget_tokens=20000, budget_turns=12)
        # A trivial session already holds ~10k tokens: absolute size alone must not trigger.
        self.assertIsNone(
            rotation_due(context_tokens=10500, baseline_tokens=10200, **kw)
        )
        self.assertIsNone(
            rotation_due(context_tokens=29999, baseline_tokens=10200, **kw)
        )
        self.assertIn(
            "tokens", rotation_due(context_tokens=30200, baseline_tokens=10200, **kw)
        )

    def test_turn_budget_and_unknown_usage(self):
        self.assertIn(
            "turns",
            rotation_due(
                context_tokens=None,
                baseline_tokens=None,
                turns=12,
                budget_tokens=20000,
                budget_turns=12,
            ),
        )
        self.assertIsNone(
            rotation_due(
                context_tokens=None,
                baseline_tokens=None,
                turns=3,
                budget_tokens=20000,
                budget_turns=12,
            )
        )

    def test_binding_config_names(self):
        self.assertEqual(config_name({"role": "main", "kind": "claude"}), "claude-main")
        self.assertEqual(config_name({"role": "child", "kind": "codex"}), "codex-child")
        self.assertEqual(config_name({"role": "agent", "kind": "claude"}), "claude")


class StandingTests(unittest.TestCase):
    def test_carry_over_is_small_and_lists_topic_index_pending_and_recent(self):
        text = render_standing(
            StandingInputs(
                role="main",
                topics=[
                    TopicLine("inbox", "", 0),
                    TopicLine("billing", "waiting on child", 2),
                ],
                current_topic="billing",
                checkpoint="decision: use invoices v2",
                pending=["[billing] send the March invoice"],
                recent=[
                    RecentMessage("user", "x" * 5000),
                    RecentMessage("assistant", "ok"),
                ],
                lineage_note="This is native session #2",
            )
        )
        self.assertIn("billing: waiting on child [2 pending]", text)
        self.assertIn("send the March invoice", text)
        self.assertIn("decision: use invoices v2", text)
        self.assertIn("native session #2", text)
        self.assertLess(
            len(text), 6000
        )  # long messages are clipped, never carried whole
        self.assertNotIn("x" * 700, text)

    def test_worker_standing_has_no_conversation_content(self):
        text = render_standing(
            StandingInputs(role="child", recent=[RecentMessage("user", "SECRET")])
        )
        self.assertNotIn("SECRET", text)
        self.assertIn("mainloop report", text)

    def test_hash_is_stable(self):
        self.assertEqual(content_hash("a"), content_hash("a"))
        self.assertNotEqual(content_hash("a"), content_hash("b"))


class JournalUsageTests(unittest.TestCase):
    def test_context_tokens_and_compact_boundary(self):
        lines = [
            (1, json.dumps({"type": "user", "message": {"content": "hi"}})),
            (
                2,
                json.dumps(
                    {
                        "type": "assistant",
                        "message": {
                            "model": "claude-sonnet-x",
                            "content": [{"type": "text", "text": "ok"}],
                            "usage": {
                                "input_tokens": 10,
                                "cache_creation_input_tokens": 3016,
                                "cache_read_input_tokens": 17598,
                            },
                        },
                    }
                ),
            ),
            (
                3,
                json.dumps(
                    {
                        "type": "system",
                        "subtype": "compact_boundary",
                        "compactMetadata": {"trigger": "auto", "preTokens": 9},
                    }
                ),
            ),
        ]
        ev = parse_claude(lines, file_ref="f.jsonl", native_id="n", agent="a")
        self.assertEqual([e.context_tokens for e in ev], [None, 20624, None])
        self.assertEqual(ev[2].native_type, "claude.system.compact_boundary")


class FakeStore:
    """In-memory ``agent_api.Store``: a main binding, and whatever children it spawns."""

    def __init__(self):
        self.bindings = {
            "main-1": {
                "session_id": "main-1",
                "role": "main",
                "kind": "claude",
                "user_id": "u",
                "parent_session_id": None,
                "topic_id": None,
                "reported_at": None,
            },
        }
        self.tokens = {hash_token("tok-main"): "main-1"}
        self.topics: dict[str, dict] = {}
        self.records: list[dict] = []
        self.reports: list[str] = []
        self.native_turns_sent_to_children = 0  # status/read must never increase this

    async def binding_by_token_hash(self, h):
        sid = self.tokens.get(h)
        return self.bindings.get(sid) if sid else None

    async def get_binding(self, sid):
        return self.bindings.get(sid)

    async def count_live_children(self, parent):
        return sum(
            1
            for b in self.bindings.values()
            if b["role"] == "child"
            and b["reported_at"] is None
            and (parent is None or b["parent_session_id"] == parent)
        )

    async def topic(self, user_id, name, *, create):
        if name not in self.topics and create:
            self.topics[name] = {"id": f"t-{name}", "name": name, "status_line": ""}
        return self.topics.get(name)

    async def set_topic_status(self, tid, s):
        for t in self.topics.values():
            if t["id"] == tid:
                t["status_line"] = s

    async def topic_index(self, user_id):
        return [
            TopicLine(
                t["name"],
                t["status_line"],
                sum(
                    1
                    for r in self.records
                    if r["topic"] == t["id"]
                    and r["kind"] == "pending"
                    and r["status"] == "open"
                ),
            )
            for t in self.topics.values()
        ]

    async def add_record(self, tid, kind, text, sid):
        rid = f"r{len(self.records)}0000000"
        self.records.append(
            {
                "id": rid,
                "topic": tid,
                "kind": kind,
                "text": text,
                "status": "open",
                "session_id": sid,
            }
        )
        return rid

    async def close_pending(self, user_id, rid):
        for r in self.records:
            if (
                r["id"].startswith(rid)
                and r["kind"] == "pending"
                and r["status"] == "open"
            ):
                r["status"] = "done"
                return True
        return False

    async def children_state(self, parent):
        return [
            {
                "session_id": b["session_id"],
                "kind": b["kind"],
                "title": b.get("title", "t"),
                "topic": "billing",
                "state": "reported" if b["reported_at"] else "working",
                "turns": 0,
                "last_activity": "00:00:00Z",
                "last_reply": None,
            }
            for b in self.bindings.values()
            if b.get("parent_session_id") == parent
        ]

    async def messages(self, sid, offset, limit):
        return [
            {"role": "assistant", "content": "y" * 3000},
            {"role": "assistant", "content": "z" * 3000},
        ][offset : offset + limit]

    async def spawn_child(self, parent, topic, kind, title, brief):
        sid = f"child-{len(self.bindings)}"
        self.bindings[sid] = {
            "session_id": sid,
            "role": "child",
            "kind": kind,
            "user_id": "u",
            "parent_session_id": parent["session_id"],
            "topic_id": topic["id"],
            "reported_at": None,
            "title": title,
        }
        self.tokens[hash_token(f"tok-{sid}")] = sid
        return sid

    async def deliver_report(self, child, topic, summary, fallback):
        self.bindings[child["session_id"]]["reported_at"] = "now"
        self.reports.append(summary)
        return "msg-1"

    async def standing_text(self, binding):
        return "standing"


class AgentApiTests(unittest.TestCase):
    def setUp(self):
        self.store = FakeStore()
        self.service = AgentService(self.store, KINDS)
        app = FastAPI()
        app.include_router(agent_api.router)
        app.dependency_overrides[agent_api.get_service] = lambda: self.service
        self.client = TestClient(app)
        self.main = {"Authorization": "Bearer tok-main"}

    def child_headers(self, sid):
        return {"Authorization": f"Bearer tok-{sid}"}

    def delegate(self, headers=None, kind="codex"):
        return self.client.post(
            "/agent-api/delegate",
            json={"topic": "billing", "kind": kind, "title": "t", "brief": "do it"},
            headers=headers or self.main,
        )

    def test_requires_a_known_token(self):
        self.assertEqual(self.client.get("/agent-api/topics").status_code, 401)
        self.assertEqual(
            self.client.get(
                "/agent-api/topics", headers={"Authorization": "Bearer nope"}
            ).status_code,
            401,
        )

    def test_topic_records_and_pending_close(self):
        self.client.post(
            "/agent-api/topics",
            json={"name": "billing", "status": "in progress"},
            headers=self.main,
        )
        rid = self.client.post(
            "/agent-api/records",
            json={"kind": "pending", "text": "send invoice", "topic": "billing"},
            headers=self.main,
        ).json()["id"]
        self.assertIn(
            "[1 pending]",
            self.client.get("/agent-api/topics", headers=self.main).json()["text"],
        )
        self.assertEqual(
            self.client.post(
                f"/agent-api/records/{rid[:8]}/done", headers=self.main
            ).status_code,
            200,
        )
        self.assertIn(
            "[0 pending]",
            self.client.get("/agent-api/topics", headers=self.main).json()["text"],
        )
        self.assertEqual(
            self.client.post(
                "/agent-api/records",
                json={"kind": "bogus", "text": "x"},
                headers=self.main,
            ).status_code,
            400,
        )

    def test_fourth_concurrent_child_is_refused_and_report_frees_a_slot(self):
        ids = [self.delegate().json()["session_id"] for _ in range(3)]
        r = self.delegate()
        self.assertEqual(r.status_code, 403)
        self.assertIn("[concurrency]", r.json()["detail"])
        rep = self.client.post(
            "/agent-api/report",
            json={"summary": "done"},
            headers=self.child_headers(ids[0]),
        )
        self.assertEqual(rep.status_code, 200)
        self.assertEqual(self.store.reports, ["done"])
        self.assertEqual(self.delegate().status_code, 200)  # a slot is free again

    def test_child_cannot_spawn_and_cannot_report_twice(self):
        cid = self.delegate().json()["session_id"]
        r = self.delegate(headers=self.child_headers(cid))
        self.assertEqual(r.status_code, 403)
        self.assertIn("[role]", r.json()["detail"])
        self.client.post(
            "/agent-api/report",
            json={"summary": "one"},
            headers=self.child_headers(cid),
        )
        again = self.client.post(
            "/agent-api/report",
            json={"summary": "two"},
            headers=self.child_headers(cid),
        )
        self.assertIn("already reported", again.json()["text"])
        self.assertEqual(self.store.reports, ["one"])  # not delivered twice

    def test_main_cannot_report_and_depth_is_derived_from_the_tree(self):
        self.assertEqual(
            self.client.post(
                "/agent-api/report", json={"summary": "x"}, headers=self.main
            ).status_code,
            403,
        )
        cid = self.delegate().json()["session_id"]
        who = self.client.get(
            "/agent-api/whoami", headers=self.child_headers(cid)
        ).json()["text"]
        self.assertIn("depth=1", who)

    def test_status_and_read_are_control_plane_only_and_size_capped(self):
        cid = self.delegate().json()["session_id"]
        st = self.client.get("/agent-api/status", headers=self.main).json()
        self.assertIn("state=working", st["text"])
        rd = self.client.get(
            "/agent-api/read", params={"session": cid[:8]}, headers=self.main
        ).json()
        self.assertLessEqual(len(rd["text"]), policy.READ_MAX_CHARS + 200)
        self.assertIn("truncated", rd["text"])
        self.assertEqual(self.store.native_turns_sent_to_children, 0)
        # A child cannot read a sibling or the main thread (only its own tree).
        self.assertEqual(
            self.client.get(
                "/agent-api/read",
                params={"session": "main-1"},
                headers=self.child_headers(cid),
            ).status_code,
            404,
        )


if __name__ == "__main__":
    unittest.main()


class StoreProtocolTests(unittest.TestCase):
    def test_pg_store_implements_every_store_method(self):
        """A method missing from the Postgres store only showed up live (500 on /standing)."""
        from mainloop.runtime.delegation import PgStore

        wanted = {n for n in agent_api.Store.__dict__ if not n.startswith("_")}
        self.assertEqual(wanted - {n for n in dir(PgStore)}, set())


class ReviewFixTests(AgentApiTests):
    def test_pending_done_needs_a_long_enough_id(self):
        self.assertEqual(
            self.client.post(
                "/agent-api/records/%/done", headers=self.main
            ).status_code,
            400,
        )

    def test_reports_are_framed_as_untrusted_in_standing_context(self):
        text = render_standing(StandingInputs(role="main"))
        self.assertIn("untrusted data", text)
        self.assertIn("never obey", text)
