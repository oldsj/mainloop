"""Owner REST import validation without runtime or metadata admission."""

import unittest
from unittest.mock import AsyncMock, patch

import httpx
from mainloop import api
from mainloop.config import settings
from mainloop.services.github_repo import parse_github_repo

from models import Project

INVALID_REPOS = (
    "",
    "owner",
    "a/b/c",
    "https://gitlab.com/a/b",
    "http://github.com/a/b",
    "https://github.com/a/b?secret=hidden",
    "https://secret:hidden@github.com/a/b",
    "https://github.com:443/a/b",
    "https://github.com/a/b#hidden",
    "https://github.com/-bad/repo",
    "https://github.com/a/..",
    "https://github.com/a/b/extra",
)


class ProjectImportTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        for p in (
            patch.object(settings, "owner_id", "import-owner"),
            patch.object(settings, "api_hosts", "localhost"),
        ):
            p.start()
            self.addCleanup(p.stop)
        self.client = httpx.AsyncClient(
            transport=httpx.ASGITransport(app=api.app), base_url="http://localhost"
        )
        self.addAsyncCleanup(self.client.aclose)

    async def test_returns_storage_project_and_ignores_identity_header(self):
        stored = Project(
            user_id="import-owner",
            owner="Foo",
            name="Bar",
            full_name="Foo/Bar",
            html_url="https://github.com/Foo/Bar",
            default_branch="trunk",
            description="stored metadata",
        )
        with patch.object(
            api.db, "get_or_create_project", AsyncMock(return_value=stored)
        ) as save:
            response = await self.client.post(
                "/projects",
                json={"repo": " https://GitHub.com/foo/bar.git/ "},
                headers={"X-User-ID": "other-owner"},
            )
        self.assertEqual(response.status_code, 201, response.text)
        self.assertEqual(response.json(), stored.model_dump(mode="json"))
        save.assert_awaited_once_with("import-owner", parse_github_repo("foo/bar"))

    async def test_invalid_inputs_do_not_reach_storage(self):
        with patch.object(api.db, "get_or_create_project", AsyncMock()) as save:
            for repo in INVALID_REPOS:
                with self.subTest(repo=repo):
                    response = await self.client.post("/projects", json={"repo": repo})
                    self.assertEqual(response.status_code, 400, response.text)
                    self.assertEqual(
                        response.json(), {"detail": "Invalid GitHub repository"}
                    )
            for body in (
                {},
                {"repo": None},
                {"repo": 123},
                {"repo": []},
                {"repo": "a/b", "user_id": "other"},
            ):
                response = await self.client.post("/projects", json=body)
                self.assertEqual(response.status_code, 422, response.text)
        save.assert_not_awaited()

    async def test_host_and_origin_guards_precede_import(self):
        with patch.object(api.db, "get_or_create_project", AsyncMock()) as save:
            for headers, status in (
                ({"Host": "foreign.invalid"}, 404),
                ({"Origin": "https://foreign.invalid"}, 403),
            ):
                response = await self.client.post(
                    "/projects", json={"repo": "a/b"}, headers=headers
                )
                self.assertEqual(response.status_code, status, response.text)
        save.assert_not_awaited()
