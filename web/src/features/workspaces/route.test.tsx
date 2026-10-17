import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  Outlet,
  RouterProvider
} from '@tanstack/react-router';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection } from '@/api';
import { API_URL } from '@/api/config';
import { Route } from '@/routes/workspaces.$id';
import { workspaceFixture } from '@/test/fixtures';
import { server } from '@/test/msw/server';
import { portsRead, problem, tasksRead, workspaceAction } from './test/handlers';

// The page's reads would otherwise queue behind the app's read limiter. See fastReads.ts.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('@/features/sessions/test/fastReads');
  return withFastReads(await importOriginal());
});

afterEach(async () => {
  vi.restoreAllMocks();
  await connection.check();
});

// The first render of this file mounts the route through a real router; under a full,
// parallel test run that cold render can take longer than the default timeouts allow.
vi.setConfig({ testTimeout: 15000 });

/**
 * Mounts the app's real workspace route, so the test covers the keying in `routes/workspaces.$id.tsx`.
 * Any workspace id is served, echoing the id back so a test can tell the two pages apart.
 */
function renderRoute(initialPath: string) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } }
  });
  const root = createRootRoute({ component: Outlet });
  // Attach the generated route to this test's root, as routeTree.gen.ts does for the app.
  const route = Route.update({
    id: '/workspaces/$id',
    path: '/workspaces/$id',
    getParentRoute: () => root
  } as never);
  const router = createRouter({
    routeTree: root.addChildren([route]),
    history: createMemoryHistory({ initialEntries: [initialPath] })
  });
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  );
  return { router };
}

describe('workspace route', () => {
  it("clears one workspace's action notice when the route moves to another workspace", async () => {
    server.use(
      http.get(`${API_URL}/workspaces/:workspace_id`, ({ params }) =>
        HttpResponse.json(workspaceFixture({ workspace_id: String(params.workspace_id) }))
      ),
      portsRead(() => HttpResponse.json({ ports: [] })),
      tasksRead([]),
      workspaceAction('suspend', problem(409, 'Workspace has an open delivery.'))
    );
    const user = userEvent.setup();
    const { router } = renderRoute('/workspaces/w1');

    // Longer than the default: the app's first render through the real router is slow under a full run.
    await user.click(
      await screen.findByRole('button', { name: 'Suspend workspace' }, { timeout: 8000 })
    );
    expect(await screen.findByTestId('workspace-action-notice')).toHaveTextContent(
      'Workspace has an open delivery.'
    );

    await router.navigate({ to: '/workspaces/$id', params: { id: 'w2' } });

    expect(await screen.findByText('w2')).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.queryByTestId('workspace-action-notice')).not.toBeInTheDocument()
    );
  });
});
