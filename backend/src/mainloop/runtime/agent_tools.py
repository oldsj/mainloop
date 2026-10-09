"""Protocol-neutral Mainloop agent tool service."""

from __future__ import annotations

from dataclasses import asdict, dataclass
from typing import Protocol

from fastapi import HTTPException
from mainloop.config import settings
from mainloop.runtime import policy
from mainloop.runtime.agent_identity import hash_token
from mainloop.runtime.policy import Actor
from mainloop.runtime.standing import TopicLine

INBOX = "inbox"

# A session in one of these is done: nothing more will run and it can be cleared from the list.
FINISHED_STATUSES = frozenset({"completed", "failed", "cancelled"})


class Store(Protocol):
    async def binding_by_token_hash(self, token_hash: str) -> dict | None: ...
    async def get_binding(self, session_id: str) -> dict | None: ...
    async def topic(self, user_id: str, name: str, *, create: bool) -> dict | None: ...
    async def set_topic_status(self, topic_id: str, status_line: str) -> None: ...
    async def topic_index(self, user_id: str) -> list[TopicLine]: ...
    async def add_record(
        self, topic_id: str, kind: str, text: str, session_id: str | None
    ) -> str: ...
    async def close_pending(self, user_id: str, record_id: str) -> bool: ...
    async def task_principal(self, binding: dict): ...
    async def task_call(self, binding: dict, action: str, arguments: dict) -> dict: ...
    async def standing_text(self, binding: dict) -> str: ...


@dataclass(slots=True)
class Ctx:
    binding: dict
    actor: Actor


class AgentService:
    def __init__(self, store: Store, allowed_kinds: frozenset[str] | None = None):
        self.store = store
        self.allowed_kinds = (
            allowed_kinds
            if allowed_kinds is not None
            else frozenset(
                k.strip() for k in settings.native_child_kinds.split(",") if k.strip()
            )
        )

    async def open_pull_request(self, ctx: Ctx, **arguments) -> dict:
        from mainloop.services.github_creation import open_pull_request

        policy.may_call(ctx.actor, "open_pull_request")
        return await open_pull_request(ctx.binding, arguments)

    async def prepare_pull_request_merge(self, ctx: Ctx, **arguments) -> dict:
        from mainloop.services.merge import prepare

        policy.may_call(ctx.actor, "prepare_pull_request_merge")
        return await prepare(ctx.binding, arguments)

    async def merge_pull_request(self, ctx: Ctx, **arguments) -> dict:
        from mainloop.services.merge import auto_merge

        policy.may_call(ctx.actor, "merge_pull_request")
        return await auto_merge(ctx.binding, arguments)

    async def merge_pull_request_with_approval(self, ctx: Ctx, **arguments) -> dict:
        from mainloop.services.merge import execute

        policy.may_call(ctx.actor, "merge_pull_request_with_approval")
        return await execute(ctx.binding, arguments, approved=True)

    async def get_pull_request_merge_status(self, ctx: Ctx, **arguments) -> dict:
        from mainloop.services.merge import status

        policy.may_call(ctx.actor, "get_pull_request_merge_status")
        return await status(ctx.binding, arguments)

    async def authenticate(self, token: str) -> Ctx:
        binding = await self.store.binding_by_token_hash(hash_token(token))
        grant_kind = binding.get("mcp_grant_kind") if binding else None
        grant_matches_role = (binding or {}).get("role") and (
            (binding["role"], grant_kind)
            in {
                ("main", "coordination"),
                ("child", "coordination"),
                ("agent", "workspace"),
                ("supervisor", "coordination"),
                ("supervisor", "workspace"),
                ("child", "workspace"),
            }
        )
        if (
            binding is None
            or not grant_matches_role
            or binding.get("archived_at")
            or binding.get("status") in FINISHED_STATUSES
        ):
            raise HTTPException(status_code=401, detail="unknown agent token")
        depth = 0
        if binding["role"] in ("supervisor", "child"):
            from mainloop.tasks import lifecycle

            try:
                principal = await self.store.task_principal(binding)
                if principal is None:
                    raise ValueError("task principal required")
                depth = principal.depth
            except (lifecycle.LifecycleDenied, ValueError) as exc:
                from mainloop.services.merge import completed_status_binding, enabled

                if enabled() and grant_kind == "workspace":
                    depth = await completed_status_binding(binding)
                    if depth is not None:
                        return Ctx(
                            binding,
                            Actor(
                                binding["role"],
                                depth,
                                grant_kind,
                                merge_status_only=True,
                            ),
                        )
                raise HTTPException(401, detail="unknown agent token") from exc
        return Ctx(binding, Actor(binding["role"], depth, grant_kind))

    async def whoami(self, ctx: Ctx) -> dict:
        """Return only server-resolved, non-secret binding and workspace scope facts."""
        binding = ctx.binding
        project_id = binding.get("session_project_id")
        repository = branch = None
        scope_status = "not_enrolled"
        grant_status = "active"
        workspace_id = None
        if binding.get("mcp_grant_kind") == "workspace":
            workspace_id = binding["session_id"]
            from mainloop.services.workspace_authority import (
                ScopeUnavailable,
                workspace_identity,
            )

            try:
                repository, branch = workspace_identity(binding)
                scope_status = "available"
            except ScopeUnavailable:
                grant_status = "scope_unavailable"
                scope_status = "scope_unavailable"
        elif binding.get("mcp_grant_kind") not in ("coordination", "workspace"):
            grant_status = "not_enrolled"

        result = {
            "text": (
                f"{binding['role']} {binding['kind']} session={binding['session_id'][:8]} "
                f"depth={ctx.actor.depth} grant={grant_status} scope={scope_status}"
            ),
            "session_id": binding["session_id"],
            "role": binding["role"],
            "depth": ctx.actor.depth,
            "mcp_grant_kind": binding.get("mcp_grant_kind", "none"),
            "grant_status": grant_status,
            "scope_status": scope_status,
            "project_id": project_id,
            "workspace_id": workspace_id,
            "repository": repository,
            "branch": branch,
        }
        if binding["role"] in ("supervisor", "child"):
            result.update(await self.store.task_call(binding, "identity", {}))
        return result

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

    # Task commands share the owner application's durable service, using binding authority.
    async def _task(self, ctx: Ctx, name: str, arguments: dict) -> dict:
        policy.may_call(ctx.actor, name)
        return await self.store.task_call(ctx.binding, name, arguments)

    async def delegate(self, ctx: Ctx, **arguments) -> dict:
        return await self._task(ctx, "delegate", arguments)

    async def report(self, ctx: Ctx, **arguments) -> dict:
        return await self._task(ctx, "report", arguments)

    async def task_get(self, ctx: Ctx, **arguments) -> dict:
        return await self._task(ctx, "task_get", arguments)

    async def task_list(self, ctx: Ctx, **arguments) -> dict:
        return await self._task(ctx, "task_list", arguments)

    async def task_history(self, ctx: Ctx, **arguments) -> dict:
        return await self._task(ctx, "task_history", arguments)

    async def task_cancel(self, ctx: Ctx, **arguments) -> dict:
        return await self._task(ctx, "task_cancel", arguments)

    async def task_retry(self, ctx: Ctx, **arguments) -> dict:
        return await self._task(ctx, "task_retry", arguments)

    async def task_reassign(self, ctx: Ctx, **arguments) -> dict:
        return await self._task(ctx, "task_reassign", arguments)

    async def standing(self, ctx: Ctx) -> dict:
        return {"text": await self.store.standing_text(ctx.binding)}
