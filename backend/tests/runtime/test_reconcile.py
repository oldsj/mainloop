"""Each reconcile step has its own ``try``: one failure does not stop the others."""

from __future__ import annotations

import logging
import unittest
from unittest.mock import AsyncMock, patch

from mainloop.push_gate import credentials as git_credentials
from mainloop.runtime import agent_credentials
from mainloop.runtime import native_sessions as ns
from mainloop.runtime import workspaces


class ReconcileStepTests(unittest.IsolatedAsyncioTestCase):
    def patched(self, **overrides):
        steps = {
            "sync": AsyncMock(),
            "observe_hitl_once": AsyncMock(),
            "reconcile_hitl_responses": AsyncMock(),
            "reconcile_cleanup": AsyncMock(),
            "cleanup_all": AsyncMock(),
            "reconcile_archived_deletes": AsyncMock(),
            "suspend_idle": AsyncMock(return_value=[]),
        }
        steps.update(overrides)
        ledger = AsyncMock()
        ledger.sessions_with_open_work.return_value = ["s1", "s2", "s3"]
        for p in (
            patch(
                "mainloop.runtime.hitl_observer.observe_hitl_once",
                steps["observe_hitl_once"],
            ),
            patch(
                "mainloop.runtime.hitl_continuation.reconcile_hitl_responses",
                steps["reconcile_hitl_responses"],
            ),
            patch.object(ns, "ledger", ledger),
            patch.object(ns, "sync", steps["sync"]),
            patch.object(
                ns, "reconcile_archived_deletes", steps["reconcile_archived_deletes"]
            ),
            patch.object(
                agent_credentials, "reconcile_cleanup", steps["reconcile_cleanup"]
            ),
            patch.object(git_credentials, "cleanup_all", steps["cleanup_all"]),
            patch.object(workspaces, "suspend_idle", steps["suspend_idle"]),
        ):
            p.start()
            self.addCleanup(p.stop)
        return steps

    async def test_one_failing_session_sync_does_not_starve_the_others(self):
        sync = AsyncMock(side_effect=[None, RuntimeError("boom"), None])
        steps = self.patched(sync=sync)
        with self.assertLogs(ns.logger, logging.ERROR) as logs:
            await ns.reconcile_once(sweep=True)
        self.assertEqual([c.args[0] for c in sync.await_args_list], ["s1", "s2", "s3"])
        steps["suspend_idle"].assert_awaited_once()
        (record,) = logs.records
        self.assertEqual(
            (record.step, record.session_id, record.error_class),
            ("sync", "s2", "RuntimeError"),
        )

    async def test_a_failing_sweep_step_does_not_stop_the_steps_after_it(self):
        steps = self.patched(
            reconcile_cleanup=AsyncMock(side_effect=OSError("cluster down")),
            reconcile_archived_deletes=AsyncMock(side_effect=ValueError("bad row")),
        )
        with self.assertLogs(ns.logger, logging.ERROR) as logs:
            await ns.reconcile_once(sweep=True)
        steps["suspend_idle"].assert_awaited_once()
        self.assertEqual(
            [(r.step, r.session_id, r.error_class) for r in logs.records],
            [
                ("credential_cleanup", "-", "OSError"),
                ("archived_deletes", "-", "ValueError"),
            ],
        )

    async def test_the_sweep_steps_wait_until_they_are_due(self):
        steps = self.patched()
        await ns.reconcile_once(sweep=False)
        self.assertEqual(steps["sync"].await_count, 3)
        steps["suspend_idle"].assert_not_awaited()
        steps["reconcile_cleanup"].assert_not_awaited()
        steps["cleanup_all"].assert_not_awaited()

    async def test_failing_git_cleanup_preserves_sweep_order_and_failure_isolation(
        self,
    ):
        steps = self.patched(
            cleanup_all=AsyncMock(side_effect=OSError("Git Secret cleanup unavailable"))
        )
        order = []
        for name in (
            "reconcile_cleanup",
            "cleanup_all",
            "reconcile_archived_deletes",
            "suspend_idle",
        ):
            mock = steps[name]
            failure = mock.side_effect

            async def record(*args, step=name, error=failure):
                order.append(step)
                if error:
                    raise error

            mock.side_effect = record
        with self.assertLogs(ns.logger, logging.ERROR) as logs:
            await ns.reconcile_once(sweep=True)
        self.assertEqual(
            order,
            [
                "reconcile_cleanup",
                "cleanup_all",
                "reconcile_archived_deletes",
                "suspend_idle",
            ],
        )
        self.assertEqual(
            [c.args[0] for c in steps["sync"].await_args_list], ["s1", "s2", "s3"]
        )
        self.assertEqual(
            [(r.step, r.session_id, r.error_class) for r in logs.records],
            [("git_credential_cleanup", "-", "OSError")],
        )
        for name in order:
            steps[name].assert_awaited_once_with()

    async def test_a_failure_listing_open_work_still_runs_the_sweep(self):
        steps = self.patched()
        ns.ledger.sessions_with_open_work.side_effect = ConnectionError("db")
        with self.assertLogs(ns.logger, logging.ERROR) as logs:
            await ns.reconcile_once(sweep=True)
        steps["suspend_idle"].assert_awaited_once()
        self.assertEqual(logs.records[0].step, "list_open_work")

    async def test_hitl_failure_does_not_starve_deliveries_or_other_observation(self):
        steps = self.patched(
            observe_hitl_once=AsyncMock(side_effect=ValueError("bad gateway"))
        )
        with self.assertLogs(ns.logger, logging.ERROR) as logs:
            await ns.reconcile_once(sweep=False)
        self.assertEqual(logs.records[0].step, "hitl_observation")
        steps["reconcile_hitl_responses"].assert_awaited_once()
        self.assertEqual(steps["sync"].await_count, 3)

    async def test_one_failing_archived_delete_does_not_block_the_next(self):
        ledger = AsyncMock()
        ledger.undeleted_archived.return_value = [
            {"session_id": "a"},
            {"session_id": "b"},
        ]
        seen: list[str] = []

        async def delete(sid):
            seen.append(sid)
            if sid == "a":
                raise RuntimeError("x")
            return True

        with (
            patch.object(ns, "ledger", ledger),
            patch.object(ns, "delete_kagent_session", delete),
            self.assertLogs(ns.logger, logging.ERROR),
        ):
            await ns.reconcile_archived_deletes()
        self.assertEqual(seen, ["a", "b"])


if __name__ == "__main__":
    unittest.main()
