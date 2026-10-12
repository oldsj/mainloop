import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory } from '@tanstack/react-router';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { attentionRead } from '@/features/inbox/test/handlers';
import { attentionSnapshot } from '@/features/inbox/test/fixtures';
import { writeLastVisit } from '@/features/home/lastVisit';
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
  window.localStorage.clear();
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
    const aside = screen.getByRole('complementary', { name: 'Work, sessions, inbox and projects' });
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

  it('updates the task count without a live event while another tab is open', async () => {
    setViewport(false);
    let needsOwner = true;
    let reads = 0;
    server.use(
      tasksList(() => {
        reads++;
        return HttpResponse.json([view({ reason: needsOwner ? 'owner_decision' : null })]);
      })
    );
    await renderAt('/?tab=chat');
    expect(await screen.findByTestId('tasks-badge')).toHaveTextContent('1');
    expect(screen.queryByTestId('inbox-panel')).not.toBeInTheDocument();
    needsOwner = false;
    await waitFor(() => expect(screen.queryByTestId('tasks-badge')).not.toBeInTheDocument(), {
      timeout: 6000
    });
    expect(reads).toBeGreaterThanOrEqual(2);
    expect(screen.getByTestId('tab-inbox')).toHaveAccessibleName('Inbox');
  });
});

describe('page titles and focus', () => {
  it('titles the page for its route', async () => {
    setViewport(true);
    const user = userEvent.setup();
    const router = await renderAt('/');
    await waitFor(() => expect(document.title).toBe('Chat · Mainloop'));
    await user.click(screen.getByTestId('header-health'));
    await waitFor(() => expect(router.state.location.pathname).toBe('/health'));
    await waitFor(() => expect(document.title).toBe('Health · Mainloop'));
  });

  it('moves focus to the main landmark after a route change', async () => {
    setViewport(true);
    const user = userEvent.setup();
    const router = await renderAt('/');
    await user.click(screen.getByTestId('header-health'));
    await waitFor(() => expect(router.state.location.pathname).toBe('/health'));
    await waitFor(() =>
      expect(document.activeElement).toBe(document.getElementById('main-content'))
    );
  });

  it('leaves focus alone on the page the app opens on', async () => {
    setViewport(true);
    await renderAt('/health');
    expect(document.activeElement).not.toBe(document.getElementById('main-content'));
  });
});

describe('compact projects tab', () => {
  it('has four short tabs, with Projects among them', async () => {
    setViewport(false);
    await renderAt('/');
    const bar = screen.getByTestId('mobile-tab-bar');
    const tabs = within(bar).getAllByRole('button');
    expect(tabs.length).toBeLessThanOrEqual(5);
    expect(tabs.map((tab) => tab.textContent?.replace(/\s+/g, ' ').trim())).toEqual([
      '[CHAT]',
      '[SESSIONS]',
      '[INBOX]',
      '[PROJECTS]'
    ]);
    expect(screen.getByTestId('tab-projects')).toHaveAttribute('aria-label', 'Projects');
  });

  it('opens the projects panel from its tab, and keeps it mounted after switching away', async () => {
    setViewport(false);
    const user = userEvent.setup();
    await renderAt('/');
    await user.click(screen.getByTestId('tab-projects'));
    const projects = await screen.findByTestId('projects-panel');
    expect(screen.getByTestId('tab-projects')).toHaveAttribute('aria-pressed', 'true');
    await user.click(screen.getByTestId('tab-chat'));
    expect(projects).not.toBeVisible();
    await user.click(screen.getByTestId('tab-projects'));
    expect(screen.getByTestId('projects-panel')).toBe(projects);
    expect(projects).toBeVisible();
  });

  it('opens from the URL with ?tab=projects', async () => {
    setViewport(false);
    await renderAt('/?tab=projects');
    expect(await screen.findByTestId('projects-panel')).toBeVisible();
  });
});

describe('compact tab state', () => {
  it('keeps an opened tab mounted when switching away and back', async () => {
    setViewport(false);
    const user = userEvent.setup();
    await renderAt('/?tab=sessions');
    const sessions = await screen.findByTestId('sessions-panel');
    await user.click(screen.getByTestId('tab-inbox'));
    expect(sessions).not.toBeVisible();
    await user.click(screen.getByTestId('tab-sessions'));
    expect(screen.getByTestId('sessions-panel')).toBe(sessions);
    expect(sessions).toBeVisible();
  });
});

describe('project page title', () => {
  it('keeps the project page title as the only title element', async () => {
    setViewport(true);
    server.use(...serveProjectPage());
    await renderAt('/projects/project-a');
    await waitFor(() => expect(document.title).toBe('acme/widgets - mainloop'));
    expect(document.querySelectorAll('title')).toHaveLength(1);
  });
});

describe('home work placement', () => {
  const MIN = 60_000;
  const failedNow = () => new Date(Date.now() - MIN).toISOString();

  function serveFailedSinceVisit() {
    writeLastVisit(Date.now() - 60 * MIN);
    server.use(
      tasksList(() =>
        HttpResponse.json([
          view({ id: 'fail-1', title: 'Broke', status: 'failed', updated_at: failedNow() })
        ])
      )
    );
  }

  it('puts the work list and the strip at the top of the desktop aside, above sessions', async () => {
    setViewport(true);
    serveFailedSinceVisit();
    await renderAt('/');
    const aside = screen.getByRole('complementary', { name: 'Work, sessions, inbox and projects' });
    const work = within(aside).getByTestId('work-panel');
    expect(await within(work).findByTestId('since-strip')).toBeInTheDocument();
    expect(within(work).getByTestId('work-list')).toBeInTheDocument();
    const sessions = within(aside).getByTestId('sessions-panel');
    expect(work.compareDocumentPosition(sessions) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('keeps the work list and the strip out of the desktop centre column', async () => {
    setViewport(true);
    serveFailedSinceVisit();
    await renderAt('/');
    await screen.findByTestId('since-strip');
    const main = screen.getByRole('main');
    expect(within(main).queryByTestId('work-list')).toBeNull();
    expect(within(main).queryByTestId('since-strip')).toBeNull();
  });

  it('shows the strip as one line above the thread on the phone chat tab', async () => {
    setViewport(false);
    serveFailedSinceVisit();
    await renderAt('/');
    const strip = await screen.findByTestId('since-strip');
    expect(strip).toBeVisible();
    expect(strip.className).toContain('flex');
    expect(within(strip).getByTestId('since-failed')).toHaveAttribute('href', '/tasks/fail-1');
    expect(screen.queryByTestId('work-list')).toBeNull();
  });

  it('puts Work above the project list in the Projects tab', async () => {
    setViewport(false);
    const user = userEvent.setup();
    serveFailedSinceVisit();
    await renderAt('/');
    await user.click(screen.getByTestId('tab-projects'));
    const work = await screen.findByTestId('work-list');
    const projects = await screen.findByTestId('projects-panel');
    expect(work.compareDocumentPosition(projects) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(within(work).getByRole('heading', { name: 'Work' })).toBeInTheDocument();
    expect(within(projects).queryByTestId('since-strip')).toBeNull();
  });

  it('records this visit when the shell opens, so the next one compares against it', async () => {
    setViewport(true);
    await renderAt('/');
    const recorded = Number(window.localStorage.getItem('mainloop.home.lastVisit'));
    expect(Math.abs(recorded - Date.now())).toBeLessThan(10_000);
  });
});
