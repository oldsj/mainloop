import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory } from '@tanstack/react-router';
import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { createFakeHITL } from '@/features/hitl/handlers';
import { activityRead, attentionRead, respondPost } from '@/features/inbox/test/handlers';
import {
  attentionItem,
  attentionSnapshot,
  hitlAttentionItem,
  queueItem
} from '@/features/inbox/test/fixtures';
import { projectsRead, serveProjectPage } from '@/features/projects/test/handlers';
import { project } from '@/features/projects/test/fixtures';
import { serveTaskPage, taskRead, taskScreen, tasksList } from '@/features/tasks/test/handlers';
import { hitlView } from '@/test/fixtures/hitl';
import type * as ConnectionModule from '@/api/connection';
import { connection } from '@/api/connection';
import { testSeen } from '@/test/msw/handlers';
import { server } from '@/test/msw/server';
import { projection, view } from '@/test/fixtures/tasks';
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
});

describe('shell switch', () => {
  it('renders the desktop shell on a wide viewport', async () => {
    setViewport(true);
    await renderAt('/');
    expect(screen.getByTestId('desktop-shell')).toBeInTheDocument();
    expect(screen.queryByTestId('mobile-tab-bar')).not.toBeInTheDocument();
  });

  it('lays the desktop out as an IDE: activity bar, sidebar, main, status bar, and no top header', async () => {
    setViewport(true);
    await renderAt('/');
    const bar = screen.getByRole('navigation', { name: 'Primary' });
    const aside = screen.getByRole('complementary', { name: 'Sessions sidebar' });
    const main = screen.getByRole('main');
    const status = screen.getByTestId('status-bar');
    expect(await within(aside).findByRole('tree')).toBeInTheDocument();
    expect(main).not.toContainElement(aside);
    // Left to right, then the status bar last.
    const follows = (a: Element, b: Element) =>
      Boolean(a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING);
    expect(follows(bar, aside)).toBe(true);
    expect(follows(aside, main)).toBe(true);
    expect(follows(main, status)).toBe(true);
    // The old top header is gone: its mark, health link and theme menu are in the activity bar.
    expect(screen.queryByText('$ mainloop')).toBeNull();
    expect(within(bar).getByRole('link', { name: 'Mainloop home' })).toBeInTheDocument();
    expect(within(bar).getByTestId('header-health')).toBeInTheDocument();
    expect(within(bar).getByTestId('theme-selector')).toBeInTheDocument();
    expect(screen.getByTestId('desktop-shell').firstElementChild?.firstElementChild).toBe(bar);
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

  it('shows the explorer tree when the explorer tab is selected', async () => {
    setViewport(false);
    await renderAt('/?tab=explorer');
    expect(await screen.findByRole('tree')).toBeVisible();
    expect(screen.getByTestId('tab-explorer')).toHaveAttribute('aria-pressed', 'true');
  });

  it('switches tabs from the tab bar', async () => {
    setViewport(false);
    const user = userEvent.setup();
    await renderAt('/');
    await user.click(screen.getByTestId('tab-explorer'));
    expect(await screen.findByRole('tree')).toBeVisible();
    await user.click(screen.getByTestId('tab-chat'));
    expect(screen.getByTestId('tab-chat')).toHaveAttribute('aria-pressed', 'true');
  });

  it.each(['sessions', 'projects'])(
    'maps the old ?tab=%s value to the explorer tab',
    async (legacy) => {
      setViewport(false);
      await renderAt(`/?tab=${legacy}`);
      expect(await screen.findByRole('tree')).toBeVisible();
      expect(screen.getByTestId('tab-explorer')).toHaveAttribute('aria-pressed', 'true');
      expect(screen.getByTestId('tab-chat')).toHaveAttribute('aria-pressed', 'false');
    }
  );
});

describe('?view= override', () => {
  it('survives navigation through a link that does not mention it', async () => {
    setViewport(true);
    const user = userEvent.setup();
    const router = await renderAt('/?view=compact');
    // The phone has no header: health is in the tree's header.
    await user.click(screen.getByTestId('tab-explorer'));
    await user.click(await screen.findByTestId('header-health'));
    await waitFor(() => expect(router.state.location.pathname).toBe('/health'));
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
  it('opens Settings from the desktop activity bar, and marks it while it shows', async () => {
    setViewport(true);
    const user = userEvent.setup();
    const router = await renderAt('/');
    const settings = screen.getByRole('link', { name: 'Settings' });
    await user.click(settings);
    await waitFor(() => expect(router.state.location.pathname).toBe('/settings'));
    expect(await screen.findByRole('heading', { name: 'Settings', level: 1 })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Session defaults' })).toBeInTheDocument();
    expect(settings).toHaveAttribute('aria-current', 'page');
    await waitFor(() => expect(document.title).toBe('Settings · Mainloop'));
  });

  it('opens Settings from the tree’s header on a phone', async () => {
    setViewport(false);
    const user = userEvent.setup();
    const router = await renderAt('/?tab=explorer');
    await user.click(await screen.findByRole('link', { name: 'Settings' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/settings'));
    expect(await screen.findByTestId('settings-page')).toBeVisible();
  });

  it('opens orchestration health from the desktop activity bar', async () => {
    setViewport(true);
    const user = userEvent.setup();
    const router = await renderAt('/');
    await user.click(screen.getByTestId('header-health'));
    await waitFor(() => expect(router.state.location.pathname).toBe('/health'));
  });

  it('opens orchestration health from the tree on a phone, and shows the page, not the tree', async () => {
    setViewport(false);
    const user = userEvent.setup();
    const router = await renderAt('/');
    await user.click(screen.getByTestId('tab-explorer'));
    await user.click(await screen.findByTestId('header-health'));
    await waitFor(() => expect(router.state.location.pathname).toBe('/health'));
    expect(router.state.location.search).not.toHaveProperty('tab');
    // A page opened from the tree keeps the tree's tab lit.
    expect(screen.getByTestId('tab-explorer')).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByTestId('tab-chat')).toHaveAttribute('aria-pressed', 'false');
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
  it('shows the Needs-you count on the Needs you tab, read from GET /attention', async () => {
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
    expect(screen.getByTestId('tab-needs')).toHaveAccessibleName('Needs you, 3');
    expect(screen.getByTestId('tab-explorer')).toHaveAccessibleName('Explorer');
  });

  it('shows no count when nothing needs the owner', async () => {
    setViewport(false);
    await renderAt('/');
    expect(screen.queryByTestId('needs-badge')).not.toBeInTheDocument();
    expect(screen.getByTestId('tab-needs')).toHaveAccessibleName('Needs you');
  });

  it('counts a task that needs an owner decision, read from GET /tasks', async () => {
    setViewport(false);
    server.use(tasksList(() => HttpResponse.json([view({ reason: 'owner_decision' })])));
    await renderAt('/');
    expect(await screen.findByTestId('needs-badge')).toHaveTextContent('1');
    expect(screen.getByTestId('tab-needs')).toHaveAccessibleName('Needs you, 1');
  });

  it('keeps polling attention while the chat tab is open', async () => {
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

  it('updates the count without a live event while the chat tab is open', async () => {
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
    expect(await screen.findByTestId('needs-badge')).toHaveTextContent('1');
    needsOwner = false;
    await waitFor(() => expect(screen.queryByTestId('needs-badge')).not.toBeInTheDocument(), {
      timeout: 6000
    });
    expect(reads).toBeGreaterThanOrEqual(2);
    expect(screen.getByTestId('tab-needs')).toHaveAccessibleName('Needs you');
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

describe('compact explorer tab', () => {
  it('has three tabs with plain labels, and no header above the page', async () => {
    setViewport(false);
    await renderAt('/');
    const bar = screen.getByTestId('mobile-tab-bar');
    const tabs = within(bar).getAllByRole('button');
    expect(tabs.map((tab) => tab.textContent?.replace(/\s+/g, ' ').trim())).toEqual([
      'Chat',
      'Needs you',
      'Explorer'
    ]);
    // The page starts at the top: the main chat's own row is the first thing in the shell.
    expect(screen.queryByText('$ mainloop')).toBeNull();
    expect(screen.getByTestId('compact-shell').firstElementChild).toBe(screen.getByRole('main'));
  });

  it.each([
    ['/', 'tab-chat'],
    ['/inbox', 'tab-needs'],
    ['/health', 'tab-explorer'],
    ['/tasks/t-1', 'tab-explorer'],
    ['/projects/project-a', 'tab-explorer'],
    ['/?tab=explorer', 'tab-explorer'],
    ['/inbox?tab=explorer', 'tab-explorer']
  ])('lights the tab for where the owner is: %s', async (url, lit) => {
    setViewport(false);
    await renderAt(url);
    for (const id of ['tab-chat', 'tab-needs', 'tab-explorer'])
      expect(screen.getByTestId(id)).toHaveAttribute('aria-pressed', String(id === lit));
  });

  it('opens the answer view from Needs you, and the main chat from Chat', async () => {
    setViewport(false);
    const user = userEvent.setup();
    const router = await renderAt('/?tab=explorer');
    await user.click(screen.getByTestId('tab-needs'));
    await waitFor(() => expect(router.state.location.pathname).toBe('/inbox'));
    expect(router.state.location.search).not.toHaveProperty('tab');
    expect(screen.getByTestId('tab-needs')).toHaveAttribute('aria-pressed', 'true');
    await user.click(screen.getByTestId('tab-chat'));
    await waitFor(() => expect(router.state.location.pathname).toBe('/'));
    expect(screen.getByTestId('tab-chat')).toHaveAttribute('aria-pressed', 'true');
  });

  it('brings the tree back over a page opened from it, keeping the page’s route', async () => {
    setViewport(false);
    const user = userEvent.setup();
    const router = await renderAt('/health');
    await user.click(screen.getByTestId('tab-explorer'));
    expect(await screen.findByRole('tree')).toBeVisible();
    expect(router.state.location.pathname).toBe('/health');
    expect(router.state.location.search).toMatchObject({ tab: 'explorer' });
  });

  it('puts health and the theme in the tree’s header', async () => {
    setViewport(false);
    const user = userEvent.setup();
    await renderAt('/');
    expect(screen.queryByTestId('theme-selector')).toBeNull();
    await user.click(screen.getByTestId('tab-explorer'));
    await screen.findByRole('tree');
    expect(screen.getByTestId('theme-selector')).toBeVisible();
    expect(screen.getByRole('link', { name: 'Orchestration health' })).toBeVisible();
  });

  it('opens the explorer from its tab, and keeps it mounted after switching away', async () => {
    setViewport(false);
    const user = userEvent.setup();
    await renderAt('/');
    await user.click(screen.getByTestId('tab-explorer'));
    const tree = await screen.findByRole('tree');
    expect(screen.getByTestId('tab-explorer')).toHaveAttribute('aria-pressed', 'true');
    await user.click(screen.getByTestId('tab-chat'));
    expect(tree).not.toBeVisible();
    await user.click(screen.getByTestId('tab-explorer'));
    expect(screen.getByRole('tree')).toBe(tree);
    expect(tree).toBeVisible();
  });
});

describe('compact tab state', () => {
  it('keeps an opened tab mounted when switching away and back', async () => {
    setViewport(false);
    const user = userEvent.setup();
    await renderAt('/?tab=explorer');
    const tree = await screen.findByRole('tree');
    await user.click(screen.getByTestId('tab-chat'));
    expect(tree).not.toBeVisible();
    await user.click(screen.getByTestId('tab-explorer'));
    expect(screen.getByRole('tree')).toBe(tree);
    expect(tree).toBeVisible();
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
      // Opening the failed task from "Since you were here" reads this task directly.
      taskRead(() => HttpResponse.json(failed))
    );
  }

  it('keeps the desktop centre to the chat alone, with what changed in the explorer', async () => {
    setViewport(true);
    serveFailedSinceVisit();
    await renderAt('/');
    const aside = screen.getByRole('complementary', { name: 'Sessions sidebar' });
    expect(
      await within(aside).findByRole('treeitem', { name: 'Since you were here, 1' })
    ).toBeInTheDocument();
    const main = screen.getByRole('main');
    expect(within(main).queryByText('Since you were here')).toBeNull();
    expect(within(main).getByTestId('main-thread')).toBeInTheDocument();
    // One scroll in the centre: the thread's own. The pane around it does not scroll.
    expect(main.className).toContain('overflow-hidden');
    expect(main.className).not.toContain('overflow-y-auto');
  });

  it('shows what changed in the phone explorer, not above the chat', async () => {
    setViewport(false);
    const user = userEvent.setup();
    serveFailedSinceVisit();
    await renderAt('/');
    expect(screen.queryByText('Since you were here')).toBeNull();
    expect(screen.getByRole('main').className).toContain('overflow-hidden');
    await user.click(screen.getByTestId('tab-explorer'));
    const row = await screen.findByRole('treeitem', { name: '1 task failed, Broke' });
    await user.click(within(row).getByText('1 task failed'));
    // The task's page shows in place of the tree, and the tree's tab stays lit.
    await waitFor(() => expect(row).not.toBeVisible());
    expect(screen.getByTestId('tab-explorer')).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByTestId('tab-chat')).toHaveAttribute('aria-pressed', 'false');
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

  it('shows nothing as changed on the owner’s first visit, which has no visit before it', async () => {
    setViewport(true);
    const failed = view({
      id: 'fail-1',
      title: 'Broke',
      status: 'failed',
      updated_at: failedNow()
    });
    server.use(tasksList(() => HttpResponse.json([failed])));
    await renderAt('/');
    await waitFor(() => expect(testSeen.visits).toBe(1));
    expect(screen.queryByRole('treeitem', { name: /Since you were here/ })).toBeNull();
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
    expect(column).toContainElement(screen.getByTestId('mobile-tab-bar'));
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
  it('hides the sidebar, and brings it back from the activity bar', async () => {
    setViewport(true);
    const user = userEvent.setup();
    await renderAt('/');
    await user.click(screen.getByRole('button', { name: 'Hide sidebar' }));
    expect(screen.queryByTestId('explorer-aside')).toBeNull();
    expect(JSON.parse(localStorage.getItem('mainloop.explorer.v1') ?? '{}')).toMatchObject({
      collapsed: true
    });
    const sessions = screen.getByRole('button', { name: 'Sessions' });
    expect(sessions).toHaveAttribute('aria-pressed', 'false');
    await user.click(sessions);
    expect(await screen.findByTestId('explorer-aside')).toBeInTheDocument();
    expect(sessions).toHaveAttribute('aria-pressed', 'true');
    // Choosing the view that is showing hides it again.
    await user.click(sessions);
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

    it('is Ctrl+B off a Mac, and ⌘B does nothing there', async () => {
      as('Win32');
      setViewport(true);
      await renderAt('/');
      expect(shown()).toBe(true);
      expect(press('metaKey').defaultPrevented).toBe(false);
      expect(shown()).toBe(true);
      expect(press('ctrlKey').defaultPrevented).toBe(true);
      expect(shown()).toBe(false);
      press('ctrlKey');
      expect(shown()).toBe(true);
    });

    it('is ⌘B on a Mac, and leaves Ctrl+B to the system’s text keys, also while typing', async () => {
      as('MacIntel');
      setViewport(true);
      await renderAt('/');
      const box = await screen.findByRole('textbox', { name: 'Message' });
      act(() => box.focus());
      // Ctrl+B is "back one character" on macOS: the app must neither act on it nor cancel it.
      expect(press('ctrlKey', box).defaultPrevented).toBe(false);
      expect(shown()).toBe(true);
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
    });
  });

  it('shows one view at a time: Sessions is the work, Projects the catalogue', async () => {
    setViewport(true);
    const user = userEvent.setup();
    await renderAt('/');
    const aside = () => screen.getByTestId('explorer-aside');
    await within(aside()).findByRole('tree');
    const top = () =>
      within(aside())
        .getAllByRole('treeitem')
        .filter((row) => row.getAttribute('aria-level') === '1')
        .map((row) => row.getAttribute('aria-label')?.split(',')[0]);
    expect(top()).toEqual(['Mainloop', 'Needs you', 'Problems', 'Working', 'Finished recently']);
    await user.click(screen.getByRole('button', { name: 'Projects' }));
    expect(screen.getByRole('complementary', { name: 'Projects sidebar' })).toBe(aside());
    expect(top()).toEqual(['Projects', 'Schedules']);
    expect(JSON.parse(localStorage.getItem('mainloop.explorer.v1') ?? '{}')).toMatchObject({
      view: 'projects'
    });
  });

  it('keeps the Needs-you count on the activity bar and the status bar, whichever view shows', async () => {
    setViewport(true);
    server.use(tasksList(() => HttpResponse.json([view({ reason: 'owner_decision' })])));
    const user = userEvent.setup();
    await renderAt('/');
    const inbox = await screen.findByRole('link', { name: 'Inbox, 1 need you' });
    expect(within(inbox).getByTestId('activity-needs')).toHaveTextContent('1');
    expect(screen.getByTestId('status-needs')).toHaveTextContent('1 needs you');
    await user.click(screen.getByRole('button', { name: 'Projects' }));
    await user.click(screen.getByRole('button', { name: 'Projects' }));
    expect(screen.queryByTestId('explorer-aside')).toBeNull();
    expect(screen.getByRole('link', { name: 'Inbox, 1 need you' })).toBeInTheDocument();
    expect(screen.getByTestId('status-needs')).toHaveTextContent('1 needs you');
    // The working count opens the Sessions view.
    await user.click(screen.getByTestId('status-working'));
    expect(screen.getByRole('complementary', { name: 'Sessions sidebar' })).toBeInTheDocument();
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
  const dotOf = (tree: ReturnType<typeof within>) =>
    within(tree.getByRole('treeitem', { name: /Ship docs/ })).getByTestId('explorer-dot');

  it('lights a task that finished after the owner’s record began, and opening it puts it at rest on the server', async () => {
    setViewport(true);
    // The owner's record began an hour ago; the task finished since.
    testSeen.begin({ since: Date.now() - HOUR, lastVisit: Date.now() - HOUR });
    serve();
    const user = userEvent.setup();
    await renderAt('/');
    const tree = within(await screen.findByTestId('explorer-aside'));
    await user.click(await tree.findByText('Finished recently'));
    await tree.findByRole('treeitem', { name: /Ship docs/ });
    await waitFor(() => expect(dotOf(tree)).toHaveAttribute('data-tone', 'unseen'));

    // Opening the task's page is looking at it: one write, to the owner's record.
    await user.click(tree.getByText('Ship docs'));
    await waitFor(() => expect(dotOf(tree)).toHaveAttribute('data-tone', 'muted'));
    expect(testSeen.marked).toEqual(['done-1']);
  });

  it('shows a task opened on another device as seen here', async () => {
    setViewport(true);
    testSeen.begin({
      since: Date.now() - HOUR,
      lastVisit: Date.now() - HOUR,
      tasks: { 'done-1': Date.now() - 1000 }
    });
    serve();
    const user = userEvent.setup();
    await renderAt('/');
    const tree = within(await screen.findByTestId('explorer-aside'));
    await user.click(await tree.findByText('Finished recently'));
    await tree.findByRole('treeitem', { name: /Ship docs/ });
    await waitFor(() => expect(testSeen.visits).toBe(1));
    expect(dotOf(tree)).toHaveAttribute('data-tone', 'muted');
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

  it('answers a queue item with no task from the tree, in the centre column', async () => {
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
    const tree = await screen.findByRole('tree');
    await user.click(await within(tree).findByText('Review the plan'));

    await waitFor(() => expect(router.state.location.pathname).toBe('/inbox'));
    expect(router.state.location.search).toMatchObject({ item: 'q-plan' });
    const main = screen.getByRole('main');
    await within(main).findByRole('heading', { name: 'Inbox' });
    expect(within(main).queryByTestId('main-thread')).toBeNull();
    const row = main.querySelector('[data-attention-key="q-plan"]');
    expect(row).toHaveAttribute('aria-current', 'true');
    await user.type(within(main).getByLabelText('Reply to Review the plan'), 'Ship it');
    await user.click(within(main).getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(bodies).toEqual([{ response: 'Ship it' }]));
  });

  it('opens the answer view from the Needs you group, with Activity beside it', async () => {
    setViewport(true);
    const user = userEvent.setup();
    server.use(
      attentionRead(() => HttpResponse.json(attentionSnapshot([attentionItem(planReview)]))),
      activityRead(() => HttpResponse.json([]))
    );
    const router = await renderAt('/');
    const group = await screen.findByRole('treeitem', { name: 'Needs you, 1' });
    await user.click(within(group).getByText('Needs you'));
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

  it('opens on the phone over the chat pane, with a way back to the explorer', async () => {
    setViewport(false);
    const user = userEvent.setup();
    server.use(
      attentionRead(() => HttpResponse.json(attentionSnapshot([attentionItem(planReview)])))
    );
    const router = await renderAt('/?tab=explorer');
    await user.click(await within(await screen.findByRole('tree')).findByText('Review the plan'));
    await waitFor(() => expect(router.state.location.pathname).toBe('/inbox'));
    expect(await screen.findByRole('heading', { name: 'Inbox' })).toBeVisible();
    await user.click(screen.getByRole('link', { name: 'Back to Explorer' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/'));
    expect(screen.getByTestId('tab-explorer')).toHaveAttribute('aria-pressed', 'true');
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

describe('answer view on desktop', () => {
  it('goes back to the chat from its back link', async () => {
    setViewport(true);
    const user = userEvent.setup();
    const router = await renderAt('/inbox');
    await user.click(await screen.findByRole('link', { name: 'Back to chat' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/'));
    expect(await screen.findByTestId('main-thread')).toBeInTheDocument();
  });
});
