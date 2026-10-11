import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory } from '@tanstack/react-router';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { attentionRead } from '@/features/inbox/test/handlers';
import { attentionSnapshot } from '@/features/inbox/test/fixtures';
import { serveProjectPage } from '@/features/projects/test/handlers';
import { tasksList } from '@/features/tasks/test/handlers';
import { server } from '@/test/msw/server';
import { view } from '@/test/fixtures/tasks';
import { createAppRouter } from '@/router';

// The shell reads several endpoints at once; past the limiter's burst, reads would wait out the default waits.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('@/features/tasks/test/fastReads');
  return withFastReads(await importOriginal());
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
  await waitFor(() =>
    expect(
      screen.queryByTestId('desktop-shell') ?? screen.queryByTestId('compact-shell')
    ).not.toBeNull()
  );
  await router.load();
  return router;
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

  it('renders each real panel inside its desktop slot', async () => {
    setViewport(true);
    await renderAt('/');
    const sessions = screen.getByTestId('sessions-panel');
    const inbox = screen.getByTestId('inbox-panel');
    expect(await within(sessions).findByRole('heading', { name: 'Sessions' })).toBeInTheDocument();
    expect(await within(inbox).findByRole('heading', { name: 'Inbox' })).toBeInTheDocument();
  });

  it('renders the projects panel in the desktop aside', async () => {
    setViewport(true);
    await renderAt('/');
    const aside = screen.getByRole('complementary', { name: 'Sessions, inbox and projects' });
    const projects = within(aside).getByTestId('projects-panel');
    expect(await within(projects).findByRole('heading', { name: 'Projects' })).toBeInTheDocument();
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

describe('?view= override', () => {
  it('survives navigation through a link that does not mention it', async () => {
    setViewport(true);
    const user = userEvent.setup();
    const router = await renderAt('/?view=compact');
    await user.click(screen.getByTestId('header-new-agent'));
    await waitFor(() => expect(router.state.location.pathname).toBe('/agents'));
    expect(router.state.location.search).toMatchObject({ view: 'compact' });
    expect(screen.getByTestId('compact-shell')).toBeInTheDocument();
  });
});

describe('routes', () => {
  it('shows the 404 page for an unknown path', async () => {
    setViewport(true);
    await renderAt('/nope');
    expect(screen.getByTestId('error-page')).toHaveTextContent('404');
  });
});

describe('header health link', () => {
  it.each([
    ['desktop', true],
    ['compact', false]
  ])('opens orchestration health from the %s header', async (_shell, wide) => {
    setViewport(wide);
    const user = userEvent.setup();
    const router = await renderAt('/');
    await user.click(screen.getByTestId('header-health'));
    await waitFor(() => expect(router.state.location.pathname).toBe('/health'));
  });
});

describe('project route', () => {
  it('fills the task slot with the tasks of that project', async () => {
    setViewport(true);
    const asked: (string | null)[] = [];
    server.use(
      ...serveProjectPage(),
      tasksList(({ request }) => {
        asked.push(new URL(request.url).searchParams.get('project_id'));
        return HttpResponse.json([
          view({ id: 'task-p', title: 'Project work', project_id: 'project-a' })
        ]);
      })
    );
    await renderAt('/projects/project-a');
    const slot = await screen.findByTestId('project-tasks');
    expect(await within(slot).findByRole('link', { name: /Project work/ })).toHaveAttribute(
      'href',
      '/tasks/task-p'
    );
    expect(asked).toContain('project-a');
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

describe('compact tab badges', () => {
  it('shows the Needs-you count on the Inbox tab, read from GET /attention', async () => {
    setViewport(false);
    server.use(attentionRead(() => HttpResponse.json(attentionSnapshot([], 3))));
    await renderAt('/');
    expect(await screen.findByTestId('inbox-badge')).toHaveTextContent('3');
    expect(screen.getByTestId('tab-inbox')).toHaveAccessibleName('Inbox, 3 need you');
  });

  it('shows no count when nothing needs the owner', async () => {
    setViewport(false);
    await renderAt('/');
    expect(screen.queryByTestId('inbox-badge')).not.toBeInTheDocument();
    expect(screen.queryByTestId('tasks-badge')).not.toBeInTheDocument();
    expect(screen.getByTestId('tab-inbox')).toHaveAccessibleName('Inbox');
  });

  it('shows the owner-task count on the Inbox tab, read from GET /tasks', async () => {
    setViewport(false);
    server.use(tasksList(() => HttpResponse.json([view({ reason: 'owner_decision' })])));
    await renderAt('/');
    expect(await screen.findByTestId('tasks-badge')).toHaveTextContent('1');
    expect(screen.getByTestId('tab-inbox')).toHaveAccessibleName('Inbox, 1 task needs you');
  });

  it('keeps polling attention while another tab is open', async () => {
    setViewport(false);
    let reads = 0;
    server.use(
      attentionRead(() => {
        reads++;
        return HttpResponse.json(attentionSnapshot([], 0));
      })
    );
    await renderAt('/?tab=chat');
    expect(screen.queryByTestId('inbox-panel')).not.toBeInTheDocument();
    // The shared poll starts at once and repeats about every four seconds, so this waits past one.
    await waitFor(() => expect(reads).toBeGreaterThanOrEqual(2), { timeout: 6000 });
  });
});
