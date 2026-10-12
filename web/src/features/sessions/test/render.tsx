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
import { NewAgentPage } from '../NewAgentPage';
import { SessionPage } from '../SessionPage';
import { SessionsPanel } from '../SessionsPanel';

function SessionRoute() {
  const { id } = useParams({ strict: false });
  // Keyed as the app's route is, so moving to another session starts a fresh page.
  return <SessionPage key={id} id={id ?? ''} />;
}

function Stub({ label }: { label: string }) {
  return <p>{label}</p>;
}

/**
 * Renders the sessions routes in a small router with the app's paths, so links and navigation can
 * be asserted without the shell. Register the test's handlers with `server.use` first.
 */
export function renderSessions(initialPath: string) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } }
  });
  const root = createRootRoute({ component: Outlet });
  const router = createRouter({
    routeTree: root.addChildren([
      createRoute({ getParentRoute: () => root, path: '/', component: SessionsPanel }),
      createRoute({ getParentRoute: () => root, path: '/agents', component: NewAgentPage }),
      createRoute({ getParentRoute: () => root, path: '/sessions/$id', component: SessionRoute }),
      createRoute({
        getParentRoute: () => root,
        path: '/tasks/$id',
        component: () => <Stub label="Task page" />
      }),
      createRoute({
        getParentRoute: () => root,
        path: '/workspaces/$id',
        component: () => <Stub label="Workspace page" />
      }),
      createRoute({
        getParentRoute: () => root,
        path: '/health',
        component: () => <Stub label="Health page" />
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
