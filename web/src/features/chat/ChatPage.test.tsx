import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { delay, http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { API_URL } from '@/api/config';
import { createSseApplier } from '@/api/invalidation';
import { keys } from '@/api';
import type { RefusalCode } from '@/api/errors';
import { server } from '@/test/msw/server';
import { SessionChatSlot } from '@/features/sessions/SessionChatSlot';
import { ChatPage } from './ChatPage';
import { loadDraft, saveDraft, sessionDraftKey } from './drafts';
import {
  CONVERSATION_ID,
  deliveryFixture,
  MAIN_SESSION_ID,
  mainThreadFixture,
  messageFixture,
  nativeFixture,
  providerFixture
} from './fixtures';
import { createChatBackend, type ChatBackendState } from './handlers';
import type { MainProviderInfo, NativeDeliveryInfo } from './types';
import { RECONCILE_MS } from './useMainProvider';
import { LOST_REPLY_RECORDED, POLL_MS, POLL_WINDOW_MS } from './useThread';

// The card has its own tests in features/hitl; these only check where the page renders it.
// The chat's reads would otherwise queue behind the app's read limiter. See fastReads.ts.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('@/features/tasks/test/fastReads');
  return withFastReads(await importOriginal());
});

vi.mock('@/features/hitl', () => ({
  HITLCard: ({ requestId }: { requestId: string }) => (
    <div data-testid="hitl-card">{requestId}</div>
  ),
  HITLRequest: () => null
}));

const connectionState = vi.hoisted(() => ({
  current: { status: 'online', offlineSince: null, recoveries: 0 } as {
    status: string;
    offlineSince: string | null;
    recoveries: number;
  }
}));
vi.mock('@/shell/ConnectionBanner', () => ({ useConnection: () => connectionState.current }));

beforeEach(() => {
  connectionState.current = { status: 'online', offlineSince: null, recoveries: 0 };
});
afterEach(() => sessionStorage.clear());

function setup(initial: Partial<ChatBackendState> = {}, props: { hitlId?: string } = {}) {
  const backend = createChatBackend(initial);
  server.use(...backend.handlers);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <ChatPage {...props} />
    </QueryClientProvider>
  );
  return { ...backend, queryClient, ...view };
}

/** A thread with one answered exchange. */
function existing(overrides: Partial<ChatBackendState> = {}): Partial<ChatBackendState> {
  return {
    mainThread: mainThreadFixture({
      conversation_id: CONVERSATION_ID,
      native: nativeFixture({ deliveries: [deliveryFixture({ message_id: 'm1' })] })
    }),
    messages: [
      messageFixture({ id: 'm1', content: 'build it' }),
      messageFixture({ id: 'm2', role: 'assistant', content: 'On it.' })
    ],
    ...overrides
  };
}

const input = () => screen.getByTestId('command-input') as HTMLTextAreaElement;

/** Adds a delivery to the main thread the backend serves, as the ledger does when it records one. */
function recordDelivery(state: ChatBackendState, delivery: NativeDeliveryInfo) {
  const native = state.mainThread.native ?? nativeFixture();
  state.mainThread = {
    ...state.mainThread,
    native: { ...native, deliveries: [...native.deliveries, delivery] }
  };
}

describe('loading', () => {
  it('shows connecting, then the empty thread', async () => {
    setup();
    expect(screen.getByText('$ connecting')).toBeInTheDocument();
    expect(await screen.findByText('$ mainloop --help')).toBeInTheDocument();
    expect(await screen.findByTestId('mt-state')).toHaveTextContent('ready');
  });

  it('shows the history and the header', async () => {
    setup(existing());
    expect(await screen.findByText('build it')).toBeInTheDocument();
    expect(screen.getByText('On it.')).toBeInTheDocument();
    expect(screen.getByTestId('mt-model')).toHaveTextContent('sonnet-5-5');
  });

  it('offers a reload when the thread cannot be read', async () => {
    server.use(
      http.get(`${API_URL}/main-thread`, () =>
        HttpResponse.json({ detail: 'down' }, { status: 500 })
      )
    );
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={queryClient}>
        <ChatPage />
      </QueryClientProvider>
    );
    expect(await screen.findByText('Could not load the main thread')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Try again' })).toBeInTheDocument();
    // Locked, but not stuck on "Loading…" next to the error.
    expect(input()).toBeDisabled();
    expect(input()).toHaveAttribute('placeholder', 'Thread did not load…');
  });
});

