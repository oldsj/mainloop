import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { API_URL } from '@/api/config';
import { connection } from '@/api';
import { loadDraft } from '@/features/chat/drafts';
import { deliveryFixture, messageFixture } from '@/features/chat/fixtures';
import { LOST_REPLY_RECORDED } from '@/features/chat/useThread';
import { createFakeHITL } from '@/features/hitl/handlers';
import { hitlView } from '@/test/fixtures/hitl';
import { server } from '@/test/msw/server';
import { testChat } from '@/test/msw/handlers';
import { native, session } from './test/fixtures';
import { nativeReadWith, problem, serveSessionChat, sessionAction } from './test/handlers';
import { renderSessions } from './test/render';

// The chat's reads would otherwise queue behind the app's read limiter. See fastReads.ts.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('./test/fastReads');
  return withFastReads(await importOriginal());
});

afterEach(async () => {
  vi.restoreAllMocks();
  // Drafts persist per session, so one test's unsent text would otherwise reach the next.
  sessionStorage.clear();
  await connection.check();
});

/**
 * Opens session `s1` with its native state and conversation. The native and conversation reads
 * come from the chat backend's state, which the test shares with the default MSW handlers, so a
 * test can change that state after the page has loaded.
 */
async function openSession(
  options: {
    turnInFlight?: boolean;
    messages?: Parameters<typeof messageFixture>[0][];
    session?: Parameters<typeof session>[0];
  } = {}
) {
  const info = native({ session_id: 's1', turn_in_flight: options.turnInFlight ?? false });
  testChat.state.sessionThreads = {
    s1: {
      native: info,
      messages: (options.messages ?? []).map((overrides) =>
        messageFixture({ conversation_id: 'conv-s1', ...overrides })
      )
    }
  };
  server.use(...serveSessionChat(session({ id: 's1', ...options.session })));
  renderSessions('/sessions/s1');
  await screen.findByRole('heading', { level: 1, name: 'Fix login redirect' });
  return screen.getByTestId('session-chat-slot');
}

const composerOf = (slot: HTMLElement) => within(slot).getByRole('textbox', { name: 'Message' });

