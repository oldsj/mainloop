"""DB-only owner observation contract with bounded fixture rows."""

import unittest
from contextlib import asynccontextmanager
from unittest.mock import AsyncMock, patch

from fastapi import HTTPException
from mainloop.runtime import task_api


class SmokeObservationTests(unittest.IsolatedAsyncioTestCase):
    async def test_missing_or_foreign_project_stops_before_ledgers(self):
        conn = AsyncMock()
        conn.fetchval.return_value = None

        @asynccontextmanager
        async def transaction():
            yield conn

        with patch.object(task_api, "transaction", transaction):
            with self.assertRaises(HTTPException) as error:
                await task_api.smoke_observations("foreign", "branch", "owner")
        self.assertEqual(error.exception.status_code, 404)
        conn.fetch.assert_not_awaited()
        self.assertEqual(conn.fetchval.call_args.args[1:], ("foreign", "owner"))

    async def test_bounded_sanitized_read_only_summary(self):
        conn = AsyncMock()
        conn.fetchval.side_effect = ["project", 0, True]
        conn.fetch.side_effect = [
            [{"id": "holder"}],
            [{"message_id": str(i), "state": "uncertain"} for i in range(101)],
            [
                {"request_id": str(i), "state": "rejected", "branch": "branch"}
                for i in range(101)
            ],
        ]

        @asynccontextmanager
        async def transaction():
            yield conn

        with patch.object(task_api, "transaction", transaction):
            value = await task_api.smoke_observations("project", "branch", "owner")
        self.assertTrue(value["deliveries_busy"])
        self.assertTrue(value["deliveries_truncated"])
        self.assertTrue(value["pushes_truncated"])
        # Confirmed indicator remains authoritative even outside the detail page.
        self.assertTrue(value["push_confirmed"])
        self.assertEqual(len(value["deliveries"]), 100)
        self.assertEqual(len(value["pushes"]), 100)
        delivery, push = conn.fetch.call_args_list[1:]
        self.assertIn("LIMIT 101", delivery.args[0])
        self.assertIn("uncertain", delivery.args[3])
        self.assertEqual(delivery.args[1:3], ("owner", "project"))
        self.assertIn("LIMIT 101", push.args[0])
        self.assertEqual(push.args[1:], ("owner", "project", "branch"))
        self.assertEqual(set(value["pushes"][0]), {"request_id", "state", "branch"})
        self.assertEqual(set(value["deliveries"][0]), {"message_id", "state"})
        conn.execute.assert_not_awaited()
