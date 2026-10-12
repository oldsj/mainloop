import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection } from '@/api';
import { server } from '@/test/msw/server';
import { workspaceFixture } from '@/test/fixtures';
import { attempt, view } from '@/test/fixtures/tasks';
import { delivery, native, session } from './test/fixtures';
import {
  nativeReadWith,
  otherListReads,
  problem,
  sessionAction,
  sessionRead,
  serveSession,
  tasksRead,
  workspacesRead
} from './test/handlers';
import { renderSessions } from './test/render';

// The page's reads would otherwise queue behind the app's read limiter, so an action's re-read
// would take seconds. See fastReads.ts for why this mock sits in each test file.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('./test/fastReads');
  return withFastReads(await importOriginal());
});

afterEach(async () => {
  vi.restoreAllMocks();
  await connection.check();
});

const heading = (name: string) => screen.findByRole('heading', { level: 1, name });

describe('session page', () => {
  it('shows the title, description, status and back link', async () => {
    server.use(
      ...serveSession(
        session({ id: 's1', title: 'Fix login redirect', description: 'Investigate the loop.' })
      )
    );
    renderSessions('/sessions/s1');
    expect(await heading('Fix login redirect')).toBeInTheDocument();
    expect(screen.getByText('Investigate the loop.')).toBeInTheDocument();
    expect(screen.getByTestId('session-status')).toHaveTextContent('[ACTIVE]');
    expect(screen.getByRole('link', { name: 'Back' })).toHaveAttribute('href', '/');
  });

  it('offers cancel and stop turn for an active session with a turn in flight', async () => {
    server.use(
      ...serveSession(
        session({ id: 's1', status: 'active' }),
        native({ session_id: 's1', turn_in_flight: true })
      )
    );
    renderSessions('/sessions/s1');
    await heading('Fix login redirect');
    expect(screen.getByTestId('session-cancel')).toBeInTheDocument();
    expect(await screen.findByTestId('session-stop-turn')).toBeInTheDocument();
  });

  it('keeps cancel but hides Stop turn for an idle session with no turn running', async () => {
    server.use(
      ...serveSession(
        session({ id: 's1', status: 'idle' }),
        native({ session_id: 's1', turn_in_flight: false })
      )
    );
    renderSessions('/sessions/s1');
    await heading('Fix login redirect');
    expect(await screen.findByTestId('identity-strip')).toBeInTheDocument();
    expect(screen.getByTestId('session-cancel')).toBeEnabled();
    expect(screen.queryByTestId('session-stop-turn')).not.toBeInTheDocument();
  });

  it('hides cancel for a completed session, and offers Clear', async () => {
    server.use(...serveSession(session({ id: 's1', status: 'completed', summary: 'Fixed it.' })));
    renderSessions('/sessions/s1');
    await heading('Fix login redirect');
    expect(screen.queryByTestId('session-cancel')).not.toBeInTheDocument();
    expect(screen.getByTestId('archive-session')).toHaveTextContent('Clear');
  });

  it('shows the summary of a completed session', async () => {
    server.use(
      ...serveSession(
        session({ id: 's1', status: 'completed', summary: 'Found and fixed the loop.' })
      )
    );
    renderSessions('/sessions/s1');
    await heading('Fix login redirect');
    expect(screen.getByRole('heading', { level: 3, name: 'Summary' })).toBeInTheDocument();
    expect(screen.getByText('Found and fixed the loop.')).toBeInTheDocument();
  });

  it('shows the error of a failed session, with Clear but no Stop turn or Cancel', async () => {
    server.use(...serveSession(session({ id: 's1', status: 'failed', error: 'boom' })));
    renderSessions('/sessions/s1');
    await heading('Fix login redirect');
    expect(screen.getByRole('heading', { level: 3, name: 'Error' })).toBeInTheDocument();
    expect(screen.getByText('boom')).toBeInTheDocument();
    expect(screen.queryByTestId('session-stop-turn')).not.toBeInTheDocument();
    expect(screen.queryByTestId('session-cancel')).not.toBeInTheDocument();
    expect(screen.getByTestId('archive-session')).toBeInTheDocument();
  });

  it('hides Stop turn for a cancelled session, and offers Clear', async () => {
    server.use(...serveSession(session({ id: 's1', status: 'cancelled' })));
    renderSessions('/sessions/s1');
    await heading('Fix login redirect');
    expect(screen.queryByTestId('session-stop-turn')).not.toBeInTheDocument();
    expect(screen.queryByTestId('session-cancel')).not.toBeInTheDocument();
    expect(screen.getByTestId('archive-session')).toHaveTextContent('Clear');
  });

  it('hides Stop turn for an archived session', async () => {
    server.use(
      ...serveSession(
        session({ id: 's1', status: 'completed', archived_at: '2026-01-02T09:00:00Z' })
      )
    );
    renderSessions('/sessions/s1');
    await heading('Fix login redirect');
    expect(screen.queryByTestId('session-stop-turn')).not.toBeInTheDocument();
  });

  it('offers Stop turn for a superseded session with a turn in flight, but not Clear', async () => {
    server.use(
      ...serveSession(
        session({ id: 's1', status: 'superseded' }),
        native({ session_id: 's1', turn_in_flight: true })
      )
    );
    renderSessions('/sessions/s1');
    await heading('Fix login redirect');
    expect(await screen.findByTestId('session-stop-turn')).toBeInTheDocument();
    expect(screen.queryByTestId('session-cancel')).not.toBeInTheDocument();
    expect(screen.queryByTestId('archive-session')).not.toBeInTheDocument();
  });

  it('mounts the session chat in the slot, with its composer', async () => {
    server.use(...serveSession(session({ id: 's1' })));
    renderSessions('/sessions/s1');
    await heading('Fix login redirect');
    const slot = screen.getByTestId('session-chat-slot');
    expect(slot).toHaveAttribute('data-session-id', 's1');
    await waitFor(() =>
      expect(within(slot).getByRole('textbox', { name: 'Message' })).toBeEnabled()
    );
  });

  it('shows the not-found state for a session that does not exist', async () => {
    server.use(sessionRead(problem(404, 'Session not found')), ...otherListReads());
    renderSessions('/sessions/missing');
    const notFound = await screen.findByTestId('session-not-found');
    expect(
      within(notFound).getByRole('heading', { name: 'Session not found' })
    ).toBeInTheDocument();
    expect(within(notFound).getByRole('link', { name: 'Back to home' })).toHaveAttribute(
      'href',
      '/'
    );
  });

  it('shows a loading state with placeholder bars until the session is read', async () => {
    let release!: () => void;
    const released = new Promise<void>((resolve) => {
      release = resolve;
    });
    server.use(
      sessionRead(async () => {
        await released;
        return HttpResponse.json(session({ id: 's1', title: 'Loaded at last' }));
      }),
      ...otherListReads()
    );
    renderSessions('/sessions/s1');

    expect(await screen.findByRole('status')).toHaveTextContent('Loading session...');
    expect(screen.getByTestId('session-page-loading')).toBeInTheDocument();

    release();
    expect(await heading('Loaded at last')).toBeInTheDocument();
    expect(screen.queryByTestId('session-page-loading')).not.toBeInTheDocument();
  });

  it('reports a body it cannot read as an error, not as unreachable', async () => {
    server.use(
      sessionRead(
        () =>
          new HttpResponse('<html>not json</html>', {
            status: 200,
            headers: { 'Content-Type': 'application/json' }
          })
      ),
      ...otherListReads()
    );
    renderSessions('/sessions/s1');
    expect(await screen.findByText('Could not load session')).toBeInTheDocument();
    expect(screen.queryByText("Can't reach the Mainloop backend.")).not.toBeInTheDocument();
  });

  it('shows a server error with a reload instruction and no retry', async () => {
    server.use(sessionRead(problem(500, 'boom')), ...otherListReads());
    renderSessions('/sessions/s1');
    expect(await screen.findByText('Server error')).toBeInTheDocument();
    expect(
      screen.getByText('Mainloop returned a server error. Reload to try again.')
    ).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /try again/i })).not.toBeInTheDocument();
  });

  it('shows the unreachable state, and a retry that loads the session', async () => {
    const item = session({ id: 's1', title: 'Back after retry' });
    let calls = 0;
    server.use(
      sessionRead(() => {
        calls += 1;
        return calls === 1 ? HttpResponse.error() : HttpResponse.json(item);
      }),
      ...otherListReads()
    );
    const user = userEvent.setup();
    renderSessions('/sessions/s1');
    expect(await screen.findByText("Can't reach the Mainloop backend.")).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: /try again/i }));
    expect(await heading('Back after retry')).toBeInTheDocument();
  });
});

