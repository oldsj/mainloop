import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { createMemoryHistory, RouterProvider } from '@tanstack/react-router';
import { render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { connection } from '@/api';
import { createAppRouter } from '@/router';
import { server } from '@/test/msw/server';
import { session } from './test/fixtures';
import { otherListReads, serveSession } from './test/handlers';

afterEach(async () => {
  await connection.check();
});

/**
 * Smoke test through the app's own router, so the route files and their paths are what is
 * checked. `view=compact` puts the compact shell in place, as the shell test does.
 */
function renderApp(url: string) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const router = createAppRouter(queryClient, createMemoryHistory({ initialEntries: [url] }));
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  );
  return router;
}

describe('sessions routes', () => {
  it('serves the new-agent form at /agents', async () => {
    server.use(...otherListReads());
    renderApp('/agents?view=compact');
    expect(await screen.findByRole('heading', { name: 'New agent session' })).toBeInTheDocument();
    expect(screen.getByTestId('agent-start')).toBeInTheDocument();
  });

  it('serves the session page at /sessions/:id', async () => {
    server.use(...serveSession(session({ id: 's1', title: 'Routed session' })));
    renderApp('/sessions/s1?view=compact');
    expect(
      await screen.findByRole('heading', { level: 1, name: 'Routed session' })
    ).toBeInTheDocument();
    expect(screen.getByTestId('session-chat-slot')).toBeInTheDocument();
  });
});
