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
import { InboxPanel } from '../InboxPanel';

function Stub({ label }: { label: string }) {
  return <p>{label}</p>;
}

/**
 * Renders the inbox in a small router with the app's paths, so its links can be followed. Register
 * the test's handlers with `server.use` first.
 */
export function renderInbox(initialPath = '/') {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } }
  });
  const root = createRootRoute({ component: Outlet });
  const router = createRouter({
    routeTree: root.addChildren([
      createRoute({ getParentRoute: () => root, path: '/', component: InboxPanel }),
      createRoute({
        getParentRoute: () => root,
        path: '/tasks/$id',
        component: () => <Stub label="Task page" />
      })
    ]),
    history: createMemoryHistory({ initialEntries: [initialPath] })
  });
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  );
  return { router, queryClient };
}
