import { HITLCard } from '@/features/hitl';
import { useSessionRequests } from '@/features/hitl/session-requests';

export interface SessionRequestsProps {
  sessionId: string;
  /** Already shown above by `?hitl=`; not shown twice. */
  skip?: string;
}

/**
 * The session's pending requests, each as a card, so the owner can answer without leaving the thread.
 * An answered card stays here with its receipt until the page reloads (see `useSessionRequests`).
 */
export function SessionRequests({ sessionId, skip }: SessionRequestsProps) {
  const { shown, isError } = useSessionRequests(sessionId, skip);
  if (!shown.length && !isError) return null;
  return (
    <div
      className="border-border max-h-[50%] shrink-0 overflow-y-auto border-b"
      aria-label="Session requests"
    >
      {isError ? (
        <p className="text-muted-foreground p-md text-sm" role="status">
          Could not refresh session requests. Retrying…
        </p>
      ) : null}
      {shown.map((id) => (
        <HITLCard key={id} requestId={id} />
      ))}
    </div>
  );
}
