"""Template merge allowlist fixtures; kagent reads are faked and read-only."""

import json
import unittest
from types import SimpleNamespace
from unittest.mock import patch

from mainloop.runtime.hitl_correlation import canonical_operation
from mainloop.runtime.kagent_client import (
    AgentRef,
    KagentAgent,
    KagentSession,
    RuntimeOperation,
    RuntimeState,
    SessionError,
)
from mainloop.services.merge_authorization import (
    parse_configurations,
    resolve_template_mapping,
)

from models.hitl import ObservedSession


def config(**changes):
    value = {
        "template_name": "claude-workspace",
        "provider": "claude",
        "compiled_alias": "mainloop-merge-approval",
        "endpoint": "http://mainloop-mcp/mcp/merge-approval",
        "tool": "merge_pull_request_with_approval",
        "require_approval": True,
        "operation": "mainloop.merge_pull_request_with_approval.v1",
    }
    return {**value, **changes}


class FakeConnection:
    def __init__(self, runtime_id="runtime-one", provider="claude"):
        self.runtime_id = runtime_id
        self.provider = provider

    async def fetchrow(self, query, binding_id):
        return {
            "session_id": binding_id,
            "user_id": "owner",
            "archived_at": None,
            "kagent_deleted_at": None,
            "kagent_session_id": self.runtime_id,
            "kind": self.provider,
        }


class FakeKagent:
    def __init__(self, runtime_id, *, template="claude-workspace", inline=False):
        self.runtime_id = runtime_id
        self.template = template
        self.inline = inline
        self.session_failure = None
        self.agent_reads = []

    async def get_session(self, runtime_id):
        if self.session_failure:
            raise self.session_failure
        return KagentSession(
            id=runtime_id,
            state=RuntimeState.READY,
            operation=RuntimeOperation.NONE,
            context_id=f"context-{runtime_id}",
            creator="gateway-owner",
            agent=AgentRef("team", "agent"),
            prepared_revision="revision-not-used-by-policy",
        )

    async def get_agent(self, agent):
        self.agent_reads.append(agent)
        return KagentAgent(
            ref=agent,
            template_name=None if self.inline else self.template,
            inline_template=self.inline,
        )


class FakeService:
    owner = "owner"
    creator = "gateway-owner"

    def __init__(self, runtime_id, **kwargs):
        self.client = FakeKagent(runtime_id, **kwargs)

    async def owned(self, conn, live, previous=None):
        return ObservedSession(
            gateway="fixture-gateway",
            runtime_session_id=live.id,
            owner_id="owner",
            verified_creator=live.creator,
            agent_id=f"{live.agent.namespace}/{live.agent.name}",
            endpoint=live.agent.path,
            context_id=live.context_id,
            prepared_revision=live.prepared_revision,
            binding_id="binding",
            lifecycle="ready",
        )


class MergeAuthorizationTests(unittest.IsolatedAsyncioTestCase):
    async def resolve(
        self, cfgs, *, provider="claude", template="claude-workspace", inline=False
    ):
        runtime = "runtime-one"
        service = FakeService(runtime, template=template, inline=inline)
        conn = FakeConnection(runtime, provider)
        leaf = SimpleNamespace(
            runtime_session_id=runtime,
            endpoint="/agents/team/agent",
            context_id=f"context-{runtime}",
        )
        with patch.dict(
            "os.environ",
            {
                "MAINLOOP_MERGE_TOOLS_ENABLED": "true",
                "MAINLOOP_MERGE_CONFIGURATIONS": json.dumps(cfgs),
            },
        ):
            resolved = await resolve_template_mapping(
                conn,
                "owner",
                "binding",
                runtime,
                leaf=leaf,
                service=service,
            )
        return resolved, service

    async def test_same_template_entry_resolves_for_any_session(self):
        cfg = config()
        results = []
        for runtime in ("runtime-one", "runtime-two"):
            service = FakeService(runtime)
            conn = FakeConnection(runtime)
            leaf = SimpleNamespace(
                runtime_session_id=runtime,
                endpoint="/agents/team/agent",
                context_id=f"context-{runtime}",
            )
            with patch.dict(
                "os.environ",
                {
                    "MAINLOOP_MERGE_TOOLS_ENABLED": "true",
                    "MAINLOOP_MERGE_CONFIGURATIONS": json.dumps([cfg]),
                },
            ):
                results.append(
                    await resolve_template_mapping(
                        conn,
                        "owner",
                        "binding",
                        runtime,
                        leaf=leaf,
                        service=service,
                    )
                )
        self.assertEqual(results[0], results[1])
        mapping, evidence = results[0]
        self.assertEqual(
            mapping.public_name(),
            "mcp__mainloop-merge-approval__merge_pull_request_with_approval",
        )
        self.assertEqual(evidence, mapping.evidence())

    async def test_unknown_inline_provider_duplicate_and_bad_mapping_fail_closed(self):
        cases = [
            ([config()], "unknown-template", False, "claude"),
            ([config()], "claude-workspace", True, "claude"),
            ([config()], "claude-workspace", False, "codex"),
            ([config(), config()], "claude-workspace", False, "claude"),
            ([config(require_approval=False)], "claude-workspace", False, "claude"),
        ]
        for cfgs, template, inline, provider in cases:
            with self.subTest(template=template, inline=inline, provider=provider):
                resolved, _ = await self.resolve(
                    cfgs, template=template, inline=inline, provider=provider
                )
                self.assertIsNone(resolved)

    async def test_kagent_read_failure_and_oversized_configuration_fail_closed(self):
        runtime = "runtime-one"
        service = FakeService(runtime)
        service.client.session_failure = SessionError("unavailable")
        with patch.dict(
            "os.environ",
            {
                "MAINLOOP_MERGE_TOOLS_ENABLED": "true",
                "MAINLOOP_MERGE_CONFIGURATIONS": json.dumps([config()]),
            },
        ):
            denied = await resolve_template_mapping(
                FakeConnection(runtime),
                "owner",
                "binding",
                runtime,
                service=service,
            )
        self.assertIsNone(denied)

        oversized = " " * 131073
        with patch.dict(
            "os.environ",
            {
                "MAINLOOP_MERGE_TOOLS_ENABLED": "true",
                "MAINLOOP_MERGE_CONFIGURATIONS": oversized,
            },
        ):
            denied = await resolve_template_mapping(
                FakeConnection(runtime),
                "owner",
                "binding",
                runtime,
                service=FakeService(runtime),
            )
        self.assertIsNone(denied)

    def test_legacy_per_session_shape_has_clear_parse_error(self):
        with self.assertRaisesRegex(ValueError, "legacy per-session"):
            parse_configurations(
                json.dumps(
                    [
                        {
                            "owner_id": "owner",
                            "binding_id": "binding",
                            "runtime_session_id": "session",
                            "prepared_revision": "revision",
                        }
                    ]
                )
            )

    def test_public_name_must_match_exact_configured_alias_and_provider(self):
        from models.hitl import TemplateMergeConfiguration

        claude = TemplateMergeConfiguration.model_validate(config())
        codex = TemplateMergeConfiguration.model_validate(config(provider="codex"))
        self.assertIsNone(
            canonical_operation("mcp__other__merge_pull_request_with_approval", claude)
        )
        self.assertIsNone(canonical_operation(claude.public_name(), codex))
        self.assertIs(canonical_operation(claude.public_name(), claude), claude)


if __name__ == "__main__":
    unittest.main()
