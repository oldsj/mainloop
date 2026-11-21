import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it, vi } from 'vitest';
import { API_URL } from '@/api/config';
import { server } from '@/test/msw/server';
import { native, session } from '@/features/sessions/test/fixtures';
import { serveSession } from '@/features/sessions/test/handlers';
import { renderSessions } from '@/features/sessions/test/render';
import type { SessionTranscript, TranscriptEntry } from './model';

// See fastReads.ts: the page's reads would otherwise queue behind the app's read limiter.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('@/features/sessions/test/fastReads');
  return withFastReads(await importOriginal());
});

const entry = (
  id: number | null,
  pos: number,
  kind: TranscriptEntry['kind'],
  text: string,
  more: Partial<TranscriptEntry> = {}
): TranscriptEntry => ({
  id,
  task_id: id === null ? 't-live' : 't1',
  pos,
  kind,
  tool_name: '',
  tool_call_id: '',
  is_error: false,
  text,
  truncated: false,
  orig_bytes: text.length,
  at: '2026-11-14T10:00:00Z',
  live: id === null,
  ...more
});

const STORED = [
  entry(1, 0, 'user', 'why does the rollout fail'),
  entry(2, 1, 'thinking', 'let me look'),
  entry(3, 2, 'tool_call', '{"command":"kubectl get pods"}', {
    tool_name: 'Bash',
    tool_call_id: 'c1'
  }),
  entry(4, 3, 'tool_result', 'NAME READY\napi 0/1', { tool_name: 'Bash', tool_call_id: 'c1' }),
  entry(5, 4, 'assistant', 'The probe never passes.'),
  entry(6, 5, 'user', 'fix it', { truncated: true, orig_bytes: 90000 })
];
const LIVE = [entry(null, 0, 'assistant', 'Working on the probe now.')];

const page = (
  entries: TranscriptEntry[],
  more: Partial<SessionTranscript> = {}
): SessionTranscript => ({
  entries,
  has_more: false,
  has_earlier: false,
  sources: [
    {
      state: 'active',
      reason: '',
      scanned_at: null,
      stored_bytes: 100,
      created_at: '2026-11-14T09:00:00Z'
    }
  ],
  live: [],
  live_error: null,
  ...more
});

/** Serves the transcript as the API pages it, and records each read's query. */
function serveTranscript(reads: URLSearchParams[]) {
  return http.get(`${API_URL}/sessions/s1/transcript`, ({ request }) => {
    const q = new URL(request.url).searchParams;
    reads.push(q);
    const ids = STORED.map((e) => e.id as number);
    if (q.get('latest') === 'true') {
      return HttpResponse.json(page(STORED.slice(4), { has_earlier: true, live: LIVE }));
    }
    if (q.get('before')) {
      const before = Number(q.get('before'));
      return HttpResponse.json(
        page(
          STORED.filter((e) => (e.id as number) < before),
          { has_more: true }
        )
      );
    }
    const after = Number(q.get('after') ?? 0);
    const rest = STORED.filter((e) => (e.id as number) > after).slice(0, 2);
    const lastId = rest[rest.length - 1]?.id ?? after;
    return HttpResponse.json(
      page(rest, {
        has_earlier: after > 0,
        has_more: ids.some((id) => id > (lastId as number)),
        live: []
      })
    );
  });
}

