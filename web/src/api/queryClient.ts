import { QueryClient } from '@tanstack/react-query';
import { ApiError } from './errors';

/**
 * One cache for server state. Polling uses `refetchInterval` with the default
 * `refetchIntervalInBackground: false`, so a hidden tab issues no reads. A recovered connection
 * invalidates everything (see ConnectionBridge).
 */
export function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        staleTime: 5_000,
        refetchOnWindowFocus: true,
        retry: (failureCount, error) => {
          // A 4xx will not change on retry; transport failures and 5xx might.
          if (error instanceof ApiError && error.status >= 400 && error.status < 500) return false;
          return failureCount < 2;
        }
      },
      mutations: { retry: false }
    }
  });
}
