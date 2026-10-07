"""Owner API and immutable storage against disposable PostgreSQL, fake OCI only."""

import asyncio
from unittest.mock import patch

import asyncpg
import httpx
from mainloop import api
from mainloop.config import settings
from mainloop.db import db
from mainloop.db.environment_schema import ENVIRONMENT_MIGRATION_SQL
from mainloop.environments.api import registry_client
from mainloop.identity import current_user
from mainloop.services.github_repo import parse_github_repo
from tests.runtime.test_postgres_ledger import PostgresTestCase
from tests.test_environments import image


class EnvironmentPostgresTests(PostgresTestCase):
    async def asyncSetUp(self):
        await super().asyncSetUp()
        self.fake, self.reference = image()
        api.app.dependency_overrides[current_user] = lambda: self.user
        api.app.dependency_overrides[registry_client] = lambda: self.fake
        self.addCleanup(api.app.dependency_overrides.pop, current_user)
        self.addCleanup(api.app.dependency_overrides.pop, registry_client)
        self.host_patch = patch.object(settings, "api_hosts", "localhost")
        self.host_patch.start()
        self.addCleanup(self.host_patch.stop)
        self.client = httpx.AsyncClient(
            transport=httpx.ASGITransport(app=api.app), base_url="http://localhost"
        )

    async def asyncTearDown(self):
        await self.client.aclose()
        await super().asyncTearDown()

    async def register(self):
        response = await self.client.post(
            "/environments",
            json={"name": "dev", "image": self.reference, "watched_tag": "latest"},
        )
        self.assertEqual(response.status_code, 201, response.text)
        data = response.json()
        return data["environment"]["id"], data["version"]["id"]

    async def project(self, owner=None):
        return await db.get_or_create_project(
            owner or self.user, parse_github_repo("https://github.com/oldsj/testrepo")
        )

    async def test_invalid_registration_persists_nothing(self):
        before = await self.pool.fetchval("SELECT count(*) FROM dev_environments")
        for user in ("root", "0:0", "1000:1000", "nonroot", None):
            self.fake, reference = image(user)
            response = await self.client.post(
                "/environments", json={"name": "bad", "image": reference}
            )
            self.assertEqual(response.status_code, 422, response.text)
            self.assertIn("Declared USER", response.text)
        self.fake.calls.clear()
        response = await self.client.post(
            "/environments",
            json={
                "name": "bad",
                "image": self.reference.replace("ghcr.io", "docker.io"),
            },
        )
        self.assertEqual(response.status_code, 422)
        self.assertEqual(self.fake.calls, [])
        self.assertEqual(
            await self.pool.fetchval("SELECT count(*) FROM dev_environments"), before
        )

    async def test_register_default_refresh_selection_and_immutable_version(self):
        env, old = await self.register()
        self.assertEqual(
            (await self.client.get(f"/environments/{env}/versions/{old}")).json()[
                "validation_status"
            ],
            "static_validated",
        )
        self.assertEqual((await self.client.get("/environments")).status_code, 200)
        self.assertEqual(
            (
                await self.client.put(
                    f"/environments/{env}/default", json={"version_id": old}
                )
            ).status_code,
            200,
        )
        project = await self.project()
        selected = await self.client.put(
            f"/projects/{project.id}/environment",
            json={"environment_id": env, "version_id": old, "expected_version": 0},
        )
        self.assertEqual(selected.status_code, 200, selected.text)
        changed, _ = image(index=False)
        self.fake.objects.update(changed.objects)
        candidate = await self.client.post(
            f"/environments/{env}/refresh", json={"version_id": old}
        )
        self.assertEqual(candidate.status_code, 201, candidate.text)
        self.assertNotEqual(candidate.json()["id"], old)
        self.assertEqual(
            (await self.client.get(f"/environments/{env}")).json()[
                "accepted_default_version_id"
            ],
            old,
        )
        self.assertEqual(
            (await self.client.get(f"/projects/{project.id}/environment")).json(),
            selected.json(),
        )
        self.assertEqual(
            len((await self.client.get(f"/environments/{env}/versions")).json()), 2
        )
        for operation in (
            "UPDATE environment_versions SET snapshot='{}' WHERE id=$1",
            "DELETE FROM environment_versions WHERE id=$1",
        ):
            with self.assertRaisesRegex(asyncpg.RaiseError, "immutable"):
                await self.pool.execute(operation, old)
        await self.pool.execute(ENVIRONMENT_MIGRATION_SQL)
        self.assertEqual(
            (await self.client.get(f"/environments/{env}")).json()[
                "accepted_default_version_id"
            ],
            old,
        )

    async def test_cross_owner_grant_revocation_and_owner_mutations(self):
        env, value = await self.register()
        project = await self.project("another-user")

        async def choose(expected=0):
            return await self.client.put(
                f"/projects/{project.id}/environment",
                json={
                    "environment_id": env,
                    "version_id": value,
                    "expected_version": expected,
                },
            )

        api.app.dependency_overrides[current_user] = lambda: "another-user"
        self.assertEqual((await choose()).status_code, 403)
        self.assertEqual(
            (await self.client.get(f"/environments/{env}")).status_code, 404
        )
        self.assertEqual(
            (
                await self.client.put(
                    f"/environments/{env}/default", json={"version_id": value}
                )
            ).status_code,
            404,
        )
        self.assertEqual(
            (
                await self.client.put(
                    f"/environments/{env}/grants/{project.id}",
                    json={"permission": "derive"},
                )
            ).status_code,
            404,
        )
        api.app.dependency_overrides[current_user] = lambda: self.user
        self.assertEqual(
            (
                await self.client.put(
                    f"/environments/{env}/grants/{project.id}",
                    json={"permission": "use"},
                )
            ).status_code,
            200,
        )
        api.app.dependency_overrides[current_user] = lambda: "another-user"
        self.assertEqual((await choose()).status_code, 200)
        api.app.dependency_overrides[current_user] = lambda: self.user
        self.assertEqual(
            (
                await self.client.delete(f"/environments/{env}/grants/{project.id}")
            ).status_code,
            204,
        )
        api.app.dependency_overrides[current_user] = lambda: "another-user"
        self.assertEqual((await choose(1)).status_code, 403)
        current = (await self.client.get(f"/projects/{project.id}/environment")).json()
        self.assertTrue(current["access_revoked"])
        self.assertEqual(current["version_id"], value)

    async def test_concurrent_selection_and_follow_default(self):
        env, value = await self.register()
        project = await self.project()
        await self.client.put(
            f"/environments/{env}/default", json={"version_id": value}
        )

        async def choose():
            return await self.client.put(
                f"/projects/{project.id}/environment",
                json={
                    "environment_id": env,
                    "follow_default": True,
                    "expected_version": 0,
                },
            )

        results = await asyncio.gather(choose(), choose())
        self.assertEqual(sorted(r.status_code for r in results), [200, 409])
        selection = (
            await self.client.get(f"/projects/{project.id}/environment")
        ).json()
        self.assertEqual(selection["resolved_version_id"], value)
        self.assertEqual(selection["revision"], 1)

    async def test_definition_pending_no_registry_and_cannot_select(self):
        response = await self.client.post(
            "/environments",
            json={
                "name": "definition",
                "source_kind": "definition_repo",
                "definition": {
                    "repository": "https://github.com/oldsj/environment",
                    "commit_sha": "a" * 40,
                    "path": ".devcontainer/devcontainer.json",
                },
            },
        )
        self.assertEqual(response.status_code, 201, response.text)
        self.assertEqual(self.fake.calls, [])
        data = response.json()
        self.assertEqual(data["version"]["validation_status"], "pending_build")
        env, value = data["environment"]["id"], data["version"]["id"]
        self.assertEqual(
            (
                await self.client.put(
                    f"/environments/{env}/default", json={"version_id": value}
                )
            ).status_code,
            422,
        )
        project = await self.project()
        response = await self.client.put(
            f"/projects/{project.id}/environment",
            json={"environment_id": env, "version_id": value, "expected_version": 0},
        )
        self.assertEqual(response.status_code, 422)

    async def test_failed_refresh_and_cross_environment_version(self):
        env, value = await self.register()
        other, other_value = await self.register()
        project = await self.project()
        response = await self.client.put(
            f"/environments/{env}/default", json={"version_id": other_value}
        )
        self.assertEqual(response.status_code, 404)
        response = await self.client.put(
            f"/projects/{project.id}/environment",
            json={
                "environment_id": env,
                "version_id": other_value,
                "expected_version": 0,
            },
        )
        self.assertEqual(response.status_code, 404)
        self.assertIsNone(
            (await self.client.get(f"/projects/{project.id}/environment")).json()
        )
        bad, _ = image("root")
        self.fake.objects.update(bad.objects)
        response = await self.client.post(
            f"/environments/{env}/refresh", json={"version_id": value}
        )
        self.assertEqual(response.status_code, 422)
        self.assertEqual(
            len((await self.client.get(f"/environments/{env}/versions")).json()), 1
        )
        self.assertIsNone(
            (await self.client.get(f"/environments/{env}")).json()[
                "accepted_default_version_id"
            ]
        )
        self.assertNotEqual(env, other)

    async def test_follow_default_resolves_without_revising_selection(self):
        env, value = await self.register()
        project = await self.project()
        await self.client.put(
            f"/environments/{env}/default", json={"version_id": value}
        )
        response = await self.client.put(
            f"/projects/{project.id}/environment",
            json={"environment_id": env, "follow_default": True, "expected_version": 0},
        )
        self.assertEqual(response.status_code, 200)
        changed, _ = image(index=False)
        self.fake.objects.update(changed.objects)
        candidate = (
            await self.client.post(
                f"/environments/{env}/refresh", json={"version_id": value}
            )
        ).json()
        await self.client.put(
            f"/environments/{env}/default", json={"version_id": candidate["id"]}
        )
        selected = (await self.client.get(f"/projects/{project.id}/environment")).json()
        self.assertEqual(selected["resolved_version_id"], candidate["id"])
        self.assertEqual(selected["revision"], 1)
        self.assertIsNone(selected["version_id"])

    async def test_concurrent_grant_and_select_complete_after_first_locks(self):
        from mainloop.db import environments as store

        from models.environment import SelectEnvironment

        env, value = await self.register()
        project = await self.project()
        env_locked, project_locked = asyncio.Event(), asyncio.Event()
        original_env, original_project = store.environment, store.project

        async def env_hook(conn, *args, **kwargs):
            result = await original_env(conn, *args, **kwargs)
            if asyncio.current_task().get_name() == "grant" and kwargs.get("lock"):
                env_locked.set()
                await project_locked.wait()
            return result

        async def project_hook(conn, *args, **kwargs):
            result = await original_project(conn, *args, **kwargs)
            if asyncio.current_task().get_name() == "select" and kwargs.get("lock"):
                project_locked.set()
                await env_locked.wait()
            return result

        async def grant():
            async with self.pool.acquire() as conn, conn.transaction():
                await store.grant(conn, env, self.user, project.id, "use")

        async def select():
            async with self.pool.acquire() as conn, conn.transaction():
                return await store.select(
                    conn,
                    project.id,
                    self.user,
                    SelectEnvironment(
                        environment_id=env, version_id=value, expected_version=0
                    ),
                )

        # Exercise real SQL and foreign-key locks; only pause after each helper's
        # first lock to force the interleaving that previously deadlocked.
        with patch.object(store, "environment", env_hook), patch.object(
            store, "project", project_hook
        ):
            async with asyncio.timeout(10):
                granted, selected = await asyncio.gather(
                    asyncio.create_task(grant(), name="grant"),
                    asyncio.create_task(select(), name="select"),
                )
        self.assertIsNone(granted)
        self.assertEqual(selected.revision, 1)
        self.assertEqual(selected.version_id, value)
        self.assertFalse(selected.access_revoked)
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT permission FROM environment_grants WHERE environment_id=$1 AND project_id=$2",
                env,
                project.id,
            ),
            "use",
        )

    async def test_revoke_and_select_serialize_authorization_in_both_orders(self):
        for first in ("revoke", "select"):
            with self.subTest(first=first):
                await self._revoke_selection_order(first)

    async def _revoke_selection_order(self, first):
        from mainloop.db import environments as store

        from models.environment import SelectEnvironment

        env, value = await self.register()
        owner = f"{self.user}-shared-{first}"
        project = await self.project(owner)
        request = SelectEnvironment(
            environment_id=env, version_id=value, expected_version=0
        )
        async with self.pool.acquire() as conn, conn.transaction():
            await store.grant(conn, env, self.user, project.id, "use")
            await store.select(conn, project.id, owner, request)
        request = request.model_copy(update={"expected_version": 1})
        first_locked, second_attempted = asyncio.Event(), asyncio.Event()
        original_env = store.environment

        async def env_hook(conn, *args, **kwargs):
            task = asyncio.current_task().get_name()
            if kwargs.get("lock") and task != first:
                await first_locked.wait()
                second_attempted.set()
            result = await original_env(conn, *args, **kwargs)
            if kwargs.get("lock") and task == first:
                first_locked.set()
                await second_attempted.wait()
            return result

        async def revoke():
            async with self.pool.acquire() as conn, conn.transaction():
                await store.grant(conn, env, self.user, project.id, None)

        async def select():
            async with self.pool.acquire() as conn, conn.transaction():
                return await store.select(conn, project.id, owner, request)

        with patch.object(store, "environment", env_hook):
            async with asyncio.timeout(10):
                revoked, selected = await asyncio.gather(
                    asyncio.create_task(revoke(), name="revoke"),
                    asyncio.create_task(select(), name="select"),
                    return_exceptions=True,
                )
        self.assertIsNone(revoked)
        if first == "revoke":
            self.assertIsInstance(selected, store.EnvironmentError)
            self.assertEqual(selected.status, 403)
        else:
            self.assertEqual(selected.revision, 2)
            self.assertFalse(selected.access_revoked)
        async with self.pool.acquire() as conn:
            current = await store.selection(conn, project.id, owner)
        self.assertTrue(current.access_revoked)
        self.assertEqual(current.version_id, value)
        self.assertEqual(current.revision, 1 if first == "revoke" else 2)