describe('session transcript', () => {
  it('opens at the end, with the turn not stored yet, and shows earlier entries on request', async () => {
    const reads: URLSearchParams[] = [];
    server.use(
      serveTranscript(reads),
      ...serveSession(session({ id: 's1' }), native({ session_id: 's1' }))
    );
    renderSessions('/sessions/s1?transcript=1');

    const log = await screen.findByTestId('transcript');
    expect(await within(log).findByText('The probe never passes.')).toBeInTheDocument();
    expect(within(log).getByText('Working on the probe now.')).toBeInTheDocument();
    expect(within(log).getByText(/Not stored yet/)).toBeInTheDocument();
    expect(within(log).getByText('Cut to 6 of 90,000 bytes when stored')).toBeInTheDocument();
    expect(reads[0]?.get('latest')).toBe('true');
    // The conversation is a link away; the chat slot is not on the page.
    expect(screen.getByTestId('session-view-toggle')).toHaveTextContent('Conversation');

    await userEvent.click(within(log).getByRole('button', { name: 'Show earlier' }));
    const tool = await within(log).findByTestId('transcript-tool');
    expect(reads.at(-1)?.get('before')).toBe('5');
    // Collapsed, a call shows its name, its argument and the result's first line.
    expect(tool).toHaveTextContent('Bash');
    expect(tool).toHaveTextContent('kubectl get pods');
    expect(tool).toHaveTextContent('⎿ NAME READY');
    expect(tool).not.toHaveAttribute('open');
    await userEvent.click(within(tool).getByText('Bash'));
    expect(tool).toHaveAttribute('open');
    expect(within(tool).getByText(/api 0\/1/)).toBeVisible();
    // Earlier entries sit above the later ones.
    const text = log.textContent ?? '';
    expect(text.indexOf('why does the rollout fail')).toBeLessThan(
      text.indexOf('The probe never passes.')
    );
    expect(within(log).queryByRole('button', { name: 'Show earlier' })).not.toBeInTheDocument();
  });

  it('leads a message with what was written, and folds away the context Mainloop sent with it', async () => {
    // Given a stored turn whose message was delivered behind a project update.
    const delivered =
      "[mainloop:project-context] This project's context changed since your brief.\n" +
      '- Added: use pnpm (memory m1)\n[mainloop:new-message]\nrebase on main';
    server.use(
      http.get(`${API_URL}/sessions/s1/transcript`, () =>
        HttpResponse.json(page([entry(1, 0, 'user', delivered)], { live: [] }))
      ),
      ...serveSession(session({ id: 's1' }), native({ session_id: 's1' }))
    );

    // When the transcript opens.
    renderSessions('/sessions/s1?transcript=1');

    // Then the row is the message, and the update opens on request.
    const row = await screen.findByTestId('transcript-user');
    expect(within(row).getByText(/rebase on main/)).toBeInTheDocument();
    expect(within(row).queryByText(/new-message/)).not.toBeInTheDocument();
    const context = within(row).getByTestId('delivered-context');
    expect(context).not.toHaveAttribute('open');
    await userEvent.click(within(context).getByText('Context Mainloop sent with this message'));
    expect(within(context).getByText(/use pnpm/)).toBeInTheDocument();
  });

  it("opens at a search hit's entry, marked, and pages on from there", async () => {
    const reads: URLSearchParams[] = [];
    server.use(
      serveTranscript(reads),
      ...serveSession(session({ id: 's1' }), native({ session_id: 's1' }))
    );
    renderSessions('/sessions/s1?transcript=1&entry=3');

    const log = await screen.findByTestId('transcript');
    const tool = await within(log).findByTestId('transcript-tool');
    expect(reads[0]?.get('after')).toBe('2');
    expect(tool.closest('[data-hit="true"]')).not.toBeNull();
    expect(within(log).getByRole('button', { name: 'Show earlier' })).toBeInTheDocument();
    await userEvent.click(within(log).getByRole('button', { name: 'Show later' }));
    expect(await within(log).findByText('The probe never passes.')).toBeInTheDocument();
    expect(reads.at(-1)?.get('after')).toBe('4');
  });

  it('says when nothing is copied yet, and when part could not be', async () => {
    server.use(
      http.get(`${API_URL}/sessions/s1/transcript`, () =>
        HttpResponse.json(page([], { sources: [] }))
      ),
      ...serveSession(session({ id: 's1' }), native({ session_id: 's1' }))
    );
    renderSessions('/sessions/s1?transcript=1');
    expect(await screen.findByTestId('transcript-empty')).toHaveTextContent('No transcript yet');
  });

  it('notes a source that could not be copied, and a live tail that could not be read', async () => {
    server.use(
      http.get(`${API_URL}/sessions/s1/transcript`, () =>
        HttpResponse.json(
          page(STORED.slice(4), {
            sources: [
              {
                state: 'unavailable',
                reason: 'kagent dropped the session',
                scanned_at: null,
                stored_bytes: 0,
                created_at: '2026-11-14T09:00:00Z'
              }
            ],
            live_error: 'kagent could not be read for the turns not yet copied'
          })
        )
      ),
      ...serveSession(session({ id: 's1' }), native({ session_id: 's1' }))
    );
    renderSessions('/sessions/s1?transcript=1');
    expect(
      await screen.findByText(/could not be copied: kagent dropped the session/)
    ).toBeInTheDocument();
    expect(
      screen.getByText('kagent could not be read for the turns not yet copied')
    ).toBeInTheDocument();
  });

  it('switches between the conversation and the transcript', async () => {
    server.use(
      serveTranscript([]),
      ...serveSession(session({ id: 's1' }), native({ session_id: 's1' }))
    );
    const { router } = renderSessions('/sessions/s1');
    await userEvent.click(await screen.findByTestId('session-view-toggle'));
    await waitFor(() => expect(router.state.location.search).toEqual({ transcript: 1 }));
    expect(await screen.findByTestId('transcript')).toBeInTheDocument();
  });
});

describe('transcript search', () => {
  it('lists hits with their matches marked, each opening its entry in the transcript', async () => {
    const queries: URLSearchParams[] = [];
    server.use(
      http.get(`${API_URL}/transcripts/search`, ({ request }) => {
        queries.push(new URL(request.url).searchParams);
        return HttpResponse.json({
          hits: [
            {
              entry_id: 5,
              session_id: 's1',
              session_title: 'Fix the build',
              task_id: 't1',
              pos: 4,
              kind: 'tool_result',
              tool_name: 'Bash',
              at: '2026-11-14T10:00:00Z',
              rank: 0.5,
              snippet: [
                { text: 'the ', match: false },
                { text: 'rollout', match: true },
                { text: ' <b>waits</b>', match: false }
              ]
            }
          ]
        });
      })
    );
    const { router } = renderSessions('/transcripts?q=rollout&session_id=s1');
    const hit = await screen.findByTestId('transcript-hit');
    expect(queries[0]?.get('q')).toBe('rollout');
    expect(queries[0]?.get('session_id')).toBe('s1');
    expect(within(hit).getByText('rollout').tagName).toBe('MARK');
    // Text from the transcript is text, never markup.
    expect(hit).toHaveTextContent('<b>waits</b>');
    expect(hit).toHaveTextContent('Tool result · Bash');
    expect(hit).toHaveAttribute('href', '/sessions/s1?transcript=1&entry=5');
    expect(screen.getByTestId('transcript-search-widen')).toHaveAttribute(
      'href',
      '/transcripts?q=rollout'
    );

    await userEvent.clear(screen.getByRole('searchbox', { name: 'Search transcripts' }));
    await userEvent.type(
      screen.getByRole('searchbox', { name: 'Search transcripts' }),
      'probe{Enter}'
    );
    await waitFor(() =>
      expect(router.state.location.search).toEqual({ q: 'probe', session_id: 's1' })
    );
  });

  it('says when nothing matched', async () => {
    server.use(http.get(`${API_URL}/transcripts/search`, () => HttpResponse.json({ hits: [] })));
    renderSessions('/transcripts?q=nothing');
    expect(await screen.findByTestId('transcript-search-empty')).toHaveTextContent('Nothing found');
  });
});
