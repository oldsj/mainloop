import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { isUnseen, markSeen, readSeen, resetSeenForTests, SEEN_KEEP_MS, SEEN_KEY } from './seen';

const T0 = Date.parse('2026-01-10T00:00:00Z');
const HOUR = 60 * 60 * 1000;
const iso = (ms: number) => new Date(ms).toISOString();

beforeEach(() => {
  localStorage.clear();
  resetSeenForTests();
});
afterEach(() => vi.restoreAllMocks());

describe('the seen record', () => {
  it('starts now on a first read, so a task that finished earlier is not lit', () => {
    const state = readSeen(T0);
    expect(state).toEqual({ since: T0, seen: {} });
    expect(isUnseen(state, 't', iso(T0 - HOUR))).toBe(false);
    expect(isUnseen(state, 't', iso(T0 + HOUR))).toBe(true);
    // The baseline is kept, not moved by a later read.
    expect(readSeen(T0 + HOUR)?.since).toBe(T0);
  });

  it('marks a task seen, and it is unseen again only when it changes afterwards', () => {
    readSeen(T0);
    const state = markSeen('t', T0 + 2 * HOUR);
    expect(isUnseen(state, 't', iso(T0 + HOUR))).toBe(false);
    expect(isUnseen(state, 't', iso(T0 + 3 * HOUR))).toBe(true);
    expect(isUnseen(state, 'other', iso(T0 + HOUR))).toBe(true);
    expect(JSON.parse(localStorage.getItem(SEEN_KEY) ?? '{}').seen.t).toBe(T0 + 2 * HOUR);
  });

  it('drops marks older than the keep window when it writes', () => {
    readSeen(T0);
    markSeen('old', T0);
    const state = markSeen('new', T0 + SEEN_KEEP_MS + HOUR);
    expect(state?.seen).toEqual({ new: T0 + SEEN_KEEP_MS + HOUR });
  });

  it('starts again from junk in storage', () => {
    localStorage.setItem(SEEN_KEY, 'not json');
    expect(readSeen(T0)).toEqual({ since: T0, seen: {} });
    localStorage.setItem(SEEN_KEY, JSON.stringify({ since: 'x', seen: { a: 'b' } }));
    expect(readSeen(T0)).toEqual({ since: T0, seen: {} });
    localStorage.setItem(SEEN_KEY, JSON.stringify({ since: T0, seen: { a: 'b', c: 5 } }));
    expect(readSeen(T0 + HOUR)).toEqual({ since: T0, seen: { c: 5 } });
  });

  it('lights nothing when storage is blocked', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    expect(readSeen(T0)).toBeNull();
    expect(markSeen('t', T0)).toBeNull();
    expect(isUnseen(null, 't', iso(T0 + HOUR))).toBe(false);
  });

  it('treats an unreadable time as seen', () => {
    expect(isUnseen({ since: T0, seen: {} }, 't', 'not a time')).toBe(false);
  });
});
