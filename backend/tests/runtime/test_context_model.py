"""Context model (plan r7) with fakes only: no cluster, no agents, no Postgres, no credentials."""

import unittest

from mainloop.runtime import agent_tools, policy
from mainloop.runtime.agent_identity import hash_token
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
            )
        )
        self.assertIn("billing: waiting on child [2 pending]", text)
        self.assertIn("send the March invoice", text)
        self.assertIn("decision: use invoices v2", text)
        self.assertLess(
            len(text), 6000
        )  # long messages are clipped, never carried whole
        self.assertNotIn("x" * 700, text)

    def test_worker_standing_has_no_conversation_content(self):
        text = render_standing(
            StandingInputs(role="child", recent=[RecentMessage("user", "SECRET")])
        )
        self.assertNotIn("SECRET", text)
        self.assertIn("`report` tool exactly once", text)

    def test_hash_is_stable(self):
        self.assertEqual(content_hash("a"), content_hash("a"))
        self.assertNotEqual(content_hash("a"), content_hash("b"))


class FakeStore:
    """In-memory ``agent_tools.Store``: a main binding, and whatever children it spawns."""

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
        self.statuses: dict[str, str] = {}
        self.cancelled: list[str] = []
        self.archived: set[str] = set()
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
                "status": self.statuses.get(b["session_id"], "active"),
                "state": (
                    "cancelled"
                    if self.statuses.get(b["session_id"]) == "cancelled"
                    else "reported" if b["reported_at"] else "working"
                ),
                "turns": 0,
                "last_activity": "00:00:00Z",
                "last_reply": None,
            }
            for b in self.bindings.values()
            if b.get("parent_session_id") == parent
            and b["session_id"] not in self.archived
        ]

    async def cancel_session(self, sid):
        self.statuses[sid] = "cancelled"
        self.cancelled.append(sid)
        return "stopped"

    async def archive_children(self, user_id, parent, ids):
        done = [
            b["session_id"]
            for b in self.bindings.values()
            if b.get("parent_session_id") == parent
            and b["session_id"] not in self.archived
            and self.statuses.get(b["session_id"])
            in ("completed", "failed", "cancelled")
            and (ids is None or b["session_id"] in ids)
        ]
        self.archived.update(done)
        return done

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


class StoreProtocolTests(unittest.TestCase):
    def test_pg_store_implements_every_store_method(self):
        """A method missing from the Postgres store only showed up live (500 on /standing)."""
        from mainloop.runtime.delegation import PgStore

        wanted = {n for n in agent_tools.Store.__dict__ if not n.startswith("_")}
        self.assertEqual(wanted - {n for n in dir(PgStore)}, set())
