import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { LiveUpdates } from './LiveUpdates';

describe('LiveUpdates', () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('closes the event stream while the page is in the back/forward cache', async () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch');
    const streams = () =>
      fetchSpy.mock.calls.filter(([input]) => String(input).endsWith('/events'));
    const { unmount } = render(
      <QueryClientProvider client={new QueryClient()}>
        <LiveUpdates />
      </QueryClientProvider>
    );
    await waitFor(() => expect(streams()).toHaveLength(1));

    act(() => {
      window.dispatchEvent(new PageTransitionEvent('pagehide', { persisted: true }));
    });
    expect(streams()[0]?.[1]?.signal?.aborted).toBe(true);

    act(() => {
      window.dispatchEvent(new PageTransitionEvent('pageshow', { persisted: true }));
    });
    await waitFor(() => expect(streams()).toHaveLength(2));
    expect(streams()[1]?.[1]?.signal?.aborted).toBe(false);
    unmount();
    expect(streams()[1]?.[1]?.signal?.aborted).toBe(true);
  });
});