describe('sending', () => {
  it('shows the message at once, posts it with the conversation id, and clears the draft', async () => {
    const { state } = setup(existing());
    await screen.findByText('build it');
    saveDraft(CONVERSATION_ID, 'stale draft');
    await userEvent.type(input(), 'next step{Enter}');
    expect(await screen.findByText('next step')).toBeInTheDocument();
    await waitFor(() =>
      expect(state.chatPosts).toEqual([{ message: 'next step', conversation_id: CONVERSATION_ID }])
    );
    await waitFor(() => expect(loadDraft(CONVERSATION_ID)).toBe(''));
    expect(input().value).toBe('');
    // The recorded message replaces the optimistic one: it appears once.
    await waitFor(() => expect(screen.getAllByText('next step')).toHaveLength(1));
  });

  it('starts the conversation with a null id from an empty thread', async () => {
    const { state } = setup();
    await screen.findByText('$ mainloop --help');
    await userEvent.type(input(), 'hello{Enter}');
    await waitFor(() =>
      expect(state.chatPosts).toEqual([{ message: 'hello', conversation_id: null }])
    );
    expect(await screen.findByText('hello')).toBeInTheDocument();
  });

  it('removes the bubble, restores the text and explains a refused send', async () => {
    const { state } = setup(existing());
    await screen.findByText('build it');
    state.rejectNextChat = { status: 409, detail: 'A turn is already running.' };
    await userEvent.type(input(), 'too soon{Enter}');
    expect(await screen.findByTestId('send-error')).toHaveTextContent(
      'A turn is already running. Your message is back in the box.'
    );
    expect(input().value).toBe('too soon');
    expect(screen.queryByText('too soon', { selector: 'p' })).toBeNull();
    expect(loadDraft(CONVERSATION_ID)).toBe('too soon');
  });

  it('uses a generic message for other failures', async () => {
    const { state } = setup(existing());
    await screen.findByText('build it');
    state.rejectNextChat = { status: 500, detail: 'boom' };
    await userEvent.type(input(), 'x{Enter}');
    expect(await screen.findByTestId('send-error')).toHaveTextContent(
      'Could not send the message. Your message is back in the box.'
    );
  });

  it('does not roll back a message the backend accepted when the refresh fails', async () => {
    const { state } = setup(existing());
    await screen.findByText('build it');
    server.use(
      http.get(`${API_URL}/conversations/:id`, () =>
        HttpResponse.json({ detail: 'down' }, { status: 500 })
      )
    );
    await userEvent.type(input(), 'accepted{Enter}');
    await waitFor(() => expect(state.chatPosts).toHaveLength(1));
    expect(await screen.findByText('accepted')).toBeInTheDocument();
    expect(screen.queryByTestId('send-error')).toBeNull();
    expect(input().value).toBe('');
  });

  it('shows a message recorded before its reply was lost, and clears the draft', async () => {
    const { state } = setup(existing());
    await screen.findByText('build it');
    state.lostSend = 'recorded';
    await userEvent.type(input(), 'kept{Enter}');
    expect(await screen.findByTestId('send-error')).toHaveTextContent(LOST_REPLY_RECORDED);
    expect(await screen.findByText('kept')).toBeInTheDocument();
    expect(input().value).toBe('');
    expect(loadDraft(CONVERSATION_ID)).toBe('');
    expect(state.chatPosts).toHaveLength(1);
  });

  it('checks once more for a delivery recorded after the first lost-reply read', async () => {
    const { state } = setup(existing());
    await screen.findByText('build it');
    let recoveryReads = 0;
    server.use(
      http.post(`${API_URL}/chat`, async ({ request }) => {
        state.chatPosts.push((await request.json()) as ChatBackendState['chatPosts'][number]);
        return HttpResponse.error();
      }),
      http.get(`${API_URL}/main-thread`, () => {
        if (state.chatPosts.length > 0) {
          recoveryReads++;
          if (recoveryReads === 2) {
            recordDelivery(state, deliveryFixture({ message_id: 'late-1' }));
            state.messages.push(messageFixture({ id: 'late-1', content: 'late commit' }));
          }
        }
        return HttpResponse.json(state.mainThread);
      })
    );
    await userEvent.type(input(), 'late commit{Enter}');
    await waitFor(() => expect(recoveryReads).toBe(1));
    expect(input()).toBeDisabled();
    expect(await screen.findByTestId('send-error')).toHaveTextContent(LOST_REPLY_RECORDED);
    expect(await screen.findByText('late commit')).toBeInTheDocument();
    expect(input().value).toBe('');
    expect(loadDraft(CONVERSATION_ID)).toBe('');
    expect(state.chatPosts).toHaveLength(1);
  });

  it('gives the text back when a lost reply shows nothing was recorded', async () => {
    const { state } = setup(existing());
    await screen.findByText('build it');
    const reads = state.mainThreadReads;
    state.lostSend = 'unrecorded';
    await userEvent.type(input(), 'lost{Enter}');
    expect(await screen.findByTestId('send-error')).toHaveTextContent(
      "Can't reach the Mainloop backend. Your message is back in the box."
    );
    expect(input().value).toBe('lost');
    expect(loadDraft(CONVERSATION_ID)).toBe('lost');
    expect(state.chatPosts).toHaveLength(1);
    // One baseline and exactly two recovery reads; the message is never automatically resent.
    expect(state.mainThreadReads - reads).toBe(3);
  });

  it('does not count a report that lands in a lost reply window as this message', async () => {
    const { state } = setup(existing());
    await screen.findByText('build it');
    server.use(
      http.post(`${API_URL}/chat`, () => {
        // A report is recorded while the reply is lost; the owner's message never reached the ledger.
        recordDelivery(state, deliveryFixture({ message_id: 'report-1', source: 'report' }));
        return HttpResponse.error();
      })
    );
    await userEvent.type(input(), 'lost{Enter}');
    expect(await screen.findByTestId('send-error')).toHaveTextContent(
      "Can't reach the Mainloop backend. Your message is back in the box."
    );
    expect(screen.queryByText(LOST_REPLY_RECORDED)).toBeNull();
    expect(input().value).toBe('lost');
    expect(loadDraft(CONVERSATION_ID)).toBe('lost');
  });

  it("does not count another client's user message that lands in a lost reply window", async () => {
    const { state } = setup(existing());
    await screen.findByText('build it');
    server.use(
      http.post(`${API_URL}/chat`, async ({ request }) => {
        // The override stands in for the default route, so it records the post as that route does.
        state.chatPosts = [
          ...state.chatPosts,
          (await request.json()) as ChatBackendState['chatPosts'][number]
        ];
        // Another client's message is recorded while the owner's reply is lost. It is a new user
        // delivery, but it is not the owner's text, so the owner's text must stay recoverable.
        state.messages = [
          ...state.messages,
          messageFixture({ id: 'other-1', content: 'from the laptop' })
        ];
        recordDelivery(state, deliveryFixture({ message_id: 'other-1' }));
        return HttpResponse.error();
      })
    );
    await userEvent.type(input(), 'lost{Enter}');
    expect(await screen.findByTestId('send-error')).toHaveTextContent(
      "Can't reach the Mainloop backend. Your message is back in the box. Mainloop could not confirm whether it was recorded."
    );
    expect(screen.queryByText(LOST_REPLY_RECORDED)).toBeNull();
    expect(input().value).toBe('lost');
    expect(loadDraft(CONVERSATION_ID)).toBe('lost');
    expect(state.chatPosts).toHaveLength(1);
  });

  it('does not count an earlier message with the same text as this send', async () => {
    const base = existing();
    const { state } = setup({
      ...base,
      messages: [messageFixture({ id: 'old-1', content: 'lost' }), ...(base.messages ?? [])]
    });
    await screen.findByText('build it');
    state.lostSend = 'unrecorded';
    await userEvent.type(input(), 'lost{Enter}');
    expect(await screen.findByTestId('send-error')).toHaveTextContent(
      "Can't reach the Mainloop backend. Your message is back in the box."
    );
    expect(screen.queryByText(LOST_REPLY_RECORDED)).toBeNull();
    expect(input().value).toBe('lost');
    expect(loadDraft(CONVERSATION_ID)).toBe('lost');
  });

  it('says so when the conversation cannot be read after a lost reply that was recorded', async () => {
    const { state } = setup(existing());
    await screen.findByText('build it');
    state.lostSend = 'recorded';
    server.use(
      http.get(`${API_URL}/conversations/:id`, () =>
        HttpResponse.json({ detail: 'down' }, { status: 500 })
      )
    );
    await userEvent.type(input(), 'kept{Enter}');
    expect(await screen.findByTestId('send-error')).toHaveTextContent(
      'Mainloop could not confirm whether it was recorded. Check the conversation before you send it again.'
    );
    expect(input().value).toBe('kept');
    expect(loadDraft(CONVERSATION_ID)).toBe('kept');
    expect(state.chatPosts).toHaveLength(1);
  });

  it('takes the baseline from the ledger just before the send, not from the last render', async () => {
    const { state } = setup(existing());
    await screen.findByText('build it');
    // Recorded after the screen last read the thread, and before this send: it is not this send's.
    recordDelivery(state, deliveryFixture({ message_id: 'early-1' }));
    state.lostSend = 'unrecorded';
    await userEvent.type(input(), 'lost{Enter}');
    expect(await screen.findByTestId('send-error')).toHaveTextContent(
      "Can't reach the Mainloop backend. Your message is back in the box."
    );
    expect(screen.queryByText(LOST_REPLY_RECORDED)).toBeNull();
    expect(input().value).toBe('lost');
  });

  it('does not send when the thread cannot be read first', async () => {
    const { state } = setup(existing());
    await screen.findByText('build it');
    server.use(
      http.get(`${API_URL}/main-thread`, () =>
        HttpResponse.json({ detail: 'down' }, { status: 500 })
      )
    );
    await userEvent.type(input(), 'held{Enter}');
    expect(await screen.findByTestId('send-error')).toHaveTextContent(
      'Could not check the conversation before sending. Your message is back in the box.'
    );
    expect(state.chatPosts).toHaveLength(0);
    expect(input().value).toBe('held');
    expect(loadDraft(CONVERSATION_ID)).toBe('held');
  });

  it('locks the composer while a turn is in flight', async () => {
    setup(
      existing({
        mainThread: mainThreadFixture({
          conversation_id: CONVERSATION_ID,
          native: nativeFixture({ turn_in_flight: true })
        })
      })
    );
    await waitFor(() => expect(input()).toHaveAttribute('placeholder', 'Working…'));
    expect(input()).toBeDisabled();
    expect(screen.getByTestId('mt-state')).toHaveTextContent('ready');
  });

  it('locks and says so while the backend is unreachable', async () => {
    connectionState.current = {
      status: 'offline',
      offlineSince: '2026-01-01T00:00:00Z',
      recoveries: 0
    };
    setup(existing());
    await screen.findByText('build it');
    expect(input()).toBeDisabled();
    expect(input()).toHaveAttribute('placeholder', 'Backend unreachable…');
    expect(screen.getByTestId('mt-state')).toHaveTextContent('unreachable');
  });

  it('keeps the draft when the owner leaves and comes back', async () => {
    const first = setup(existing());
    await screen.findByText('build it');
    await userEvent.type(input(), 'half a thought');
    first.unmount();
    setup(existing());
    await screen.findByText('build it');
    await waitFor(() => expect(input().value).toBe('half a thought'));
  });
});

