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
import type { ReactElement } from 'react';
import { TaskPage } from '../TaskPage';

function TaskRoute() {
  const { id } = useParams({ strict: false });
  // Keyed as the app's route is, so moving to another task starts a fresh page.
  return <TaskPage key={id} id={id ?? ''} />;
}

function Stub({ label }: { label: string }) {
  return <p>{label}</p>;
}

/**
 * Renders the task routes in a small router with the app's paths, so links and navigation can be
 * asserted without the shell. `Home` stands in for the inbox at `/`; a test that needs the project
 * tree passes it. Register the test's handlers with `server.use` first.
 */
export function renderTasks(
  initialPath: string,
  Home: () => ReactElement = () => <Stub label="Home page" />
) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } }
  });
  const root = createRootRoute({ component: Outlet });
  const router = createRouter({
    routeTree: root.addChildren([
      createRoute({ getParentRoute: () => root, path: '/', component: Home }),
      createRoute({ getParentRoute: () => root, path: '/tasks/$id', component: TaskRoute }),
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
