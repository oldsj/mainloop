import { useQueryClient } from '@tanstack/react-query';
import { useEffect } from 'react';
import { connection } from '@/api/connection';
import { createSseApplier } from '@/api/invalidation';
import { createSseClient } from '@/api/sse';
import { useConnection } from './ConnectionBanner';

/**
 * Starts what keeps the cache fresh while the app is open: the /health probe, the SSE stream
 * and a refetch of everything when the backend comes back. Renders nothing.
 */
export function LiveUpdates() {
  const queryClient = useQueryClient();
  const { recoveries } = useConnection();

  useEffect(() => connection.start(), []);

  useEffect(() => {
    const apply = createSseApplier(queryClient);
    const client = createSseClient({
      onMessage: (message) => apply(message)
    });
    // A page kept in the back/forward cache would keep its stream open. Over HTTP/1.1 each one
    // holds one of the browser's six connections to the API, and once they are used up every
    // request waits and the app reads as offline. Close the stream on the way out and reopen it
    // (from the last event seen) if the page comes back.
    const leave = () => client.stop();
    const restore = (event: PageTransitionEvent) => {
      if (event.persisted) client.start();
    };
    window.addEventListener('pagehide', leave);
    window.addEventListener('pageshow', restore);
    client.start();
    return () => {
      window.removeEventListener('pagehide', leave);
      window.removeEventListener('pageshow', restore);
      client.stop();
    };
  }, [queryClient]);

  useEffect(() => {
    if (recoveries > 0) void queryClient.invalidateQueries();
  }, [recoveries, queryClient]);

  return null;
}
