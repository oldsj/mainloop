import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory } from '@tanstack/react-router';
import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createFakeHITL } from '@/features/hitl/handlers';
import { activityRead, attentionRead, respondPost } from '@/features/inbox/test/handlers';
import {
  attentionItem,
  attentionSnapshot,
  hitlAttentionItem,
  queueItem
} from '@/features/inbox/test/fixtures';
import {
  projectsRead,
  projectsReadWith,
  serveProjectPage
} from '@/features/projects/test/handlers';
import { native, session } from '@/features/sessions/test/fixtures';
import { nativeRead, sessionRead } from '@/features/sessions/test/handlers';
import { project } from '@/features/projects/test/fixtures';
import { serveTaskPage, taskRead, taskScreen, tasksList } from '@/features/tasks/test/handlers';
import { hitlView } from '@/test/fixtures/hitl';
import type * as ConnectionModule from '@/api/connection';
import { API_URL } from '@/api/config';
import { connection } from '@/api/connection';
import { testSeen } from '@/test/msw/handlers';
import { server } from '@/test/msw/server';
import { justNow, projection, view } from '@/test/fixtures/tasks';
import { createAppRouter } from '@/router';

// The first render of this file mounts the whole app through the real router; under a full,
// parallel test run that cold render can take longer than the default timeouts allow.
vi.setConfig({ testTimeout: 15000 });

// The banner reads one shared connection; a test flips its probe to take the app offline and back.
const reach = vi.hoisted(() => ({ up: true }));
vi.mock('@/api/connection', async (importOriginal) => {
  const actual = await importOriginal<typeof ConnectionModule>();
  return { ...actual, connection: actual.createConnection({ probe: async () => reach.up }) };
});

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
  // Longer than the default: the app's first render through the real router is slow under a full run.
  await waitFor(
    () =>
      expect(
        screen.queryByTestId('desktop-shell') ?? screen.queryByTestId('compact-shell')
      ).not.toBeNull(),
    { timeout: 8000 }
  );
  await router.load();
  return router;
}

afterEach(() => {
  document.documentElement.removeAttribute('data-theme');
  window.localStorage.clear();
  // A composer draft is kept per browser tab, so one test's unsent text would open the next test's chat.
  window.sessionStorage.clear();
});

describe('shell switch', () => {
  it('renders the desktop shell on a wide viewport', async () => {
    setViewport(true);
    await renderAt('/');
    expect(screen.getByTestId('desktop-shell')).toBeInTheDocument();
    expect(screen.queryByTestId('mobile-tab-bar')).not.toBeInTheDocument();
  });

  it('lays the desktop out left to right: activity bar, sidebar, tabs over the page, and a status bar under them', async () => {
    setViewport(true);
    await renderAt('/');
    const bar = screen.getByRole('navigation', { name: 'Primary' });
    const aside = screen.getByRole('complementary', { name: 'Sidebar' });
    const strip = screen.getByTestId('tab-strip');
    const main = screen.getByRole('main');
    const status = screen.getByTestId('status-bar');
    expect(await within(aside).findByRole('group', { name: 'Agents' })).toBeInTheDocument();
    expect(main).not.toContainElement(aside);
    const follows = (a: Element, b: Element) =>
      Boolean(a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING);
    expect(follows(bar, aside)).toBe(true);
    expect(follows(aside, strip)).toBe(true);
    expect(follows(strip, main)).toBe(true);
    expect(follows(main, status)).toBe(true);
    // There is no top header: the bar of icons is the first thing on the left.
    expect(screen.queryByText('$ mainloop')).toBeNull();
    expect(screen.getByTestId('desktop-shell').firstElementChild?.firstElementChild).toBe(bar);
    // Mainloop's mark leads; then the Inbox, the sidebar's button and search; health, Settings and
    // the theme are at the foot.
    const home = within(bar).getByRole('link', { name: 'Mainloop home' });
    expect(within(home).getByTestId('logo-mark')).toBeInTheDocument();
    expect(
      [...bar.querySelectorAll('a, button')].map((item) => item.getAttribute('aria-label'))
    ).toEqual([
      'Mainloop home',
      'Inbox',
      'Spaces and agents',
      'Search',
      'Orchestration health',
      'Settings',
      expect.stringMatching(/^Theme: /)
    ]);
    // The sidebar's Agents heading has its buttons: start something, and search and commands.
    const row = within(aside).getByTestId('column-actions');
    expect(
      within(row)
        .getAllByRole('button')
        .map((button) => button.getAttribute('aria-label'))
    ).toEqual(['New: ask Mainloop', 'Search and commands']);
  });

  it('puts a strip of tabs over the page: the current space’s agents, with the page’s own lit', async () => {
    setViewport(true);
    const user = userEvent.setup();
    const running = view({ id: 'task-r', title: 'Add retry button', status: 'running' });
    server.use(
      projectsRead([project({ id: running.task.project_id ?? undefined })]),
      tasksList(() => HttpResponse.json([running])),
      taskRead(() => HttpResponse.json(running)),
      // The agent's conversation is the session of the task's current attempt.
      sessionRead(() => HttpResponse.json(session({ id: 'session-1' }))),
      nativeRead(native({ session_id: 'session-1' }))
    );
    const router = await renderAt('/');
    // Given the main chat: the strip is the main thread's space, above the page and outside it
    const strip = () => screen.getByTestId('tab-strip');
    expect(strip()).toHaveAccessibleName('Agents in main');
    expect(screen.getByRole('main')).not.toContainElement(strip());
    expect(within(strip()).getByRole('button', { current: 'page' })).toHaveTextContent('Mainloop');

    // When the owner opens an agent from the column
    await user.click(
      await within(screen.getByTestId('explorer-aside')).findByRole('button', {
        name: /^Add retry button/
      })
    );

    // Then its conversation opens, and the strip is that agent's space, with its tab lit beside
    // the project's overview
    await waitFor(() => expect(router.state.location.pathname).toBe('/sessions/session-1'));
    await waitFor(() => expect(strip()).toHaveAccessibleName('Agents in widgets'));
    expect(
      within(strip())
        .getAllByTestId('tab')
        .map((tab) => tab.textContent)
    ).toEqual(['Overview', 'Add retry button']);
    expect(within(strip()).getByRole('button', { current: 'page' })).toHaveTextContent(
      'Add retry button'
    );

    // And a page that is no agent's gets a tab of its own beside the main chat's
    await act(() => router.navigate({ to: '/settings' }));
    await waitFor(() =>
      expect(
        within(strip())
          .getAllByTestId('tab')
          .map((tab) => tab.textContent)
      ).toEqual(['Mainloop', 'Settings'])
    );
  });

  it('renders the compact shell on a narrow viewport: the tabs on top, the page, and a bar at the bottom', async () => {
    setViewport(false);
    await renderAt('/');
    const shell = screen.getByTestId('compact-shell');
    expect(screen.queryByTestId('status-bar')).toBeNull();
    expect(screen.queryByTestId('activity-bar')).toBeNull();
    // The row on top is the space's tabs alone.
    const strip = screen.getByTestId('tab-strip');
    expect(shell.firstElementChild).toBe(strip);
    expect([...strip.children].map((child) => child.getAttribute('data-testid'))).toEqual([
      'tab-scroller'
    ]);
    expect(within(strip).getByRole('button', { current: 'page' })).toHaveTextContent('Mainloop');
    // The bar at the bottom is the desktop's activity bar laid flat, with words under the icons.
    const bar = screen.getByTestId('mobile-tab-bar');
    expect(shell.lastElementChild).toBe(bar);
    expect(
      within(bar)
        .getAllByRole('button')
        .map((item) => [item.textContent, item.getAttribute('aria-label')])
    ).toEqual([
      ['Chat', 'Chat'],
      ['Inbox', 'Inbox'],
      ['Agents', 'Spaces and agents'],
      ['Search', 'Search']
    ]);
    expect(within(bar).getByRole('button', { name: 'Chat' })).toHaveAttribute(
      'aria-current',
      'page'
    );
    expect(within(bar).getByTestId('logo-mark')).toBeInTheDocument();
    // The page is between them, and the lists are not drawn until they are asked for.
    expect(screen.getByRole('main')).not.toContainElement(strip);
    expect(screen.queryByTestId('agent-column')).toBeNull();
  });

  it('opens the command palette from the phone’s Search', async () => {
    setViewport(false);
    const user = userEvent.setup();
    await renderAt('/');
    await user.click(screen.getByTestId('tab-search'));
    expect(await screen.findByRole('combobox')).toHaveFocus();
  });

  it('lets ?view=compact win on a wide viewport', async () => {
    setViewport(true);
    await renderAt('/?view=compact');
    expect(screen.getByTestId('compact-shell')).toBeInTheDocument();
  });

  it('shows the spaces and agents over the page when the URL asks for them', async () => {
    setViewport(false);
    await renderAt('/?tab=explorer');
    expect(await screen.findByRole('group', { name: 'Agents' })).toBeVisible();
    expect(screen.getByTestId('agents-toggle')).toHaveAttribute('aria-expanded', 'true');
    expect(screen.queryByRole('tree')).toBeNull();
  });

  it('opens the spaces and agents from their button, and puts them away again', async () => {
    setViewport(false);
    const user = userEvent.setup();
    const router = await renderAt('/');
    const toggle = screen.getByTestId('agents-toggle');
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await user.click(toggle);
    expect(await screen.findByRole('group', { name: 'Agents' })).toBeVisible();
    // It is in the URL, so the phone's Back puts the list away.
    expect(router.state.location.search).toMatchObject({ tab: 'explorer' });
    await user.click(toggle);
    await waitFor(() => expect(screen.queryByTestId('agent-column')).toBeNull());
    expect(router.state.location.search).not.toHaveProperty('tab');
    expect(screen.getByTestId('main-thread')).toBeVisible();
  });

  it.each(['sessions', 'projects'])(
    'maps the old ?tab=%s value to the spaces and agents',
    async (legacy) => {
      setViewport(false);
      await renderAt(`/?tab=${legacy}`);
      expect(await screen.findByRole('group', { name: 'Agents' })).toBeVisible();
      expect(screen.getByTestId('agents-toggle')).toHaveAttribute('aria-expanded', 'true');
    }
  );
});

