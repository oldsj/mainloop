"""Environment pinning through real workspace SQL and a fake kagent gateway."""

import json
import uuid
from dataclasses import replace
from unittest.mock import patch

from mainloop.db import environments as store
from mainloop.runtime import native_sessions as ns
from mainloop.runtime import workspaces
from mainloop.runtime.kagent_client import OutcomeUnknown, RuntimeComposition
from tests.runtime.test_postgres_ledger import KagentFakeCase
from tests.test_workspace_environments import (
    composition_reply,
    malformed_composition_fields,
    validated_version,
)

from models.environment import DevEnvironment, SelectEnvironment
from models.workspace import WorkspaceManifest, WorkspaceObservedState


class WorkspaceEnvironmentPostgresTests(KagentFakeCase):
    async def asyncSetUp(self):
        await super().asyncSetUp()
        self.fake.next_session_ids = [str(uuid.uuid4()) for _ in range(3)]
        self.project_id = "project-" + self.user
        self.env_id = "env-" + self.user
        self.v1 = "v1-" + self.user
        self.v2 = "v2-" + self.user
        self.pending = "pending-" + self.user
        await self.pool.execute(
            "INSERT INTO projects(id,user_id,owner,name,full_name,html_url) VALUES($2,$1,'example','app','example/app','https://github.com/example/app')",
            self.user,
            self.project_id,
        )
        self.version = validated_version().model_copy(
            update={"id": self.v1, "environment_id": self.env_id}
        )
        self.env = DevEnvironment(
            id=self.env_id, owner_id=self.user, name="dev", source_kind="prebuilt_image"
        )
        async with self.pool.acquire() as conn:
            async with conn.transaction():
                await store.register(conn, self.env, self.version)
                await store.set_default(conn, self.env_id, self.user, self.v1)
                await store.select(
                    conn,
                    self.project_id,
                    self.user,
                    SelectEnvironment(
                        environment_id=self.env_id,
                        follow_default=True,
                        expected_version=0,
                    ),
                )
        patcher = patch.dict(
            "os.environ", {"WORKSPACE_DEVELOPMENT_PLATFORM": "linux/arm64"}
        )
        patcher.start()
        self.addCleanup(patcher.stop)

    async def create(self, branch="feature"):
        return await workspaces.create(
            self.user,
            self.project_id,
            WorkspaceManifest(repo_url="https://github.com/example/app", branch=branch),
        )

    async def test_persisted_recovery_replacement_and_reported_composition(self):
        client = ns.get_client()
        create = client.create_session
        calls = []

        async def lost(agent, **kwargs):
            calls.append(kwargs)
            session = await create(agent, **kwargs)
            if len(calls) == 1:
                raise OutcomeUnknown("fixture lost reply")
            return replace(
                session,
                development_environment=kwargs["development_environment"],
                runtime_composition=RuntimeComposition("payload", "claude", 1, "1.0"),
            )

        with patch.object(client, "create_session", lost):
            lifecycle = await self.create()
            wid = lifecycle.workspace_id
            self.assertEqual(
                lifecycle.manifest.development_environment.version_id, self.v1
            )
            async with self.pool.acquire() as conn:
                await store.add_version(
                    conn,
                    self.version.model_copy(
                        update={
                            "id": self.v2,
                            "platform_manifest_digest": "sha256:" + "c" * 64,
                        }
                    ),
                )
                await store.set_default(conn, self.env_id, self.user, self.v2)
            await workspaces.refresh(wid, self.user)
            self.assertEqual(calls[0], calls[1])
            wire = self.fake.session_calls("CreateSession")
            self.assertEqual(wire[0], wire[1])
            binding = await ns.get_binding(wid)
            binding["kagent_request_id"] = "replacement"
            binding["kagent_session_id"] = (
                None  # replacement has no confirmed runtime yet
            )
            await ns._create_session_with_credentials(binding, ())
        self.assertEqual(
            calls[2]["development_environment"], calls[0]["development_environment"]
        )
        row = await self.pool.fetchrow(
            "SELECT * FROM workspaces WHERE session_id=$1", wid
        )
        self.assertEqual(
            json.loads(row["development_environment"])["version_id"], self.v1
        )
        self.assertEqual(
            json.loads(row["reported_development_environment"])["policy_identity"],
            self.v1 + ":oci-static-v2",
        )
        self.assertEqual(json.loads(row["runtime_composition"])["cli_version"], "1.0")
        self.assertEqual(
            (await self.create("next")).manifest.development_environment.version_id,
            self.v2,
        )

    async def test_invalid_selection_never_calls_kagent_or_persists_workspace(self):
        await self.pool.execute(
            "UPDATE dev_environments SET owner_id=$1,snapshot=jsonb_set(snapshot,'{owner_id}',to_jsonb($1::text)) WHERE id=$2",
            "other",
            self.env_id,
        )
        async with self.pool.acquire() as conn:
            async with conn.transaction():
                await store.grant(conn, self.env_id, "other", self.project_id, "use")
                shared = await store.environment(conn, self.env_id)
                self.assertTrue(
                    await store.access(conn, shared, self.project_id, self.user)
                )
                await store.grant(conn, self.env_id, "other", self.project_id, None)
        with self.assertRaisesRegex(workspaces.WorkspaceRejected, "revoked"):
            await self.create()
        self.assertEqual(self.fake.session_calls("CreateSession"), [])
        await self.pool.execute(
            "UPDATE dev_environments SET owner_id=$1,snapshot=jsonb_set(snapshot,'{owner_id}',to_jsonb($1::text)) WHERE id=$2",
            self.user,
            self.env_id,
        )
        with patch.dict(
            "os.environ", {"WORKSPACE_DEVELOPMENT_PLATFORM": "linux/amd64"}
        ):
            with self.assertRaisesRegex(workspaces.WorkspaceRejected, "platform"):
                await self.create()
        async with self.pool.acquire() as conn:
            await store.add_version(
                conn,
                self.version.model_copy(
                    update={"id": self.pending, "validation_status": "pending_build"}
                ),
            )
        await self.pool.execute(
            "UPDATE project_environment_selections SET follow_default=false,version_id=$1 WHERE project_id=$2",
            self.pending,
            self.project_id,
        )
        with self.assertRaisesRegex(workspaces.WorkspaceRejected, "validated"):
            await self.create()
        self.assertEqual(self.fake.session_calls("CreateSession"), [])
        self.assertEqual(await self.pool.fetchval("SELECT count(*) FROM workspaces"), 0)

    async def test_malformed_create_reply_keeps_frozen_request_for_recovery(self):
        client = ns.get_client()
        frames = client._session_frames
        cases = list(malformed_composition_fields())
        self.fake.next_session_ids = [str(uuid.uuid4()) for _ in cases]
        for name, field in cases:
            with self.subTest(name=name):

                async def malformed(method, message, *, field=field):
                    reply = await frames(method, message)
                    return (
                        [composition_reply(field)]
                        if method == "CreateSession"
                        else reply
                    )

                with patch.object(client, "_session_frames", malformed):
                    lifecycle = await self.create(name)
                wid = lifecycle.workspace_id
                self.assertEqual(
                    lifecycle.observed_state, WorkspaceObservedState.UNKNOWN
                )
                binding = await ns.get_binding(wid)
                self.assertIsNone(binding["kagent_session_id"])
                frozen = await ns.ledger.get_development_environment(wid)
                self.assertEqual(frozen["version_id"], self.v1)
                original_request = self.fake.session_calls("CreateSession")[-1]
                recovered = await workspaces.refresh(wid, self.user)
                self.assertEqual(
                    recovered.observed_state, WorkspaceObservedState.RUNNING
                )
                self.assertEqual(
                    self.fake.session_calls("CreateSession")[-1], original_request
                )
                self.assertEqual(
                    await ns.ledger.get_development_environment(wid), frozen
                )
                self.assertEqual(
                    (await ns.get_binding(wid))["kagent_request_id"],
                    binding["kagent_request_id"],
                )
