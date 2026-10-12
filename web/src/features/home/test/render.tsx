import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  RouterProvider
} from '@tanstack/react-router';
import { render } from '@testing-library/react';
import { HomeStrip } from '../HomeStrip';
import { WorkList } from '../WorkList';
import { readLastVisit } from '../lastVisit';

function Stub({ label }: { label: string }) {
  return <p>{label}</p>;
}

/**
 * Renders home's work surface (the strip above the list, as on desktop) in a small router with the
 * app's paths, so links can be followed. The baseline is the last visit as stored before the render.
 */
export function renderHome() {
  const lastVisit = readLastVisit();
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } }
  });
  const root = createRootRoute({ component: Outlet });
  const router = createRouter({
    routeTree: root.addChildren([
      createRoute({
        getParentRoute: () => root,
        path: '/',
        component: () => (
          <>
            <HomeStrip lastVisit={lastVisit} />
            <WorkList lastVisit={lastVisit} />
          </>
        )
      }),
      createRoute({
        getParentRoute: () => root,
        path: '/tasks/$id',
        component: () => <Stub label="Task page" />
      })
    ]),
    history: createMemoryHistory({ initialEntries: ['/'] })
  });
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  );
  return { router, queryClient };
}
