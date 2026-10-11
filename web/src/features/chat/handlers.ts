import { http, HttpResponse } from 'msw';
import { API_URL } from '@/api/config';
import type { components } from '@/api/schema.gen';
import {
  CONVERSATION_ID,
  MAIN_SESSION_ID,
  mainThreadFixture,
  messageFixture,
  type ChatMessage,
  type MainThreadInfo
} from './fixtures';

type Schemas = components['schemas'];

export interface ChatBackendState {
  mainThread: MainThreadInfo;
  messages: ChatMessage[];
  /** Pending request ids that `GET /sessions/{id}/hitl` returns for the main session. */
  hitlIds: string[];
  /** Every `POST /chat` body the backend received, in order. */
  chatPosts: Schemas['ChatRequest'][];
  /** Every session id that got a stop-turn POST. */
  stopped: string[];
  /** The next `POST /chat` is refused with this status and detail, then the field clears. */
  rejectNextChat: { status: number; detail: string } | null;
}

const url = (path: string) => `${API_URL}${path}`;

/**
 * An in-memory main thread behind the contract's chat routes. Tests read and edit `state`
 * to set up a scenario (a failed delivery, a turn in flight) and to assert what was sent; the
 * local stack (`VITE_MSW=1`) can spread `handlers` for a working demo thread.
 */
export function createChatBackend(initial: Partial<ChatBackendState> = {}) {
  const state: ChatBackendState = {
    mainThread: mainThreadFixture(),
    messages: [],
    hitlIds: [],
    chatPosts: [],
    stopped: [],
    rejectNextChat: null,
    ...initial
  };
  let counter = state.messages.length;

  const handlers = [
    http.get(url('/main-thread'), () => HttpResponse.json(state.mainThread)),
    http.get(url('/conversations/:conversationId'), ({ params }) => {
      if (params.conversationId !== state.mainThread.conversation_id)
        return HttpResponse.json({ detail: 'Conversation not found' }, { status: 404 });
      const now = state.messages.at(-1)?.created_at ?? '2026-01-01T10:00:00Z';
      const body: Schemas['ConversationResponse'] = {
        conversation: {
          id: params.conversationId as string,
          user_id: 'dev-user',
          title: null,
          summary: null,
          summarized_through_id: null,
          message_count: state.messages.length,
          created_at: now,
          updated_at: now
        },
        messages: state.messages
      };
      return HttpResponse.json(body);
    }),
    http.post(url('/chat'), async ({ request }) => {
      const body = (await request.json()) as Schemas['ChatRequest'];
      state.chatPosts.push(body);
      if (state.rejectNextChat) {
        const { status, detail } = state.rejectNextChat;
        state.rejectNextChat = null;
        return HttpResponse.json({ detail }, { status });
      }
      if (state.mainThread.native?.turn_in_flight)
        return HttpResponse.json({ detail: 'A turn is already running.' }, { status: 409 });
      state.mainThread = {
        ...state.mainThread,
        conversation_id: state.mainThread.conversation_id ?? CONVERSATION_ID
      };
      const message = messageFixture({
        id: `sent-${++counter}`,
        conversation_id: state.mainThread.conversation_id ?? CONVERSATION_ID,
        content: body.message,
        created_at: new Date().toISOString()
      });
      state.messages = [...state.messages, message];
      const response: Schemas['ChatResponse'] = {
        conversation_id: message.conversation_id,
        message,
        spawned_session_id: null,
        pending: false,
        delivery_message_id: message.id
      };
      return HttpResponse.json(response);
    }),
    http.post(url('/sessions/:sessionId/stop-turn'), ({ params }) => {
      state.stopped.push(params.sessionId as string);
      const body: Schemas['SessionStopTurnResponse'] = { status: 'stopped' };
      return HttpResponse.json(body);
    }),
    http.get(url('/sessions/:sessionId/hitl'), ({ params }) =>
      HttpResponse.json(params.sessionId === MAIN_SESSION_ID ? state.hitlIds : [])
    )
  ];

  return { state, handlers };
}