describe('the native /chat answer', () => {
  it('locks the composer after a send and polls until the turn ends', async () => {
    const { state, endTurn } = setup(existing());
    await screen.findByText('build it');
    await userEvent.type(input(), 'next step{Enter}');
    // No message body came back, yet the bubble stays and the composer is locked.
    expect(await screen.findByText('next step')).toBeInTheDocument();
    await waitFor(() => expect(input()).toHaveAttribute('placeholder', 'Working…'));
    expect(input()).toBeDisabled();
    expect(screen.getAllByText('next step')).toHaveLength(1);

    // Started by the send alone: the backend finishes the turn and no SSE event arrives.
    const reads = state.mainThreadReads;
    endTurn('All done.');
    await waitFor(() => expect(input()).toBeEnabled(), { timeout: 5000 });
    expect(state.mainThreadReads).toBeGreaterThan(reads);
    expect(await screen.findByText('All done.')).toBeInTheDocument();
    expect(screen.getAllByText('next step')).toHaveLength(1);
  });

  it('keeps the accepted message on screen when the refresh fails', async () => {
    const { state } = setup(existing());
    await screen.findByText('build it');
    // The read before the send succeeds; only a refresh after the send is recorded fails.
    server.use(
      http.get(`${API_URL}/conversations/:id`, () =>
        HttpResponse.json({ detail: 'down' }, { status: 500 })
      ),
      http.get(`${API_URL}/main-thread`, () =>
        state.chatPosts.length === 0
          ? HttpResponse.json(state.mainThread)
          : HttpResponse.json({ detail: 'down' }, { status: 500 })
      )
    );
    await userEvent.type(input(), 'accepted{Enter}');
    await waitFor(() => expect(state.chatPosts).toHaveLength(1));
    await waitFor(() => expect(input().value).toBe(''));
    expect(screen.getByText('accepted')).toBeInTheDocument();
    expect(screen.queryByTestId('send-error')).toBeNull();
    // The cached thread is still known: the composer is not left on "Loading…".
    expect(input()).not.toHaveAttribute('placeholder', 'Loading…');
    expect(screen.queryByText('Could not load the main thread')).toBeNull();
  });

  it('keeps the composer usable when a background read fails and the thread is cached', async () => {
    const { queryClient } = setup(existing());
    await screen.findByText('build it');
    server.use(
      http.get(`${API_URL}/main-thread`, () =>
        HttpResponse.json({ detail: 'down' }, { status: 500 })
      ),
      http.get(`${API_URL}/conversations/:id`, () =>
        HttpResponse.json({ detail: 'down' }, { status: 500 })
      )
    );
    await queryClient.invalidateQueries();
    expect(
      await screen.findByText('Could not refresh the conversation. Showing the last messages.')
    ).toBeInTheDocument();
    expect(input()).toBeEnabled();
    expect(input()).toHaveAttribute('placeholder', 'Enter command...');
    expect(screen.getByText('build it')).toBeInTheDocument();
    expect(screen.queryByText('Could not load the conversation')).toBeNull();
  });

  it('starts a new window when another turn begins after the old one expired', async () => {
    const { state, queryClient } = setup(
      existing({
        mainThread: mainThreadFixture({
          conversation_id: CONVERSATION_ID,
          native: nativeFixture({
            turn_in_flight: true,
            deliveries: [deliveryFixture({ message_id: 'm1', state: 'recorded' })]
          })
        })
      })
    );
    await waitFor(() => expect(input()).toHaveAttribute('placeholder', 'Working…'));
    const start = Date.now();
    const now = vi.spyOn(Date, 'now').mockReturnValue(start + POLL_WINDOW_MS + 1);
    try {
      await delay(POLL_MS * 2 + 500);
      const settled = state.mainThreadReads;
      await delay(POLL_MS + 500);
      expect(state.mainThreadReads).toBe(settled);
      // A new turn (another message id) arrives and one read sees it: polling is back.
      state.mainThread = mainThreadFixture({
        conversation_id: CONVERSATION_ID,
        native: nativeFixture({
          turn_in_flight: true,
          deliveries: [deliveryFixture({ message_id: 'm9', state: 'recorded' })]
        })
      });
      await queryClient.invalidateQueries();
      const resumed = state.mainThreadReads;
      await delay(POLL_MS * 2 + 500);
      expect(state.mainThreadReads).toBeGreaterThan(resumed);
    } finally {
      now.mockRestore();
    }
  }, 20000);

  it('does not bring a sent message back when the conversation changes', async () => {
    const { state, queryClient, endTurn } = setup(existing());
    await screen.findByText('build it');
    await userEvent.type(input(), 'only once{Enter}');
    await waitFor(() => expect(state.chatPosts).toHaveLength(1));
    endTurn();
    await queryClient.invalidateQueries();
    await waitFor(() => expect(screen.getAllByText('only once')).toHaveLength(1));
    // The main thread moves to a new conversation that does not list the message.
    state.messages = [];
    state.mainThread = mainThreadFixture({ conversation_id: 'conv-2' });
    await queryClient.invalidateQueries();
    await waitFor(() => expect(screen.queryByText('only once')).toBeNull());
    expect(screen.queryByText('build it')).toBeNull();
  });

  it('stops polling six minutes after the turn was first seen in flight', async () => {
    const { state } = setup(
      existing({
        mainThread: mainThreadFixture({
          conversation_id: CONVERSATION_ID,
          native: nativeFixture({ turn_in_flight: true })
        })
      })
    );
    await waitFor(() => expect(input()).toHaveAttribute('placeholder', 'Working…'));
    const start = Date.now();
    const now = vi.spyOn(Date, 'now').mockReturnValue(start + POLL_WINDOW_MS + 1);
    try {
      // One poll may already be queued; after that the window has closed.
      await delay(POLL_MS * 2 + 500);
      const settled = state.mainThreadReads;
      await delay(POLL_MS * 2 + 500);
      expect(state.mainThreadReads).toBe(settled);
    } finally {
      now.mockRestore();
    }
  }, 15000);
});

