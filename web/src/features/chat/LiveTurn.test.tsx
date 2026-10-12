import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { liveProgress, type ProgressFrame } from '@/api';
import { createSseApplier } from '@/api/invalidation';
import { elapsedSince } from '@/lib/time';
import { ChatPage } from './ChatPage';
import {
  CONVERSATION_ID,
  deliveryFixture,
  MAIN_SESSION_ID,
  mainThreadFixture,
  messageFixture,
  nativeFixture
} from './fixtures';
import { createChatBackend, type ChatBackendState } from './handlers';
import { LiveTurn, VISIBLE_TOOLS } from './LiveTurn';
import { server } from '@/test/msw/server';

// The chat's reads would otherwise queue behind the app's read limiter. See fastReads.ts.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('@/features/tasks/test/fastReads');
  return withFastReads(await importOriginal());
});
vi.mock('@/features/hitl', () => ({ HITLCard: () => null, HITLRequest: () => null }));
vi.mock('@/shell/ConnectionBanner', () => ({
  useConnection: () => ({ status: 'online', offlineSince: null, recoveries: 0 })
}));

beforeEach(() => liveProgress.reset());
afterEach(() => {
  liveProgress.reset();
  sessionStorage.clear();
});

function frame(overrides: Partial<ProgressFrame> = {}): ProgressFrame {
  return {
    session_id: MAIN_SESSION_ID,
    message_id: 'm1',
    seq: 1,
    active: true,
    started_at: new Date(Date.now() - 7_000).toISOString().replace('Z', ''),
    status: { kind: 'thinking', label: 'Thinking…' },
    text: '',
    text_truncated: false,
    tools: [],
    ...overrides
  };
}

function runningTurn(): Partial<ChatBackendState> {
  return {
    mainThread: mainThreadFixture({
      conversation_id: CONVERSATION_ID,
      native: nativeFixture({
        turn_in_flight: true,
        deliveries: [deliveryFixture({ message_id: 'm1', state: 'delivered' })]
      })
    }),
    messages: [messageFixture({ id: 'm1', content: 'build it' })]
  };
}

function setup(initial: Partial<ChatBackendState> = runningTurn()) {
  const backend = createChatBackend(initial);
  server.use(...backend.handlers);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const apply = createSseApplier(queryClient);
  const view = render(
    <QueryClientProvider client={queryClient}>
      <ChatPage />
    </QueryClientProvider>
  );
  const send = (f: ProgressFrame) =>
    act(() => apply({ event: 'session:progress', data: JSON.stringify(f) }));
  return { ...backend, queryClient, apply, send, ...view };
}

const live = () => screen.getByTestId('live-turn');

