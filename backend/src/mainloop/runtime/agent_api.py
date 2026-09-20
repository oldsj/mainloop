"""Control-plane API used by the ``mainloop`` CLI inside agent workspaces.

Authentication is a per-binding token (HMAC of the session id, hash stored on the binding).
The token identifies the acting binding; the CLI never names itself, and every verb is limited
to that binding's own tree. Policy (depth, concurrency, allowed roles) is enforced here.
LIMIT: this scopes the CLI, it is not a security boundary. The rest of the backend API is
unauthenticated and reachable from the workspace pods, and tokens are readable by agents that
share a pod; a hostile agent could bypass this policy (see docs/spikes/native-main-thread-context.md).
Responses carry a rendered ``text`` so the CLI stays a thin, dumb client.
"""

from __future__ import annotations

import hashlib
import hmac
from dataclasses import asdict, dataclass
from typing import Annotated, Any, Protocol

from fastapi import APIRouter, Depends, Header, HTTPException
from mainloop.config import settings
from mainloop.runtime import policy
from mainloop.runtime.policy import Actor, PolicyError
from mainloop.runtime.standing import TopicLine
from pydantic import BaseModel, Field

INBOX = "inbox"
_LIVE = ("failed", "cancelled", "completed")


def token_for(session_id: str) -> str:
    key = settings.agent_token_key or settings.db_password
    if not key:
        raise RuntimeError(
            "AGENT_TOKEN_KEY (or DB password) must be set to issue agent tokens"
        )
    return (
        "ml_" + hmac.new(key.encode(), session_id.encode(), hashlib.sha256).hexdigest()
    )


def hash_token(token: str) -> str:
    return hashlib.sha256(token.encode()).hexdigest()


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
        if binding is None:
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
            "arrive in this thread. Use `mainloop status` to check it.",
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


# -- FastAPI wiring ---------------------------------------------------------------------------
router = APIRouter(prefix="/agent-api", tags=["agent-api"])
_service: AgentService | None = None


def get_service() -> AgentService:
    global _service
    if _service is None:
        from mainloop.runtime.delegation import PgStore

        _service = AgentService(PgStore())
    return _service


SvcDep = Annotated[AgentService, Depends(get_service)]


async def get_ctx(
    service: SvcDep,
    authorization: Annotated[str, Header()] = "",
) -> Ctx:
    scheme, _, token = authorization.partition(" ")
    if scheme.lower() != "bearer" or not token:
        raise HTTPException(status_code=401, detail="bearer token required")
    return await service.authenticate(token)


CtxDep = Annotated[Ctx, Depends(get_ctx)]


class TopicOpen(BaseModel):
    name: str
    status: str | None = None


class RecordIn(BaseModel):
    kind: str
    text: str
    topic: str | None = None


class DelegateIn(BaseModel):
    topic: str = INBOX
    kind: str
    title: str = ""
    brief: str


class ReportIn(BaseModel):
    summary: str = Field(..., min_length=1)


@router.get("/whoami")
async def whoami(ctx: CtxDep) -> dict[str, Any]:
    b = ctx.binding
    return {
        "text": f"{b['role']} {b['kind']} session={b['session_id'][:8]} depth={ctx.actor.depth}"
    }


@router.get("/topics")
async def topics(ctx: CtxDep, s: SvcDep):
    return await s.topics(ctx)


@router.post("/topics")
async def topic_open(body: TopicOpen, ctx: CtxDep, s: SvcDep):
    return await s.topic_open(ctx, body.name, body.status)


@router.post("/records")
async def record(body: RecordIn, ctx: CtxDep, s: SvcDep):
    return await s.record(ctx, body.kind, body.text, body.topic)


@router.post("/records/{record_id}/done")
async def done(record_id: str, ctx: CtxDep, s: SvcDep):
    return await s.done(ctx, record_id)


@router.post("/delegate")
async def delegate(
    body: DelegateIn,
    ctx: CtxDep,
    s: SvcDep,
):
    return await s.delegate(ctx, body.topic, body.kind, body.title, body.brief)


@router.post("/report")
async def report(body: ReportIn, ctx: CtxDep, s: SvcDep):
    return await s.report(ctx, body.summary)


@router.get("/status")
async def status(
    ctx: CtxDep,
    s: SvcDep,
    session: str | None = None,
):
    return await s.status(ctx, session)


@router.get("/read")
async def read(
    session: str,
    ctx: CtxDep,
    s: SvcDep,
    since: int = 0,
):
    return await s.read(ctx, session, since)


@router.get("/standing")
async def standing(ctx: CtxDep, s: SvcDep):
    return await s.standing(ctx)