describe('loading the conversation', () => {
  it('locks the composer until the thread is read', async () => {
    server.use(
      http.get(`${API_URL}/main-thread`, async () => {
        await delay(150);
        return HttpResponse.json(mainThreadFixture());
      })
    );
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={queryClient}>
        <ChatPage />
      </QueryClientProvider>
    );
    expect(input()).toBeDisabled();
    expect(input()).toHaveAttribute('placeholder', 'Loading…');
    await waitFor(() => expect(input()).toBeEnabled());
  });

  it('is not an empty thread while the conversation is still loading', async () => {
    setup(existing());
    server.use(
      http.get(`${API_URL}/conversations/:id`, async () => {
        await delay(300);
        return HttpResponse.json({ detail: 'down' }, { status: 500 });
      })
    );
    await screen.findByTestId('mt-state');
    expect(screen.queryByText('$ mainloop --help')).toBeNull();
    expect(screen.getByText('$ connecting')).toBeInTheDocument();
  });

  it('says so when the conversation cannot be read, even with no messages', async () => {
    setup(existing());
    server.use(
      http.get(`${API_URL}/conversations/:id`, () =>
        HttpResponse.json({ detail: 'down' }, { status: 500 })
      )
    );
    expect(await screen.findByText('Could not load the conversation')).toBeInTheDocument();
    expect(screen.queryByText('$ mainloop --help')).toBeNull();
    expect(screen.getByRole('button', { name: 'Try again' })).toBeInTheDocument();
  });

  it('leaves the composer usable when the conversation cannot be read', async () => {
    setup(existing());
    server.use(
      http.get(`${API_URL}/conversations/:id`, () =>
        HttpResponse.json({ detail: 'down' }, { status: 500 })
      )
    );
    expect(await screen.findByText('Could not load the conversation')).toBeInTheDocument();
    expect(input()).toBeEnabled();
    await userEvent.type(input(), 'still here');
    expect(input()).toHaveValue('still here');
  });
});

