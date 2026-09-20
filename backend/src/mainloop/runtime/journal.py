"""Read real native journals (Claude transcript JSONL, Codex rollout JSONL).

The journal is the authority for receipts, replies, completion and model. Herdr only
delivers input and reports liveness. ``NativeEvent`` carries no text, so reply text is
extracted here from the raw record; the same record is also passed through the existing
adapters (``ClaudeSessionNormalizer``, ``observe_codex_event``) after a small translation
from the real journal shape to the shape those adapters were written against. A record
the adapters reject is still usable evidence here: ``normalized_type`` is then ``None``.

Measured against Claude Code 2.1.278 and codex-cli 0.155.1 (see docs/spikes).
"""

from __future__ import annotations

import json
import re
from collections.abc import Iterable
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import Any, Literal

from mainloop.runtime.claude import ClaudeSessionNormalizer
from mainloop.runtime.codex import observe_codex_event

from models.native_agent import NativeBinding

EventKind = Literal["prompt", "reply", "turn_complete", "turn_aborted", "other"]

_PASTED = re.compile(
    r"\A\s*<pasted_content id=\"[^\"]*\">\n?(.*?)\n?</pasted_content(?: id=\"[^\"]*\")?>\s*\Z",
    re.DOTALL,
)


@dataclass(frozen=True, slots=True)
class JournalEvent:
    cursor: int  # 1-based line number in the journal file
    kind: EventKind
    evidence_ref: str  # "<file basename>#L<line>"
    native_type: str
    text: str | None = None
    model: str | None = None
    at: str | None = None
    normalized_type: str | None = (
        None  # from the existing adapter, when it accepts the record
    )
    # Claude: input + cache_creation + cache_read tokens of this call = the whole context the
    # model saw (measured, E3). None when the record carries no usage.
    context_tokens: int | None = None


def unwrap_paste(text: str) -> str:
    """Claude Code wraps pasted (Herdr-delivered) input in ``<pasted_content>`` tags."""
    match = _PASTED.match(text)
    return (match.group(1) if match else text).strip()


def _text_blocks(content: Any, block_types: tuple[str, ...]) -> str:
    if isinstance(content, str):
        return content
    if not isinstance(content, list):
        return ""
    parts = [
        b.get("text", "")
        for b in content
        if isinstance(b, dict)
        and b.get("type") in block_types
        and isinstance(b.get("text"), str)
    ]
    return "\n".join(p for p in parts if p)


def _binding(kind: str, native_id: str, agent: str) -> NativeBinding:
    return NativeBinding(
        binding_id=f"{kind}-{native_id}",
        workspace_id="herdr-spike/workspace-0",
        provider=kind,
        runtime_type=f"{kind}-native-cli",
        native_session_id=native_id,
        herdr_session_id="mainloop-spike",
        herdr_agent_id=agent,
        creation_mode="created",
        ownership_generation=1,
    )


def _iso(value: Any) -> str | None:
    return value if isinstance(value, str) else None


_EPOCH = datetime.fromtimestamp(0, tz=UTC)


def parse_claude(
    lines: Iterable[tuple[int, str]], *, file_ref: str, native_id: str, agent: str
) -> list[JournalEvent]:
    normalizer = ClaudeSessionNormalizer(_binding("claude", native_id, agent))
    out: list[JournalEvent] = []
    for cursor, line in lines:
        try:
            rec = json.loads(line)
        except json.JSONDecodeError:
            continue
        if not isinstance(rec, dict):
            continue
        rtype = str(rec.get("type", ""))
        subtype = rec.get("subtype") if isinstance(rec.get("subtype"), str) else None
        msg = rec.get("message") if isinstance(rec.get("message"), dict) else {}
        kind: EventKind = "other"
        text = None
        model = None
        if rtype == "user" and not rec.get("isMeta"):
            content = msg.get("content")
            if isinstance(content, str) or (
                isinstance(content, list)
                and not any(
                    isinstance(b, dict) and b.get("type") == "tool_result"
                    for b in content
                )
            ):
                text = unwrap_paste(_text_blocks(content, ("text",)))
                kind = "prompt" if text else "other"
        elif rtype == "assistant":
            text = _text_blocks(msg.get("content"), ("text",)) or None
            kind = "reply" if text else "other"
            if isinstance(msg.get("model"), str) and not msg["model"].startswith("<"):
                model = msg["model"]
        elif rtype == "system" and subtype == "turn_duration":
            kind = "turn_complete"
        ctx_tokens = _context_tokens(msg.get("usage")) if rtype == "assistant" else None
        ref = f"{file_ref}#L{cursor}"
        normalized = _claude_normalize(normalizer, rec, cursor, ref, native_id, kind)
        out.append(
            JournalEvent(
                cursor,
                kind,
                ref,
                f"claude.{rtype}" + (f".{subtype}" if subtype else ""),
                text,
                model,
                _iso(rec.get("timestamp")),
                normalized,
                ctx_tokens,
            )
        )
    return out


def _context_tokens(usage: Any) -> int | None:
    if not isinstance(usage, dict):
        return None
    parts = [
        usage.get(k)
        for k in (
            "input_tokens",
            "cache_creation_input_tokens",
            "cache_read_input_tokens",
        )
    ]
    if not any(isinstance(p, int) for p in parts):
        return None
    return sum(p for p in parts if isinstance(p, int))


