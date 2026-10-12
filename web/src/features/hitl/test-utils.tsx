import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterContextProvider, createMemoryHistory } from '@tanstack/react-router';
import { render } from '@testing-library/react';
import type { ReactNode } from 'react';
import { createAppRouter } from '@/router';
import type { HITLState } from '@/lib/hitl';
import { hitlView } from '@/test/fixtures/hitl';
import type { HITLRequestProps } from './hitl-request';

/**
 * Renders under the real router so `Link` resolves, without navigating anywhere. The providers
 * are a wrapper, so `rerender(ui)` keeps them.
 */
export function renderRouted(ui: ReactNode) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const router = createAppRouter(queryClient, createMemoryHistory({ initialEntries: ['/'] }));
  function Providers({ children }: { children: ReactNode }) {
    return (
      <QueryClientProvider client={queryClient}>
        <RouterContextProvider router={router}>{children}</RouterContextProvider>
      </QueryClientProvider>
    );
  }
  return { queryClient, ...render(ui, { wrapper: Providers }) };
}

export function snapshotOf(overrides: Partial<HITLState> = {}): HITLState {
  return {
    view: hitlView(),
    busy: false,
    error: null,
    uncertain: false,
    stale: false,
    ...overrides
  };
}

export function requestProps(overrides: Partial<HITLRequestProps> = {}): HITLRequestProps {
  return {
    snapshot: snapshotOf(),
    onRespond: () => {},
    onRefresh: () => {},
    onLoadMergeDetails: () => Promise.reject(new Error('unexpected details request')),
    ...overrides
  };
}
