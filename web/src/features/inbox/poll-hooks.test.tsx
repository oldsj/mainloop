import type { ReactNode } from 'react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { keys } from '@/api';
import { useOwnerTaskPoll } from '@/features/tasks/queries';
import * as polling from '@/lib/visiblePolling';
import { useActivityPoll, useAttentionPoll } from './poll';

function harness() {
  vi.useFakeTimers();
  const hidden = vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  // Read functions have MSW tests; this isolates mounted hook wiring with the real coordinator.
  const reads = vi.spyOn(client, 'fetchQuery').mockResolvedValue(undefined);
  const polls = polling.createVisiblePolling({
    visible: () => !document.hidden,
    now: () => Date.now(),
    later: (fn, ms) => {
      const timer = setTimeout(fn, ms);
      return () => clearTimeout(timer);
    },
    onVisibility: (fn) => {
      document.addEventListener('visibilitychange', fn);
      return () => document.removeEventListener('visibilitychange', fn);
    }
  });
  vi.spyOn(polling, 'visiblePolling').mockReturnValue(polls);
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  const visibility = (isHidden: boolean) => {
    hidden.mockReturnValue(isHidden);
    document.dispatchEvent(new Event('visibilitychange'));
  };
  const tick = (ms: number) =>
    act(async () => {
      await vi.advanceTimersByTimeAsync(ms);
    });
  return { client, reads, wrapper, visibility, tick };
}

afterEach(() => {
  vi.restoreAllMocks();
  vi.useRealTimers();
});

describe.each([
  ['attention', useAttentionPoll, keys.attention()],
  ['activity', useActivityPoll, keys.activity()],
  ['owner tasks', () => useOwnerTaskPoll(true), [...keys.tasks(), {}]]
] as const)('%s poll hook', (_name, usePoll, queryKey) => {
  it('shares mounts, pauses while hidden, resumes and stops after the last unmount', async () => {
    const { client, reads, wrapper, visibility, tick } = harness();
    const first = renderHook(usePoll, { wrapper });
    const second = renderHook(usePoll, { wrapper });
    await tick(12000);
    expect(reads).not.toHaveBeenCalled();

    act(() => visibility(false));
    await tick(0);
    expect(reads).toHaveBeenCalledTimes(1);
    expect(reads.mock.calls[0]?.[0].queryKey).toEqual(queryKey);
    await tick(4000);
    expect(reads).toHaveBeenCalledTimes(2);
    first.unmount();
    await tick(4000);
    expect(reads).toHaveBeenCalledTimes(3);

    act(() => visibility(true));
    await tick(12000);
    expect(reads).toHaveBeenCalledTimes(3);
    act(() => visibility(false));
    await tick(0);
    expect(reads).toHaveBeenCalledTimes(4);
    second.unmount();
    await tick(12000);
    expect(reads).toHaveBeenCalledTimes(4);
    expect(vi.getTimerCount()).toBe(0);
    client.clear();
  });
});

it('does not start the owner-task poll for a noncompact badge', async () => {
  const { client, reads, wrapper, visibility, tick } = harness();
  const mounted = renderHook(() => useOwnerTaskPoll(false), { wrapper });
  act(() => visibility(false));
  await tick(12000);
  expect(reads).not.toHaveBeenCalled();
  mounted.unmount();
  client.clear();
});
