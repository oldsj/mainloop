import type * as Explorer from '@/features/explorer';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory } from '@tanstack/react-router';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createAppRouter } from '@/router';

// The first render of this file mounts the whole app through the real router; under a full,
// parallel test run that cold render can take longer than the default timeouts allow.
vi.setConfig({ testTimeout: 15000 });

const failing = vi.hoisted(() => ({ on: true }));

vi.mock('@/features/explorer', async (importOriginal) => {
  const actual = await importOriginal<typeof Explorer>();
  return {
    ...actual,
    ExplorerAside: () => {
      if (failing.on) throw new Error('explorer exploded');
      return <section data-testid="explorer-aside">Explorer</section>;
    },
    ExplorerPane: () => {
      if (failing.on) throw new Error('explorer exploded');
      return <section data-testid="explorer-pane">Explorer</section>;
    }
  };
});

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
  // Longer than the default: the app's first render through the real router is slow under a full run.
  await waitFor(
    () =>
      expect(
        screen.queryByTestId('desktop-shell') ?? screen.queryByTestId('compact-shell')
      ).not.toBeNull(),
    { timeout: 8000 }
  );
}

describe('panel error boundaries', () => {
  beforeEach(() => {
    failing.on = true;
    // React reports a caught render error to the console.
    vi.spyOn(console, 'error').mockImplementation(() => {});
  });
  afterEach(() => vi.restoreAllMocks());

  it('keeps the chat when the explorer throws, and retries on request', async () => {
    setViewport(true);
    await renderAt('/');
    expect(await screen.findByTestId('panel-error-explorer')).toHaveTextContent(
      'explorer exploded'
    );
    expect(screen.getByTestId('main-thread')).toBeInTheDocument();

    failing.on = false;
    await userEvent.click(screen.getByRole('button', { name: 'Try again' }));
    expect(await screen.findByTestId('explorer-aside')).toBeInTheDocument();
  });

  it('contains a failing explorer in the compact shell too', async () => {
    setViewport(false);
    await renderAt('/?tab=explorer');
    expect(await screen.findByTestId('panel-error-explorer')).toBeInTheDocument();
    expect(screen.getByTestId('mobile-tab-bar')).toBeInTheDocument();
  });
});
