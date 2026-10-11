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
    client.start();
    return () => client.stop();
  }, [queryClient]);

  useEffect(() => {
    if (recoveries > 0) void queryClient.invalidateQueries();
  }, [recoveries, queryClient]);

  return null;
}
