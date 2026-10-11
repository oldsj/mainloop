import { $api } from '@/api';
import { useAnswered } from './hitl-client';

/** The session's pending approval and question requests are re-read this often while visible. */
export const REQUESTS_POLL_MS = 4000;

/**
 * A session's requests as the page shows them. `pending` are what the owner must still answer:
 * listed by the server and not answered in this browser. `shown` adds this browser's receipts for
 * the session, so an answered card stays in place after the server stops listing it. `skip` drops
 * one id from both, for a request the caller already shows.
 */
export function useSessionRequests(sessionId: string, skip?: string) {
  const requests = $api.useQuery(
    'get',
    '/sessions/{session_id}/hitl',
    { params: { path: { session_id: sessionId } } },
    { refetchInterval: REQUESTS_POLL_MS }
  );
  const answered = useAnswered();
  const served = [...new Set(requests.data ?? [])].filter((id) => id !== skip);
  const pending = served.filter((id) => !answered.has(id));
  const receipts = [...answered]
    .filter(([id, entry]) => entry.sessionId === sessionId && id !== skip)
    .map(([id]) => id)
    .reverse();
  return {
    pending,
    shown: [...new Set([...receipts, ...pending])],
    isError: requests.isError
  };
}
