"""Export the MCP tool contract from the Python registry to api/mcp-tools.json.

The file lists every tool with its input JSON Schema and the exact tools each kind of actor sees
on each MCP surface. The Go server tests its own tool set against it. Run from the repository
root: uv run --project backend python scripts/export-mcp-tools.py [--check]
"""

import argparse
import itertools
import json
import os
import sys
from pathlib import Path

os.environ.setdefault("MAINLOOP_MERGE_TOOLS_ENABLED", "false")

from mainloop.mcp_app import TOOLS  # noqa: E402
from mainloop.runtime import policy  # noqa: E402

OUT = Path(__file__).resolve().parent.parent / "api" / "mcp-tools.json"

# The retry and reassign tools only exist behind a handoff port the Go server drops.
DROPPED = {"task_retry", "task_reassign"}

ROLES = ("main", "supervisor", "child", "agent")
DEPTHS = (0, 1, 2)
GRANTS = ("none", "coordination", "workspace")


def visible(actor: policy.Actor, merge_enabled: bool) -> dict[str, list[str]]:
    os.environ["MAINLOOP_MERGE_TOOLS_ENABLED"] = "true" if merge_enabled else "false"
    return {
        surface: sorted(policy.surface_tools(actor, surface) - DROPPED)
        for surface in ("ordinary", "approval")
    }


def export() -> dict:
    tools = [
        {
            "name": name,
            "description": description,
            "input_schema": model.model_json_schema(),
        }
        for name, (model, description) in TOOLS.items()
        if name not in DROPPED
    ]
    visibility = []
    for merge_enabled in (False, True):
        # A merge-status-only actor sees the same tools whatever its role, depth or grant.
        seen = {
            json.dumps(visible(policy.Actor(r, d, g, True), merge_enabled))
            for r, d, g in itertools.product(ROLES, DEPTHS, GRANTS)
        }
        if len(seen) != 1:
            raise SystemExit("merge_status_only depends on role, depth or grant")
        surfaces = json.loads(seen.pop())
        if any(surfaces.values()):
            visibility.append(
                {
                    "role": None,
                    "depth": None,
                    "grant": None,
                    "merge_status_only": True,
                    "merge_tools_enabled": merge_enabled,
                    **surfaces,
                }
            )
    for role, depth, grant, merge_enabled in itertools.product(
        ROLES, DEPTHS, GRANTS, (False, True)
    ):
        surfaces = visible(policy.Actor(role, depth, grant, False), merge_enabled)
        if not any(surfaces.values()):
            continue  # every actor not listed sees nothing
        visibility.append(
            {
                "role": role,
                "depth": depth,
                "grant": grant,
                "merge_status_only": False,
                "merge_tools_enabled": merge_enabled,
                **surfaces,
            }
        )
    return {
        "version": 1,
        "surfaces": {"ordinary": "/mcp", "approval": "/mcp/merge-approval"},
        "tools": tools,
        "visibility": visibility,
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--check", action="store_true", help="fail if the file is out of date"
    )
    args = parser.parse_args()
    text = json.dumps(export(), indent=2, sort_keys=False) + "\n"
    if args.check:
        # Compare the data, not the bytes: the repository formatter reflows the file.
        if not OUT.exists() or json.loads(OUT.read_text()) != json.loads(text):
            print("api/mcp-tools.json is out of date", file=sys.stderr)
            return 1
        return 0
    OUT.write_text(text)
    return 0


if __name__ == "__main__":
    sys.exit(main())