describe('delivery notices and retry', () => {
  const failedThread = (detail = 'not sent: agent offline') =>
    existing({
      mainThread: mainThreadFixture({
        conversation_id: CONVERSATION_ID,
        native: nativeFixture({
          deliveries: [deliveryFixture({ message_id: 'm1', state: 'failed', detail })]
        })
      }),
      messages: [messageFixture({ id: 'm1', content: 'build it' })]
    });

  it('shows the failure on the message and the header', async () => {
    setup(failedThread());
    const notice = await screen.findByTestId('delivery-notice');
    expect(within(notice).getByTestId('delivery-notice-label')).toHaveTextContent('Not sent');
    expect(within(notice).getByTestId('delivery-notice-reason')).toHaveTextContent('agent offline');
    expect(screen.getByTestId('mt-state')).toHaveTextContent('last message failed');
  });

  // Replaces the frozen messageSend test that read Chat.svelte's source: Retry belongs to the
  // displayed main conversation, and never goes anywhere else.
  it('Retry sends the same text once, to the owning conversation only', async () => {
    const { state } = setup(failedThread());
    await userEvent.click(await screen.findByTestId('delivery-retry'));
    await waitFor(() => expect(state.chatPosts).toHaveLength(1));
    expect(state.chatPosts[0]).toEqual({ message: 'build it', conversation_id: CONVERSATION_ID });
    expect(await screen.findAllByText('build it')).toHaveLength(2);
  });

  it('Retry leaves the text in the box and its stored draft alone', async () => {
    const { state } = setup(failedThread());
    await screen.findByTestId('delivery-retry');
    await userEvent.type(input(), 'something else');
    await userEvent.click(screen.getByTestId('delivery-retry'));
    await waitFor(() => expect(state.chatPosts).toHaveLength(1));
    await waitFor(() => expect(input()).toHaveAttribute('placeholder', 'Working…'));
    expect(loadDraft(CONVERSATION_ID)).toBe('something else');
    expect(input().value).toBe('something else');
  });

  it('Retry does nothing when the thread moved on since the page loaded', async () => {
    const { state } = setup(failedThread());
    const button = await screen.findByTestId('delivery-retry');
    // The backend now reports a turn in flight (or a newer delivery): the fresh read blocks it.
    state.mainThread = mainThreadFixture({
      conversation_id: CONVERSATION_ID,
      native: nativeFixture({
        turn_in_flight: true,
        deliveries: [deliveryFixture({ message_id: 'm1', state: 'sending' })]
      })
    });
    await userEvent.click(button);
    // The refresh after the refused retry brings the screen up to date: no failure, no Retry.
    await waitFor(() => expect(screen.queryByTestId('delivery-retry')).toBeNull());
    expect(state.chatPosts).toHaveLength(0);
  });

  it('never offers Retry for an unconfirmed delivery', async () => {
    setup(
      existing({
        mainThread: mainThreadFixture({
          conversation_id: CONVERSATION_ID,
          native: nativeFixture({
            deliveries: [
              deliveryFixture({ message_id: 'm1', state: 'uncertain', detail: 'timeout' })
            ]
          })
        }),
        messages: [messageFixture({ id: 'm1', content: 'build it' })]
      })
    );
    const notice = await screen.findByTestId('delivery-notice');
    expect(notice).toHaveTextContent('Delivery unconfirmed');
    expect(notice).toHaveTextContent('Not replayed. Check for a reply before sending again.');
    expect(screen.queryByTestId('delivery-retry')).toBeNull();
    expect(screen.getByTestId('mt-state')).toHaveTextContent('last message unconfirmed');
  });
});

describe('stopping a turn', () => {
  it('posts stop-turn for the main session', async () => {
    const { state } = setup(
      existing({
        mainThread: mainThreadFixture({
          conversation_id: CONVERSATION_ID,
          native: nativeFixture({ turn_in_flight: true })
        })
      })
    );
    await userEvent.click(await screen.findByTestId('mt-stop-turn'));
    await waitFor(() => expect(state.stopped).toEqual([MAIN_SESSION_ID]));
  });
});

describe('staying current', () => {
  it('shows a new reply when session:message arrives', async () => {
    const { state, queryClient } = setup(existing());
    await screen.findByText('On it.');
    state.messages = [
      ...state.messages,
      messageFixture({ id: 'm3', role: 'assistant', content: 'Finished the build.' })
    ];
    createSseApplier(queryClient)({
      event: 'session:message',
      data: JSON.stringify({ session_id: MAIN_SESSION_ID })
    });
    expect(await screen.findByText('Finished the build.')).toBeInTheDocument();
  });

  it('polls while a turn is in flight and unlocks when it ends', async () => {
    const { state } = setup(
      existing({
        mainThread: mainThreadFixture({
          conversation_id: CONVERSATION_ID,
          native: nativeFixture({ turn_in_flight: true })
        })
      })
    );
    await waitFor(() => expect(input()).toBeDisabled());
    state.mainThread = mainThreadFixture({
      conversation_id: CONVERSATION_ID,
      native: nativeFixture({ turn_in_flight: false })
    });
    state.messages = [
      ...state.messages,
      messageFixture({ id: 'm3', role: 'assistant', content: 'Done.' })
    ];
    // No SSE event: the poll alone picks it up.
    await waitFor(() => expect(input()).toBeEnabled(), { timeout: 5000 });
    // The turn ending also re-reads the conversation, so the last reply is not missed.
    expect(await screen.findByText('Done.')).toBeInTheDocument();
  });
});

