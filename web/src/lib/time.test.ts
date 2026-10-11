import { expect, test } from 'vitest';
import { messageTime } from './time';

// Local times, so the same-day and same-year checks do not depend on the machine's zone.
const now = new Date(2026, 9, 11, 15, 30);
const at = (date: Date) => date.toISOString();

test('a message from today shows only the time', () => {
  const sent = new Date(2026, 9, 11, 9, 5);
  const expected = sent.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
  expect(messageTime(at(sent), now)).toBe(expected);
});

test('an earlier day this year shows the day and the time', () => {
  const sent = new Date(2026, 9, 9, 9, 5);
  const day = sent.toLocaleDateString([], { month: 'short', day: 'numeric' });
  const time = sent.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
  expect(messageTime(at(sent), now)).toBe(`${day}, ${time}`);
});

test('a message from an earlier year also shows the year', () => {
  const sent = new Date(2025, 11, 31, 23, 59);
  const day = sent.toLocaleDateString([], { month: 'short', day: 'numeric', year: 'numeric' });
  const time = sent.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
  expect(messageTime(at(sent), now)).toBe(`${day}, ${time}`);
});

test('an unreadable timestamp renders as nothing rather than "Invalid Date"', () => {
  expect(messageTime('not a time', now)).toBe('');
});
