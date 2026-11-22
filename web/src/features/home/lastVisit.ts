import { useQueryClient, type QueryClient } from '@tanstack/react-query';
import { useEffect } from 'react';
import { $api, fetchClient } from '@/api';

/**
 * The owner's visits are kept on the server (`POST /seen/visits`; see docs/specs/seen.md), so the
 * last one is the last time they opened the app on any device, not on this one.
 *
 * One visit is recorded per page load. A second record in the same load would make the server's
 * "visit before" this very load.
 */
const recorded = new WeakSet<QueryClient>();

/** The keys this browser kept these records under before they moved to the server. */
const LEGACY_KEYS = ['mainloop.home.lastVisit', 'mainloop.seen.v1'];

function forgetLocalRecords(): void {
  try {
    for (const key of LEGACY_KEYS) window.localStorage.removeItem(key);
  } catch {
    // Storage is blocked: there is nothing to forget.
  }
}

/** Records this visit and keeps the owner's state after it, when the server answers with one. */
async function recordVisit(queryClient: QueryClient): Promise<void> {
  forgetLocalRecords();
  try {
    const { data } = await fetchClient.POST('/seen/visits');
    if (!data) return;
    // The answer is the whole record, so the explorer's read of it needs no request of its own.
    queryClient.setQueryData($api.queryOptions('get', '/seen').queryKey, data);
  } catch {
    // Offline, or the server has no record to give: the visit is not recorded this time.
  }
}

/**
 * Records the owner's visit, once per page load however many times it is mounted. The shell calls
 * it, so opening the app on any route counts. Nothing in the web app reads the visit back.
 */
export function useRecordVisit(): void {
  const queryClient = useQueryClient();
  useEffect(() => {
    if (recorded.has(queryClient)) return;
    recorded.add(queryClient);
    void recordVisit(queryClient);
  }, [queryClient]);
}
