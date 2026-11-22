import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { StrictMode, type ReactNode } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { $api, connection } from '@/api';
import { API_URL } from '@/api/config';
import { testSeen } from '@/test/msw/handlers';
import { server } from '@/test/msw/server';
import { useRecordVisit } from './lastVisit';

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

describe('recording the visit', () => {
  it('records the owner’s visit when it mounts', async () => {
    // Given an owner who has not been here before.
    const { wrapper } = setup();

    // When the shell mounts the hook.
    renderHook(() => useRecordVisit(), { wrapper });

    // Then the server has one visit.
    await waitFor(() => expect(testSeen.visits).toBe(1));
  });

  it('records one visit per page load, however many times it is mounted', async () => {
    // Given an owner whose last visit was an hour ago.
    const before = Date.now() - HOUR;
    testSeen.begin({ since: before, lastVisit: before });
    const { queryClient, wrapper } = setup(true);

    // When the hook mounts twice, under StrictMode's doubled effects.
    renderHook(() => useRecordVisit(), { wrapper });
    renderHook(() => useRecordVisit(), { wrapper });
    const key = $api.queryOptions('get', '/seen').queryKey;
    await waitFor(() => expect(queryClient.getQueryData(key)).toBeDefined());
    await new Promise((resolve) => setTimeout(resolve, 50));

    // Then one visit was recorded, and the visit before it is still the hour-old one.
    expect(testSeen.visits).toBe(1);
    expect(queryClient.getQueryData(key)).toMatchObject({
      previous_visit: new Date(before).toISOString()
    });
  });

  it('hands the answer to the explorer’s read of the record, so it needs no request of its own', async () => {
    testSeen.begin({ since: Date.now() - HOUR, tasks: { t1: Date.now() - 1000 } });
    const { queryClient, wrapper } = setup();
    renderHook(() => useRecordVisit(), { wrapper });
    const key = $api.queryOptions('get', '/seen').queryKey;
    await waitFor(() => expect(queryClient.getQueryData(key)).toBeDefined());
    expect(queryClient.getQueryData(key)).toMatchObject({ tasks: [{ task_id: 't1' }] });
  });

  it('forgets the records this browser used to keep', async () => {
    window.localStorage.setItem('mainloop.home.lastVisit', '1700000000000');
    window.localStorage.setItem('mainloop.seen.v1', '{}');
    window.localStorage.setItem('mainloop.explorer.v1', '{}');
    const { wrapper } = setup();
    renderHook(() => useRecordVisit(), { wrapper });
    await waitFor(() => expect(testSeen.visits).toBe(1));
    expect(window.localStorage.getItem('mainloop.home.lastVisit')).toBeNull();
    expect(window.localStorage.getItem('mainloop.seen.v1')).toBeNull();
    // The layout is still this browser's own.
    expect(window.localStorage.getItem('mainloop.explorer.v1')).toBe('{}');
  });

  it('leaves the record unread when the visit cannot be recorded', async () => {
    // Given a server that refuses the visit.
    server.use(
      http.post(`${API_URL}/seen/visits`, () =>
        HttpResponse.json({ detail: 'no' }, { status: 500 })
      )
    );
    const { queryClient, wrapper } = setup();

    // When the hook mounts.
    renderHook(() => useRecordVisit(), { wrapper });
    await new Promise((resolve) => setTimeout(resolve, 100));

    // Then nothing is put in the cache for the explorer to mistake for the owner's record.
    expect(queryClient.getQueryData($api.queryOptions('get', '/seen').queryKey)).toBeUndefined();
  });
});
