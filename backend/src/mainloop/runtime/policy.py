"""Server-side spawn policy for the Mainloop MCP tools (owner decision D7).

Agents cannot bypass these rules: the MCP transport forwards requests, and every request is checked
here against control-plane state. Pure functions; callers pass in the counts they read.
"""

from __future__ import annotations

import os
from dataclasses import dataclass

# Depth counts task edges: main=0, supervisor=1, child=2.
MAX_DEPTH = 2
MAX_CHILDREN_PER_PARENT = 3
MAX_CHILDREN_GLOBAL = 6
# The durable task service resolves actual hierarchy and atomically enforces capacity.
SPAWN_ROLES = frozenset({"main", "supervisor"})


class PolicyError(Exception):
    """A request refused by policy. ``code`` is stable; ``message`` is shown to the agent."""

    def __init__(self, code: str, message: str):
        super().__init__(message)
        self.code = code
        self.message = message


@dataclass(frozen=True, slots=True)
class Actor:
    role: str  # main | supervisor | child | agent
    depth: int
    mcp_grant_kind: str = "coordination"
    merge_status_only: bool = False


def check_spawn(
    actor: Actor,
    *,
    kind: str,
    allowed_kinds: frozenset[str],
    live_children_of_actor: int,
    live_children_global: int,
) -> None:
    """Raise ``PolicyError`` unless ``actor`` may start one more child agent."""
    if kind not in allowed_kinds:
        raise PolicyError(
            "kind",
            f"agent kind {kind!r} is not allowed (allowed: {sorted(allowed_kinds)})",
        )
    if actor.depth + 1 > MAX_DEPTH:
        raise PolicyError(
            "depth",
            f"spawn refused: would create a level-{actor.depth + 2} agent; "
            f"the maximum depth below the main thread is {MAX_DEPTH}",
        )
    if (actor.role, actor.depth) not in (("main", 0), ("supervisor", 1)):
        raise PolicyError(
            "role",
            f"spawn refused: a {actor.role} agent may not start agents "
            "(only main and task supervisors delegate; report instead)",
        )
    if live_children_of_actor >= MAX_CHILDREN_PER_PARENT:
        raise PolicyError(
            "concurrency",
            f"spawn refused: {live_children_of_actor} children are already running "
            f"(limit {MAX_CHILDREN_PER_PARENT}); wait for a report before delegating more",
        )
    if live_children_global >= MAX_CHILDREN_GLOBAL:
        raise PolicyError(
            "global-concurrency",
            f"spawn refused: {live_children_global} children are running system-wide "
            f"(limit {MAX_CHILDREN_GLOBAL})",
        )


def may_manage_children(actor: Actor) -> None:
    if (actor.role, actor.depth) not in (("main", 0), ("supervisor", 1)):
        raise PolicyError(
            "role", "only main and supervisors can manage their direct tasks"
        )


def may_report(actor: Actor) -> None:
    may_report_task(actor)


REPORT_MAX_CHARS = 4000
READ_MAX_CHARS = 4000
NOTE_MAX_CHARS = 2000


# Discovery and direct invocation share the same exact role/depth/grant table.
TASK_READ_TOOLS = frozenset({"task_get", "task_list", "task_history"})
TASK_MANAGEMENT_TOOLS = frozenset({"delegate", "task_cancel"})
WORKSPACE_TOOLS = frozenset({"whoami", "open_pull_request"})
MERGE_TOOLS = frozenset(
    {
        "prepare_pull_request_merge",
        "merge_pull_request",
        "merge_pull_request_with_approval",
        "get_pull_request_merge_status",
    }
)


def tools_for(actor: Actor) -> frozenset[str]:
    if actor.merge_status_only:
        return (
            frozenset({"get_pull_request_merge_status"})
            if os.environ.get("MAINLOOP_MERGE_TOOLS_ENABLED") == "true"
            else frozenset()
        )
    if (actor.role, actor.depth, actor.mcp_grant_kind) == ("main", 0, "coordination"):
        tools = frozenset(
            {
                "whoami",
                "topics",
                "topic_open",
                "note",
                "decide",
                "pending_add",
                "pending_done",
                "open_pull_request",
            }
        )
        tools |= TASK_READ_TOOLS | TASK_MANAGEMENT_TOOLS
    elif (actor.role, actor.depth) in (
        ("supervisor", 1),
        ("child", 2),
    ) and actor.mcp_grant_kind in ("workspace", "coordination"):
        tools = frozenset({"whoami", "report"}) | TASK_READ_TOOLS
        if actor.role == "supervisor":
            tools |= TASK_MANAGEMENT_TOOLS
        if actor.mcp_grant_kind == "workspace":
            tools |= WORKSPACE_TOOLS
    elif (actor.role, actor.depth, actor.mcp_grant_kind) == ("agent", 0, "workspace"):
        tools = WORKSPACE_TOOLS
    else:
        tools = frozenset()
    if tools & TASK_MANAGEMENT_TOOLS:
        from mainloop.tasks.service import ports

        if ports.handoff is not None:
            tools |= {"task_retry", "task_reassign"}
    if (
        "open_pull_request" in tools
        and os.environ.get("MAINLOOP_MERGE_TOOLS_ENABLED") == "true"
    ):
        tools |= MERGE_TOOLS
    return tools


def surface_tools(actor: Actor, surface: str = "ordinary") -> frozenset[str]:
    tools = tools_for(actor)
    if surface == "approval":
        return tools & {"merge_pull_request_with_approval"}
    if surface == "ordinary":
        return tools - {"merge_pull_request_with_approval"}
    return frozenset()


def may_call(actor: Actor, tool: str) -> None:
    if tool not in tools_for(actor):
        raise PolicyError("role", f"a {actor.role} agent may not call {tool}")


def may_report_task(actor: Actor) -> None:
    """Only current task supervisors and children may report."""
    if (actor.role, actor.depth) not in (("supervisor", 1), ("child", 2)):
        raise PolicyError("role", "invalid task reporting role/depth")
