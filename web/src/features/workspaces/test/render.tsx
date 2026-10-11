import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  RouterProvider,
  useParams
} from '@tanstack/react-router';
import { render } from '@testing-library/react';
import { WorkspacePage } from '../WorkspacePage';

function WorkspaceRoute() {
  const { id } = useParams({ strict: false });
  // Keyed as the app's route is, so moving to another workspace starts a fresh page.
  return <WorkspacePage key={id} id={id ?? ''} />;
}

function Stub({ label }: { label: string }) {
  return <p>{label}</p>;
}

/**
 * Renders the workspace route in a small router with the app's paths, so links and the delete
 * redirect can be asserted without the shell. Register the test's handlers with `server.use` first.
 */
export function renderWorkspace(initialPath: string) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } }
  });
  const root = createRootRoute({ component: Outlet });
  const router = createRouter({
    routeTree: root.addChildren([
      createRoute({
        getParentRoute: () => root,
        path: '/',
        component: () => <Stub label="Sessions list" />
      }),
      createRoute({
        getParentRoute: () => root,
        path: '/workspaces/$id',
        component: WorkspaceRoute
      }),
      createRoute({
        getParentRoute: () => root,
        path: '/sessions/$id',
        component: () => <Stub label="Session page" />
      }),
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