describe('inline requests', () => {
  it('shows the ?hitl= card at the top', async () => {
    setup(existing(), { hitlId: 'req-1' });
    expect(await screen.findByTestId('hitl-card')).toHaveTextContent('req-1');
    const root = screen.getByTestId('main-thread');
    expect(root.querySelector('[data-testid="hitl-card"]')).toBeTruthy();
  });

  it('shows the main session’s pending requests once each', async () => {
    setup(existing({ hitlIds: ['req-2', 'req-2', 'req-3'] }), { hitlId: 'req-3' });
    await waitFor(() => expect(screen.getAllByTestId('hitl-card')).toHaveLength(2));
    expect(screen.getAllByTestId('hitl-card').map((card) => card.textContent)).toEqual([
      'req-3',
      'req-2'
    ]);
  });

  it('says so when the requests cannot be refreshed', async () => {
    setup(existing());
    await screen.findByText('build it');
    server.use(
      http.get(`${API_URL}/sessions/:id/hitl`, () =>
        HttpResponse.json({ detail: 'down' }, { status: 500 })
      )
    );
    expect(
      await screen.findByText(
        'Could not refresh session requests. Retrying…',
        {},
        { timeout: 5000 }
      )
    ).toBeInTheDocument();
  });
});

/** A main thread with a conversation and nothing in flight, so the provider select is open. */
function idleThread(provider: MainProviderInfo | null = providerFixture()): Partial<ChatBackendState> {
  return {
    mainThread: mainThreadFixture({
      conversation_id: CONVERSATION_ID,
      provider,
      native: nativeFixture()
    }),
    messages: [messageFixture({ id: 'm1', content: 'build it' })]
  };
}

const providerSelect = () => screen.getByTestId('mt-provider-select');
const pause = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

