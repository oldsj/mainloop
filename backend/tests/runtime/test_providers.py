"""Provider configuration and routing, with scoped fixture evidence and no live agents."""

import unittest
from unittest.mock import AsyncMock, patch

import httpx
from mainloop import api
from mainloop.config import Settings, settings
from mainloop.providers import ProviderRegistry, registry
from mainloop.runtime import native_sessions, workspaces
from mainloop.runtime.agent_tools import AgentService, Ctx
from mainloop.runtime.policy import Actor
from pydantic import ValidationError

from models import CapabilityResult, CapabilityState, SessionCreate, WorkspaceManifest
from models.agent_tools import Delegate
from models.provider import ProviderProfile


def fixture(**changes):
    return ProviderProfile.model_validate(
        {
            "id": "codex-review",
            "display_name": "Codex review",
            "native_provider": "codex",
            "configuration_revision": "fixture-v1",
            "aliases": ["review"],
            "agents": {
                "agent": {"namespace": "workspace-team", "name": "review-workspace"},
                "child": {"namespace": "child-team", "name": "review-child"},
            },
            "capabilities": [
                {
                    "capability": "native_create",
                    "state": "proved",
                    "scope": "fixture",
                    "evidence_ref": "tests/runtime/test_providers.py",
                }
            ],
            **changes,
        }
    )


class ProviderTests(unittest.TestCase):
    def test_legacy_defaults_reproduce_every_role_and_namespace(self):
        with patch.object(settings, "kagent_namespace", "team"):
            for kind in ("claude", "codex"):
                for role, expected in (
                    ("main", settings.kagent_main_agent),
                    ("child", getattr(settings, f"kagent_{kind}_agent")),
                    ("agent", getattr(settings, f"kagent_workspace_{kind}_agent")),
                ):
                    ref = native_sessions.agent_ref(kind, role)
                    self.assertEqual((ref.namespace, ref.name), ("team", expected))
            self.assertTrue(
                all(
                    c.state == CapabilityState.UNKNOWN
                    for p in registry().profiles
                    for c in p.capabilities
                )
            )

    def test_custom_profile_alias_and_role_specific_namespace(self):
        with patch.object(settings, "provider_profiles", [fixture()]):
            for kind in ("review", "codex-review"):
                self.assertEqual(
                    registry().resolve(kind, "agent", selecting=True).id, "codex-review"
                )
                ref = native_sessions.agent_ref(kind, "child")
                self.assertEqual(
                    (ref.namespace, ref.name), ("child-team", "review-child")
                )
            with self.assertRaises(ValueError):
                registry().resolve("review", "main", selecting=True)
            with self.assertRaises(ValueError):
                registry().resolve("unconfigured", "child")

    def test_disabled_profile_still_routes_existing_identity(self):
        with patch.object(settings, "provider_profiles", [fixture(enabled=False)]):
            self.assertEqual(
                native_sessions.agent_name("codex-review"), "review-workspace"
            )
            with self.assertRaises(ValueError):
                registry().resolve("review", "agent", selecting=True)

    def test_operator_json_configuration_and_legacy_override(self):
        import json

        with patch.dict(
            "os.environ",
            {"PROVIDER_PROFILES": json.dumps([fixture().model_dump(mode="json")])},
        ):
            configured = Settings(_env_file=None)
        self.assertEqual(configured.provider_profiles[0].id, "codex-review")
        legacy = fixture(id="codex", aliases=[])
        with patch.object(settings, "provider_profiles", [legacy]):
            self.assertEqual(len(registry().profiles), 2)
            self.assertEqual(native_sessions.agent_name("codex"), "review-workspace")

    def test_invalid_operator_wiring_fails_closed(self):
        for changes in (
            {"runtime_adapter": "model-loop"},
            {"native_provider": "pi"},
            {"agents": {}},
            {"agents": {"agent": {"namespace": "../evil", "name": "agent"}}},
            {"enabled": "false"},
            {"secret": "caller-secret"},
            {"capabilities": [{"capability": "resume", "state": "proved"}]},
        ):
            with self.subTest(changes=changes), self.assertRaises(ValidationError):
                fixture(**changes)
        with self.assertRaises(ValueError):
            ProviderRegistry([fixture(), fixture()])
        for profiles in (
            [fixture(aliases=["claude"])],
            [fixture(id="claude")],
            [fixture(), fixture()],
        ):
            with self.assertRaises(ValidationError):
                Settings(_env_file=None, provider_profiles=profiles)

    def test_evidence_states_are_not_promoted(self):
        profile = fixture(
            capabilities=[
                CapabilityResult(
                    capability="import", state=CapabilityState.UNSUPPORTED
                ),
                CapabilityResult(capability="resume"),
                CapabilityResult(
                    capability="create",
                    state=CapabilityState.PARTIAL,
                    scope="fixture",
                    evidence_ref="fixture:create",
                ),
            ]
        )
        self.assertEqual(
            [c.state.value for c in profile.capabilities],
            ["unsupported", "unknown", "partial"],
        )

    def test_models_accept_ids_without_caller_owned_agent_configuration(self):
        self.assertEqual(
            Delegate(kind="codex-review", brief="review").kind, "codex-review"
        )
        self.assertEqual(
            SessionCreate(
                title="t", description="d", prompt="p", agent_kind="codex-review"
            ).agent_kind,
            "codex-review",
        )
        for body in (
            {"kind": "review", "brief": "b", "agents": {}},
            {"kind": "review", "brief": "b", "secret": "s"},
        ):
            with self.assertRaises(ValidationError):
                Delegate(**body)


class ProviderApiTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        for patcher in (
            patch.object(settings, "api_hosts", "test"),
            patch.object(settings, "provider_profiles", [fixture()]),
        ):
            patcher.start()
            self.addCleanup(patcher.stop)
        self.client = httpx.AsyncClient(
            transport=httpx.ASGITransport(app=api.app), base_url="http://test"
        )
        self.addAsyncCleanup(self.client.aclose)

    async def test_owner_list_has_scoped_evidence_and_no_credentials(self):
        response = await self.client.get(
            "/providers", headers={"X-User-ID": "someone-else"}
        )
        self.assertEqual(response.status_code, 200)
        profile = response.json()[-1]
        self.assertEqual(profile["id"], "codex-review")
        self.assertEqual(profile["capabilities"][0]["scope"], "fixture")
        self.assertNotIn("secret", response.text.lower())
        # There is no write endpoint on the owner-owned registry.
        self.assertEqual(
            (await self.client.post("/providers", json={})).status_code, 405
        )

    async def test_bad_selection_rejected_before_any_records(self):
        for profiles, kind in (
            ([fixture()], "missing"),
            ([fixture(enabled=False)], "review"),
            (
                [fixture(agents={"child": {"namespace": "team", "name": "child"}})],
                "review",
            ),
        ):
            with patch.object(settings, "provider_profiles", profiles), patch.object(
                workspaces, "project_for_repo", AsyncMock()
            ) as project, patch.object(
                api.db, "create_conversation", AsyncMock()
            ) as conversation:
                response = await self.client.post(
                    "/workspaces", json={"repo": "example/app", "agent_kind": kind}
                )
                self.assertEqual(response.status_code, 422, response.text)
                project.assert_not_called()
                response = await self.client.post(
                    "/sessions",
                    json={
                        "title": "t",
                        "description": "d",
                        "prompt": "p",
                        "agent_kind": kind,
                    },
                )
                self.assertEqual(response.status_code, 422, response.text)
                conversation.assert_not_called()

    async def test_workspace_alias_is_canonicalized_before_creation(self):
        from datetime import UTC, datetime

        from models import WorkspaceLifecycle, WorkspaceObservedState

        async def create(owner, project, manifest):
            self.assertEqual(manifest.agent_kind, "codex-review")
            return WorkspaceLifecycle(
                workspace_id="w",
                session_id="w",
                observed_state=WorkspaceObservedState.UNKNOWN,
                manifest=manifest,
                updated_at=datetime.now(UTC),
            )

        with patch.object(
            workspaces,
            "project_for_repo",
            AsyncMock(
                return_value={
                    "id": "p",
                    "html_url": "https://github.com/example/app",
                    "default_branch": "main",
                }
            ),
        ), patch.object(
            workspaces, "create", AsyncMock(side_effect=create)
        ), patch.object(
            workspaces, "publish", AsyncMock()
        ):
            response = await self.client.post(
                "/workspaces", json={"repo": "example/app", "agent_kind": "review"}
            )
        self.assertEqual(response.status_code, 201, response.text)

    async def test_disabled_profile_cannot_create_binding_or_workspace(self):
        with patch.object(
            settings, "provider_profiles", [fixture(enabled=False)]
        ), patch.object(
            native_sessions.ledger, "create_binding", AsyncMock()
        ) as binding, patch.object(
            workspaces.db, "connection"
        ) as connection:
            with self.assertRaises(ValueError):
                await native_sessions.create_binding(
                    "new-session", "review", role="child"
                )
            with self.assertRaises(workspaces.WorkspaceRejected):
                await workspaces.create(
                    "owner",
                    "project",
                    WorkspaceManifest(
                        repo_url="https://github.com/example/app",
                        branch="b",
                        agent_kind="review",
                    ),
                )
            binding.assert_not_called()
            connection.assert_not_called()

    async def test_delegation_resolves_alias_and_keeps_policy_caps(self):
        store = AsyncMock()
        store.count_live_children.return_value = 0
        store.topic.return_value = {"name": "inbox"}
        store.spawn_child.return_value = "child-session"
        service = AgentService(store, allowed_kinds=frozenset({"review"}))
        ctx = Ctx({"session_id": "parent", "user_id": "owner"}, Actor("main", 0))
        await service.delegate(ctx, "inbox", "review", "review", "brief")
        self.assertEqual(store.spawn_child.call_args.args[2], "codex-review")
        store.spawn_child.reset_mock()
        store.count_live_children.return_value = 6
        from fastapi import HTTPException

        with self.assertRaises(HTTPException):
            await service.delegate(ctx, "inbox", "review", "review", "brief")
        store.spawn_child.assert_not_called()
