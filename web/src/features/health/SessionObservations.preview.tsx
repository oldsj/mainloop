import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { useState } from 'react';
import { SessionObservations } from './SessionObservations';

/**
 * Shows the state before the owner asks for events. The read is disabled until then, so the
 * preview makes no request. The loaded list needs a read, so it is covered by the page tests.
 */
export default function SessionObservationsPreview() {
  const [client] = useState(
    () => new QueryClient({ defaultOptions: { queries: { retry: false } } })
  );
  return (
    <QueryClientProvider client={client}>
      <div className="p-md">
        <SessionObservations sessionId="session-1" hours={24} />
      </div>
    </QueryClientProvider>
  );
}
