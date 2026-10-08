"""Main project discovery and delegated first briefs with fakes only."""

import unittest
from contextlib import asynccontextmanager
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch

from mainloop.mcp_app import TOOLS
from mainloop.runtime.agent_identity import hash_token
from mainloop.runtime.delegation import PgStore, render_for_binding
from mainloop.runtime.standing import StandingInputs, render_standing


@asynccontextmanager
async def context(value=None, *args, **kwargs):
    yield value


class GuidanceTests(unittest.IsolatedAsyncioTestCase):
    async def test_main_projects_are_owner_scoped_and_selection_is_explicit(self):
        projects = [
            {"id": "p1", "full_name": "owner/one", "environment_selected": True},
            {"id": "p2", "full_name": "owner/two", "environment_selected": False},
        ]
        conn = SimpleNamespace(
            fetch=AsyncMock(side_effect=[projects, [], [], []]),
            fetchrow=AsyncMock(return_value=None),
        )
        with (
            patch("mainloop.runtime.delegation.db.connection", lambda: context(conn)),
            patch(
                "mainloop.runtime.delegation.db.get_session",
                AsyncMock(
                    return_value=SimpleNamespace(user_id="owner", conversation_id="c")
                ),
            ),
            patch.object(PgStore, "task_call", AsyncMock(return_value={"tasks": []})),
        ):
            text = await render_for_binding({"session_id": "main", "role": "main"})
        query, owner = conn.fetch.call_args_list[0].args
        self.assertIn("WHERE p.user_id=$1", query)
        self.assertEqual(owner, "owner")
        self.assertIn("p1 owner/one: environment selected=yes", text)
        self.assertIn("p2 owner/two: environment selected=no", text)
        self.assertIn("not a readiness guarantee", text)
        self.assertNotIn(
            "owner/one",
            render_standing(StandingInputs(role="child", projects=projects)),
        )

    async def test_delegate_persists_role_and_tool_guidance_before_assigned_work(self):
        fresh = dict(
            session_id="main",
            user_id="owner",
            role="main",
            token_hash=hash_token("fixture-token"),
            mcp_grant_kind="coordination",
            archived_at=None,
            kagent_deleted_at=None,
            status="waiting_on_user",
        )
        conn = SimpleNamespace(
            fetchrow=AsyncMock(return_value=fresh), transaction=context
        )
        result = SimpleNamespace(
            id="op", state="pending", reason=None, model_dump=lambda **kw: {}
        )
        mutate = AsyncMock(return_value=result)
        with (
            patch("mainloop.runtime.delegation.db.connection", lambda: context(conn)),
            patch("mainloop.tasks.lifecycle.authority_locked", context),
            patch("mainloop.tasks.service.mutate", mutate),
        ):
            for mode in ("code", "coordination"):
                args = dict(
                    request_id="request",
                    title="Work",
                    brief="assigned change",
                    mode=mode,
                )
                if mode == "code":
                    args.update(project_id="p1", checkout={"branch": "feature/test"})
                await PgStore().task_call(fresh, "delegate", args)
                brief = mutate.call_args.args[3].brief
                self.assertIn("task supervisor", brief)
                self.assertIn("`whoami`", brief)
                self.assertIn("Then `report`", brief)
                self.assertTrue(brief.endswith("assigned change"))
                if mode == "code":
                    for step in (
                        "project's checks",
                        "`git push`",
                        "`open_pull_request`",
                        "not `gh`",
                        "`merge_pull_request` once CI is green",
                        "If it returns `approval_required`, call `merge_pull_request_with_approval`"
                        " (the owner approves in Mainloop), or report missing approval tooling as a blocker.",
                    ):
                        self.assertIn(step, brief)
                else:
                    self.assertNotIn("`git push`", brief)
                    self.assertIn("no repository authority", brief)

    def test_delegate_description_distinguishes_unknown_from_terminal_blocked(self):
        description = TOOLS["delegate"][1]
        self.assertIn("Reuse request_id for lost or uncertain responses", description)
        self.assertIn("confirmed terminal blocked create", description)
        self.assertIn("NEW request_id", description)
        self.assertIn("returns the blocked operation", description)
