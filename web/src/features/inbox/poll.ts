/**
 * Visible-only polling for the inbox. SSE `inbox:updated` is the primary refresh; these reads cover
 * a missed event. Attention polls while anything in the inbox is mounted; activity polls only while
 * the Activity view is open. Each read goes through the query cache, so its data and error reach
 * the observers, and it resolves without a value, so polling keeps going after a failure.
 */
import { useEffect } from 'react';
import { useQueryClient, type QueryClient, type QueryKey } from '@tanstack/react-query';
import { $api, keys } from '@/api';
import { visiblePolling } from '@/lib/visiblePolling';

const ATTENTION_POLL = 'inbox:attention';
const ACTIVITY_POLL = 'inbox:activity';

/**
 * Runs one bounded load for a poll. An abort cancels the load and reverts the query, so a hidden
 * tab leaves the previous data in place rather than a half-written state.
 */
function pollOnce(
  queryClient: QueryClient,
  key: QueryKey,
  load: () => Promise<unknown>,
  signal: AbortSignal
): Promise<void> {
  if (signal.aborted) return Promise.resolve();
  const cancel = () => void queryClient.cancelQueries({ queryKey: key }, { revert: true });
  signal.addEventListener('abort', cancel, { once: true });
  return load()
    .then(
      () => undefined,
      () => undefined
    )
    .finally(() => signal.removeEventListener('abort', cancel));
}

/** One attention read. Errors stay on the query; the poll itself never stops on them. */
export function readAttention(queryClient: QueryClient, signal: AbortSignal): Promise<void> {
  return pollOnce(
    queryClient,
    keys.attention(),
    () =>
      queryClient.fetchQuery({
        ...$api.queryOptions('get', '/attention'),
        staleTime: 0,
        retry: false
      }),
    signal
  );
}

/** One activity read, with the same rules as {@link readAttention}. */
export function readActivity(queryClient: QueryClient, signal: AbortSignal): Promise<void> {
  return pollOnce(
    queryClient,
    keys.activity(),
    () =>
      queryClient.fetchQuery({
        ...$api.queryOptions('get', '/activity'),
        staleTime: 0,
        retry: false
      }),
    signal
  );
}

/** Keeps attention fresh while the calling component is mounted. Several mounts share one poll. */
export function useAttentionPoll(): void {
  const queryClient = useQueryClient();
  useEffect(
    () => visiblePolling().watch(ATTENTION_POLL, (signal) => readAttention(queryClient, signal)),
    [queryClient]
  );
}

/** Keeps the activity log fresh while it is on screen. */
export function useActivityPoll(): void {
  const queryClient = useQueryClient();
  useEffect(
    () => visiblePolling().watch(ACTIVITY_POLL, (signal) => readActivity(queryClient, signal)),
    [queryClient]
  );
}
