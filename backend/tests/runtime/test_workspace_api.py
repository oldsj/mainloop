"""POST /workspaces: the project_id/repo choice, repo validation and how the manifest is built.

The workspaces module is faked: no database and no kagent.
"""

from __future__ import annotations

import unittest
from datetime import UTC, datetime
from unittest.mock import AsyncMock, patch

import httpx
from mainloop import api
from mainloop.config import settings
from mainloop.runtime import workspaces
from mainloop.services.github_repo import GithubRepo

from models import WorkspaceLifecycle, WorkspaceObservedState

PROJECT = {
    "id": "proj-1",
    "html_url": "https://github.com/oldsj/mainloop",
    "default_branch": "main",
}


class CreateWorkspaceApiTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.project_for = AsyncMock(return_value=dict(PROJECT))
        self.project_for_repo = AsyncMock(return_value=dict(PROJECT))
        self.create = AsyncMock(side_effect=self.lifecycle)
        for patcher in (
            patch.object(settings, "owner_id", "user-1"),
            patch.object(settings, "api_hosts", "test"),
            patch.object(workspaces, "project_for", self.project_for),
            patch.object(workspaces, "project_for_repo", self.project_for_repo),
            patch.object(workspaces, "create", self.create),
            patch.object(workspaces, "publish", AsyncMock()),
        ):
            patcher.start()
            self.addCleanup(patcher.stop)
        self.client = httpx.AsyncClient(
            transport=httpx.ASGITransport(app=api.app), base_url="http://test"
        )
        self.addAsyncCleanup(self.client.aclose)

    @staticmethod
    def lifecycle(user_id, project_id, manifest) -> WorkspaceLifecycle:
        return WorkspaceLifecycle(
            workspace_id="ws-1",
            session_id="ws-1",
            observed_state=WorkspaceObservedState.RESUMING,
            manifest=manifest,
            updated_at=datetime.now(UTC),
        )

    async def post(self, **body) -> httpx.Response:
        return await self.client.post("/workspaces", json=body)

    def manifest(self):
        return self.create.call_args.args[2]

    async def test_project_id_still_creates_from_the_stored_project(self):
        response = await self.post(project_id="proj-1", branch="feature/x")
        self.assertEqual(response.status_code, 201, response.text)
        self.project_for.assert_awaited_once_with("user-1", "proj-1")
        self.project_for_repo.assert_not_awaited()
        self.assertEqual(self.create.call_args.args[:2], ("user-1", "proj-1"))
        manifest = self.manifest()
        self.assertEqual(manifest.repo_url, PROJECT["html_url"])
        self.assertEqual((manifest.ref, manifest.branch), ("main", "feature/x"))

    async def test_an_unknown_project_id_is_404(self):
        self.project_for.return_value = None
        response = await self.post(project_id="nope", branch="x")
        self.assertEqual(response.status_code, 404)
        self.create.assert_not_called()

    async def test_repo_finds_or_creates_the_project_then_creates_the_workspace(self):
        for text in (
            "oldsj/mainloop",
            "https://github.com/oldsj/mainloop.git",
        ):
            self.project_for_repo.reset_mock()
            response = await self.post(repo=text, branch="feature/x")
            self.assertEqual(response.status_code, 201, response.text)
            self.project_for_repo.assert_awaited_once_with(
                "user-1", GithubRepo("oldsj", "mainloop")
            )
            self.project_for.assert_not_awaited()
            self.assertEqual(self.create.call_args.args[:2], ("user-1", "proj-1"))
            self.assertEqual(self.manifest().repo_url, PROJECT["html_url"])

    async def test_a_new_project_has_no_default_so_the_ref_stays_empty(self):
        self.project_for_repo.return_value = {**PROJECT, "default_branch": ""}
        response = await self.post(repo="oldsj/mainloop", branch="feature/x")
        self.assertEqual(response.status_code, 201, response.text)
        self.assertEqual(self.manifest().ref, "")
        self.assertEqual(self.manifest().branch, "feature/x")

    async def test_an_explicit_ref_wins_over_the_default(self):
        await self.post(repo="oldsj/mainloop", branch="b", ref="v1.2")
        self.assertEqual(self.manifest().ref, "v1.2")

    async def test_an_omitted_branch_uses_the_default_or_a_generated_name(self):
        await self.post(repo="oldsj/mainloop")
        self.assertEqual(self.manifest().branch, "main")
        self.project_for_repo.return_value = {**PROJECT, "default_branch": ""}
        await self.post(repo="oldsj/mainloop", branch="")
        self.assertRegex(self.manifest().branch, r"^mainloop/[0-9a-f]{8}$")

    async def test_exactly_one_of_project_id_or_repo(self):
        for body in (
            {"branch": "x"},
            {"project_id": "proj-1", "repo": "oldsj/mainloop", "branch": "x"},
            {"project_id": None, "repo": None},
        ):
            with self.subTest(body=body):
                response = await self.post(**body)
                self.assertEqual(response.status_code, 422)
        self.create.assert_not_called()
        self.project_for_repo.assert_not_awaited()
        self.project_for.assert_not_awaited()

    async def test_an_invalid_repo_is_422_and_creates_nothing(self):
        for text in (
            "",
            "oldsj",
            "oldsj/mainloop/tree/main",
            "https://gitlab.com/oldsj/mainloop",
            "http://github.com/oldsj/mainloop",
            "https://token@github.com/oldsj/mainloop",
            "oldsj/main loop",
        ):
            with self.subTest(text=text):
                response = await self.post(repo=text, branch="x")
                self.assertEqual(response.status_code, 422, response.text)
        self.project_for_repo.assert_not_awaited()
        self.create.assert_not_called()

    async def test_unknown_fields_and_non_string_repo_are_422(self):
        self.assertEqual((await self.post(repo=5, branch="x")).status_code, 422)
        self.assertEqual(
            (await self.post(repo="oldsj/mainloop", url="x")).status_code, 422
        )
