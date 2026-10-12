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
import { HomeWork } from '../HomeWork';

function Stub({ label }: { label: string }) {
  return <p>{label}</p>;
}

/** Renders home's work surface in a small router with the app's paths, so links can be followed. */
export function renderHome() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } }
  });
  const root = createRootRoute({ component: Outlet });
  const router = createRouter({
    routeTree: root.addChildren([
      createRoute({ getParentRoute: () => root, path: '/', component: HomeWork }),
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