describe('session chat', () => {
  it('shows the session conversation, not the main thread', async () => {
    const slot = await openSession({
      messages: [{ id: 'a1', role: 'assistant', content: 'The redirect loops on /login.' }]
    });
    expect(await within(slot).findByText('The redirect loops on /login.')).toBeInTheDocument();
  });

  it('sends to the session route and shows the message in the thread', async () => {
    const user = userEvent.setup();
    const slot = await openSession();
    await waitFor(() => expect(composerOf(slot)).toBeEnabled());

    await user.type(composerOf(slot), 'Check the logs');
    await user.click(within(slot).getByRole('button', { name: 'Send message' }));

    await waitFor(() =>
      expect(testChat.state.sessionPosts.s1).toEqual([{ message: 'Check the logs' }])
    );
    expect(testChat.state.chatPosts).toEqual([]);
    expect(await within(slot).findByText('Check the logs')).toBeInTheDocument();
  });

  it('keeps an unsent draft under the session, not the main conversation', async () => {
    const user = userEvent.setup();
    const slot = await openSession();
    await waitFor(() => expect(composerOf(slot)).toBeEnabled());

    await user.type(composerOf(slot), 'half a thought');

    await waitFor(() => expect(loadDraft('session:s1')).toBe('half a thought'), { timeout: 3000 });
    expect(loadDraft('new')).toBe('');
  });

  it('stops the turn in flight through the session route', async () => {
    const user = userEvent.setup();
    const slot = await openSession({ turnInFlight: true });

    await user.click(await within(slot).findByTestId('session-chat-stop'));

    await waitFor(() => expect(testChat.state.stopped).toEqual(['s1']));
  });

  it('gives the text back when the backend refuses the send because a turn is running', async () => {
    const user = userEvent.setup();
    const slot = await openSession();
    await waitFor(() => expect(composerOf(slot)).toBeEnabled());
    // A turn starts that the page has not read, so the send is refused.
    testChat.state.sessionThreads.s1!.native.turn_in_flight = true;

    await user.type(composerOf(slot), 'too soon');
    await user.click(within(slot).getByRole('button', { name: 'Send message' }));

    expect(await within(slot).findByTestId('send-error')).toHaveTextContent(
      'A turn is already running. Your message is back in the box.'
    );
    expect(composerOf(slot)).toHaveValue('too soon');
    expect(loadDraft('session:s1')).toBe('too soon');
    // The bubble is a paragraph; the textarea also holds the text, so the selector matters.
    expect(within(slot).queryByText('too soon', { selector: 'p' })).toBeNull();
  });

  it('gives the text back when the send never reaches the backend', async () => {
    const user = userEvent.setup();
    testChat.state.lostSend = 'unrecorded';
    const slot = await openSession();
    await waitFor(() => expect(composerOf(slot)).toBeEnabled());

    await user.type(composerOf(slot), 'Check the logs');
    await user.click(within(slot).getByRole('button', { name: 'Send message' }));

    expect(await within(slot).findByTestId('send-error')).toHaveTextContent(
      "Can't reach the Mainloop backend. Your message is back in the box."
    );
    expect(composerOf(slot)).toHaveValue('Check the logs');
    expect(loadDraft('session:s1')).toBe('Check the logs');
    expect(within(slot).queryByText('Check the logs', { selector: 'p' })).toBeNull();
  });

  it('shows a message the backend recorded when its reply was lost, and does not send it twice', async () => {
    const user = userEvent.setup();
    testChat.state.lostSend = 'recorded';
    const slot = await openSession();
    await waitFor(() => expect(composerOf(slot)).toBeEnabled());

    await user.type(composerOf(slot), 'Check the logs');
    await user.click(within(slot).getByRole('button', { name: 'Send message' }));

    expect(await within(slot).findByTestId('send-error')).toHaveTextContent(LOST_REPLY_RECORDED);
    expect(await within(slot).findByText('Check the logs')).toBeInTheDocument();
    expect(composerOf(slot)).toHaveValue('');
    expect(loadDraft('session:s1')).toBe('');
    expect(testChat.state.sessionPosts.s1).toHaveLength(1);
  });

  it("does not count another client's message that lands in a lost reply window", async () => {
    const user = userEvent.setup();
    const slot = await openSession();
    await waitFor(() => expect(composerOf(slot)).toBeEnabled());
    server.use(
      http.post(`${API_URL}/sessions/:sessionId/message`, () => {
        // Another client's message is recorded while the owner's reply is lost.
        const thread = testChat.state.sessionThreads.s1;
        if (!thread) throw new Error('openSession sets the session thread');
        testChat.state.sessionThreads = {
          ...testChat.state.sessionThreads,
          s1: {
            native: {
              ...thread.native,
              deliveries: [...thread.native.deliveries, deliveryFixture({ message_id: 'other-1' })]
            },
            messages: [
              ...thread.messages,
              messageFixture({
                id: 'other-1',
                conversation_id: 'conv-s1',
                content: 'from the laptop'
              })
            ]
          }
        };
        return HttpResponse.error();
      })
    );

    await user.type(composerOf(slot), 'Check the logs');
    await user.click(within(slot).getByRole('button', { name: 'Send message' }));

    expect(await within(slot).findByTestId('send-error')).toHaveTextContent(
      "Can't reach the Mainloop backend. Your message is back in the box. Mainloop could not confirm whether it was recorded."
    );
    expect(within(slot).queryByText(LOST_REPLY_RECORDED)).toBeNull();
    expect(composerOf(slot)).toHaveValue('Check the logs');
    expect(loadDraft('session:s1')).toBe('Check the logs');
  });

  it('says so when a lost reply cannot be checked, and gives the text back', async () => {
    const user = userEvent.setup();
    testChat.state.lostSend = 'unrecorded';
    const slot = await openSession();
    await waitFor(() => expect(composerOf(slot)).toBeEnabled());
    // The thread cannot be re-read after the lost reply, so nothing is known about the send. The read
    // before the send is not under test, so it succeeds; only a read after the send is recorded fails.
    server.use(
      nativeReadWith(() =>
        testChat.state.sessionPosts.s1?.length
          ? HttpResponse.json({ detail: 'boom' }, { status: 500 })
          : HttpResponse.json(native())
      )
    );

    await user.type(composerOf(slot), 'Check the logs');
    await user.click(within(slot).getByRole('button', { name: 'Send message' }));

    expect(await within(slot).findByTestId('send-error')).toHaveTextContent(
      'Mainloop could not confirm whether it was recorded. Check the conversation before you send it again.'
    );
    expect(composerOf(slot)).toHaveValue('Check the logs');
  });

  it('says so when the stop fails, and keeps the stop control', async () => {
    const user = userEvent.setup();
    server.use(sessionAction('stop-turn', problem(500, 'boom')));
    const slot = await openSession({ turnInFlight: true });

    await user.click(await within(slot).findByTestId('session-chat-stop'));

    expect(await within(slot).findByRole('alert')).toHaveTextContent(
      'Could not stop the turn. Try again.'
    );
    expect(within(slot).getByTestId('session-chat-stop')).toHaveTextContent('stop');
  });

  it('locks an archived session and says why it takes no messages', async () => {
    const slot = await openSession({ session: { archived_at: '2026-10-01T10:00:00Z' } });

    expect(
      await within(slot).findByText('This session is archived, so it takes no new messages.')
    ).toBeInTheDocument();
    expect(composerOf(slot)).toBeDisabled();
  });

  it('hides stop for a failed session, even while its last turn is marked in flight', async () => {
    const slot = await openSession({ turnInFlight: true, session: { status: 'failed' } });

    expect(
      await within(slot).findByText('This session failed, so it takes no new messages.')
    ).toBeInTheDocument();
    expect(composerOf(slot)).toBeDisabled();
    expect(within(slot).queryByTestId('session-chat-stop')).toBeNull();
  });

  it('fills the rest of the page instead of a share of the visible viewport', async () => {
    // A soft keyboard shrinks the visual viewport; the chat takes the height its parent gives it.
    const viewport = Object.assign(new EventTarget(), { height: 400, scale: 1 });
    Object.defineProperty(window, 'visualViewport', { value: viewport, configurable: true });
    try {
      const slot = await openSession();
      const panel = within(slot).getByTestId('session-chat-panel');
      expect(panel.style.height).toBe('');
      expect(panel).toHaveClass('flex-1');

      act(() => {
        viewport.height = 200;
        viewport.dispatchEvent(new Event('resize'));
      });
      expect(panel.style.height).toBe('');
      expect(panel).toHaveClass('flex-1');
    } finally {
      Reflect.deleteProperty(window, 'visualViewport');
    }
  });
});

