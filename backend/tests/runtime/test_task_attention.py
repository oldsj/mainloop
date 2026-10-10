"""Attention is native input state, independent of merge eligibility."""

import unittest
from types import SimpleNamespace

from mainloop.tasks import attention

from models.hitl import (
    ContinuationIdentity,
    HITLProjection,
    LeafIdentity,
    ToolApprovalRequest,
    normalized_hash,
)


def card():
    payload = ToolApprovalRequest.model_validate(
        {
            "type": "tool_approval_request",
            "tools": [
                {
                    "id": "retry",
                    "call_id": "native-call",
                    "name": "merge",
                    "args": {"proposal_id": "expired"},
                }
            ],
        }
    )
    identity = dict(
        gateway="gateway",
        endpoint="http://fixture",
        runtime_session_id="runtime",
        context_id="context",
        task_id="native-task",
        request_hash=normalized_hash(payload.model_dump(mode="json")),
    )
    return HITLProjection(
        id="unanswered-retry",
        owner_id="owner",
        outer=ContinuationIdentity(**identity, status_message_id="message"),
        payload=payload,
        leaves=(
            LeafIdentity(
                **identity,
                owner_id="owner",
                binding_id="binding",
                pending_request_id="request",
            ),
        ),
        availability="pending",
    )


class AttentionTests(unittest.IsolatedAsyncioTestCase):
    async def test_stale_merge_retry_remains_attention_until_answered(self):
        request = card()
        task = SimpleNamespace(owner_id="owner", id="task")
        attempt = SimpleNamespace(
            id="attempt", binding_id="binding", writer_generation=1
        )

        class Connection:
            answered = False

            async def fetch(self, query, *_):
                return [{"id": request.id, "snapshot": request.model_dump(mode="json")}]

            async def fetchval(self, query, *_):
                if "SELECT projection" in query:
                    return {}
                return 1 if self.answered else None

            async def fetchrow(self, query, *_):
                return {
                    "binding_id": "binding",
                    "active_proposal_id": "expired",
                    "state": "expired",
                    "facts": {"attempt_id": "attempt", "writer_generation": 1},
                }

        conn = Connection()
        # Old merge filtering drops this still-unanswered native retry.
        self.assertEqual(
            await attention.pending(conn, task, attempt, "runtime"), (request.id,)
        )
        self.assertEqual(
            await attention.pending(conn, task, attempt, "other-runtime"), ()
        )
        conn.answered = True
        self.assertEqual(await attention.pending(conn, task, attempt, "runtime"), ())
