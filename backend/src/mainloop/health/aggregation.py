"""Pure turn accounting. No prompt, response, argument or diagnostic text."""

from collections import Counter

from models.orchestration_health import ToolEvent, TurnEvent, UsageEvent


def summarize(events):
    events = sorted(events, key=lambda event: event.sequence)
    starts, results = {}, {}
    boundary = [event for event in events if isinstance(event, TurnEvent)]
    complete = bool(
        boundary
        and boundary[0].turn.state == "started"
        and boundary[-1].turn.state in {"finished", "failed"}
        and boundary[-1].turn.coverage == "complete"
        and [event.sequence for event in events]
        == list(range(1, events[-1].sequence + 1))
    )
    for event in events:
        if isinstance(event, ToolEvent):
            key = event.tool.call_key
            if event.tool.phase == "started":
                if key in starts:
                    complete = False
                starts[key] = event.tool
            else:
                if key in results:
                    complete = False
                results[key] = event.tool
    failed = {
        key: tool
        for key, tool in results.items()
        if tool.outcome in {"error", "denied"}
    }
    patterns = Counter(
        (tool.operation_key, tool.action, tool.path, tool.category)
        for tool in failed.values()
    )
    categories = Counter(tool.category for tool in failed.values())
    unresolved = len(starts.keys() - results.keys())
    if unresolved or results.keys() - starts.keys():
        complete = False
    for key in starts.keys() & results.keys():
        start, result = starts[key], results[key]
        if (start.tool_class, start.operation_key, start.action, start.path) != (
            result.tool_class,
            result.operation_key,
            result.action,
            result.path,
        ):
            complete = False
    usage = [event for event in events if isinstance(event, UsageEvent)]
    # A qualified result total replaces earlier iterations. Native-child usage
    # is not imported as exclusive samples, so inclusive totals are never added twice.
    latest = max(usage, key=lambda event: event.usage.revision).usage if usage else None
    if usage and [event.usage.revision for event in usage] != list(
        range(1, len(usage) + 1)
    ):
        complete = False
    usage_complete = bool(
        complete
        and latest
        and latest.coverage == "complete"
        and latest.input_tokens is not None
        and latest.output_tokens is not None
    )
    return {
        "failures": len(failed),
        "categories": dict(categories),
        "repeated_failure_groups": sum(count >= 3 for count in patterns.values()),
        "unresolved_calls": unresolved,
        "friction_coverage": "complete" if complete else "partial",
        "input_tokens": latest.input_tokens if latest else None,
        "output_tokens": latest.output_tokens if latest else None,
        "usage_coverage": (
            "complete" if usage_complete else "partial" if latest else "unknown"
        ),
        "inclusion": "tree_inclusive",
        "child_attribution": "unavailable",
    }
