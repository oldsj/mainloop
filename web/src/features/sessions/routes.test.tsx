import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { createMemoryHistory, RouterProvider } from '@tanstack/react-router';
import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection } from '@/api';
import { createAppRouter } from '@/router';
import { server } from '@/test/msw/server';
import { attentionSnapshot, native, session, type SessionFixture } from './test/fixtures';
import {
  attentionRead,
  nativeReadWith,
  otherListReads,
  sessionAction,
  sessionRead,
  serveSession
} from './test/handlers';

// The session reads here would otherwise queue behind the app's read limiter. See fastReads.ts.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('./test/fastReads');
  return withFastReads(await importOriginal());
});

// The first render of this file mounts the whole app through the real router; under a full,
// parallel test run that cold render can take longer than the default timeouts allow.
vi.setConfig({ testTimeout: 15000 });

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
  return { router, queryClient };
}

/** Attention and the page's reads for several sessions, each answered for its own id. */
function servePages(items: SessionFixture[]) {
  return [
    attentionRead(() => HttpResponse.json(attentionSnapshot(items))),
    sessionRead(({ params }) => {
      const item = items.find((candidate) => candidate.id === params.session_id);
      return item ? HttpResponse.json(item) : new HttpResponse(null, { status: 404 });
    }),
    // A turn is running, so Stop turn is offered on each page.
    nativeReadWith(({ params }) =>
      HttpResponse.json(native({ session_id: String(params.session_id), turn_in_flight: true }))
    ),
    ...otherListReads()
  ];
}

const FIRST = session({ id: 's1', title: 'First job', status: 'active' });
const SECOND = session({ id: 's2', title: 'Second job', status: 'active' });

describe('sessions routes', () => {
  it('serves the new-agent form at /agents', async () => {
    server.use(...otherListReads());
    renderApp('/agents?view=compact');
    // Longer than the default: the app's first render through the real router is slow under a full run.
    expect(
      await screen.findByRole('heading', { name: 'New agent session' }, { timeout: 8000 })
    ).toBeInTheDocument();
    expect(screen.getByTestId('agent-start')).toBeInTheDocument();
  });

  it('serves the session page at /sessions/:id', async () => {
    server.use(...serveSession(session({ id: 's1', title: 'Routed session' })));
    renderApp('/sessions/s1?view=compact');
    // Longer than the default: the app's first render through the real router is slow under a full run.
    expect(
      await screen.findByRole('heading', { level: 1, name: 'Routed session' }, { timeout: 8000 })
    ).toBeInTheDocument();
    expect(screen.getByTestId('session-chat-slot')).toBeInTheDocument();
  });
});

describe('moving between sessions', () => {
  it('does not carry a finished action notice over to the next session', async () => {
    server.use(
      sessionAction('stop-turn', () => HttpResponse.json({ status: 'stopped' })),
      ...servePages([FIRST, SECOND])
    );
    const { router } = renderApp('/sessions/s1?view=compact');
    const user = userEvent.setup();
    // Longer than the default: the app's first render through the real router is slow under a full run.
    await user.click(await screen.findByTestId('session-stop-turn', {}, { timeout: 8000 }));
    expect(await screen.findByTestId('session-action-notice')).toHaveTextContent('Turn stopped.');

    await act(() =>
      router.navigate({ to: '/sessions/$id', params: { id: 's2' }, search: (prev) => prev })
    );
    expect(
      await screen.findByRole('heading', { level: 1, name: 'Second job' })
    ).toBeInTheDocument();
    expect(screen.queryByTestId('session-action-notice')).not.toBeInTheDocument();
    expect(screen.getByTestId('session-stop-turn')).toBeEnabled();
  });

  it('does not carry a pending action over: the next session keeps its buttons enabled', async () => {
    let release!: () => void;
    const released = new Promise<void>((resolve) => {
      release = resolve;
    });
    server.use(
      sessionAction('stop-turn', async () => {
        await released;
        return HttpResponse.json({ status: 'stopped' });
      }),
      ...servePages([FIRST, SECOND])
    );
    const { router, queryClient } = renderApp('/sessions/s1?view=compact');
    const user = userEvent.setup();
    // Longer than the default: the app's first render through the real router is slow under a full run.
    await user.click(await screen.findByTestId('session-stop-turn', {}, { timeout: 8000 }));
    expect(await screen.findByRole('button', { name: 'Stopping…' })).toBeDisabled();

    await act(() =>
      router.navigate({ to: '/sessions/$id', params: { id: 's2' }, search: (prev) => prev })
    );
    expect(
      await screen.findByRole('heading', { level: 1, name: 'Second job' })
    ).toBeInTheDocument();
    expect(screen.getByTestId('session-stop-turn')).toBeEnabled();
    expect(screen.getByTestId('session-stop-turn')).toHaveTextContent(/^Stop turn$/);

    release();
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
  });
});
