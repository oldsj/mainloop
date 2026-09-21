"""Server-side spawn policy for the ``mainloop`` CLI (owner decision D7).

Agents cannot bypass these rules: the CLI only forwards requests, and every request is checked
here against control-plane state. Pure functions; callers pass in the counts they read.
"""

from __future__ import annotations

from dataclasses import dataclass

# Depth counts edges below the main thread: main=0, its child=1, a grandchild=2.
MAX_DEPTH = 2
MAX_CHILDREN_PER_PARENT = 3
MAX_CHILDREN_GLOBAL = 6
# Only the main thread delegates in this slice. Topic supervisors (next slice) will add a
# ``supervisor`` role that may spawn workers at depth 2.
SPAWN_ROLES = frozenset({"main"})


class PolicyError(Exception):
    """A request refused by policy. ``code`` is stable; ``message`` is shown to the agent."""

    def __init__(self, code: str, message: str):
        super().__init__(message)
        self.code = code
        self.message = message


@dataclass(frozen=True, slots=True)
class Actor:
    role: str  # main | child
    depth: int


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
    if actor.role not in SPAWN_ROLES:
        raise PolicyError(
            "role",
            f"spawn refused: a {actor.role} agent may not start agents "
            "(only the main thread delegates in this release; report instead)",
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
    if actor.role != "main":
        raise PolicyError(
            "role", "only the main thread can cancel or clear its child agents"
        )


def may_report(actor: Actor) -> None:
    if actor.role != "child":
        raise PolicyError("role", "only a child agent can report to its parent")


REPORT_MAX_CHARS = 4000
READ_MAX_CHARS = 4000
NOTE_MAX_CHARS = 2000
