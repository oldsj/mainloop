import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection } from '@/api';
import { API_URL } from '@/api/config';
import { dotfilesHandlers } from '@/test/msw/handlers';
import { server } from '@/test/msw/server';
import { DotfilesRefresh } from './DotfilesRefresh';
import { dotfilesHint, type SessionDotfiles, type SessionDotfilesInfo } from './model';

// The hint reads through the app's limiter, which the tests skip. See fastReads.ts.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('@/features/sessions/test/fastReads');
  return withFastReads(await importOriginal());
});

afterEach(async () => {
  await connection.check();
});

const SESSION = '5f0c6a9e-4c1d-4b7a-9a43-2f8e1d6b0c11';
const OLD = 'a1b2c3d4e5f60718293a4b5c6d7e8f9012345678';
const NEW = 'e4f5a6b7c8d90123456789abcdef0123456789ab';
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

function pin(commit: string, over: Partial<SessionDotfiles> = {}): SessionDotfiles {
  return {
    status: 'ready',
    reason: '',
    repository: 'example/dotfiles',
    ref: 'main',
    path: 'home/.agents',
    commit,
    digest: `sha256:${'1'.repeat(64)}`,
    ...over
  };
}

/** A main thread whose native Session holds OLD while the source's ref names NEW. */
function behind(over: Partial<SessionDotfilesInfo> = {}): SessionDotfilesInfo {
  return {
    current: pin(OLD),
    source: {
      repository: 'example/dotfiles',
      ref: 'main',
      path: 'home/.agents',
      commit: NEW,
      reason: ''
    },
    behind: true,
    refreshable: true,
    refresh: null,
    ...over
  };
}

function renderHint() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } }
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <DotfilesRefresh sessionId={SESSION} />
    </QueryClientProvider>
  );
}

describe('dotfilesHint', () => {
  it('names both commits and links their comparison', () => {
    expect(dotfilesHint(behind())).toEqual({
      text: 'dotfiles changed: a1b2c3d → e4f5a6b',
      compareUrl: `https://github.com/example/dotfiles/compare/${OLD}...${NEW}`,
      problem: null
    });
  });

  it('names the repository when the source moved to another one', () => {
    const hint = dotfilesHint(
      behind({ current: pin(OLD, { repository: 'example/old-dotfiles' }) })
    );
    expect(hint?.text).toBe(
      'dotfiles changed: example/old-dotfiles @ a1b2c3d, home/.agents/ → example/dotfiles @ e4f5a6b, home/.agents/'
    );
    expect(hint?.compareUrl).toBeNull();
  });

  it('offers dotfiles to a session created without any', () => {
    expect(dotfilesHint(behind({ current: null }))?.text).toBe(
      'dotfiles available: example/dotfiles @ e4f5a6b, home/.agents/'
    );
  });

  it('says nothing when the session is current or a refresh does not apply to it', () => {
    expect(dotfilesHint(behind({ behind: false }))).toBeNull();
    expect(dotfilesHint(behind({ refreshable: false }))).toBeNull();
  });

  it('shows a refresh that has not settled, even once the session looks current', () => {
    const refresh = {
      id: 'r1',
      state: 'pending' as const,
      reason: '' as const,
      dotfiles: pin(NEW),
      created_at: '2026-11-15T10:00:00Z'
    };
    expect(dotfilesHint(behind({ behind: false, refresh }))?.problem).toEqual({
      pending: true,
      text: 'refresh not confirmed yet'
    });
  });

  it('shows why a refresh of the current commit failed, and forgets an older failure', () => {
    const failed = (commit: string, reason: 'no_bundle' | 'unsupported', over = {}) => ({
      id: 'r1',
      state: 'failed' as const,
      reason,
      dotfiles: pin(commit, over),
      created_at: '2026-11-15T10:00:00Z'
    });
    expect(dotfilesHint(behind({ refresh: failed(NEW, 'unsupported') }))?.problem?.text).toBe(
      'last refresh failed: kagent cannot update a running session yet'
    );
    expect(
      dotfilesHint(
        behind({
          refresh: failed(NEW, 'no_bundle', { status: 'refused', reason: 'secret_found' })
        })
      )?.problem?.text
    ).toBe(
      'last refresh failed: example/dotfiles @ e4f5a6b, home/.agents/: refused, a possible secret was found'
    );
    expect(dotfilesHint(behind({ refresh: failed(OLD, 'unsupported') }))?.problem).toBeNull();
  });
});

