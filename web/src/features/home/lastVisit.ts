import { useQueryClient, type QueryClient } from '@tanstack/react-query';
import { useEffect, useState } from 'react';
import { $api, fetchClient } from '@/api';
import type { components } from '@/api';

type Seen = components['schemas']['Seen'];

/**
 * The owner's previous visit, the baseline for "Since you were here". It is the owner's, kept on
 * the server (`POST /seen/visits`; see docs/specs/seen.md), so it is the last time they opened the
 * app on any device, not on this one.
 *
 * One visit is recorded per page load. A second record in the same load would make "the visit
 * before" this very load, and nothing would have changed since it.
 */
const recorded = new WeakMap<QueryClient, Promise<Seen | null>>();

/** The keys this browser kept these records under before they moved to the server. */
const LEGACY_KEYS = ['mainloop.home.lastVisit', 'mainloop.seen.v1'];

function forgetLocalRecords(): void {
  try {
    for (const key of LEGACY_KEYS) window.localStorage.removeItem(key);
  } catch {
    // Storage is blocked: there is nothing to forget.
  }
}

/** Records this visit and answers with the owner's state after it, or null when it cannot be. */
async function recordVisit(queryClient: QueryClient): Promise<Seen | null> {
  forgetLocalRecords();
  try {
    const { data } = await fetchClient.POST('/seen/visits');
    if (!data) return null;
    // The answer is the whole record, so the explorer's read of it needs no request of its own.
    queryClient.setQueryData($api.queryOptions('get', '/seen').queryKey, data);
    return data;
  } catch {
    // Offline, or the server has no record to give: "Since you were here" has no baseline.
    return null;
  }
}

/**
 * The previous visit's time in ms, or null: before the server has answered, on a first visit, or
 * when the visit could not be recorded. The shell calls this once and passes the value down, so
 * the desktop sidebar and the phone tabs compare against the same baseline.
 */
export function useLastVisit(): number | null {
  const queryClient = useQueryClient();
  const [previous, setPrevious] = useState<number | null>(null);
  useEffect(() => {
    let live = true;
    let visit = recorded.get(queryClient);
    if (!visit) recorded.set(queryClient, (visit = recordVisit(queryClient)));
    void visit.then((seen) => {
      const at = seen?.previous_visit ? Date.parse(seen.previous_visit) : Number.NaN;
      if (live && Number.isFinite(at)) setPrevious(at);
    });
    return () => {
      live = false;
    };
  }, [queryClient]);
  return previous;
}
