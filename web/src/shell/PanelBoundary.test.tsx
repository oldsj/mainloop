import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory } from '@tanstack/react-router';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createAppRouter } from '@/router';

const failing = vi.hoisted(() => ({ on: true }));

vi.mock('./slots', () => ({
  SessionsPanel: () => {
    if (failing.on) throw new Error('sessions exploded');
    return <section data-testid="sessions-panel">Sessions</section>;
  },
  InboxPanel: () => <section data-testid="inbox-panel">Inbox</section>,
  ProjectsPanel: () => <section data-testid="projects-panel">Projects</section>
}));

function setViewport(wide: boolean) {
  window.matchMedia = ((query: string) =>
    ({
      matches: wide,
      media: query,
      addEventListener: () => {},
      removeEventListener: () => {}
    }) as unknown as MediaQueryList) as typeof window.matchMedia;
}

async function renderAt(url: string) {
  const queryClient = new QueryClient();
  const router = createAppRouter(queryClient, createMemoryHistory({ initialEntries: [url] }));
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  );
  await waitFor(() =>
    expect(
      screen.queryByTestId('inbox-panel') ?? screen.queryByTestId('compact-shell')
    ).not.toBeNull()
  );
}

describe('panel error boundaries', () => {
  beforeEach(() => {
    failing.on = true;
    // React reports a caught render error to the console.
    vi.spyOn(console, 'error').mockImplementation(() => {});
  });
  afterEach(() => vi.restoreAllMocks());

  it('keeps the other desktop panels when one panel throws, and retries on request', async () => {
    setViewport(true);
    await renderAt('/');
    expect(await screen.findByTestId('panel-error-sessions')).toHaveTextContent(
      'sessions exploded'
    );
    expect(screen.getByTestId('inbox-panel')).toBeInTheDocument();
    expect(screen.getByTestId('projects-panel')).toBeInTheDocument();
    expect(screen.getByTestId('main-thread')).toBeInTheDocument();

    failing.on = false;
    await userEvent.click(screen.getByRole('button', { name: 'Try again' }));
    expect(await screen.findByTestId('sessions-panel')).toBeInTheDocument();
  });

  it('contains a failing panel in the compact shell too', async () => {
    setViewport(false);
    await renderAt('/?tab=sessions');
    expect(await screen.findByTestId('panel-error-sessions')).toBeInTheDocument();
    expect(screen.getByTestId('mobile-tab-bar')).toBeInTheDocument();
  });
});