describe('DotfilesRefresh', () => {
  it('shows nothing for a session with nothing to refresh', async () => {
    let read = false;
    server.use(
      http.get(`${API_URL}/sessions/:session_id/dotfiles`, () => {
        read = true;
        return HttpResponse.json(behind({ behind: false }));
      })
    );
    renderHint();
    await waitFor(() => expect(read).toBe(true));
    expect(screen.queryByTestId('dotfiles-hint')).toBeNull();
  });

  it('refreshes with a request id, then says the next turn reads the new commit', async () => {
    const user = userEvent.setup();
    const bodies: unknown[] = [];
    server.use(...dotfilesHandlers({ sessions: { [SESSION]: behind() } }));
    server.events.on('request:start', ({ request }) => {
      if (request.method === 'POST' && request.url.endsWith('/dotfiles/refresh')) {
        void request
          .clone()
          .json()
          .then((body) => bodies.push(body));
      }
    });
    renderHint();

    expect(await screen.findByTestId('dotfiles-change')).toHaveTextContent(
      'dotfiles changed: a1b2c3d → e4f5a6b'
    );
    expect(screen.getByRole('link', { name: 'diff' })).toHaveAttribute(
      'href',
      `https://github.com/example/dotfiles/compare/${OLD}...${NEW}`
    );
    await user.click(screen.getByTestId('dotfiles-refresh'));

    expect(await screen.findByTestId('dotfiles-done')).toHaveTextContent(
      'dotfiles refreshed to e4f5a6b; the next turn reads them'
    );
    expect(screen.queryByTestId('dotfiles-change')).toBeNull();
    server.events.removeAllListeners();
    expect(bodies).toEqual([{ request_id: expect.stringMatching(UUID) }]);
  });

  it('gives the reason a refresh does not apply, without asking to try again', async () => {
    const user = userEvent.setup();
    server.use(
      http.get(`${API_URL}/sessions/:session_id/dotfiles`, () => HttpResponse.json(behind())),
      http.post(`${API_URL}/sessions/:session_id/dotfiles/refresh`, () =>
        HttpResponse.json({ detail: 'This session is archived; start a new one.' }, { status: 409 })
      )
    );
    renderHint();

    await user.click(await screen.findByTestId('dotfiles-refresh'));
    expect(await screen.findByTestId('dotfiles-error')).toHaveTextContent(
      /^This session is archived; start a new one\.$/
    );
  });

  it('repeats the same request id after a refresh kagent did not answer', async () => {
    const user = userEvent.setup();
    const ids: string[] = [];
    let info = behind();
    server.use(
      http.get(`${API_URL}/sessions/:session_id/dotfiles`, () => HttpResponse.json(info)),
      http.post(`${API_URL}/sessions/:session_id/dotfiles/refresh`, async ({ request }) => {
        const body = (await request.json()) as { request_id: string };
        ids.push(body.request_id);
        if (ids.length === 1) {
          info = behind({
            refresh: {
              id: body.request_id,
              state: 'pending',
              reason: '',
              dotfiles: pin(NEW),
              created_at: '2026-11-15T10:00:00Z'
            }
          });
          return HttpResponse.json(
            { detail: "Could not refresh the session's dotfiles: unavailable" },
            { status: 502 }
          );
        }
        return HttpResponse.json({ detail: 'Nothing to refresh' }, { status: 409 });
      })
    );
    renderHint();

    await user.click(await screen.findByTestId('dotfiles-refresh'));
    expect(await screen.findByTestId('dotfiles-error')).toHaveTextContent(
      "Could not refresh the session's dotfiles: unavailable. Try again."
    );
    expect(await screen.findByTestId('dotfiles-problem')).toHaveTextContent(
      'refresh not confirmed yet'
    );
    await user.click(screen.getByRole('button', { name: 'Retry refresh' }));
    await waitFor(() => expect(ids).toHaveLength(2));
    expect(ids[1]).toBe(ids[0]);
  });
});