describe('session requests (UI-C)', () => {
  it('shows a pending request as a card and reads "needs you" instead of working', async () => {
    server.use(
      http.get(`${API_URL}/sessions/s1/hitl`, () => HttpResponse.json(['req-s1'])),
      ...createFakeHITL({ 'req-s1': hitlView('req-s1', { context: { session_id: 's1' } }) })
        .handlers
    );
    const slot = await openSession();
    expect(
      await within(slot).findByRole('heading', { name: 'Session needs input' })
    ).toBeInTheDocument();
    expect(within(slot).getByTestId('session-chat-state')).toHaveTextContent(/^needs you$/);
  });

  it('with pending requests, ends the conversation with them in its one scroll and keeps the composer', async () => {
    server.use(
      http.get(`${API_URL}/sessions/s1/hitl`, () => HttpResponse.json(['req-s1', 'req-s2'])),
      ...createFakeHITL({
        'req-s1': hitlView('req-s1', { context: { session_id: 's1' } }),
        'req-s2': hitlView('req-s2', { context: { session_id: 's1' } })
      }).handlers
    );
    const slot = await openSession();
    await within(slot).findByRole('heading', { name: 'Session needs input' });
    // The cards scroll with the conversation, so the slot never splits into two scrollers.
    const requests = within(slot).getByTestId('session-requests-list');
    await waitFor(() => expect(requests.children.length).toBe(2));
    expect(within(slot).getByTestId('message-list')).toContainElement(requests);
    expect(requests).not.toHaveClass('overflow-y-auto');
    const panel = within(slot).getByTestId('session-chat-panel');
    expect(panel).toHaveClass('min-h-40', 'flex-1');
    expect(panel).toContainElement(composerOf(slot));
    expect(slot.contains(requests)).toBe(true);
  });

  it('does not say needs you when no request is pending', async () => {
    server.use(http.get(`${API_URL}/sessions/s1/hitl`, () => HttpResponse.json([])));
    const slot = await openSession();
    await waitFor(() =>
      expect(within(slot).getByTestId('session-chat-state')).not.toHaveTextContent('needs you')
    );
    expect(
      within(slot).queryByRole('heading', { name: 'Session needs input' })
    ).not.toBeInTheDocument();
  });
});
