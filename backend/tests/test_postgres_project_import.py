"""Non-admitting import against the existing disposable PostgreSQL harness."""

import asyncio
from unittest.mock import AsyncMock, patch

import httpx
from mainloop import api
from mainloop.config import settings
from mainloop.identity import current_user
from mainloop.runtime import workspaces

from tests.runtime.test_postgres_ledger import PostgresTestCase
from tests.test_project_import import INVALID_REPOS


class ProjectImportPostgresTests(PostgresTestCase):
    async def asyncSetUp(self):
        await super().asyncSetUp()
        api.app.dependency_overrides[current_user] = lambda: self.user
        self.addCleanup(api.app.dependency_overrides.pop, current_user)
        p = patch.object(settings, "api_hosts", "localhost")
        p.start()
        self.addCleanup(p.stop)
        self.metadata = AsyncMock(
            side_effect=AssertionError("unexpected metadata fetch")
        )
        p = patch.object(api, "get_repo_metadata", self.metadata)
        p.start()
        self.addCleanup(p.stop)
        self.client = httpx.AsyncClient(
            transport=httpx.ASGITransport(app=api.app), base_url="http://localhost"
        )
        self.addAsyncCleanup(self.client.aclose)
        self.admission = AsyncMock(side_effect=AssertionError("unexpected admission"))
        p = patch.object(workspaces, "create", self.admission)
        p.start()
        self.addCleanup(p.stop)

    async def import_repo(self, repo):
        response = await self.client.post("/projects", json={"repo": repo})
        self.assertEqual(response.status_code, 201, response.text)
        return response.json()

    async def test_canonical_idempotent_concurrent_and_owner_isolated(self):
        first = await self.import_repo(" https://GitHub.com/Foo/Bar.git/ ")
        self.assertEqual(first["full_name"], "Foo/Bar")
        self.assertEqual(first["html_url"], "https://github.com/Foo/Bar")
        self.assertEqual(first["default_branch"], "")
        self.assertIsNone(first["metadata_updated_at"])
        await self.pool.execute(
            "UPDATE projects SET default_branch='trunk', description='cached' WHERE id=$1",
            first["id"],
        )
        repeated = await self.import_repo("foo/bar")
        self.assertEqual(repeated["id"], first["id"])
        self.assertEqual(repeated["default_branch"], "trunk")
        self.assertEqual(repeated["description"], "cached")
        # Race first insertion as well as reuse of an existing row.
        for repo in ("Foo/Bar", "Concurrent/New"):
            results = await asyncio.gather(
                *(self.import_repo(repo if i % 2 else repo.lower()) for i in range(12))
            )
            self.assertEqual(len({r["id"] for r in results}), 1)
            self.assertEqual(
                await self.pool.fetchval(
                    "SELECT count(*) FROM projects WHERE user_id=$1 AND lower(full_name)=lower($2)",
                    self.user,
                    repo,
                ),
                1,
            )
        api.app.dependency_overrides[current_user] = lambda: self.user + "-other"
        other = await self.import_repo("foo/bar")
        self.assertNotEqual(other["id"], first["id"])
        self.assertEqual(other["user_id"], self.user + "-other")
        self.assertEqual(
            (await self.client.get(f"/projects/{first['id']}")).status_code, 404
        )
        self.assertEqual(other["default_branch"], "")
        self.admission.assert_not_awaited()
        self.metadata.assert_not_awaited()
        for table in (
            "sessions",
            "workspaces",
            "native_bindings",
            "push_grants",
            "tasks",
            "project_environment_selections",
        ):
            self.assertEqual(
                await self.pool.fetchval(f"SELECT count(*) FROM {table}"), 0, table
            )

    async def test_invalid_import_has_no_database_effect(self):
        before = await self.pool.fetchval("SELECT count(*) FROM projects")
        for repo in INVALID_REPOS:
            response = await self.client.post("/projects", json={"repo": repo})
            self.assertEqual(response.status_code, 400, response.text)
        response = await self.client.post(
            "/projects", json={"repo": "a/b", "environment_id": "x"}
        )
        self.assertEqual(response.status_code, 422)
        self.assertEqual(
            await self.pool.fetchval("SELECT count(*) FROM projects"), before
        )
        self.admission.assert_not_awaited()
        self.metadata.assert_not_awaited()
