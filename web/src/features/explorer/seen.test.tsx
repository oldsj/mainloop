import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import type { ReactNode } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection } from '@/api';
import { API_URL } from '@/api/config';
import { testSeen } from '@/test/msw/handlers';
import { server } from '@/test/msw/server';
import { isUnseen, seenState, useSeen } from './seen';

// The record is read through the app's client, whose limiter the tests skip. See fastReads.ts.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('@/features/sessions/test/fastReads');
  return withFastReads(await importOriginal());
});

afterEach(async () => {
  await connection.check();
});

const T0 = Date.parse('2026-01-10T00:00:00Z');
const HOUR = 60 * 60 * 1000;
const iso = (ms: number) => new Date(ms).toISOString();

describe('the seen record', () => {
  it('reads the server’s record into a start time and a mark per task', () => {
    const state = seenState({
      since: iso(T0),
      last_visit: null,
      previous_visit: null,
      tasks: [
        { task_id: 'a', seen_at: iso(T0 + HOUR) },
        { task_id: 'junk', seen_at: 'not a time' }
      ]
    });
    expect(state).toEqual({ since: T0, seen: { a: T0 + HOUR } });
    expect(seenState(undefined)).toBeNull();
    expect(seenState(null)).toBeNull();
  });

  it('counts a task as unseen when it changed after the record began and after its mark', () => {
    const state = { since: T0, seen: { t: T0 + 2 * HOUR } };
    expect(isUnseen(state, 't', iso(T0 + HOUR))).toBe(false);
    expect(isUnseen(state, 't', iso(T0 + 3 * HOUR))).toBe(true);
    expect(isUnseen(state, 'other', iso(T0 + HOUR))).toBe(true);
    // Finished before the record began: seen, so starting the record lights nothing.
    expect(isUnseen(state, 'old', iso(T0 - HOUR))).toBe(false);
    expect(isUnseen(null, 't', iso(T0 + HOUR))).toBe(false);
    expect(isUnseen(state, 't', 'not a time')).toBe(false);
  });
});

describe('useSeen', () => {
  function setup() {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const wrapper = ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    );
    return { wrapper };
  }
  const now = () => Date.now();
  const changed = () => iso(now() - 60_000);

  it('reads the owner’s record from the server', async () => {
    testSeen.begin({ since: now() - HOUR, tasks: { a: now() - 1000 } });
    const { wrapper } = setup();
    const { result } = renderHook(() => useSeen(), { wrapper });
    expect(result.current.seen).toBeNull();
    await waitFor(() => expect(result.current.seen).not.toBeNull());
    expect(Object.keys(result.current.seen?.seen ?? {})).toEqual(['a']);
  });

  it('marks a task with one write, after which it is seen', async () => {
    testSeen.begin({ since: now() - HOUR });
    const { wrapper } = setup();
    const { result } = renderHook(() => useSeen(), { wrapper });
    await waitFor(() => expect(result.current.seen).not.toBeNull());
    const at = changed();
    expect(isUnseen(result.current.seen, 't', at)).toBe(true);
    act(() => result.current.mark('t', at));
    await waitFor(() => expect(isUnseen(result.current.seen, 't', at)).toBe(false));
    expect(testSeen.marked).toEqual(['t']);
  });

  it('does not write for a task already marked after its last change', async () => {
    testSeen.begin({ since: now() - HOUR, tasks: { t: now() - 1000 } });
    const { wrapper } = setup();
    const { result } = renderHook(() => useSeen(), { wrapper });
    await waitFor(() => expect(result.current.seen).not.toBeNull());
    act(() => result.current.mark('t', changed()));
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(testSeen.marked).toEqual([]);
  });

  it('writes again when the task changed after its mark', async () => {
    testSeen.begin({ since: now() - HOUR, tasks: { t: now() - 10 * 60_000 } });
    const { wrapper } = setup();
    const { result } = renderHook(() => useSeen(), { wrapper });
    await waitFor(() => expect(result.current.seen).not.toBeNull());
    act(() => result.current.mark('t', changed()));
    await waitFor(() => expect(testSeen.marked).toEqual(['t']));
  });

  it('sends one write when two trees on the page mark the same task', async () => {
    testSeen.begin({ since: now() - HOUR });
    const { wrapper } = setup();
    const first = renderHook(() => useSeen(), { wrapper });
    const second = renderHook(() => useSeen(), { wrapper });
    await waitFor(() => expect(second.result.current.seen).not.toBeNull());
    const at = changed();
    act(() => {
      first.result.current.mark('t', at);
      second.result.current.mark('t', at);
    });
    await waitFor(() => expect(isUnseen(second.result.current.seen, 't', at)).toBe(false));
    expect(testSeen.marked).toEqual(['t']);
  });

  it('does nothing before the record has been read', async () => {
    const { wrapper } = setup();
    const { result } = renderHook(() => useSeen(), { wrapper });
    act(() => result.current.mark('t', changed()));
    await waitFor(() => expect(result.current.seen).not.toBeNull());
    expect(testSeen.marked).toEqual([]);
  });

  it('leaves the task unseen when the write fails, and can try again', async () => {
    testSeen.begin({ since: now() - HOUR });
    let fail = true;
    server.use(
      http.put(`${API_URL}/seen/tasks/:taskId`, () =>
        fail ? HttpResponse.json({ detail: 'no' }, { status: 500 }) : undefined
      )
    );
    const { wrapper } = setup();
    const { result } = renderHook(() => useSeen(), { wrapper });
    await waitFor(() => expect(result.current.seen).not.toBeNull());
    const at = changed();
    act(() => result.current.mark('t', at));
    await new Promise((resolve) => setTimeout(resolve, 100));
    expect(isUnseen(result.current.seen, 't', at)).toBe(true);
    fail = false;
    act(() => result.current.mark('t', at));
    await waitFor(() => expect(isUnseen(result.current.seen, 't', at)).toBe(false));
  });

  it('shows nothing as unseen when the record cannot be read', async () => {
    server.use(
      http.get(`${API_URL}/seen`, () => HttpResponse.json({ detail: 'no' }, { status: 500 }))
    );
    const { wrapper } = setup();
    const { result } = renderHook(() => useSeen(), { wrapper });
    await new Promise((resolve) => setTimeout(resolve, 100));
    expect(result.current.seen).toBeNull();
    expect(isUnseen(result.current.seen, 't', changed())).toBe(false);
  });
});