describe('the main provider', () => {
  it('sends one explicit selection and shows the provider once it is confirmed', async () => {
    const { state } = setup(idleThread());
    await screen.findByText('build it');
    await userEvent.selectOptions(providerSelect(), 'codex');
    await waitFor(() =>
      expect(screen.getByTestId('mt-provider-effective')).toHaveTextContent('effective: Codex')
    );
    expect(screen.getByTestId('mt-provider-status')).toHaveTextContent('ready');
    expect(state.providerPosts).toEqual([{ provider: 'codex', restart_failed: false }]);
    expect(input()).toBeEnabled();
  });

  it('sends nothing when the provider that is already ready is chosen', async () => {
    const { state } = setup(idleThread());
    await screen.findByText('build it');
    await userEvent.selectOptions(providerSelect(), 'claude');
    await pause(20);
    expect(state.providerPosts).toHaveLength(0);
  });

  it('holds the selection and the main input while a turn is open', async () => {
    setup({
      mainThread: mainThreadFixture({
        conversation_id: CONVERSATION_ID,
        native: nativeFixture({
          turn_in_flight: true,
          deliveries: [deliveryFixture({ message_id: 'm1', state: 'sending' })]
        })
      }),
      messages: [messageFixture({ id: 'm1', content: 'build it' })]
    });
    await screen.findByText('build it');
    expect(providerSelect()).toBeDisabled();
    expect(input()).toBeDisabled();
  });

  it('locks the selection and the main input while the provider is preparing', async () => {
    setup(idleThread(providerFixture({ desired_provider: 'codex', effective_provider: 'claude', status: 'pending' })));
    await screen.findByText('build it');
    expect(providerSelect()).toBeDisabled();
    expect(input()).toBeDisabled();
    expect(input()).toHaveAttribute('placeholder', 'Main input is locked');
    expect(screen.getByText('Main provider is preparing.')).toBeInTheDocument();
  });

  it('locks the main input when the provider state is unknown', async () => {
    setup(idleThread(null));
    await screen.findByText('build it');
    expect(providerSelect()).toBeDisabled();
    expect(input()).toBeDisabled();
    expect(screen.getByTestId('mt-provider-status')).toHaveTextContent('state unknown');
  });

  it('lets the first message go out while the provider has not started', async () => {
    const { state } = setup(
      idleThread(providerFixture({ status: 'not_started', effective_provider: null }))
    );
    await screen.findByText('build it');
    expect(input()).toBeEnabled();
    await userEvent.type(input(), 'first{Enter}');
    await waitFor(() => expect(state.chatPosts).toHaveLength(1));
  });

  it('holds the selection while the backend is unreachable', async () => {
    connectionState.current = { status: 'offline', offlineSince: '2026-01-01T00:00:00Z', recoveries: 0 };
    setup(idleThread());
    await screen.findByText('build it');
    expect(providerSelect()).toBeDisabled();
    expect(screen.getByTestId('mt-provider-status')).toHaveTextContent('offline, selection unavailable');
  });

  it('shows a 409 and the refreshed state, and offers no retry', async () => {
    const { state } = setup(idleThread(), {});
    state.rejectNextProvider = { status: 409, detail: 'Main provider is busy.' };
    await screen.findByText('build it');
    await userEvent.selectOptions(providerSelect(), 'codex');
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Provider change blocked (409): Main provider is busy.'
    );
    expect(state.providerPosts).toHaveLength(1);
    expect(screen.getByTestId('mt-provider-effective')).toHaveTextContent('effective: Claude');
    expect(screen.queryByTestId('mt-provider-retry')).toBeNull();
  });

  it('does not repeat a write that failed on the network, and Retry repeats the same write', async () => {
    const { state } = setup(idleThread());
    state.lostProvider = 'unrecorded';
    await screen.findByText('build it');
    await userEvent.selectOptions(providerSelect(), 'codex');
    expect(await screen.findByRole('alert')).toHaveTextContent('Provider selection was not confirmed');
    // The read after the failed write shows the old provider, so Retry is offered.
    expect(screen.getByTestId('mt-provider-effective')).toHaveTextContent('effective: Claude');
    await pause(2 * RECONCILE_MS);
    expect(state.providerPosts).toHaveLength(1);

    await userEvent.click(screen.getByTestId('mt-provider-retry'));
    await waitFor(() =>
      expect(screen.getByTestId('mt-provider-effective')).toHaveTextContent('effective: Codex')
    );
    expect(state.providerPosts).toEqual([
      { provider: 'codex', restart_failed: false },
      { provider: 'codex', restart_failed: false }
    ]);
    expect(screen.queryByTestId('mt-provider-retry')).toBeNull();
  });

  it('keeps the page locked until a read succeeds, without repeating the write', async () => {
    const { state } = setup(idleThread());
    state.failProviderReads = 1;
    await screen.findByText('build it');
    await userEvent.selectOptions(providerSelect(), 'codex');
    expect(await screen.findByText(/Could not confirm provider state/)).toBeInTheDocument();
    expect(providerSelect()).toBeDisabled();
    expect(input()).toBeDisabled();
    expect(screen.getByTestId('mt-provider-status')).toHaveTextContent('provider state unconfirmed');

    await waitFor(() => expect(providerSelect()).toBeEnabled(), { timeout: 3000 });
    expect(input()).toBeEnabled();
    expect(screen.getByTestId('mt-provider-effective')).toHaveTextContent('effective: Codex');
    expect(state.providerPosts).toHaveLength(1);
  });

  it('shows the bounded error of a failed start, and offers Retry without Restart when it is not recoverable', async () => {
    setup(idleThread(providerFixture({ status: 'failed', effective_provider: null, error: 'Codex did not start.' })));
    await screen.findByText('build it');
    expect(screen.getByTestId('mt-provider-error')).toHaveTextContent('Codex did not start.');
    expect(screen.getByTestId('mt-provider-retry')).toBeInTheDocument();
    expect(screen.queryByTestId('mt-provider-restart')).toBeNull();
    expect(input()).toBeDisabled();
  });

  it('offers Restart for a recoverable failure, which sends restart_failed', async () => {
    const { state } = setup(
      idleThread(providerFixture({ status: 'failed', effective_provider: null, error: 'Codex did not start.', recoverable: true }))
    );
    await screen.findByText('build it');
    await userEvent.click(screen.getByTestId('mt-provider-restart'));
    await waitFor(() => expect(input()).toBeEnabled());
    expect(state.providerPosts).toEqual([{ provider: 'claude', restart_failed: true }]);
    expect(screen.getByTestId('mt-provider-status')).toHaveTextContent('ready');
  });

  it('does not let an older read overwrite a newer selection', async () => {
    const { queryClient, state } = setup(idleThread());
    await screen.findByText('build it');
    const stale = state.mainThread;
    // A read of the thread that started before the selection, and answers after it.
    server.use(
      http.get(`${API_URL}/main-thread`, async () => {
        await delay(300);
        return HttpResponse.json(stale);
      })
    );
    void queryClient.invalidateQueries({ queryKey: keys.mainThread() });
    await pause(20);
    await userEvent.selectOptions(providerSelect(), 'codex');
    await waitFor(() =>
      expect(screen.getByTestId('mt-provider-effective')).toHaveTextContent('effective: Codex')
    );
    await pause(400);
    expect(screen.getByTestId('mt-provider-effective')).toHaveTextContent('effective: Codex');
    expect(screen.getByTestId('mt-provider-select')).toHaveValue('codex');
  });
});

describe('the main lock while a send, a stop or a load is in flight', () => {
  it('locks the selection and the main input while a send is in flight', async () => {
    const { state, endTurn, hold } = setup(idleThread());
    await screen.findByText('build it');
    const release = hold('chat');
    await userEvent.type(input(), 'next step{Enter}');
    await waitFor(() => expect(state.chatPosts).toHaveLength(1));
    expect(providerSelect()).toBeDisabled();
    expect(input()).toBeDisabled();

    release();
    // The send was recorded, so the turn runs and the lock holds until it ends.
    await waitFor(() => expect(input()).toHaveAttribute('placeholder', 'Working…'));
    expect(providerSelect()).toBeDisabled();
    endTurn('done');
    await waitFor(() => expect(input()).toBeEnabled(), { timeout: 5000 });
    expect(providerSelect()).toBeEnabled();
  });

  it('locks the selection and the main input while a stop is in flight', async () => {
    // The turn is in flight, so the lock also comes from the turn rule. The held stop is what
    // shows the lock holding while the stop itself is pending.
    const { state, endTurn, hold } = setup(
      existing({
        mainThread: mainThreadFixture({
          conversation_id: CONVERSATION_ID,
          native: nativeFixture({ turn_in_flight: true })
        })
      })
    );
    await screen.findByText('build it');
    const release = hold('stop-turn');
    await userEvent.click(await screen.findByTestId('mt-stop-turn'));
    await waitFor(() => expect(state.stopped).toEqual([MAIN_SESSION_ID]));
    expect(screen.getByTestId('mt-stop-turn')).toHaveTextContent('stopping…');
    expect(providerSelect()).toBeDisabled();
    expect(input()).toBeDisabled();

    release();
    await waitFor(() =>
      expect(screen.getByTestId('mt-stop-turn')).not.toHaveTextContent('stopping…')
    );
    expect(providerSelect()).toBeDisabled();
    endTurn();
    // Polling notices the turn has ended within one poll interval.
    await waitFor(() => expect(providerSelect()).toBeEnabled(), { timeout: 5000 });
    expect(input()).toBeEnabled();
  });

  it('locks the selection and the main input until the conversation is read', async () => {
    const { hold } = setup(idleThread());
    const release = hold('conversation');
    expect(await screen.findByTestId('mt-provider-select')).toBeDisabled();
    expect(input()).toBeDisabled();

    release();
    await screen.findByText('build it');
    await waitFor(() => expect(providerSelect()).toBeEnabled());
    expect(input()).toBeEnabled();
  });
});