describe('cancel', () => {
  it('asks in the app before it posts, and never uses window.confirm', async () => {
    const confirmSpy = vi.spyOn(window, 'confirm');
    let posts = 0;
    server.use(
      sessionAction('cancel', () => {
        posts += 1;
        return HttpResponse.json({ status: 'cancelled', agent: 'stopped' });
      }),
      ...serveSession(session({ id: 's1', status: 'active' }))
    );
    const user = userEvent.setup();
    renderSessions('/sessions/s1');
    await heading('Fix login redirect');

    await user.click(screen.getByTestId('session-cancel'));
    const dialog = await screen.findByRole('alertdialog');
    expect(within(dialog).getByText('Cancel this session?')).toBeInTheDocument();
    expect(posts).toBe(0);

    await user.click(within(dialog).getByRole('button', { name: 'Keep it running' }));
    await waitFor(() => expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument());
    expect(posts).toBe(0);

    await user.click(screen.getByTestId('session-cancel'));
    await user.click(await screen.findByRole('button', { name: 'Yes, cancel' }));
    expect(await screen.findByText('Session cancelled.')).toBeInTheDocument();
    expect(posts).toBe(1);
    expect(confirmSpy).not.toHaveBeenCalled();
  });

  it('warns when the agent state after a cancel is unconfirmed', async () => {
    server.use(
      sessionAction('cancel', () => HttpResponse.json({ status: 'cancelled', agent: 'unknown' })),
      ...serveSession(session({ id: 's1', status: 'active' }))
    );
    const user = userEvent.setup();
    renderSessions('/sessions/s1');
    await heading('Fix login redirect');
    await user.click(screen.getByTestId('session-cancel'));
    await user.click(await screen.findByRole('button', { name: 'Yes, cancel' }));
    const notice = await screen.findByTestId('session-action-notice');
    expect(notice).toHaveTextContent("couldn't confirm the agent stopped");
    expect(notice).toHaveClass('text-term-yellow');
  });

  it('re-reads the session after a 409 and shows the owner the reason', async () => {
    let reads = 0;
    server.use(
      sessionAction('cancel', problem(409, 'A task is still running.')),
      sessionRead(() => {
        reads += 1;
        return HttpResponse.json(session({ id: 's1', status: 'active' }));
      }),
      ...serveSession(session({ id: 's1', status: 'active' }))
    );
    const user = userEvent.setup();
    renderSessions('/sessions/s1');
    await heading('Fix login redirect');
    const readsBefore = reads;

    await user.click(screen.getByTestId('session-cancel'));
    await user.click(await screen.findByRole('button', { name: 'Yes, cancel' }));

    expect(await screen.findByText('A task is still running.')).toBeInTheDocument();
    // The re-read runs after the notice is set, so wait for it rather than checking once.
    await waitFor(() => expect(reads).toBeGreaterThan(readsBefore));
  });

  it('is disabled while the backend is unreachable', async () => {
    connection.reportFailure();
    server.use(
      ...serveSession(
        session({ id: 's1', status: 'active' }),
        native({ session_id: 's1', turn_in_flight: true })
      )
    );
    renderSessions('/sessions/s1');
    await heading('Fix login redirect');
    expect(screen.getByTestId('session-cancel')).toBeDisabled();
    expect(await screen.findByTestId('session-stop-turn')).toBeDisabled();
  });
});

