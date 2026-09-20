"""Herdr adapter over a fake pod-exec transport: no cluster, no agents, no credentials."""

import asyncio
import unittest

from mainloop.runtime.herdr import ExecResult, HerdrWorkspace, TransportError


class FakeWorkspace(HerdrWorkspace):
    def __init__(self, results):
        super().__init__(namespace="ns", pod="pod")
        self.results = list(results)
        self.calls: list[list[str]] = []

    async def _exec(self, command, timeout=45):
        self.calls.append(command)
        result = self.results.pop(0)
        if isinstance(result, Exception):
            raise result
        return result


def run(coro):
    return asyncio.run(coro)


class HerdrAdapterTests(unittest.TestCase):
    def test_send_is_one_exec_and_transport_error_is_not_retried(self):
        ws = FakeWorkspace([TransportError("boom")])
        with self.assertRaises(TransportError):
            run(ws.send("agent", "hi"))
        self.assertEqual(
            ws.calls, [["agentctl", "send", "agent", "hi"]]
        )  # exactly one attempt

    def test_prompt_text_is_argv_not_shell(self):
        ws = FakeWorkspace([ExecResult(0, "sent\n", "")])
        run(ws.send("agent", "a; rm -rf / $(x) 'q'"))
        self.assertEqual(ws.calls[0][-1], "a; rm -rf / $(x) 'q'")

    def test_journal_parses_header_and_numbered_lines(self):
        out = '#file\t/w/.claude/projects/p/s.jsonl\t3\n2\t{"a":1}\n3\t{"b":2}\n'
        ws = FakeWorkspace([ExecResult(0, out, "")])
        sl = run(ws.journal("agent", "sid", 1))
        self.assertEqual(
            (sl.file, sl.total_lines, sl.lines),
            ("/w/.claude/projects/p/s.jsonl", 3, [(2, '{"a":1}'), (3, '{"b":2}')]),
        )
        self.assertEqual(ws.calls[0], ["agentctl", "journal", "agent", "sid", "1"])

    def test_missing_journal(self):
        ws = FakeWorkspace([ExecResult(0, "#nofile\n", "")])
        self.assertIsNone(run(ws.journal("agent", "sid", 0)).file)

    def test_start_uses_resume_or_new_id(self):
        ident = '{"pane_id":"w1:p1","terminal_id":"t"}\n'
        ws = FakeWorkspace([ExecResult(0, ident, ""), ExecResult(0, ident, "")])
        run(ws.start("claude", "n", native_id="sid", resume=False))
        run(ws.start("claude", "n", native_id="sid", resume=True))
        self.assertEqual(ws.calls[0][-2:], ["--new-id", "sid"])
        self.assertEqual(ws.calls[1][-2:], ["--resume", "sid"])

    def test_status_none_when_agent_not_live(self):
        ws = FakeWorkspace([ExecResult(1, "", "no agent")])
        self.assertIsNone(run(ws.agent_status("n")))


if __name__ == "__main__":
    unittest.main()