describe('the main lock does not reach drafts or a child session', () => {
  const CHILD_SESSION_ID = 'child-session';

  it('keeps the main draft, and lets a child session take input and send, while the main is locked', async () => {
    const backend = createChatBackend(
      idleThread(
        providerFixture({ desired_provider: 'codex', effective_provider: 'claude', status: 'pending' })
      )
    );
    server.use(...backend.handlers);
    saveDraft(CONVERSATION_ID, 'main draft');
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={queryClient}>
        <ChatPage />
        <SessionChatSlot sessionId={CHILD_SESSION_ID} />
      </QueryClientProvider>
    );
    await screen.findByText('build it');
    const main = within(screen.getByTestId('main-thread'));
    const child = within(await screen.findByTestId('session-chat-slot'));

    expect(main.getByTestId('mt-provider-select')).toBeDisabled();
    expect(main.getByTestId('command-input')).toBeDisabled();
    expect(main.getByTestId('command-input')).toHaveValue('main draft');
    expect(loadDraft(CONVERSATION_ID)).toBe('main draft');

    await waitFor(() => expect(child.getByTestId('command-input')).toBeEnabled());
    await userEvent.type(child.getByTestId('command-input'), 'child note');
    await waitFor(() => expect(loadDraft(sessionDraftKey(CHILD_SESSION_ID))).toBe('child note'));
    await userEvent.type(child.getByTestId('command-input'), '{Enter}');
    await waitFor(() =>
      expect(backend.state.sessionPosts[CHILD_SESSION_ID]?.map((post) => post.message)).toEqual([
        'child note'
      ])
    );
    // Sending to the child changes neither the main lock nor the main draft.
    expect(main.getByTestId('command-input')).toBeDisabled();
    expect(loadDraft(CONVERSATION_ID)).toBe('main draft');
  });
});

// The backend answers a refusal with `{detail, code}`. These go through the real client, so the
// code the page reads is the one the middleware kept from the response body.
describe('refusal codes from the backend', () => {
  it.each<[RefusalCode, string]>([
    ['busy', 'Provider change refused: the main has open work or changed while the request ran.'],
    ['pending', 'Provider change refused: a switch is already pending and must settle or be recoverable first.'],
    ['unconfirmed', 'Provider change refused: kagent could not confirm the main is idle.'],
    ['unresolved', 'Provider change refused: a native create for the main is still unresolved.'],
    ['agent_unavailable', "Codex isn't installed here. Nothing changed."]
  ])('a switch refused as %s shows its notice, not the server detail', async (code, text) => {
    const { state } = setup(idleThread());
    state.rejectNextProvider = { status: 409, detail: 'Raw server prose.', code };
    await screen.findByText('build it');
    await userEvent.selectOptions(providerSelect(), 'codex');
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(`${text} State is refreshed; no automatic retry.`);
    expect(alert).not.toHaveTextContent('Raw server prose');
    expect(state.providerPosts).toHaveLength(1);
  });

  it('a switch refused with a code the contract does not name shows the server detail', async () => {
    const { state } = setup(idleThread());
    state.rejectNextProvider = {
      status: 409,
      detail: 'Main provider is busy.',
      code: 'mystery' as RefusalCode
    };
    await screen.findByText('build it');
    await userEvent.selectOptions(providerSelect(), 'codex');
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Provider change blocked (409): Main provider is busy. State is refreshed; no automatic retry.'
    );
  });

  it.each<[RefusalCode, string]>([
    ['busy', 'Message not sent: the main has open work or changed while the request ran.'],
    ['pending', 'Message not sent: a provider switch is pending.'],
    ['unconfirmed', 'Message not sent: kagent could not confirm the main is idle.'],
    ['unresolved', 'Message not sent: a native create for the main is unresolved.'],
    ['agent_unavailable', "Message not sent: the other provider isn't installed here. Nothing changed."]
  ])('a message refused as %s shows its notice, and the box keeps the text', async (code, text) => {
    const { state } = setup(existing());
    await screen.findByText('build it');
    state.rejectNextChat = { status: 409, detail: 'Raw server prose.', code };
    await userEvent.type(input(), 'too soon{Enter}');
    const error = await screen.findByTestId('send-error');
    expect(error).toHaveTextContent(`${text} Your message is back in the box.`);
    expect(error).not.toHaveTextContent('Raw server prose');
    expect(input().value).toBe('too soon');
  });

  it('a message refused with a code the contract does not name shows the server detail', async () => {
    const { state } = setup(existing());
    await screen.findByText('build it');
    state.rejectNextChat = { status: 409, detail: 'Raw server prose.', code: 'mystery' as RefusalCode };
    await userEvent.type(input(), 'too soon{Enter}');
    expect(await screen.findByTestId('send-error')).toHaveTextContent(
      'Raw server prose. Your message is back in the box.'
    );
  });
});
