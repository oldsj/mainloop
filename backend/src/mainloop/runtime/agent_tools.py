"""Protocol-neutral Mainloop agent tool service."""

from __future__ import annotations

from dataclasses import asdict, dataclass
from typing import Protocol

from fastapi import HTTPException
from mainloop.config import settings
from mainloop.runtime import policy
from mainloop.runtime.agent_identity import hash_token
from mainloop.runtime.policy import Actor, PolicyError
from mainloop.runtime.standing import TopicLine

INBOX = "inbox"

# A session in one of these is done: nothing more will run and it can be cleared from the list.
FINISHED_STATUSES = frozenset({"completed", "failed", "cancelled"})


class Store(Protocol):
    async def binding_by_token_hash(self, token_hash: str) -> dict | None: ...
    async def get_binding(self, session_id: str) -> dict | None: ...
    async def count_live_children(self, parent_session_id: str | None) -> int: ...
    async def topic(self, user_id: str, name: str, *, create: bool) -> dict | None: ...
    async def set_topic_status(self, topic_id: str, status_line: str) -> None: ...
    async def topic_index(self, user_id: str) -> list[TopicLine]: ...
    async def add_record(
        self, topic_id: str, kind: str, text: str, session_id: str | None
    ) -> str: ...
    async def close_pending(self, user_id: str, record_id: str) -> bool: ...
    async def children_state(self, parent_session_id: str) -> list[dict]: ...
    async def messages(
        self, session_id: str, offset: int, limit: int
    ) -> list[dict]: ...
    async def cancel_session(self, session_id: str) -> str: ...
    async def archive_children(
        self, user_id: str, parent_session_id: str, session_ids: list[str] | None
    ) -> list[str]: ...
    async def spawn_child(
        self, parent: dict, topic: dict, kind: str, title: str, brief: str
    ) -> str: ...
    async def deliver_report(
        self, child: dict, topic: dict | None, summary: str, fallback: bool
    ) -> str: ...
    async def standing_text(self, binding: dict) -> str: ...


@dataclass(slots=True)
class Ctx:
    binding: dict
    actor: Actor