describe('?view= override', () => {
  it('survives navigation through a link that does not mention it', async () => {
    setViewport(true);
    const user = userEvent.setup();
    const router = await renderAt('/?view=compact');
    // The phone's Inbox is a button in its top row.
    await user.click(screen.getByTestId('inbox-link'));
    await waitFor(() => expect(router.state.location.pathname).toBe('/inbox'));
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
  it('opens Settings from the desktop sidebar’s menu, with a tab of its own', async () => {
    setViewport(true);
    const user = userEvent.setup();
    const router = await renderAt('/');
    await user.click(screen.getByRole('button', { name: 'Search and commands' }));
    await user.type(await screen.findByRole('combobox'), 'settings');
    await user.keyboard('{Enter}');
    await waitFor(() => expect(router.state.location.pathname).toBe('/settings'));
    expect(await screen.findByRole('heading', { name: 'Settings', level: 1 })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Session defaults' })).toBeInTheDocument();
    expect(
      within(screen.getByTestId('tab-strip')).getByRole('button', { current: 'page' })
    ).toHaveTextContent('Settings');
    await waitFor(() => expect(document.title).toBe('Settings · Mainloop'));
  });

  it('opens Settings on a phone from the menu, under the spaces and agents', async () => {
    setViewport(false);
    const user = userEvent.setup();
    const router = await renderAt('/?tab=explorer');
    await user.click(await screen.findByRole('button', { name: 'Search and commands' }));
    await user.type(await screen.findByRole('combobox'), 'settings');
    await user.keyboard('{Enter}');
    await waitFor(() => expect(router.state.location.pathname).toBe('/settings'));
    expect(await screen.findByTestId('settings-page')).toBeVisible();
    // The page shows, not the lists it was opened from.
    expect(screen.queryByTestId('agent-column')).toBeNull();
  });

  it('opens orchestration health from the desktop status bar’s connection', async () => {
    setViewport(true);
    const user = userEvent.setup();
    const router = await renderAt('/');
    await user.click(screen.getByTestId('status-connection'));
    await waitFor(() => expect(router.state.location.pathname).toBe('/health'));
  });

  it('opens an agent on a phone from the list, and shows its page with its tab lit, not the list', async () => {
    setViewport(false);
    const user = userEvent.setup();
    const queued = view({
      id: 'task-q',
      title: 'Queue backend',
      status: 'queued',
      current_attempt_id: null
    });
    server.use(
      tasksList(() => HttpResponse.json([queued])),
      taskRead(() => HttpResponse.json(queued))
    );
    const router = await renderAt('/');
    await user.click(screen.getByTestId('agents-toggle'));
    await user.click(
      await within(await screen.findByTestId('agent-column')).findByRole('button', {
        name: /^Queue backend/
      })
    );
    await waitFor(() => expect(router.state.location.pathname).toBe('/tasks/task-q'));
    expect(router.state.location.search).not.toHaveProperty('tab');
    await waitFor(() => expect(screen.queryByTestId('agent-column')).toBeNull());
    expect(
      within(screen.getByTestId('tab-strip')).getByRole('button', { current: 'page' })
    ).toHaveTextContent('Queue backend');
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
    // The theme menu is at the foot of the activity bar; Settings and the palette have the themes too.
    await renderAt('/');
    await user.click(within(screen.getByTestId('activity-bar')).getByTestId('theme-selector'));
    await user.click(await screen.findByTestId('theme-nord'));
    expect(document.documentElement).toHaveAttribute('data-theme', 'nord');
    expect(localStorage.getItem('mainloop-theme')).toBe('nord');
  });
});

describe('the phone’s Inbox count', () => {
  it('shows the Needs-you count on the Inbox button, read from GET /attention', async () => {
    setViewport(false);
    server.use(
      attentionRead(() =>
        HttpResponse.json(
          attentionSnapshot(['a', 'b', 'c'].map((id) => hitlAttentionItem(`req-${id}`)))
        )
      )
    );
    await renderAt('/');
    expect(await screen.findByTestId('needs-badge')).toHaveTextContent('3');
    expect(screen.getByTestId('inbox-link')).toHaveAccessibleName('Inbox, 3 need you');
  });

  it('shows no count when nothing needs the owner', async () => {
    setViewport(false);
    await renderAt('/');
    expect(screen.queryByTestId('needs-badge')).not.toBeInTheDocument();
    expect(screen.getByTestId('inbox-link')).toHaveAccessibleName('Inbox');
  });

  it('counts a task that needs an owner decision, read from GET /tasks', async () => {
    setViewport(false);
    server.use(tasksList(() => HttpResponse.json([view({ reason: 'owner_decision' })])));
    await renderAt('/');
    expect(await screen.findByTestId('needs-badge')).toHaveTextContent('1');
    expect(screen.getByTestId('inbox-link')).toHaveAccessibleName('Inbox, 1 need you');
  });

  it('keeps polling attention while the chat is open', async () => {
    setViewport(false);
    let reads = 0;
    server.use(
      attentionRead(() => {
        reads++;
        return HttpResponse.json(attentionSnapshot([], 0));
      })
    );
    await renderAt('/?tab=chat');
    // The shared poll starts at once and repeats about every four seconds, so this waits past one.
    await waitFor(() => expect(reads).toBeGreaterThanOrEqual(2), { timeout: 6000 });
  });

  it('updates the count from the poll while the event stream is down and the chat is open', async () => {
    setViewport(false);
    let needsOwner = true;
    let reads = 0;
    server.use(
      // With the stream open a change arrives as an event and the poll slows to a safety net.
      http.get(`${API_URL}/events`, () => new HttpResponse(null, { status: 503 })),
      tasksList(() => {
        reads++;
        return HttpResponse.json([view({ reason: needsOwner ? 'owner_decision' : null })]);
      })
    );
    await renderAt('/?tab=chat');
    expect(await screen.findByTestId('needs-badge')).toHaveTextContent('1');
    needsOwner = false;
    await waitFor(() => expect(screen.queryByTestId('needs-badge')).not.toBeInTheDocument(), {
      timeout: 6000
    });
    expect(reads).toBeGreaterThanOrEqual(2);
    expect(screen.getByTestId('inbox-link')).toHaveAccessibleName('Inbox');
  });
});

describe('page titles and focus', () => {
  it('titles the page for its route', async () => {
    setViewport(true);
    const user = userEvent.setup();
    const router = await renderAt('/');
    await waitFor(() => expect(document.title).toBe('Chat · Mainloop'));
    await user.click(screen.getByTestId('status-connection'));
    await waitFor(() => expect(router.state.location.pathname).toBe('/health'));
    await waitFor(() => expect(document.title).toBe('Health · Mainloop'));
  });

  it('moves focus to the main landmark after a route change', async () => {
    setViewport(true);
    const user = userEvent.setup();
    const router = await renderAt('/');
    await user.click(screen.getByTestId('status-connection'));
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

describe('the phone’s top row', () => {
  const lit = () =>
    within(screen.getByTestId('tab-strip'))
      .getAllByTestId('tab')
      .filter((tab) => tab.getAttribute('aria-current') === 'page')
      .map((tab) => tab.textContent);

  it('has no header above it: the row is the first thing on the screen', async () => {
    setViewport(false);
    await renderAt('/');
    expect(screen.queryByText('$ mainloop')).toBeNull();
    expect(screen.getByTestId('compact-shell').firstElementChild).toBe(
      screen.getByTestId('tab-strip')
    );
  });

  it.each([
    ['/', 'Mainloop'],
    ['/inbox', 'Inbox'],
    ['/health', 'Health'],
    ['/settings', 'Settings'],
    ['/inbox?tab=explorer', 'Inbox']
  ])('lights the tab for where the owner is: %s', async (url, tab) => {
    setViewport(false);
    await renderAt(url);
    expect(lit()).toEqual([tab]);
  });

  it('marks the Inbox while the answer view shows, and opens it and the main chat', async () => {
    setViewport(false);
    const user = userEvent.setup();
    const router = await renderAt('/?tab=explorer');
    const inbox = screen.getByTestId('inbox-link');
    expect(inbox).not.toHaveAttribute('aria-current');
    await user.click(inbox);
    await waitFor(() => expect(router.state.location.pathname).toBe('/inbox'));
    expect(router.state.location.search).not.toHaveProperty('tab');
    expect(inbox).toHaveAttribute('aria-current', 'page');
    // The main chat is the first tab of the main thread's space.
    await user.click(
      within(screen.getByTestId('tab-strip')).getByRole('button', { name: 'Mainloop' })
    );
    await waitFor(() => expect(router.state.location.pathname).toBe('/'));
    expect(lit()).toEqual(['Mainloop']);
  });

  it('brings the lists up over a page, keeping the page’s route', async () => {
    setViewport(false);
    const user = userEvent.setup();
    const router = await renderAt('/health');
    await user.click(screen.getByTestId('agents-toggle'));
    expect(await screen.findByRole('group', { name: 'Agents' })).toBeVisible();
    expect(router.state.location.pathname).toBe('/health');
    expect(router.state.location.search).toMatchObject({ tab: 'explorer' });
  });

  it('has the menu under the spaces, which reaches health, Settings and the themes', async () => {
    setViewport(false);
    const user = userEvent.setup();
    await renderAt('/');
    await user.click(screen.getByTestId('agents-toggle'));
    await user.click(await screen.findByRole('button', { name: 'Search and commands' }));
    const options = within(await screen.findByRole('listbox'))
      .getAllByRole('option')
      .map((option) => option.textContent);
    expect(options).toEqual(
      expect.arrayContaining([
        expect.stringMatching(/^Orchestration health/),
        expect.stringMatching(/^Settings/),
        expect.stringMatching(/^Theme: nord/)
      ])
    );
  });
});

describe('the phone’s page under the lists', () => {
  it('keeps the page mounted while the lists cover it, so it is the same page afterwards', async () => {
    setViewport(false);
    const user = userEvent.setup();
    await renderAt('/');
    const thread = await screen.findByTestId('main-thread');
    await user.click(screen.getByTestId('agents-toggle'));
    await screen.findByRole('group', { name: 'Agents' });
    expect(thread).not.toBeVisible();
    await user.click(screen.getByTestId('agents-toggle'));
    await waitFor(() => expect(thread).toBeVisible());
    expect(screen.getByTestId('main-thread')).toBe(thread);
  });

  it('does not build the page until it is the thing showing', async () => {
    setViewport(false);
    const user = userEvent.setup();
    await renderAt('/?tab=explorer');
    await screen.findByRole('group', { name: 'Agents' });
    expect(screen.queryByTestId('main-thread')).toBeNull();
    await user.click(screen.getByTestId('agents-toggle'));
    expect(await screen.findByTestId('main-thread')).toBeVisible();
  });
});

describe('project page title', () => {
  it('keeps the project page title as the only title element', async () => {
    setViewport(true);
    server.use(...serveProjectPage());
    await renderAt('/projects/project-a');
    await waitFor(() => expect(document.title).toBe('acme/widgets · Mainloop'));
    expect(document.querySelectorAll('title')).toHaveLength(1);
  });
});

describe('home work placement', () => {
  const MIN = 60_000;
  const failedNow = () => new Date(Date.now() - MIN).toISOString();

  function serveFailedSinceVisit() {
    // The owner's last visit, on any device, was an hour ago.
    testSeen.begin({ since: Date.now() - 120 * MIN, lastVisit: Date.now() - 60 * MIN });
    const failed = view({
      id: 'fail-1',
      title: 'Broke',
      status: 'failed',
      updated_at: failedNow()
    });
    server.use(
      tasksList(() => HttpResponse.json([failed])),
      // Opening the failed task reads this task directly.
      taskRead(() => HttpResponse.json(failed))
    );
  }

  it('keeps the desktop centre to the chat alone, with the work in the left column', async () => {
    setViewport(true);
    serveFailedSinceVisit();
    await renderAt('/');
    const aside = screen.getByRole('complementary', { name: 'Sidebar' });
    // The failed task is an agent in the column, with the red dot; nothing about it is in the centre.
    const row = await within(aside).findByRole('button', { name: /^Broke/ });
    expect(within(row).getByTestId('agent-dot')).toHaveAttribute('data-tone', 'bad');
    const main = screen.getByRole('main');
    expect(within(main).queryByText('Broke')).toBeNull();
    expect(within(main).getByTestId('main-thread')).toBeInTheDocument();
    // One scroll in the centre: the thread's own. The pane around it does not scroll.
    expect(main.className).toContain('overflow-hidden');
    expect(main.className).not.toContain('overflow-y-auto');
  });

  it('shows the work in the phone’s list, not above the chat', async () => {
    setViewport(false);
    const user = userEvent.setup();
    serveFailedSinceVisit();
    await renderAt('/');
    expect(within(screen.getByRole('main')).queryByText('Broke')).toBeNull();
    expect(screen.getByRole('main').className).toContain('overflow-hidden');
    await user.click(screen.getByTestId('agents-toggle'));
    // The failed task is an agent in the list, with the red dot.
    const row = await within(await screen.findByTestId('agent-column')).findByRole('button', {
      name: /^Broke/
    });
    expect(within(row).getByTestId('agent-dot')).toHaveAttribute('data-tone', 'bad');
    await user.click(row);
    // The task's page shows in place of the list.
    await waitFor(() => expect(screen.queryByTestId('agent-column')).toBeNull());
    expect(
      within(screen.getByTestId('tab-strip')).getByRole('button', { current: 'page' })
    ).toHaveTextContent('Broke');
  });

  it('lets other pages scroll in the main pane', async () => {
    setViewport(true);
    await renderAt('/health');
    expect(screen.getByRole('main').className).toContain('overflow-y-auto');
  });

  // An absolutely placed sr-only label would otherwise size the document, so bringing a row into view
  // scrolled the whole page and pushed the header off a phone screen.
  it.each([
    ['desktop', true],
    ['phone', false]
  ])('makes the main pane the containing block on %s', async (_, wide) => {
    setViewport(wide);
    await renderAt('/inbox');
    expect(screen.getByRole('main')).toHaveClass('relative');
  });

  it('records this visit once when the shell opens, so the next one compares against it', async () => {
    setViewport(true);
    await renderAt('/');
    await waitFor(() => expect(testSeen.visits).toBe(1));
    // Not again as the page settles: a second record would make "the visit before" this one.
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(testSeen.visits).toBe(1);
  });
});

describe('offline banner', () => {
  afterEach(async () => {
    reach.up = true;
    await act(async () => {
      await connection.check();
    });
  });

  it('sits in the screen-high column above main in the compact shell', async () => {
    setViewport(false);
    await renderAt('/');
    reach.up = false;
    await act(async () => {
      await connection.check();
    });

    const banner = await screen.findByTestId('connection-banner');
    const column = banner.parentElement;
    expect(column).toHaveClass('h-dvh');
    expect(column).toContainElement(screen.getByTestId('compact-shell'));
    const main = screen.getByRole('main');
    expect(column).toContainElement(main);
    expect(banner.compareDocumentPosition(main) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    // The top row is under the banner, so the banner takes its line from the page, not from the row.
    const strip = screen.getByTestId('tab-strip');
    expect(banner.compareDocumentPosition(strip) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('is the status bar on desktop: it turns to the offline colour in place, with no banner', async () => {
    setViewport(true);
    await renderAt('/');
    const status = screen.getByTestId('status-bar');
    expect(within(status).getByRole('status')).toHaveTextContent('Connected');
    reach.up = false;
    await act(async () => {
      await connection.check();
    });
    await waitFor(() => expect(status).toHaveAttribute('data-offline', 'true'));
    expect(within(status).getByRole('status')).toHaveTextContent(/^Offline.*· reconnecting…$/);
    expect(status).toHaveClass('bg-destructive');
    // Nothing is added above the page, so nothing moves.
    expect(screen.queryByTestId('connection-banner')).toBeNull();
    expect(screen.getByTestId('desktop-shell')).toContainElement(status);
  });
});

describe('explorer panel', () => {
  it('shows placeholder rows in the tree’s shape while it loads, then the tree', async () => {
    setViewport(true);
    let release!: () => void;
    const held = new Promise<void>((resolve) => {
      release = resolve;
    });
    server.use(
      tasksList(async () => {
        await held;
        return HttpResponse.json([]);
      })
    );
    await renderAt('/');
    const aside = screen.getByTestId('explorer-aside');
    // The rows are for the eye; the status line is what a screen reader hears.
    expect(await within(aside).findByTestId('explorer-loading')).toHaveAttribute(
      'aria-hidden',
      'true'
    );
    expect(within(aside).getByRole('status')).toHaveTextContent('Loading Mainloop…');
    release();
    expect(await within(aside).findByRole('group', { name: 'Agents' })).toBeInTheDocument();
    expect(within(aside).queryByTestId('explorer-loading')).toBeNull();
  });

  it('waits for the projects before placing an agent, so none is drawn under main and then moved', async () => {
    setViewport(true);
    let release!: () => void;
    const held = new Promise<void>((resolve) => {
      release = resolve;
    });
    const running = view({ id: 'task-r', title: 'Add retry button', status: 'running' });
    server.use(
      tasksList(() => HttpResponse.json([running])),
      projectsReadWith(async () => {
        await held;
        return HttpResponse.json([project({ id: running.task.project_id ?? undefined })]);
      })
    );
    await renderAt('/');
    const aside = screen.getByTestId('explorer-aside');
    const strip = screen.getByTestId('tab-strip');
    // Given the tasks have answered and the projects have not: the counts are known already
    expect(await screen.findByTestId('status-working')).toHaveTextContent('1 working');
    // Then the lists still hold their placeholder, and the strip has only the tab that is certain
    expect(within(aside).getByTestId('explorer-loading')).toBeInTheDocument();
    expect(within(aside).queryByTestId('agent-row')).toBeNull();
    expect(
      within(strip)
        .getAllByTestId('tab')
        .map((tab) => tab.textContent)
    ).toEqual(['Mainloop']);
    expect(within(strip).queryByText('Add retry button')).toBeNull();

    // When the projects answer
    release();

    // Then the agent appears once, under its project, and the tabs are the main thread's own
    const widgets = await within(aside).findByRole('list', { name: 'widgets' });
    expect(within(widgets).getByRole('button', { name: /^Add retry button/ })).toBeInTheDocument();
    // The main thread is pinned above the projects, and nothing is left over for No project
    expect(within(aside).getByTestId('main-entry')).toBeInTheDocument();
    expect(within(aside).queryByText('No project')).toBeNull();
    expect(
      within(strip)
        .getAllByTestId('tab')
        .map((tab) => tab.textContent)
    ).toEqual(['Mainloop']);
  });

  it('says the counts are loading until the first read answers, not that nothing needs the owner', async () => {
    setViewport(true);
    let release!: () => void;
    const held = new Promise<void>((resolve) => {
      release = resolve;
    });
    server.use(
      tasksList(async () => {
        await held;
        return HttpResponse.json([view({ reason: 'owner_decision' })]);
      })
    );
    await renderAt('/');
    const bar = screen.getByTestId('status-bar');
    expect(within(bar).getByTestId('status-counts-pending')).toHaveTextContent('Loading…');
    expect(bar).not.toHaveTextContent('Nothing needs you');
    expect(within(bar).queryByTestId('status-needs')).toBeNull();
    release();
    expect(await within(bar).findByTestId('status-needs')).toHaveTextContent('1 needs you');
    expect(within(bar).queryByTestId('status-counts-pending')).toBeNull();
  });

  it('hides the sidebar, and brings it back from the activity bar', async () => {
    setViewport(true);
    const user = userEvent.setup();
    await renderAt('/');
    const toggle = screen.getByRole('button', { name: 'Spaces and agents' });
    expect(toggle).toHaveAttribute('aria-pressed', 'true');
    await user.click(screen.getByRole('button', { name: 'Hide sidebar' }));
    expect(screen.queryByTestId('explorer-aside')).toBeNull();
    expect(JSON.parse(localStorage.getItem('mainloop.explorer.v1') ?? '{}')).toMatchObject({
      collapsed: true
    });
    // The bar's button says the sidebar is away, and brings it back.
    expect(toggle).toHaveAttribute('aria-pressed', 'false');
    await user.click(toggle);
    expect(await screen.findByTestId('explorer-aside')).toBeInTheDocument();
    expect(toggle).toHaveAttribute('aria-pressed', 'true');
    // Pressing it while the sidebar shows hides it again.
    await user.click(toggle);
    expect(screen.queryByTestId('explorer-aside')).toBeNull();
  });

  describe('the sidebar shortcut', () => {
    afterEach(() => vi.restoreAllMocks());
    const as = (platform: string) =>
      vi.spyOn(window.navigator, 'platform', 'get').mockReturnValue(platform);
    const shown = () => screen.queryByTestId('explorer-aside') !== null;
    // A real key event on whatever has focus. Nothing is typed, so no draft is left behind.
    const press = (modifier: 'metaKey' | 'ctrlKey', target: Element = document.body) => {
      const event = new KeyboardEvent('keydown', {
        key: 'b',
        [modifier]: true,
        bubbles: true,
        cancelable: true
      });
      act(() => {
        target.dispatchEvent(event);
      });
      return event;
    };

    // The key after the prefix, on whatever has focus.
    const then = (key: string, target: Element = document.body) => {
      const event = new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true });
      act(() => {
        target.dispatchEvent(event);
      });
      return event;
    };

    it('is the prefix and then B off a Mac, where ⌘B does nothing', async () => {
      as('Win32');
      setViewport(true);
      await renderAt('/');
      expect(shown()).toBe(true);
      expect(press('metaKey').defaultPrevented).toBe(false);
      expect(shown()).toBe(true);
      // Ctrl+B alone is the prefix: it is taken, and waits for its key.
      expect(press('ctrlKey').defaultPrevented).toBe(true);
      expect(shown()).toBe(true);
      expect(then('b').defaultPrevented).toBe(true);
      expect(shown()).toBe(false);
      press('ctrlKey');
      then('b');
      expect(shown()).toBe(true);
      expect(screen.getByRole('button', { name: 'Hide sidebar' })).toHaveAttribute(
        'title',
        'Hide sidebar (Ctrl+B B)'
      );
    });

    it('is ⌘B on a Mac, also while typing, and the prefix and B there too', async () => {
      as('MacIntel');
      setViewport(true);
      await renderAt('/');
      const box = await screen.findByRole('textbox', { name: 'Message' });
      act(() => box.focus());
      // ⌘B works with the caret in the composer, and the box keeps the caret.
      expect(press('metaKey', box).defaultPrevented).toBe(true);
      expect(shown()).toBe(false);
      expect(box).toHaveFocus();
      press('metaKey', box);
      expect(shown()).toBe(true);
      expect(screen.getByRole('button', { name: 'Hide sidebar' })).toHaveAttribute(
        'title',
        'Hide sidebar (⌘B)'
      );
      // Ctrl+B is the prefix on a Mac as well: in a text field it replaces "back one character".
      expect(press('ctrlKey', box).defaultPrevented).toBe(true);
      expect(shown()).toBe(true);
      then('b', box);
      expect(shown()).toBe(false);
      // The b went to the sidebar, not into the message.
      expect(box).toHaveValue('');
    });
  });

  describe('the prefix keys', () => {
    const prefix = (target: Element = document.body) => {
      act(() => {
        target.dispatchEvent(
          new KeyboardEvent('keydown', { key: 'b', ctrlKey: true, bubbles: true, cancelable: true })
        );
      });
    };
    const then = (key: string, target: Element = document.body) => {
      act(() => {
        target.dispatchEvent(
          new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true })
        );
      });
    };
    const tabs = () =>
      within(screen.getByTestId('tab-strip'))
        .getAllByTestId('tab')
        .map((tab) => `${tab.textContent}${tab.getAttribute('aria-current') ? '*' : ''}`);

    it('lists the keys in the status bar while the prefix waits', async () => {
      setViewport(true);
      await renderAt('/');
      const keys = screen.getByTestId('status-keys');
      expect(keys).toBeEmptyDOMElement();
      prefix();
      expect(keys).toHaveTextContent('ctrl+b · n next · p previous · 1–9 tab · c new');
      then('Escape');
      expect(keys).toBeEmptyDOMElement();
    });

    it('moves between the current space’s tabs with n, p and a number, also while typing', async () => {
      setViewport(true);
      // Two tasks of the main thread's own space (their project is not loaded), neither with a
      // session, so each opens on its task page.
      const first = view({
        id: 'task-a',
        title: 'First',
        status: 'queued',
        current_attempt_id: null
      });
      const second = view({
        id: 'task-b',
        title: 'Second',
        status: 'queued',
        current_attempt_id: null,
        created_at: justNow()
      });
      server.use(
        tasksList(() => HttpResponse.json([first, second])),
        taskRead(({ params }) => HttpResponse.json(params.task_id === 'task-a' ? first : second))
      );
      const router = await renderAt('/');
      await waitFor(() => expect(tabs()).toEqual(['Mainloop*', 'First', 'Second']));
      const box = await screen.findByRole('textbox', { name: 'Message' });
      act(() => box.focus());

      // n: the next tab, from the composer, and the n is not typed
      prefix(box);
      then('n', box);
      await waitFor(() => expect(router.state.location.pathname).toBe('/tasks/task-a'));
      await waitFor(() => expect(tabs()).toEqual(['Mainloop', 'First*', 'Second']));

      // 3: the third tab; p: back one; n from the last wraps to the first
      prefix();
      then('3');
      await waitFor(() => expect(router.state.location.pathname).toBe('/tasks/task-b'));
      prefix();
      then('p');
      await waitFor(() => expect(router.state.location.pathname).toBe('/tasks/task-a'));
      prefix();
      then('1');
      await waitFor(() => expect(router.state.location.pathname).toBe('/'));
      // The composer got none of those keys.
      expect(await screen.findByRole('textbox', { name: 'Message' })).toHaveValue('');
    });

    it('opens the menu with g, and goes to the spaces with w', async () => {
      setViewport(true);
      await renderAt('/');
      await within(screen.getByTestId('explorer-aside')).findByRole('group', { name: 'Agents' });

      prefix();
      then('w');
      // The current space's row (the pinned main thread, on home) has focus, so the arrows move
      // from there and Enter opens.
      await waitFor(() => expect(screen.getByTestId('main-entry')).toHaveFocus());

      prefix();
      then('g');
      expect(await screen.findByTestId('command-palette')).toBeInTheDocument();
    });
  });

  it('is two lists, spaces and their agents, with no tree and no second view', async () => {
    setViewport(true);
    server.use(projectsRead([project()]));
    await renderAt('/');
    const aside = screen.getByTestId('explorer-aside');
    // The main thread pinned at the top, then each project's heading over its agents: none yet.
    expect(await within(aside).findByTestId('main-entry')).toHaveTextContent('MainloopIdle');
    const agents = await within(aside).findByRole('group', { name: 'Agents' });
    await waitFor(() =>
      expect(
        within(agents)
          .getAllByTestId('space-row')
          .map((row) => row.textContent)
      ).toEqual(['widgets'])
    );
    expect(within(agents).queryByTestId('agent-row')).toBeNull();
    expect(within(aside).queryByRole('tree')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Projects' })).toBeNull();
  });

  it('leads from the menu to where a project is imported', async () => {
    setViewport(true);
    const user = userEvent.setup();
    const router = await renderAt('/');
    await user.click(screen.getByRole('button', { name: 'Search and commands' }));
    await user.type(await screen.findByRole('combobox'), 'import');
    await user.keyboard('{Enter}');
    await waitFor(() => expect(router.state.location.pathname).toBe('/settings'));
    expect(router.state.location.hash).toBe('import-project');
  });

  it('starts new work in the current space from the sidebar’s new, in the main chat, unsent', async () => {
    setViewport(true);
    const user = userEvent.setup();
    server.use(...serveProjectPage(), projectsRead([project()]));
    const router = await renderAt('/projects/project-a');
    // The sidebar's "new" and the strip's "+" are the same thing, named for the space.
    await waitFor(() =>
      expect(screen.getByTestId('tab-new')).toHaveAccessibleName('New task in widgets')
    );
    expect(screen.getByTestId('column-new')).toHaveAccessibleName('New task in widgets');
    await user.click(screen.getByTestId('column-new'));
    await waitFor(() => expect(router.state.location.pathname).toBe('/'));
    await waitFor(() =>
      expect(screen.getByRole('textbox', { name: 'Message' })).toHaveValue(
        'Start a task in acme/widgets: '
      )
    );
  });

  it('keeps the Needs-you count on the activity bar and the status bar, with the sidebar hidden too', async () => {
    setViewport(true);
    server.use(tasksList(() => HttpResponse.json([view({ reason: 'owner_decision' })])));
    const user = userEvent.setup();
    await renderAt('/');
    const inbox = await screen.findByRole('link', { name: 'Inbox, 1 need you' });
    expect(within(inbox).getByTestId('activity-needs')).toHaveTextContent('1');
    expect(screen.getByTestId('status-needs')).toHaveTextContent('1 needs you');
    await user.click(screen.getByRole('button', { name: 'Hide sidebar' }));
    expect(screen.queryByTestId('explorer-aside')).toBeNull();
    expect(screen.getByRole('link', { name: 'Inbox, 1 need you' })).toBeInTheDocument();
    expect(screen.getByTestId('status-needs')).toHaveTextContent('1 needs you');
    // The working count brings the sidebar back, where the working agents are.
    await user.click(screen.getByTestId('status-working'));
    expect(screen.getByRole('complementary', { name: 'Sidebar' })).toBeInTheDocument();
  });

  it('resizes from the keyboard, clamps the width and remembers it', async () => {
    setViewport(true);
    const user = userEvent.setup();
    await renderAt('/');
    const handle = screen.getByRole('separator', { name: 'Resize sidebar' });
    expect(handle).toHaveAttribute('aria-valuenow', '300');
    handle.focus();
    // The sidebar is on the left, so the right arrow widens it.
    await user.keyboard('{ArrowRight}{ArrowRight}');
    expect(handle).toHaveAttribute('aria-valuenow', '332');
    await user.keyboard('{ArrowRight}'.repeat(12));
    expect(handle).toHaveAttribute('aria-valuenow', '480');
    expect(JSON.parse(localStorage.getItem('mainloop.explorer.v1') ?? '{}')).toMatchObject({
      width: 480
    });
  });
});

describe('finished and not yet seen', () => {
  const HOUR = 60 * 60 * 1000;
  const done = () =>
    view({
      id: 'done-1',
      title: 'Ship docs',
      status: 'completed',
      updated_at: new Date(Date.now() - 60_000).toISOString()
    });
  const serve = (task = done()) =>
    server.use(
      tasksList(() => HttpResponse.json([task])),
      taskRead(() => HttpResponse.json(task))
    );
  const dotOf = (column: ReturnType<typeof within>) =>
    within(column.getByRole('button', { name: /^Ship docs/ })).getByTestId('agent-dot');

  it('lights a task that finished after the owner’s record began, and opening it puts it at rest on the server', async () => {
    setViewport(true);
    // The owner's record began an hour ago; the task finished since.
    testSeen.begin({ since: Date.now() - HOUR, lastVisit: Date.now() - HOUR });
    serve();
    const user = userEvent.setup();
    const router = await renderAt('/');
    const column = within(await screen.findByTestId('explorer-aside'));
    // A finished task nobody has looked at is still an agent in the column, with the teal dot.
    await column.findByRole('button', { name: /^Ship docs/ });
    await waitFor(() => expect(dotOf(column)).toHaveAttribute('data-tone', 'unseen'));

    // Opening the task's page is looking at it: one write, to the owner's record. Its row stays
    // while its page shows, at rest.
    await user.click(column.getByText('Ship docs'));
    await waitFor(() => expect(dotOf(column)).toHaveAttribute('data-tone', 'muted'));
    expect(testSeen.marked).toEqual(['done-1']);
    expect(column.getByRole('button', { name: /^Ship docs/ })).toHaveAttribute(
      'aria-current',
      'page'
    );

    // Once the owner moves on, a finished task that has been looked at leaves the column.
    await act(() => router.navigate({ to: '/' }));
    await waitFor(() => expect(column.queryByRole('button', { name: /^Ship docs/ })).toBeNull());
  });

  it('shows a task opened on another device as seen here', async () => {
    setViewport(true);
    testSeen.begin({
      since: Date.now() - HOUR,
      lastVisit: Date.now() - HOUR,
      tasks: { 'done-1': Date.now() - 1000 }
    });
    serve();
    await renderAt('/tasks/done-1');
    const column = within(await screen.findByTestId('explorer-aside'));
    await column.findByRole('button', { name: /^Ship docs/ });
    await waitFor(() => expect(testSeen.visits).toBe(1));
    expect(dotOf(column)).toHaveAttribute('data-tone', 'muted');
    expect(testSeen.marked).toEqual([]);
  });

  it('does not write again for a task that is already marked after its last change', async () => {
    setViewport(true);
    testSeen.begin({ since: Date.now() - HOUR, tasks: { 'done-1': Date.now() - 1000 } });
    serve();
    await renderAt('/tasks/done-1');
    await waitFor(() => expect(testSeen.visits).toBe(1));
    await new Promise((resolve) => setTimeout(resolve, 100));
    expect(testSeen.marked).toEqual([]);
  });
});

describe('chat starters', () => {
  it('suggests starters on an empty chat, and fills the composer from one without sending', async () => {
    setViewport(true);
    const user = userEvent.setup();
    await renderAt('/');
    const group = await screen.findByRole('group', { name: 'Suggestions' });
    expect(within(group).getByRole('button', { name: 'What needs me?' })).toBeInTheDocument();
    // Inside the empty thread, not a band of its own above it.
    expect(screen.getByTestId('message-list')).toContainElement(group);
    expect(within(group).getByRole('button', { name: "What's running?" })).toBeInTheDocument();
    expect(within(group).getByRole('button', { name: 'Pause a schedule' })).toBeInTheDocument();
    expect(within(group).queryByRole('button', { name: /^Start a task in/ })).toBeNull();

    await user.click(within(group).getByRole('button', { name: "What's running?" }));
    // The composer takes the text once and the route clears it, so the box is what to check.
    expect(await screen.findByRole('textbox', { name: 'Message' })).toHaveValue("What's running?");
  });

  it('offers to start a task in the first project', async () => {
    server.use(projectsRead([project()]));
    setViewport(false);
    await renderAt('/');
    const group = await screen.findByRole('group', { name: 'Suggestions' });
    expect(
      await within(group).findByRole('button', { name: 'Start a task in acme/widgets' })
    ).toBeInTheDocument();
  });
});

describe('answer view', () => {
  const planReview = queueItem({
    id: 'q-plan',
    item_type: 'plan_review',
    task_id: null,
    title: 'Review the plan',
    content: 'Approve or revise the migration plan.'
  });

  it('answers a queue item with no task from the status bar’s count, in the centre column', async () => {
    setViewport(true);
    const user = userEvent.setup();
    const bodies: unknown[] = [];
    server.use(
      attentionRead(() => HttpResponse.json(attentionSnapshot([attentionItem(planReview)]))),
      respondPost(async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json({ status: 'ok', message: 'Sent' });
      })
    );
    const router = await renderAt('/');
    // An owner action that belongs to no task is no agent's: it turns the main thread's dot red,
    // and the count in the status bar leads to it.
    const column = within(screen.getByTestId('explorer-aside'));
    await waitFor(() => expect(column.getByTestId('main-entry')).toHaveTextContent('1 needs you'));
    await user.click(await screen.findByRole('link', { name: '1 needs you' }));

    await waitFor(() => expect(router.state.location.pathname).toBe('/inbox'));
    const main = screen.getByRole('main');
    await within(main).findByRole('heading', { name: 'Inbox' });
    expect(within(main).queryByTestId('main-thread')).toBeNull();
    expect(main.querySelector('[data-attention-key="q-plan"]')).not.toBeNull();
    await user.type(within(main).getByLabelText('Reply to Review the plan'), 'Ship it');
    await user.click(within(main).getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(bodies).toEqual([{ response: 'Ship it' }]));
  });

  it('opens the answer view from the activity bar’s Inbox, with Activity beside it', async () => {
    setViewport(true);
    const user = userEvent.setup();
    server.use(
      attentionRead(() => HttpResponse.json(attentionSnapshot([attentionItem(planReview)]))),
      activityRead(() => HttpResponse.json([]))
    );
    const router = await renderAt('/');
    await user.click(await screen.findByRole('link', { name: 'Inbox, 1 need you' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/inbox'));
    await user.click(await screen.findByRole('button', { name: 'Activity' }));
    expect(router.state.location.search).toMatchObject({ show: 'activity' });
    expect(screen.getByRole('button', { name: 'Activity' })).toHaveAttribute(
      'aria-pressed',
      'true'
    );
  });

  it('lands a task approval link on its answer card', async () => {
    setViewport(true);
    const user = userEvent.setup();
    const waiting = view(
      { reason: 'approval' },
      { projection: projection({ pending_approval_ids: ['approval-1'] }) }
    );
    server.use(
      ...serveTaskPage(taskScreen(waiting)),
      attentionRead(() => HttpResponse.json(attentionSnapshot([hitlAttentionItem('approval-1')]))),
      ...createFakeHITL({ 'approval-1': hitlView('approval-1') }).handlers
    );
    const router = await renderAt('/tasks/task-1');
    await user.click(await screen.findByRole('link', { name: 'Open Inbox' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/inbox'));
    const main = screen.getByRole('main');
    await waitFor(() =>
      expect(main.querySelector('[data-attention-key="approval-1"]')).toHaveAttribute(
        'aria-current',
        'true'
      )
    );
  });

  it('opens on the phone from the Inbox button, with a way back to the chat', async () => {
    setViewport(false);
    const user = userEvent.setup();
    server.use(
      attentionRead(() => HttpResponse.json(attentionSnapshot([attentionItem(planReview)])))
    );
    const router = await renderAt('/?tab=explorer');
    await waitFor(() =>
      expect(screen.getByTestId('inbox-link')).toHaveAccessibleName('Inbox, 1 need you')
    );
    await user.click(screen.getByTestId('inbox-link'));
    await waitFor(() => expect(router.state.location.pathname).toBe('/inbox'));
    expect(await screen.findByRole('heading', { name: 'Inbox' })).toBeVisible();
    expect(screen.getByText('Review the plan')).toBeVisible();
    await user.click(screen.getByRole('link', { name: 'Back to chat' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/'));
    expect(await screen.findByTestId('main-thread')).toBeVisible();
  });

  it('sends an old ?tab=inbox link to the answer view', async () => {
    setViewport(false);
    server.use(
      attentionRead(() => HttpResponse.json(attentionSnapshot([attentionItem(planReview)])))
    );
    const router = await renderAt('/?tab=inbox');
    await waitFor(() => expect(router.state.location.pathname).toBe('/inbox'));
    expect(router.state.location.search).not.toHaveProperty('tab');
    expect(await screen.findByText('Review the plan')).toBeInTheDocument();
  });
});

describe('command palette', () => {
  // A composer draft lives in session storage for the tab, so one left by an earlier test would be
  // joined to the text these tests put in the box.
  beforeEach(() => sessionStorage.clear());
  afterEach(() => {
    vi.restoreAllMocks();
    sessionStorage.clear();
  });
  const as = (platform: string) =>
    vi.spyOn(window.navigator, 'platform', 'get').mockReturnValue(platform);
  const palette = () => screen.queryByTestId('command-palette');
  const press = (modifier: 'metaKey' | 'ctrlKey', target: Element = document.body) => {
    const event = new KeyboardEvent('keydown', {
      key: 'k',
      [modifier]: true,
      bubbles: true,
      cancelable: true
    });
    act(() => {
      target.dispatchEvent(event);
    });
    return event;
  };
  const seedTask = () =>
    server.use(
      tasksList(() =>
        HttpResponse.json([view({ id: 'task-q', title: 'Queue backend', status: 'running' })])
      ),
      taskRead(() => HttpResponse.json(view({ id: 'task-q', title: 'Queue backend' })))
    );

  it('is Ctrl+K off a Mac, and ⌘K does nothing there', async () => {
    as('Win32');
    setViewport(true);
    await renderAt('/');
    expect(press('metaKey').defaultPrevented).toBe(false);
    expect(palette()).toBeNull();
    expect(press('ctrlKey').defaultPrevented).toBe(true);
    expect(await screen.findByTestId('command-palette')).toBeInTheDocument();
    // The same key closes it.
    press('ctrlKey');
    await waitFor(() => expect(palette()).toBeNull());
  });

  it('is ⌘K on a Mac, and leaves Ctrl+K to the system’s text keys, also while typing', async () => {
    as('MacIntel');
    setViewport(true);
    await renderAt('/');
    const box = await screen.findByRole('textbox', { name: 'Message' });
    act(() => box.focus());
    // Ctrl+K is "delete to the end of the line" on macOS: the app neither acts on it nor cancels it.
    expect(press('ctrlKey', box).defaultPrevented).toBe(false);
    expect(palette()).toBeNull();
    expect(press('metaKey', box).defaultPrevented).toBe(true);
    expect(await screen.findByTestId('command-palette')).toBeInTheDocument();
    // The page behind an open dialog is hidden from the accessibility tree, so it is found by id.
    expect(screen.getByTestId('column-menu')).toHaveAttribute('title', 'Search and commands (⌘K)');
  });

  it('jumps to a task: type, then Enter', async () => {
    as('Win32');
    setViewport(true);
    seedTask();
    const user = userEvent.setup();
    const router = await renderAt('/');
    await within(screen.getByTestId('explorer-aside')).findByRole('group', { name: 'Agents' });
    await user.click(screen.getByTestId('column-menu'));
    const field = await screen.findByRole('combobox');
    expect(field).toHaveFocus();
    await user.type(field, 'queue');
    const options = within(screen.getByRole('listbox')).getAllByRole('option');
    expect(options.map((option) => option.textContent)).toEqual([
      expect.stringMatching(/^Queue backend.*Task$/),
      'Ask Mainloop: queueAsk'
    ]);
    // The first row is the highlighted one, and the field says so to a screen reader.
    expect(options[0]).toHaveAttribute('aria-selected', 'true');
    expect(field).toHaveAttribute('aria-activedescendant', options[0]?.id);
    await user.keyboard('{Enter}');
    await waitFor(() => expect(router.state.location.pathname).toBe('/tasks/task-q'));
    expect(palette()).toBeNull();
  });

  it('moves with the arrows and wraps, and Escape closes without going anywhere', async () => {
    as('Win32');
    setViewport(true);
    const user = userEvent.setup();
    const router = await renderAt('/');
    await user.click(screen.getByTestId('column-menu'));
    const field = await screen.findByRole('combobox');
    const selected = () =>
      within(screen.getByRole('listbox'))
        .getAllByRole('option')
        .find((option) => option.getAttribute('aria-selected') === 'true')?.textContent;
    expect(selected()).toBe('Main chatPage');
    await user.keyboard('{ArrowDown}');
    expect(selected()).toBe('Inbox: Needs youPage');
    await user.keyboard('{ArrowUp}{ArrowUp}');
    // Up from the first row wraps to the last, a theme.
    expect(selected()).toMatch(/^Theme: /);
    expect(field).toHaveFocus();
    await user.keyboard('{Escape}');
    await waitFor(() => expect(palette()).toBeNull());
    expect(router.state.location.pathname).toBe('/');
  });

  it('asks Mainloop about the highlighted row with the modifier and Enter', async () => {
    as('Win32');
    setViewport(true);
    seedTask();
    const user = userEvent.setup();
    const router = await renderAt('/inbox');
    await within(screen.getByTestId('explorer-aside')).findByRole('group', { name: 'Agents' });
    await user.click(screen.getByTestId('column-menu'));
    await user.type(await screen.findByRole('combobox'), 'queue');
    await user.keyboard('{Control>}{Enter}{/Control}');
    await waitFor(() => expect(router.state.location.pathname).toBe('/'));
    // The main chat takes the text into its composer, unsent.
    await waitFor(() =>
      expect(screen.getByRole('textbox', { name: 'Message' })).toHaveValue(
        'About task Queue backend (task-q): '
      )
    );
  });

  it('asks Mainloop the query itself from the last row', async () => {
    as('Win32');
    setViewport(true);
    const user = userEvent.setup();
    await renderAt('/inbox');
    await user.click(screen.getByTestId('column-menu'));
    await user.type(await screen.findByRole('combobox'), 'what is running');
    await user.click(screen.getByRole('option', { name: /^Ask Mainloop: what is running/ }));
    await waitFor(() =>
      expect(screen.getByRole('textbox', { name: 'Message' })).toHaveValue('what is running')
    );
  });

  it('runs the prefix keys’ actions by name: the next tab, and the spaces', async () => {
    as('Win32');
    setViewport(true);
    // A task of the main thread's own space with no session yet, so its tab opens its task page.
    const queued = view({
      id: 'task-q',
      title: 'Queue backend',
      status: 'queued',
      current_attempt_id: null
    });
    server.use(
      tasksList(() => HttpResponse.json([queued])),
      taskRead(() => HttpResponse.json(queued))
    );
    const user = userEvent.setup();
    const router = await renderAt('/');
    await within(screen.getByTestId('explorer-aside')).findByRole('button', {
      name: /^Queue backend/
    });

    // Given the palette, where each action is a row that says its key
    await user.click(screen.getByTestId('column-menu'));
    await user.type(await screen.findByRole('combobox'), 'next tab');
    expect(within(screen.getByRole('listbox')).getAllByRole('option')[0]).toHaveTextContent(
      /^Next tabCtrl\+B nCommand$/
    );

    // When the owner runs Next tab
    await user.keyboard('{Enter}');

    // Then the palette closes and the next tab of the space opens
    await waitFor(() => expect(palette()).toBeNull());
    await waitFor(() => expect(router.state.location.pathname).toBe('/tasks/task-q'));

    // And Go to the spaces puts keyboard focus on the current space's row, not back in the page
    await user.click(screen.getByTestId('column-menu'));
    await user.type(await screen.findByRole('combobox'), 'go to the spaces');
    await user.keyboard('{Enter}');
    await waitFor(() => expect(palette()).toBeNull());
    await waitFor(() => expect(screen.getByTestId('main-entry')).toHaveFocus());
  });

  it('offers the same actions on a phone, without keys, and opens the lists from one', async () => {
    setViewport(false);
    const user = userEvent.setup();
    const router = await renderAt('/?tab=explorer');
    await user.click(await screen.findByTestId('column-menu'));
    await user.type(await screen.findByRole('combobox'), 'spaces and agents');
    const row = within(screen.getByRole('listbox')).getAllByRole('option')[0]!;
    expect(row).toHaveTextContent(/^Show or hide the spaces and agentsCommand$/);

    await user.keyboard('{Enter}');

    // The lists were showing, so the command puts them away.
    await waitFor(() => expect(router.state.location.search).not.toHaveProperty('tab'));
    await waitFor(() => expect(screen.queryByTestId('agent-column')).toBeNull());
  });

  it('changes the theme', async () => {
    as('Win32');
    setViewport(true);
    const user = userEvent.setup();
    await renderAt('/');
    await user.click(screen.getByTestId('column-menu'));
    await user.type(await screen.findByRole('combobox'), 'nord');
    await user.keyboard('{Enter}');
    await waitFor(() => expect(document.documentElement).toHaveAttribute('data-theme', 'nord'));
    document.documentElement.removeAttribute('data-theme');
    localStorage.removeItem('mainloop-theme');
  });

  it('opens from the menu on a phone', async () => {
    setViewport(false);
    const user = userEvent.setup();
    await renderAt('/?tab=explorer');
    await user.click(await screen.findByTestId('column-menu'));
    expect(await screen.findByRole('combobox')).toHaveFocus();
  });
});

describe('answer view on desktop', () => {
  it('goes back to the chat from its back link', async () => {
    setViewport(true);
    const user = userEvent.setup();
    const router = await renderAt('/inbox');
    await user.click(await screen.findByRole('link', { name: 'Back to chat' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/'));
    expect(await screen.findByTestId('main-thread')).toBeInTheDocument();
  });

  it('shows the same Needs-you number on the activity bar, the status bar and the Inbox', async () => {
    setViewport(true);
    // One listed owner action, and one failed task that can be retried and that no item names.
    server.use(
      attentionRead(() => HttpResponse.json(attentionSnapshot([hitlAttentionItem('req-a')]))),
      tasksList(() =>
        HttpResponse.json([
          view({ id: 'task-f', title: 'Fix CI flake', status: 'failed', updated_at: justNow() })
        ])
      )
    );
    await renderAt('/inbox');
    const inbox = await screen.findByRole('link', { name: 'Inbox, 2 need you' });
    // The owner is on the Inbox, so the bar's item for it is marked.
    expect(inbox).toHaveAttribute('aria-current', 'page');
    expect(screen.getByTestId('status-needs')).toHaveTextContent('2 need you');
    // The column says it per agent: the failed task has the red dot, and so does the main thread.
    const column = within(screen.getByTestId('explorer-aside'));
    expect(column.getByTestId('main-entry')).toHaveTextContent('2 need you');
    expect(
      within(column.getByRole('button', { name: /^Fix CI flake/ })).getByTestId('agent-dot')
    ).toHaveAttribute('data-tone', 'bad');
    expect(screen.getByRole('button', { name: /^Needs you/ })).toHaveTextContent('Needs you2');
    // The Inbox lists both, so the number is the number of rows.
    const list = screen.getByRole('list', { name: 'Needs you' });
    expect(within(list).getAllByRole('listitem')).toHaveLength(2);
  });
});
