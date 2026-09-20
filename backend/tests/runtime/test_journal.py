"""Real-journal shapes (Claude transcript, Codex rollout), hand-written and sanitized.

The records mirror the measured structure of Claude Code 2.1.278 and codex-cli 0.155.1
journals; they contain no captured session content, instructions or credentials.
"""

import json
import unittest

from mainloop.runtime.journal import completed_turns, parse_journal, unwrap_paste

CLAUDE_ID = "11111111-2222-3333-4444-555555555555"
CODEX_ID = "01a0beec-0000-7000-8000-000000000000"


def numbered(records: list[dict], start: int = 1) -> list[tuple[int, str]]:
    return [(i, json.dumps(r)) for i, r in enumerate(records, start)]


CLAUDE_TURN = [
    {"type": "mode", "mode": "normal", "sessionId": CLAUDE_ID},
    {
        "type": "user",
        "sessionId": CLAUDE_ID,
        "timestamp": "2026-09-20T13:04:57.797Z",
        "message": {
            "role": "user",
            "content": '\n\n<pasted_content id="5871">\nhello nonce-1\n</pasted_content id="5871">',
        },
    },
    {
        "type": "assistant",
        "sessionId": CLAUDE_ID,
        "timestamp": "2026-09-20T13:05:07.587Z",
        "message": {
            "role": "assistant",
            "model": "claude-sonnet-5",
            "content": [{"type": "thinking", "thinking": ""}],
        },
    },
    {
        "type": "assistant",
        "sessionId": CLAUDE_ID,
        "timestamp": "2026-09-20T13:05:07.621Z",
        "message": {
            "role": "assistant",
            "model": "claude-sonnet-5",
            "stop_reason": "end_turn",
            "content": [{"type": "text", "text": "PONG-1"}],
        },
    },
    {
        "type": "system",
        "subtype": "turn_duration",
        "sessionId": CLAUDE_ID,
        "timestamp": "2026-09-20T13:05:07.650Z",
    },
    {"type": "ai-title", "sessionId": CLAUDE_ID},
]

CODEX_TURN = [
    {"type": "session_meta", "payload": {"id": CODEX_ID}},
    {"type": "event_msg", "payload": {"type": "task_started"}},
    {"type": "turn_context", "payload": {"model": "gpt-test"}},
    {
        "type": "response_item",
        "payload": {
            "type": "message",
            "role": "user",
            "content": [{"type": "input_text", "text": "hello nonce-2"}],
        },
    },
    {
        "type": "response_item",
        "payload": {
            "type": "message",
            "role": "assistant",
            "phase": "final_answer",
            "content": [{"type": "output_text", "text": "PONG-2"}],
        },
    },
    {
        "type": "event_msg",
        "payload": {"type": "task_complete", "last_agent_message": "PONG-2"},
    },
]


class JournalTests(unittest.TestCase):
    def test_unwrap_paste_handles_id_on_closing_tag(self):
        self.assertEqual(
            unwrap_paste('<pasted_content id="1">\nhi\n</pasted_content id="1">'), "hi"
        )
        self.assertEqual(unwrap_paste("plain"), "plain")

    def test_claude_turn_prompt_reply_completion_model(self):
        events = parse_journal(
            "claude",
            numbered(CLAUDE_TURN),
            file_ref="s.jsonl",
            native_id=CLAUDE_ID,
            agent="a",
        )
        kinds = [e.kind for e in events]
        self.assertEqual(
            kinds, ["other", "prompt", "other", "reply", "turn_complete", "other"]
        )
        self.assertEqual(events[1].text, "hello nonce-1")
        # The existing adapter classifies the real records it can understand.
        self.assertEqual(events[3].normalized_type, "output")
        self.assertEqual(events[4].normalized_type, "completed")
        turns, safe = completed_turns(events)
        self.assertEqual(
            [(t.reply, t.model, t.end_cursor) for t in turns],
            [("PONG-1", "claude-sonnet-5", 5)],
        )
        self.assertEqual(turns[0].evidence_ref, "s.jsonl#L5")
        self.assertEqual(safe, 6)

    def test_open_turn_is_not_persisted_and_cursor_stays_before_it(self):
        events = parse_journal(
            "claude",
            numbered(CLAUDE_TURN[:4]),
            file_ref="s.jsonl",
            native_id=CLAUDE_ID,
            agent="a",
        )
        turns, safe = completed_turns(events)
        self.assertEqual(turns, [])
        self.assertEqual(safe, 1)  # re-read from the prompt next time

    def test_codex_turn(self):
        events = parse_journal(
            "codex",
            numbered(CODEX_TURN),
            file_ref="r.jsonl",
            native_id=CODEX_ID,
            agent="a",
        )
        self.assertEqual(events[3].kind, "prompt")
        self.assertEqual(events[5].normalized_type, "completed")
        turns, safe = completed_turns(events)
        self.assertEqual([(t.reply, t.model) for t in turns], [("PONG-2", "gpt-test")])
        self.assertEqual(safe, 6)
        self.assertIn(4, turns[0].prompt_cursors)

    def test_malformed_and_unknown_lines_are_ignored(self):
        lines = [
            (1, "not json"),
            (2, json.dumps({"type": "queue-operation"})),
            (3, "[]"),
        ]
        self.assertEqual(
            len(
                parse_journal(
                    "claude", lines, file_ref="s", native_id=CLAUDE_ID, agent="a"
                )
            ),
            1,
        )

    def test_unknown_kind_is_rejected(self):
        with self.assertRaises(ValueError):
            parse_journal("pi", [], file_ref="s", native_id="x", agent="a")


if __name__ == "__main__":
    unittest.main()