class AgentService:
    def __init__(self, store: Store, allowed_kinds: frozenset[str] | None = None):
        self.store = store
        self.allowed_kinds = allowed_kinds or frozenset(
            k.strip() for k in settings.native_child_kinds.split(",") if k.strip()
        )

    async def authenticate(self, token: str) -> Ctx:
        binding = await self.store.binding_by_token_hash(hash_token(token))
        if (
            binding is None
            or binding.get("archived_at")
            or binding.get("status") in FINISHED_STATUSES
        ):
            raise HTTPException(status_code=401, detail="unknown agent token")
        return Ctx(binding, Actor(binding["role"], await self._depth(binding)))

    async def _depth(self, binding: dict) -> int:
        depth, cur, seen = 0, binding, set()
        while cur.get("parent_session_id") and cur["session_id"] not in seen:
            seen.add(cur["session_id"])
            depth += 1
            cur = await self.store.get_binding(cur["parent_session_id"]) or {}
        return depth

    # -- topics and records -------------------------------------------------------------
    async def topics(self, ctx: Ctx) -> dict:
        index = await self.store.topic_index(ctx.binding["user_id"])
        lines = [
            f"- {t.name}: {t.status_line or '(no status)'} [{t.pending} pending]"
            for t in index
        ]
        return {
            "text": "\n".join(lines) or "(no topics yet)",
            "topics": [asdict(t) for t in index],
        }

    async def topic_open(self, ctx: Ctx, name: str, status: str | None) -> dict:
        topic = await self.store.topic(
            ctx.binding["user_id"], name.strip() or INBOX, create=True
        )
        if status is not None:
            await self.store.set_topic_status(
                topic["id"], status[: policy.NOTE_MAX_CHARS]
            )
        return {"text": f"topic {topic['name']} ready", "topic": topic["name"]}

    async def record(self, ctx: Ctx, kind: str, text: str, topic: str | None) -> dict:
        if kind not in ("note", "decision", "pending"):
            raise HTTPException(
                status_code=400, detail="kind must be note, decision or pending"
            )
        if not text.strip():
            raise HTTPException(status_code=400, detail="text is required")
        t = await self.store.topic(ctx.binding["user_id"], topic or INBOX, create=True)
        rid = await self.store.add_record(
            t["id"],
            kind,
            text.strip()[: policy.NOTE_MAX_CHARS],
            ctx.binding["session_id"],
        )
        return {"text": f"{kind} recorded in {t['name']} ({rid[:8]})", "id": rid}

    async def done(self, ctx: Ctx, record_id: str) -> dict:
        if len(record_id) < 8:
            raise HTTPException(
                status_code=400, detail="give at least 8 characters of the pending id"
            )
        ok = await self.store.close_pending(ctx.binding["user_id"], record_id)
        if not ok:
            raise HTTPException(status_code=404, detail="no such open pending item")
        return {"text": "pending closed"}

    # -- delegation ------------------------------------------------------------------------
    async def delegate(
        self, ctx: Ctx, topic: str, kind: str, title: str, brief: str
    ) -> dict:
        if not brief.strip():
            raise HTTPException(status_code=400, detail="a task brief is required")
        sid = ctx.binding["session_id"]
        try:
            policy.check_spawn(
                ctx.actor,
                kind=kind,
                allowed_kinds=self.allowed_kinds,
                live_children_of_actor=await self.store.count_live_children(sid),
                live_children_global=await self.store.count_live_children(None),
            )
        except PolicyError as exc:
            raise HTTPException(
                status_code=403, detail=f"[{exc.code}] {exc.message}"
            ) from exc
        t = await self.store.topic(ctx.binding["user_id"], topic or INBOX, create=True)
        child_id = await self.store.spawn_child(
            ctx.binding, t, kind, title.strip() or "task", brief
        )
        return {
            "text": f"started {kind} child {child_id[:8]} for topic {t['name']}; its report will "
            "arrive in this thread. Use the `status` tool to check it.",
            "session_id": child_id,
        }

    async def report(self, ctx: Ctx, summary: str, *, fallback: bool = False) -> dict:
        try:
            policy.may_report(ctx.actor)
        except PolicyError as exc:
            raise HTTPException(
                status_code=403, detail=f"[{exc.code}] {exc.message}"
            ) from exc
        if ctx.binding.get("reported_at") is not None:
            return {"text": "already reported; nothing more to do"}
        topic = None
        if ctx.binding.get("topic_id"):
            topic = {"id": ctx.binding["topic_id"]}
        mid = await self.store.deliver_report(
            ctx.binding, topic, summary.strip()[: policy.REPORT_MAX_CHARS], fallback
        )
        return {
            "text": "report recorded and delivered to the main thread",
            "message_id": mid,
        }

    # -- cleanup: end a child, clear finished ones from the list -------------------------------
    async def _child(self, ctx: Ctx, session_id: str) -> dict:
        rows = await self.store.children_state(ctx.binding["session_id"])
        match = [r for r in rows if r["session_id"].startswith(session_id)]
        if not match:
            raise HTTPException(status_code=404, detail="no such child in your tree")
        if len(match) > 1:
            raise HTTPException(
                status_code=400, detail="ambiguous session id; give more characters"
            )
        return match[0]

    @staticmethod
    def _require_manager(ctx: Ctx) -> None:
        try:
            policy.may_manage_children(ctx.actor)
        except PolicyError as exc:
            raise HTTPException(
                status_code=403, detail=f"[{exc.code}] {exc.message}"
            ) from exc

    async def cancel(self, ctx: Ctx, session_id: str) -> dict:
        self._require_manager(ctx)
        child = await self._child(ctx, session_id)
        short = child["session_id"][:8]
        if child["status"] in FINISHED_STATUSES:
            return {"text": f"{short} is already {child['status']}; nothing to cancel"}
        outcome = await self.store.cancel_session(child["session_id"])
        text = f"cancelled {short}"
        if outcome == "unknown":
            text += "; stopping its agent could not be confirmed, so it may still be running"
        return {"text": text, "agent": outcome}

    async def clear(self, ctx: Ctx, session_id: str | None) -> dict:
        """Clear finished children from the user's list (kept for audit, never deleted)."""
        self._require_manager(ctx)
        parent = ctx.binding["session_id"]
        only = (
            [(await self._child(ctx, session_id))["session_id"]] if session_id else None
        )
        archived = await self.store.archive_children(
            ctx.binding["user_id"], parent, only
        )
        left = [
            r
            for r in await self.store.children_state(parent)
            if (only is None or r["session_id"] in only)
            and r["status"] not in FINISHED_STATUSES
        ]
        text = f"cleared {len(archived)} finished child session(s)"
        if left:
            names = ", ".join(r["session_id"][:8] for r in left)
            text += (
                f"; not cleared because they are still running or waiting: {names} "
                "(call the `cancel` tool first)"
            )
        return {"text": text, "cleared": archived}

    # -- state, answered from Postgres only (no native turn) ----------------------------------
    async def status(self, ctx: Ctx, session_id: str | None) -> dict:
        rows = await self.store.children_state(ctx.binding["session_id"])
        if session_id:
            rows = [r for r in rows if r["session_id"].startswith(session_id)]
        if not rows:
            return {
                "text": (
                    "no children" if not session_id else "no such child in your tree"
                )
            }
        lines = []
        for r in rows:
            lines.append(
                f"- {r['session_id'][:8]} {r['kind']} '{r['title']}' topic={r['topic']} "
                f"state={r['state']} turns={r['turns']} last_activity={r['last_activity']}"
                + (
                    f"\n    last reply: {r['last_reply']}"
                    if r.get("last_reply")
                    else ""
                )
            )
        return {"text": "\n".join(lines), "children": rows}

    async def read(self, ctx: Ctx, session_id: str, since: int) -> dict:
        rows = await self.store.children_state(ctx.binding["session_id"])
        match = [r for r in rows if r["session_id"].startswith(session_id)]
        if not match:
            raise HTTPException(status_code=404, detail="no such child in your tree")
        msgs = await self.store.messages(match[0]["session_id"], since, 20)
        out, used = [], 0
        for i, m in enumerate(msgs, start=since + 1):
            line = f"#{i} {m['role']}: {m['content']}"
            if used + len(line) > policy.READ_MAX_CHARS:
                out.append(f"... truncated; continue with --since {i - 1}")
                break
            out.append(line)
            used += len(line)
        return {
            "text": "\n".join(out) or "(nothing new)",
            "next_since": since + len(msgs),
        }

    async def standing(self, ctx: Ctx) -> dict:
        return {"text": await self.store.standing_text(ctx.binding)}
