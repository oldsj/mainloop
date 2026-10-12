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
import { Route } from '@/routes/health';

function Stub({ label }: { label: string }) {
  return <p>{label}</p>;
}

/**
 * Renders the app's real health route in a small router, so the URL-held filters are read the way
 * the app reads them. Register the test's handlers with `server.use` first.
 */
export function renderHealth(initialPath: string) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } }
  });
  const root = createRootRoute({ component: Outlet });
  // Attach the generated route to this test's root, as routeTree.gen.ts does for the app.
  const route = Route.update({
    id: '/health',
    path: '/health',
    getParentRoute: () => root
  } as never);
  const router = createRouter({
    routeTree: root.addChildren([
      route,
      createRoute({
        getParentRoute: () => root,
        path: '/',
        component: () => <Stub label="Home" />
      }),
      createRoute({
        getParentRoute: () => root,
        path: '/tasks/$id',
        component: () => <Stub label="Task page" />
      }),
      createRoute({
        getParentRoute: () => root,
        path: '/sessions/$id',
        component: () => <Stub label="Session page" />
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
