import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { StrictMode, type ReactNode } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { $api, connection } from '@/api';
import { API_URL } from '@/api/config';
import { testSeen } from '@/test/msw/handlers';
import { server } from '@/test/msw/server';
import { useLastVisit } from './lastVisit';

// The visit goes through the app's client, whose limiter the tests skip. See fastReads.ts.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('@/features/sessions/test/fastReads');
  return withFastReads(await importOriginal());
});

afterEach(async () => {
  window.localStorage.clear();
  await connection.check();
});

const HOUR = 60 * 60 * 1000;

function setup(strict = false) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => {
    const tree = <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
    return strict ? <StrictMode>{tree}</StrictMode> : tree;
  };
  return { queryClient, wrapper };
}

describe('the previous visit', () => {
  it('is null on the owner’s first visit, and records it', async () => {
    const { wrapper } = setup();
    const { result } = renderHook(() => useLastVisit(), { wrapper });
    expect(result.current).toBeNull();
    await waitFor(() => expect(testSeen.visits).toBe(1));
    expect(result.current).toBeNull();
  });

  it('is the owner’s last visit before this one, from whichever device made it', async () => {
    const before = Date.now() - HOUR;
    testSeen.begin({ since: before, lastVisit: before });
    const { wrapper } = setup();
    const { result } = renderHook(() => useLastVisit(), { wrapper });
    await waitFor(() => expect(result.current).toBe(before));
    expect(testSeen.visits).toBe(1);
  });

  it('records one visit per page load, however many times it is asked', async () => {
    const before = Date.now() - HOUR;
    testSeen.begin({ since: before, lastVisit: before });
    const { wrapper } = setup(true);
    const first = renderHook(() => useLastVisit(), { wrapper });
    const second = renderHook(() => useLastVisit(), { wrapper });
    await waitFor(() => expect(first.result.current).toBe(before));
    await waitFor(() => expect(second.result.current).toBe(before));
    // A second record would have answered with the first one's time, not the hour-old visit.
    expect(testSeen.visits).toBe(1);
  });

  it('hands the answer to the explorer’s read of the record, so it needs no request of its own', async () => {
    testSeen.begin({ since: Date.now() - HOUR, tasks: { t1: Date.now() - 1000 } });
    const { queryClient, wrapper } = setup();
    renderHook(() => useLastVisit(), { wrapper });
    const key = $api.queryOptions('get', '/seen').queryKey;
    await waitFor(() => expect(queryClient.getQueryData(key)).toBeDefined());
    expect(queryClient.getQueryData(key)).toMatchObject({ tasks: [{ task_id: 't1' }] });
  });

  it('forgets the records this browser used to keep', async () => {
    window.localStorage.setItem('mainloop.home.lastVisit', '1700000000000');
    window.localStorage.setItem('mainloop.seen.v1', '{}');
    window.localStorage.setItem('mainloop.explorer.v1', '{}');
    const { wrapper } = setup();
    renderHook(() => useLastVisit(), { wrapper });
    await waitFor(() => expect(testSeen.visits).toBe(1));
    expect(window.localStorage.getItem('mainloop.home.lastVisit')).toBeNull();
    expect(window.localStorage.getItem('mainloop.seen.v1')).toBeNull();
    // The layout is still this browser's own.
    expect(window.localStorage.getItem('mainloop.explorer.v1')).toBe('{}');
  });

  it('has no baseline when the visit cannot be recorded', async () => {
    server.use(
      http.post(`${API_URL}/seen/visits`, () =>
        HttpResponse.json({ detail: 'no' }, { status: 500 })
      )
    );
    const { wrapper } = setup();
    const { result } = renderHook(() => useLastVisit(), { wrapper });
    await new Promise((resolve) => setTimeout(resolve, 100));
    expect(result.current).toBeNull();
  });
});
