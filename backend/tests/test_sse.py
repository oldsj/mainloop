"""SSE wire format."""

import unittest

from mainloop.sse import EventType, SSEEvent


class EncodeTests(unittest.TestCase):
    def test_event_name_is_the_enum_value_not_its_repr(self):
        for event_type in EventType:
            wire = SSEEvent(event=event_type, data={}, id="x").encode()
            self.assertIn(f"\nevent: {event_type.value}\n", wire)

    def test_session_message_matches_the_name_the_frontend_lists(self):
        wire = SSEEvent(event=EventType.SESSION_MESSAGE, data={"a": 1}, id="x").encode()
        self.assertEqual(wire, 'id: x\nevent: session:message\ndata: {"a": 1}\n\n')

    def test_plain_string_names_pass_through(self):
        self.assertIn("event: custom\n", SSEEvent(event="custom", data={}).encode())


if __name__ == "__main__":
    unittest.main()