describe('the live bubble', () => {
  it('shows nothing until the server sends progress', async () => {
    setup();
    await screen.findByText('build it');
    expect(screen.queryByTestId('live-turn')).toBeNull();
  });

  it('grows with the reply, and keeps only the newest frame of a turn', async () => {
    const { send } = setup();
    await screen.findByText('build it');
    send(frame({ seq: 1, status: { kind: 'writing', label: 'Writing…' }, text: 'Hel' }));
    expect(await screen.findByTestId('live-turn-text')).toHaveTextContent('Hel');
    send(frame({ seq: 3, status: { kind: 'writing', label: 'Writing…' }, text: 'Hello **wor' }));
    expect(screen.getByTestId('live-turn-text')).toHaveTextContent('Hello **wor');
    // A frame that arrives out of order is older than what is shown.
    send(frame({ seq: 2, text: 'Hello' }));
    expect(screen.getByTestId('live-turn-text')).toHaveTextContent('Hello **wor');
    expect(screen.getAllByTestId('live-turn')).toHaveLength(1);
  });

  it('shows the truncated start as an ellipsis', async () => {
    const { send } = setup();
    await screen.findByText('build it');
    send(frame({ text: 'the end of a long reply', text_truncated: true }));
    expect(await screen.findByTestId('live-turn-text')).toHaveTextContent(
      '…the end of a long reply'
    );
  });

  it('shows the status line with elapsed time and the tool calls', async () => {
    const { send } = setup();
    await screen.findByText('build it');
    send(
      frame({
        status: { kind: 'tool', label: 'Running Bash: go test ./...' },
        tools: [
          { id: 'c1', name: 'Read', label: 'Reading main.go', state: 'done' },
          { id: 'c2', name: 'Bash', label: 'Running Bash: go test ./...', state: 'running' },
          { id: 'c0', name: 'Bash', label: 'Running Bash: false', state: 'error' }
        ]
      })
    );
    const status = await screen.findByTestId('live-turn-status');
    expect(status).toHaveTextContent('Running Bash: go test ./...');
    expect(screen.getByTestId('live-turn-elapsed')).toHaveTextContent(/^[789]s$/);
    const tools = within(screen.getByTestId('live-turn-tools')).getAllByRole('listitem', {
      hidden: true
    });
    expect(tools.map((li) => li.textContent)).toEqual([
      'Reading main.go',
      'Running Bash: go test ./...',
      'Running Bash: false'
    ]);
  });

  it('collapses older tool lines into a count', async () => {
    const { send } = setup();
    await screen.findByText('build it');
    const tools = Array.from({ length: VISIBLE_TOOLS + 3 }, (_, i) => ({
      id: `c${i}`,
      name: 'Read',
      label: `Reading f${i}.go`,
      state: 'done' as const
    }));
    send(frame({ tools }));
    const list = await screen.findByTestId('live-turn-tools');
    expect(within(list).getAllByRole('listitem', { hidden: true })).toHaveLength(VISIBLE_TOOLS + 1);
    expect(list).toHaveTextContent('+3 earlier');
    expect(list).toHaveTextContent(`Reading f${VISIBLE_TOOLS + 2}.go`);
    expect(list).not.toHaveTextContent('Reading f0.go');
  });

  it('announces the status politely and hides the growing text from screen readers', async () => {
    const { send } = setup();
    await screen.findByText('build it');
    send(frame({ text: 'a few words', status: { kind: 'waiting', label: 'Waiting for you' } }));
    const status = await screen.findByTestId('live-turn-status');
    expect(status).toHaveAttribute('role', 'status');
    expect(status).toHaveAttribute('aria-live', 'polite');
    expect(status).toHaveTextContent('Waiting for you');
    expect(screen.getByTestId('live-turn-text')).toHaveAttribute('aria-hidden', 'true');
    expect(screen.getByTestId('live-turn-elapsed')).toHaveAttribute('aria-hidden', 'true');
  });

  it('keeps the polite status outside the busy region, which holds back live updates', async () => {
    const { send } = setup();
    await screen.findByText('build it');
    send(
      frame({
        text: 'a few words',
        tools: [{ id: 'c1', name: 'Read', label: 'Reading a.go', state: 'running' }]
      })
    );
    const status = await screen.findByTestId('live-turn-status');
    expect(status.closest('[aria-busy="true"]')).toBeNull();
    const busy = live().querySelector('[aria-busy="true"]');
    expect(busy).not.toBeNull();
    expect(busy).toContainElement(screen.getByTestId('live-turn-text'));
    expect(busy).toContainElement(screen.getByTestId('live-turn-tools'));
  });

  it('does not touch the cache: progress asks for no re-read', async () => {
    const { send, state } = setup();
    await screen.findByText('build it');
    const reads = state.mainThreadReads;
    send(frame({ text: 'x' }));
    send(frame({ seq: 2, text: 'xy' }));
    await screen.findByTestId('live-turn');
    expect(state.mainThreadReads).toBe(reads);
  });

  it('keeps Stop working while the bubble is up', async () => {
    const { send, state } = setup();
    await screen.findByText('build it');
    send(frame({ text: 'busy' }));
    await screen.findByTestId('live-turn');
    await userEvent.click(await screen.findByTestId('mt-stop-turn'));
    await waitFor(() => expect(state.stopped).toEqual([MAIN_SESSION_ID]));
  });

  it('is not shown for a turn the thread no longer has in flight', async () => {
    const { send } = setup({
      ...runningTurn(),
      mainThread: mainThreadFixture({ conversation_id: CONVERSATION_ID, native: nativeFixture() })
    });
    await screen.findByText('build it');
    send(frame({ text: 'stale' }));
    expect(screen.queryByTestId('live-turn')).toBeNull();
  });
});

