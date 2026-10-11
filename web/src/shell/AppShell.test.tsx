import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory } from '@tanstack/react-router';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it } from 'vitest';
import { createAppRouter } from '@/router';

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
      screen.queryByTestId('desktop-shell') ?? screen.queryByTestId('compact-shell')
    ).not.toBeNull()
  );
  await router.load();
}

afterEach(() => {
  document.documentElement.removeAttribute('data-theme');
});

describe('shell switch', () => {
  it('renders the desktop shell on a wide viewport', async () => {
    setViewport(true);
    await renderAt('/');
    expect(screen.getByTestId('desktop-shell')).toBeInTheDocument();
    expect(screen.queryByTestId('mobile-tab-bar')).not.toBeInTheDocument();
  });

  it('renders the compact shell on a narrow viewport', async () => {
    setViewport(false);
    await renderAt('/');
    expect(screen.getByTestId('compact-shell')).toBeInTheDocument();
    expect(screen.getByTestId('mobile-tab-bar')).toBeInTheDocument();
  });

  it('lets ?view=compact win on a wide viewport', async () => {
    setViewport(true);
    await renderAt('/?view=compact');
    expect(screen.getByTestId('compact-shell')).toBeInTheDocument();
  });

  it('shows the sessions panel when the sessions tab is selected', async () => {
    setViewport(false);
    await renderAt('/?tab=sessions');
    expect(screen.getByTestId('sessions-panel')).toBeInTheDocument();
    expect(screen.getByTestId('tab-sessions')).toHaveAttribute('aria-pressed', 'true');
  });

  it('switches tabs from the tab bar', async () => {
    setViewport(false);
    const user = userEvent.setup();
    await renderAt('/');
    await user.click(screen.getByTestId('tab-inbox'));
    expect(await screen.findByTestId('inbox-panel')).toBeInTheDocument();
  });
});

describe('routes', () => {
  it('shows the 404 page for an unknown path', async () => {
    setViewport(true);
    await renderAt('/nope');
    expect(screen.getByTestId('error-page')).toHaveTextContent('404');
  });
});

describe('theme selector', () => {
  it('sets data-theme on <html> and saves the choice', async () => {
    setViewport(true);
    const user = userEvent.setup();
    await renderAt('/');
    await user.click(screen.getByTestId('theme-selector'));
    await user.click(await screen.findByTestId('theme-nord'));
    expect(document.documentElement).toHaveAttribute('data-theme', 'nord');
    expect(localStorage.getItem('mainloop-theme')).toBe('nord');
  });
});
