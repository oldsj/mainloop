import { $api } from '@/api';
import { HITLCard } from '@/features/hitl';

/** The session's pending approval and question requests are re-read this often while visible. */
export const REQUESTS_POLL_MS = 4000;

export interface SessionRequestsProps {
  sessionId: string;
  /** Already shown above by `?hitl=`; not shown twice. */
  skip?: string;
}

/** The main session's pending requests, each as a card, so the owner can answer without leaving the thread. */
export function SessionRequests({ sessionId, skip }: SessionRequestsProps) {
  const requests = $api.useQuery(
    'get',
    '/sessions/{session_id}/hitl',
    { params: { path: { session_id: sessionId } } },
    { refetchInterval: REQUESTS_POLL_MS }
  );
  const ids = [...new Set(requests.data ?? [])].filter((id) => id !== skip);
  if (!ids.length && !requests.isError) return null;
  return (
    <div
      className="border-border max-h-[50%] shrink-0 overflow-y-auto border-b"
      aria-label="Session requests"
    >
      {requests.isError ? (
        <p className="text-muted-foreground p-md text-sm" role="status">
          Could not refresh session requests. Retrying…
        </p>
      ) : null}
      {ids.map((id) => (
        <HITLCard key={id} requestId={id} />
      ))}
    </div>
  );
}