describe('the end of the turn', () => {
  it('swaps the bubble for the stored reply with no gap and no duplicate', async () => {
    const { send, state, apply } = setup();
    await screen.findByText('build it');
    const tools = [{ id: 'c1', name: 'Read', label: 'Reading main.go', state: 'done' as const }];
    send(
      frame({ seq: 2, text: 'All done.', tools, status: { kind: 'writing', label: 'Writing…' } })
    );
    await screen.findByTestId('live-turn');

    // The settle commits: the reply is stored, then the closing frame names it.
    state.mainThread = {
      ...state.mainThread,
      native: nativeFixture({
        turn_in_flight: false,
        deliveries: [deliveryFixture({ state: 'completed' })]
      })
    };
    // The closing frame can beat the re-read: the bubble waits for the reply and keeps showing
    // what it showed, so nothing blanks in between.
    send(frame({ seq: 3, active: false, reply_id: 'reply-1', text: 'All done.', tools }));
    expect(live()).toBeInTheDocument();
    expect(screen.getByTestId('live-turn-text')).toHaveTextContent('All done.');
    expect(screen.getByTestId('live-turn-tools')).toHaveTextContent('Reading main.go');

    state.messages = [
      ...state.messages,
      messageFixture({ id: 'reply-1', role: 'assistant', content: 'All done.' })
    ];
    act(() =>
      apply({ event: 'session:message', data: JSON.stringify({ session_id: MAIN_SESSION_ID }) })
    );
    await waitFor(() => expect(screen.queryByTestId('live-turn')).toBeNull());
    expect(screen.getAllByText('All done.')).toHaveLength(1);
  });

  it('keeps the last text when a closing frame carries none', async () => {
    const { send } = setup();
    await screen.findByText('build it');
    const tools = [{ id: 'c1', name: 'Read', label: 'Reading main.go', state: 'done' as const }];
    send(frame({ seq: 2, text: 'Nearly there.', tools }));
    await screen.findByTestId('live-turn');
    send(frame({ seq: 3, active: false, reply_id: 'reply-1', text: '', tools: [] }));
    expect(screen.getByTestId('live-turn-text')).toHaveTextContent('Nearly there.');
    expect(screen.getByTestId('live-turn-tools')).toHaveTextContent('Reading main.go');
  });

  it('drops the bubble at once when the turn stored no reply', async () => {
    const { send } = setup();
    await screen.findByText('build it');
    send(frame({ text: 'partial' }));
    await screen.findByTestId('live-turn');
    send(frame({ seq: 2, active: false }));
    expect(screen.queryByTestId('live-turn')).toBeNull();
  });

  it('does not bring the bubble back for a late frame of the finished turn', async () => {
    const { send } = setup();
    await screen.findByText('build it');
    send(frame({ seq: 1, text: 'partial' }));
    send(frame({ seq: 3, active: false }));
    send(frame({ seq: 2, text: 'late' }));
    expect(screen.queryByTestId('live-turn')).toBeNull();
  });

  it('starts a fresh bubble for the next turn', async () => {
    const { send } = setup();
    await screen.findByText('build it');
    send(frame({ seq: 5, text: 'first turn' }));
    send(frame({ seq: 6, active: false }));
    send(frame({ message_id: 'm2', seq: 1, text: 'second turn' }));
    expect(await screen.findByTestId('live-turn-text')).toHaveTextContent('second turn');
  });
});

describe('reconnecting mid-turn', () => {
  it('shows the current partial state from the snapshot the new stream sends', async () => {
    const { send, apply } = setup();
    await screen.findByText('build it');
    send(frame({ seq: 4, text: 'before the drop' }));
    await screen.findByTestId('live-turn');

    // A reconnect: the stream says connected, then sends the state as it stands.
    act(() => apply({ event: 'connected', data: JSON.stringify({ user_id: 'u' }) }));
    act(() => apply({ event: 'connected', data: JSON.stringify({ user_id: 'u' }) }));
    expect(screen.queryByTestId('live-turn')).toBeNull();
    send(frame({ seq: 1, text: 'before the drop and after' }));
    expect(await screen.findByTestId('live-turn-text')).toHaveTextContent(
      'before the drop and after'
    );
  });

  it('shows the partial text of a page opened mid-turn', async () => {
    const { send } = setup();
    // The snapshot frame may arrive before the thread has loaded.
    send(frame({ seq: 9, text: 'already well under way' }));
    expect(await screen.findByTestId('live-turn-text')).toHaveTextContent('already well under way');
  });

  it('drops the bubble of a turn that ended while the stream was down', async () => {
    const { send, apply } = setup();
    await screen.findByText('build it');
    send(frame({ text: 'partial' }));
    await screen.findByTestId('live-turn');
    act(() => apply({ event: 'connected', data: JSON.stringify({ user_id: 'u' }) }));
    act(() => apply({ event: 'connected', data: JSON.stringify({ user_id: 'u' }) }));
    expect(screen.queryByTestId('live-turn')).toBeNull();
  });
});

describe('elapsedSince', () => {
  const at = Date.parse('2026-10-11T12:10:00Z');
  it('counts from a UTC stamp with or without an offset', () => {
    expect(elapsedSince('2026-10-11T12:09:53', at)).toBe('7s');
    expect(elapsedSince('2026-10-11T12:09:53Z', at)).toBe('7s');
    expect(elapsedSince('2026-10-11T12:07:55.250000', at)).toBe('2m 04s');
    expect(elapsedSince('2026-10-11T10:55:00', at)).toBe('1h 15m');
  });
  it('never goes negative, and is empty for a bad stamp', () => {
    expect(elapsedSince('2026-10-11T12:10:30', at)).toBe('0s');
    expect(elapsedSince('soon', at)).toBe('');
  });
});

describe('the elapsed timer', () => {
  it('counts up each second without a new frame, and stops when the turn closes', () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval', 'Date'] });
    try {
      vi.setSystemTime(Date.parse('2026-10-11T12:00:10Z'));
      const f = frame({ started_at: '2026-10-11T12:00:00' });
      const { rerender } = render(<LiveTurn frame={f} />);
      expect(screen.getByTestId('live-turn-elapsed')).toHaveTextContent('10s');
      act(() => vi.advanceTimersByTime(3000));
      expect(screen.getByTestId('live-turn-elapsed')).toHaveTextContent('13s');
      rerender(<LiveTurn frame={{ ...f, active: false }} />);
      act(() => vi.advanceTimersByTime(5000));
      expect(screen.getByTestId('live-turn-elapsed')).toHaveTextContent('13s');
    } finally {
      vi.useRealTimers();
    }
  });
});
