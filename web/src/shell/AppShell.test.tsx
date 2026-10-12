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
import { writeLastVisit } from '@/features/home/lastVisit';
import { projectsRead, serveProjectPage } from '@/features/projects/test/handlers';
import { project } from '@/features/projects/test/fixtures';
import { serveTaskPage, taskScreen, tasksList } from '@/features/tasks/test/handlers';
import { hitlView } from '@/test/fixtures/hitl';
import type * as ConnectionModule from '@/api/connection';
import { connection } from '@/api/connection';
import { server } from '@/test/msw/server';
import { projection, view } from '@/test/fixtures/tasks';
import { createAppRouter } from '@/router';

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

  it('renders the explorer as the desktop aside, with the tree', async () => {
    setViewport(true);
    await renderAt('/');
    const aside = screen.getByRole('complementary', { name: 'Explorer' });
    expect(await within(aside).findByRole('tree')).toBeInTheDocument();
    expect(screen.getByRole('main')).not.toContainElement(aside);
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
    await user.click(screen.getByTestId('header-health'));
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
  it('shows the Needs-you count on the Explorer tab, read from GET /attention', async () => {
    setViewport(false);
    server.use(
      attentionRead(() =>
        HttpResponse.json(
          attentionSnapshot(['a', 'b', 'c'].map((id) => hitlAttentionItem(`req-${id}`)))
        )
      )
    );
    await renderAt('/');
    expect(await screen.findByTestId('explorer-needs-badge')).toHaveTextContent('3');
    expect(screen.getByTestId('tab-explorer')).toHaveAccessibleName('Explorer, 3 need you');
  });

  it('shows no count when nothing needs the owner', async () => {
    setViewport(false);
    await renderAt('/');
    expect(screen.queryByTestId('explorer-needs-badge')).not.toBeInTheDocument();
    expect(screen.getByTestId('tab-explorer')).toHaveAccessibleName('Explorer');
  });

  it('counts a task that needs an owner decision, read from GET /tasks', async () => {
    setViewport(false);
    server.use(tasksList(() => HttpResponse.json([view({ reason: 'owner_decision' })])));
    await renderAt('/');
    expect(await screen.findByTestId('explorer-needs-badge')).toHaveTextContent('1');
    expect(screen.getByTestId('tab-explorer')).toHaveAccessibleName('Explorer, 1 need you');
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
    expect(await screen.findByTestId('explorer-needs-badge')).toHaveTextContent('1');
    needsOwner = false;
    await waitFor(
      () => expect(screen.queryByTestId('explorer-needs-badge')).not.toBeInTheDocument(),
      { timeout: 6000 }
    );
    expect(reads).toBeGreaterThanOrEqual(2);
    expect(screen.getByTestId('tab-explorer')).toHaveAccessibleName('Explorer');
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
  it('has two tabs, Chat and Explorer', async () => {
    setViewport(false);
    await renderAt('/');
    const bar = screen.getByTestId('mobile-tab-bar');
    const tabs = within(bar).getAllByRole('button');
    expect(tabs.map((tab) => tab.textContent?.replace(/\s+/g, ' ').trim())).toEqual([
      '[CHAT]',
      '[EXPLORER]'
    ]);
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
    writeLastVisit(Date.now() - 60 * MIN);
    server.use(
      tasksList(() =>
        HttpResponse.json([
          view({ id: 'fail-1', title: 'Broke', status: 'failed', updated_at: failedNow() })
        ])
      )
    );
  }

  it('keeps the desktop centre to the chat alone, with what changed in the explorer', async () => {
    setViewport(true);
    serveFailedSinceVisit();
    await renderAt('/');
    const aside = screen.getByRole('complementary', { name: 'Explorer' });
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
    await waitFor(() =>
      expect(screen.getByTestId('tab-chat')).toHaveAttribute('aria-pressed', 'true')
    );
  });

  it('lets other pages scroll in the main pane', async () => {
    setViewport(true);
    await renderAt('/health');
    expect(screen.getByRole('main').className).toContain('overflow-y-auto');
  });

  it('records this visit when the shell opens, so the next one compares against it', async () => {
    setViewport(true);
    await renderAt('/');
    const recorded = Number(window.localStorage.getItem('mainloop.home.lastVisit'));
    expect(Math.abs(recorded - Date.now())).toBeLessThan(10_000);
  });
});

describe('offline banner', () => {
  afterEach(async () => {
    reach.up = true;
    await act(async () => {
      await connection.check();
    });
  });

  it.each([
    ['compact', false, 'compact-shell'],
    ['desktop', true, 'desktop-shell']
  ])('sits in the screen-high column above main in the %s shell', async (_name, wide, shell) => {
    setViewport(wide);
    await renderAt('/');
    reach.up = false;
    await act(async () => {
      await connection.check();
    });

    const banner = await screen.findByTestId('connection-banner');
    const column = banner.parentElement;
    expect(column).toHaveClass('h-dvh');
    expect(column).toContainElement(screen.getByTestId(shell));
    const main = screen.getByRole('main');
    expect(column).toContainElement(main);
    expect(banner.compareDocumentPosition(main) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    if (!wide) expect(column).toContainElement(screen.getByTestId('mobile-tab-bar'));
  });
});

describe('explorer panel', () => {
  it('folds the explorer to a rail, and opens it again from the rail', async () => {
    setViewport(true);
    const user = userEvent.setup();
    await renderAt('/');
    await user.click(screen.getByRole('button', { name: 'Collapse explorer' }));
    expect(screen.getByTestId('explorer-rail')).toBeInTheDocument();
    expect(JSON.parse(localStorage.getItem('mainloop.explorer.v1') ?? '{}')).toMatchObject({
      collapsed: true
    });
    await user.click(screen.getByRole('button', { name: 'Show explorer' }));
    expect(await screen.findByTestId('explorer-aside')).toBeInTheDocument();
  });

  it('resizes from the keyboard, clamps the width and remembers it', async () => {
    setViewport(true);
    const user = userEvent.setup();
    await renderAt('/');
    const handle = screen.getByRole('separator', { name: 'Resize explorer' });
    expect(handle).toHaveAttribute('aria-valuenow', '384');
    handle.focus();
    await user.keyboard('{ArrowLeft}{ArrowLeft}');
    expect(handle).toHaveAttribute('aria-valuenow', '416');
    await user.keyboard('{ArrowLeft}'.repeat(10));
    expect(handle).toHaveAttribute('aria-valuenow', '480');
    expect(JSON.parse(localStorage.getItem('mainloop.explorer.v1') ?? '{}')).toMatchObject({
      width: 480
    });
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