describe('stop turn and clear', () => {
  it('stops the turn and keeps the session open', async () => {
    let posts = 0;
    server.use(
      sessionAction('stop-turn', () => {
        posts += 1;
        return HttpResponse.json({ status: 'stopped' });
      }),
      ...serveSession(
        session({ id: 's1', status: 'active' }),
        native({ session_id: 's1', turn_in_flight: true })
      )
    );
    const user = userEvent.setup();
    renderSessions('/sessions/s1');
    await heading('Fix login redirect');
    await user.click(await screen.findByTestId('session-stop-turn'));
    expect(await screen.findByText('Turn stopped. The session is still open.')).toBeInTheDocument();
    expect(posts).toBe(1);
  });

  it('clears a finished session and returns to the list', async () => {
    let archived = 0;
    server.use(
      sessionAction('archive', () => {
        archived += 1;
        return HttpResponse.json({ status: 'archived' });
      }),
      ...serveSession(session({ id: 's1', status: 'completed', summary: 'Done.' }))
    );
    const user = userEvent.setup();
    const { router } = renderSessions('/sessions/s1');
    await heading('Fix login redirect');
    await user.click(screen.getByTestId('archive-session'));

    expect(await screen.findByRole('heading', { name: 'Sessions' })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/');
    expect(archived).toBe(1);
  });
});

describe('identity strip', () => {
  it('is collapsed by default and shows the agent details on toggle', async () => {
    server.use(
      ...serveSession(
        session({ id: 's1' }),
        native({
          session_id: 's1',
          kind: 'claude',
          agent_name: 'claude-child',
          kagent_session_id: 'kagent-session-123',
          turns: 4,
          model: 'claude-sonnet-5-5'
        })
      )
    );
    const user = userEvent.setup();
    renderSessions('/sessions/s1');
    await heading('Fix login redirect');

    const toggle = await screen.findByTestId('identity-toggle');
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    expect(toggle).not.toHaveAttribute('aria-controls');
    expect(screen.queryByTestId('id-kind')).not.toBeInTheDocument();
    expect(screen.getByTestId('identity-strip')).toHaveTextContent('sonnet-5-5');

    await user.click(toggle);
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    expect(toggle).toHaveAttribute('aria-controls', 'identity-details');
    expect(document.getElementById('identity-details')).toBeInTheDocument();
    expect(screen.getByTestId('id-kind')).toHaveTextContent('claude');
    expect(screen.getByTestId('id-agent')).toHaveTextContent('claude-child');
    expect(screen.getByTestId('id-session')).toHaveTextContent('kagent-s');
    expect(screen.getByTestId('id-turns')).toHaveTextContent('4');
  });

  it('shows a failed last delivery as a problem, never as ready', async () => {
    server.use(
      ...serveSession(
        session({ id: 's1' }),
        native({
          session_id: 's1',
          session_state: 'ready',
          deliveries: [delivery({ message_id: 'm1', state: 'failed', detail: 'refused' })]
        })
      )
    );
    renderSessions('/sessions/s1');
    await heading('Fix login redirect');

    const strip = await screen.findByTestId('identity-strip');
    expect(within(strip).getByText('last message failed')).toBeInTheDocument();
    expect(strip).not.toHaveTextContent(/\bready\b/);
    expect(screen.getByTestId('id-delivery-problem')).toHaveTextContent('Failed: refused');
  });

  it('keeps Cancel but hides Stop turn when the native payload is malformed', async () => {
    server.use(
      nativeReadWith(() => HttpResponse.json({ session_id: 's1', kind: 'claude', turns: 1 })),
      ...serveSession(session({ id: 's1', status: 'active' }))
    );
    renderSessions('/sessions/s1');
    await heading('Fix login redirect');

    expect(await screen.findByTestId('panel-error-identity')).toBeInTheDocument();
    expect(screen.getByTestId('session-cancel')).toBeEnabled();
    // Without a readable native state the page cannot tell a turn is running, so it offers no Stop.
    expect(screen.queryByTestId('session-stop-turn')).not.toBeInTheDocument();
  });

  it('never shows a cancelled session as live, even when the agent still reports ready', async () => {
    server.use(
      ...serveSession(
        session({ id: 's1', status: 'cancelled' }),
        native({ session_id: 's1', session_state: 'ready' })
      )
    );
    renderSessions('/sessions/s1');
    await heading('Fix login redirect');

    const strip = await screen.findByTestId('identity-strip');
    expect(within(strip).getByText('cancelled')).toBeInTheDocument();
    expect(strip).not.toHaveTextContent(/\blive\b/);
  });
});

describe('task and workspace', () => {
  it('the back link goes to the task that owns this session', async () => {
    server.use(
      tasksRead([
        view(
          { id: 'task-1', title: 'Add retry button' },
          { attempts: [attempt({ task_id: 'task-1', session_id: 's1' })] }
        )
      ]),
      ...serveSession(session({ id: 's1' }))
    );
    renderSessions('/sessions/s1');
    await heading('Fix login redirect');
    expect(await screen.findByRole('link', { name: 'Back to task' })).toHaveAttribute(
      'href',
      '/tasks/task-1'
    );
  });

  it('the back link goes to the project when the session has no task', async () => {
    server.use(...serveSession(session({ id: 's1', project_id: 'project-a' })));
    renderSessions('/sessions/s1');
    await heading('Fix login redirect');
    expect(await screen.findByRole('link', { name: 'Back to project' })).toHaveAttribute(
      'href',
      '/projects/project-a'
    );
  });

  it('links to the task that owns this session', async () => {
    server.use(
      tasksRead([
        view(
          { id: 'task-1', title: 'Add retry button' },
          { attempts: [attempt({ task_id: 'task-1', session_id: 's1' })] }
        )
      ]),
      ...serveSession(session({ id: 's1' }))
    );
    renderSessions('/sessions/s1');
    await heading('Fix login redirect');
    const link = await screen.findByTestId('session-task-link');
    expect(link).toHaveTextContent('Add retry button');
    expect(link).toHaveAttribute('href', '/tasks/task-1');
  });

  it('shows the workspace state and links to manage it', async () => {
    server.use(
      workspacesRead([workspaceFixture({ session_id: 's1', observed_state: 'running' })]),
      ...serveSession(session({ id: 's1' }))
    );
    renderSessions('/sessions/s1');
    await heading('Fix login redirect');
    expect(await screen.findByTestId('workspace-lifecycle-badge')).toHaveTextContent('WS RUNNING');
    expect(screen.getByTestId('workspace-publication-badge')).toHaveTextContent(
      'Read only · disabled'
    );
    expect(screen.getByRole('link', { name: 'Manage workspace' })).toHaveAttribute(
      'href',
      '/workspaces/w1'
    );
  });
});

describe('chat slot layout and state', () => {
  /** A visual viewport of the given height, as iOS reports it. Removed by the caller. */
  function stubVisualViewport(height: number) {
    const viewport = Object.assign(new EventTarget(), { height, scale: 1 });
    Object.defineProperty(window, 'visualViewport', { value: viewport, configurable: true });
    return viewport;
  }

  afterEach(() => {
    Reflect.deleteProperty(window, 'visualViewport');
  });

  it('a cancelled session reads cancelled in the chat, not the agent ready', async () => {
    server.use(
      ...serveSession(
        session({ id: 's1', status: 'cancelled' }),
        native({ session_id: 's1', session_state: 'ready' })
      )
    );
    renderSessions('/sessions/s1');
    await heading('Fix login redirect');
    expect(await screen.findByTestId('session-chat-state')).toHaveTextContent(/^cancelled$/);
  });

  it('the chat takes the rest of the page instead of a share of the viewport', async () => {
    stubVisualViewport(600);
    server.use(...serveSession(session({ id: 's1' })));
    renderSessions('/sessions/s1');
    await heading('Fix login redirect');
    const panel = await screen.findByTestId('session-chat-panel');
    expect(panel).toHaveClass('flex-1', 'min-h-40');
    expect(panel.style.height).toBe('');
    expect(screen.getByTestId('session-chat-slot')).toHaveClass('flex-1', 'min-h-0');
    // The page is its container's height, capped at the bottom of the visible viewport. The header is
    // measured as 0 in jsdom.
    const page = screen.getByRole('article');
    expect(page).toHaveClass('h-full');
    expect(page.style.height).toBe('');
    expect(page.style.maxHeight).toBe('600px');
  });

  it('re-measures when its container changes, so a banner that appears later is accounted for', async () => {
    stubVisualViewport(600);
    // The page is re-measured when its container changes, not only when the viewport does.
    let notify: () => void = () => {};
    const observed: Element[] = [];
    class FakeResizeObserver {
      constructor(callback: () => void) {
        notify = callback;
      }
      observe(target: Element) {
        observed.push(target);
      }
      unobserve() {}
      disconnect() {}
    }
    vi.stubGlobal('ResizeObserver', FakeResizeObserver);
    try {
      server.use(...serveSession(session({ id: 's1' })));
      renderSessions('/sessions/s1');
      await heading('Fix login redirect');
      const page = screen.getByRole('article');
      expect(observed).toContain(page.parentElement);
      expect(page.style.maxHeight).toBe('600px');

      // An offline banner appears above the page and pushes its top edge down by 56 px.
      let top = 0;
      vi.spyOn(page, 'getBoundingClientRect').mockImplementation(
        () =>
          ({
            top,
            bottom: 0,
            left: 0,
            right: 0,
            width: 0,
            height: 0,
            x: 0,
            y: top,
            toJSON: () => ({})
          }) as DOMRect
      );
      top = 56;
      act(() => notify());
      expect(page.style.maxHeight).toBe('544px');
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it('without a visual viewport the page fills its container', async () => {
    server.use(...serveSession(session({ id: 's1' })));
    renderSessions('/sessions/s1');
    await heading('Fix login redirect');
    const page = screen.getByRole('article');
    expect(page).toHaveClass('h-full');
    expect(page.style.height).toBe('');
  });
});