def _claude_normalize(
    normalizer: ClaudeSessionNormalizer,
    rec: dict,
    cursor: int,
    ref: str,
    native_id: str,
    kind: EventKind,
) -> str | None:
    """Existing adapter classification. Real transcripts use sessionId (camelCase) and
    signal turn end with system/turn_duration, which the stream-json adapter does not know.
    """
    event = {
        k: v
        for k, v in rec.items()
        if k
        in (
            "type",
            "subtype",
            "uuid",
            "message",
            "usage",
            "timestamp",
            "error",
            "version",
        )
    }
    if kind == "turn_complete":
        event = {
            "type": "result",
            "subtype": "success",
            "is_error": False,
            "timestamp": rec.get("timestamp"),
        }
    event["session_id"] = native_id
    try:
        raw = {
            "source_cursor": cursor,
            "raw_evidence_ref": ref,
            "event": {k: v for k, v in event.items() if v is not None},
        }
        return normalizer.normalize(raw, ingested_at=datetime.now(UTC)).normalized_type
    except (ValueError, TypeError):
        return None


def parse_codex(
    lines: Iterable[tuple[int, str]], *, file_ref: str, native_id: str, agent: str
) -> list[JournalEvent]:
    binding = _binding("codex", native_id, agent)
    out: list[JournalEvent] = []
    model: str | None = None
    for cursor, line in lines:
        try:
            rec = json.loads(line)
        except json.JSONDecodeError:
            continue
        if not isinstance(rec, dict):
            continue
        rtype = str(rec.get("type", ""))
        payload = rec.get("payload") if isinstance(rec.get("payload"), dict) else {}
        ptype = payload.get("type") if isinstance(payload.get("type"), str) else None
        kind: EventKind = "other"
        text = None
        translated: dict | None = None
        if rtype == "turn_context" and isinstance(payload.get("model"), str):
            model = payload["model"]
        elif rtype == "event_msg" and ptype == "task_started":
            translated = {"type": "turn.started", "params": {"threadId": native_id}}
        elif rtype == "event_msg" and ptype == "task_complete":
            kind = "turn_complete"
            text = (
                payload.get("last_agent_message")
                if isinstance(payload.get("last_agent_message"), str)
                else None
            )
            translated = {"type": "turn.completed", "params": {"threadId": native_id}}
        elif rtype == "event_msg" and ptype == "turn_aborted":
            kind = "turn_aborted"
            translated = {"type": "turn.interrupted", "params": {"threadId": native_id}}
        elif rtype == "response_item" and ptype == "message":
            role = payload.get("role")
            body = _text_blocks(payload.get("content"), ("input_text", "output_text"))
            if role == "user" and body:
                kind, text = "prompt", body
            elif (
                role == "assistant" and body and payload.get("phase") == "final_answer"
            ):
                kind, text = "reply", body
                translated = {
                    "type": "item.completed",
                    "params": {
                        "threadId": native_id,
                        "item": {"type": "agent_message", "text": body},
                    },
                }
        ref = f"{file_ref}#L{cursor}"
        normalized = None
        if translated is not None:
            try:
                normalized = observe_codex_event(
                    {**translated, "raw_evidence_ref": ref},
                    binding,
                    source_cursor=cursor,
                    ingested_at=datetime.now(UTC),
                ).event.normalized_type
            except (ValueError, TypeError, KeyError):
                normalized = None
        out.append(
            JournalEvent(
                cursor,
                kind,
                ref,
                f"codex.{rtype}" + (f".{ptype}" if ptype else ""),
                text,
                model if kind == "turn_complete" or rtype == "turn_context" else None,
                _iso(rec.get("timestamp")),
                normalized,
            )
        )
    return out


def parse_journal(
    kind: str,
    lines: Iterable[tuple[int, str]],
    *,
    file_ref: str,
    native_id: str,
    agent: str,
) -> list[JournalEvent]:
    if kind == "claude":
        return parse_claude(lines, file_ref=file_ref, native_id=native_id, agent=agent)
    if kind == "codex":
        return parse_codex(lines, file_ref=file_ref, native_id=native_id, agent=agent)
    raise ValueError(f"no journal reader for kind {kind}")


@dataclass(frozen=True, slots=True)
class Turn:
    """One completed native turn, from the first prompt record to the completion record."""

    prompt: str | None
    reply: str
    end_cursor: int
    evidence_ref: str
    model: str | None
    prompt_cursors: tuple[int, ...] = ()


def completed_turns(events: Iterable[JournalEvent]) -> tuple[list[Turn], int]:
    """Group events into completed turns. Returns (turns, safe_cursor).

    ``safe_cursor`` is the last line that ends a completed turn, or the last line seen when
    no turn is open; a partly written turn is re-read next time, so replies persist once.
    """
    turns: list[Turn] = []
    prompts: list[str] = []
    prompt_cursors: list[int] = []
    replies: list[str] = []
    model: str | None = None
    safe = 0
    open_turn = False
    last = 0
    for ev in events:
        last = ev.cursor
        if ev.model:
            model = ev.model
        if ev.kind == "prompt":
            open_turn = True
            prompts.append(ev.text or "")
            prompt_cursors.append(ev.cursor)
        elif ev.kind == "reply":
            open_turn = True
            replies.append(ev.text or "")
        elif ev.kind in ("turn_complete", "turn_aborted"):
            # Codex task_complete.last_agent_message repeats the final_answer text; it is only
            # the fallback when no reply record was seen.
            reply = "\n\n".join(r for r in replies if r) or (ev.text or "")
            turns.append(
                Turn(
                    prompts[-1] if prompts else None,
                    reply,
                    ev.cursor,
                    ev.evidence_ref,
                    model,
                    tuple(prompt_cursors),
                )
            )
            prompts, prompt_cursors, replies = [], [], []
            open_turn = False
            safe = ev.cursor
        elif not open_turn:
            safe = ev.cursor
    if not open_turn:
        safe = max(safe, last)
    return turns, safe
