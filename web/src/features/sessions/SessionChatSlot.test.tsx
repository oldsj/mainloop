import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection } from '@/api';
import { loadDraft } from '@/features/chat/drafts';
import { messageFixture } from '@/features/chat/fixtures';
import { server } from '@/test/msw/server';
import { testChat } from '@/test/msw/handlers';
import { native, session } from './test/fixtures';
import { serveSession } from './test/handlers';
import { renderSessions } from './test/render';

// The chat's reads would otherwise queue behind the app's read limiter. See fastReads.ts.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('./test/fastReads');
  return withFastReads(await importOriginal());
});

afterEach(async () => {
  vi.restoreAllMocks();
  await connection.check();
});

/**
 * Opens session `s1` with its native state and conversation. The chat's conversation read comes
 * from the chat backend's state, which the test shares with the default MSW handlers.
 */
async function openSession(
  options: { turnInFlight?: boolean; messages?: Parameters<typeof messageFixture>[0][] } = {}
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
  server.use(...serveSession(session({ id: 's1' }), info));
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
});
