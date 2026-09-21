"""Session status from agent activity: ended sessions stay ended, reported children complete."""

import unittest

from mainloop.runtime.native_sessions import ENDED_STATUSES, next_status

from models import SessionStatus as S


def status(current, *, turn_open=False, is_child=False, reported=False):
    return next_status(
        current, turn_open=turn_open, is_child=is_child, reported=reported
    )


class NextStatusTests(unittest.TestCase):
    def test_cancelled_and_failed_sessions_stay_that_way(self):
        for ended in (S.CANCELLED, S.FAILED):
            for turn_open in (True, False):
                for is_child, reported in ((True, True), (True, False), (False, False)):
                    self.assertEqual(
                        status(
                            ended,
                            turn_open=turn_open,
                            is_child=is_child,
                            reported=reported,
                        ),
                        ended,
                    )
        self.assertEqual(ENDED_STATUSES, {S.CANCELLED, S.FAILED})

    def test_an_open_turn_is_active(self):
        self.assertEqual(status(S.WAITING_ON_USER, turn_open=True), S.ACTIVE)

    def test_an_idle_child_that_reported_is_completed(self):
        self.assertEqual(
            status(S.WAITING_ON_USER, is_child=True, reported=True), S.COMPLETED
        )

    def test_a_reported_child_is_active_again_while_the_user_talks_to_it(self):
        self.assertEqual(
            status(S.COMPLETED, turn_open=True, is_child=True, reported=True), S.ACTIVE
        )
        self.assertEqual(status(S.ACTIVE, is_child=True, reported=True), S.COMPLETED)

    def test_an_idle_child_that_has_not_reported_waits_on_the_user(self):
        self.assertEqual(status(S.ACTIVE, is_child=True), S.WAITING_ON_USER)

    def test_an_idle_stand_alone_agent_waits_on_the_user(self):
        self.assertEqual(status(S.ACTIVE), S.WAITING_ON_USER)
        self.assertEqual(status(S.COMPLETED), S.WAITING_ON_USER)
