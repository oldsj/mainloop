/**
 * Transcript reads, through the generated client. A session's transcript pages both ways from where
 * it opens: the last entries, or a search hit's entry. The view re-reads while a turn is live.
 */
import { useInfiniteQuery } from '@tanstack/react-query';
import { $api, fetchClient, keys } from '@/api';
import { errorDetail } from '@/api/errors';
import type { SessionTranscript } from './model';

/** Entries read per page. */
export const TRANSCRIPT_PAGE = 200;
/** How often an open transcript with a live tail is read again. */
export const LIVE_REFRESH_MS = 15_000;

export type PageParam = { after: number } | { before: number } | { latest: true };

const firstId = (page: SessionTranscript) => page.entries[0]?.id ?? undefined;
const lastId = (page: SessionTranscript) => page.entries[page.entries.length - 1]?.id ?? undefined;

/** One session's transcript, opening at `entry` when given and at the end otherwise. */
export function useSessionTranscript(sessionId: string, entry?: number) {
  return useInfiniteQuery({
    queryKey: [...keys.sessionTranscript(sessionId), 'pages', entry ?? null],
    initialPageParam: (entry ? { after: entry - 1 } : { latest: true }) as PageParam,
    queryFn: async ({ pageParam, signal }) => {
      const { data, error } = await fetchClient.GET('/sessions/{session_id}/transcript', {
        params: {
          path: { session_id: sessionId },
          query: { ...pageParam, limit: TRANSCRIPT_PAGE }
        },
        signal
      });
      if (!data) throw new Error(errorDetail(error, 'The transcript could not be read.'));
      return data;
    },
    getNextPageParam: (last, _all, param): PageParam | undefined => {
      if (!last.has_more) return undefined;
      const id = lastId(last);
      if (id !== undefined) return { after: id };
      // An empty page before an entry: the next one starts at that entry.
      return 'before' in param ? { after: param.before - 1 } : undefined;
    },
    getPreviousPageParam: (first, _all, param): PageParam | undefined => {
      if (!first.has_earlier) return undefined;
      const id = firstId(first);
      if (id !== undefined) return { before: id };
      return 'after' in param && param.after > 0 ? { before: param.after + 1 } : undefined;
    },
    refetchInterval: (query) => {
      const pages = query.state.data?.pages;
      const last = pages?.[pages.length - 1];
      return last && last.live.length > 0 ? LIVE_REFRESH_MS : false;
    },
    refetchIntervalInBackground: false
  });
}

/** Full-text search over the owner's transcripts, run once there is a query. */
export function useTranscriptSearch(q: string, sessionId?: string, projectId?: string) {
  const query = q.trim();
  return $api.useQuery(
    'get',
    '/transcripts/search',
    {
      params: {
        query: {
          q: query,
          limit: 50,
          ...(sessionId ? { session_id: sessionId } : {}),
          ...(projectId ? { project_id: projectId } : {})
        }
      }
    },
    { enabled: query.length > 0 }
  );
}
