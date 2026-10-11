import { http, HttpResponse } from 'msw';
import { API_URL } from '@/api/config';
import type { components } from '@/api/schema.gen';
import {
  CONVERSATION_ID,
  deliveryFixture,
  MAIN_SESSION_ID,
  mainThreadFixture,
  messageFixture,
  nativeFixture
} from './fixtures';
import type { ChatMessage, MainThreadInfo } from './types';

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
  /** How many times `GET /main-thread` was read. */
  mainThreadReads: number;
  /** The next `POST /chat` is refused with this status and detail, then the field clears. */
  rejectNextChat: { status: number; detail: string } | null;
}

const url = (path: string) => `${API_URL}${path}`;

/**
 * An in-memory main thread behind the contract's chat routes. Tests read and edit `state`
 * to set up a scenario (a failed delivery, a turn in flight) and to assert what was sent; the
 * local stack (`VITE_MSW=1`) can spread `handlers` for a working demo thread.
 */
export function createChatBackend(
  initial: Partial<ChatBackendState> = {},
  options: { autoEndTurnMs?: number } = {}
) {
  const state: ChatBackendState = {
    mainThread: mainThreadFixture(),
    messages: [],
    hitlIds: [],
    chatPosts: [],
    stopped: [],
    mainThreadReads: 0,
    rejectNextChat: null,
    ...initial
  };
  let counter = state.messages.length;

  /**
   * Ends the running turn the way the native backend does: the user message's delivery completes,
   * and the agent's reply, when given, joins the conversation.
   */
  function endTurn(reply?: string) {
    const native = state.mainThread.native ?? nativeFixture();
    state.mainThread = {
      ...state.mainThread,
      native: {
        ...native,
        turn_in_flight: false,
        deliveries: native.deliveries.map((delivery) =>
          delivery.state === 'recorded' ? { ...delivery, state: 'completed' } : delivery
        )
      }
    };
    if (reply)
      state.messages = [
        ...state.messages,
        messageFixture({
          id: `reply-${++counter}`,
          conversation_id: state.mainThread.conversation_id ?? CONVERSATION_ID,
          role: 'assistant',
          content: reply,
          created_at: new Date().toISOString()
        })
      ];
  }

  const handlers = [
    http.get(url('/main-thread'), () => {
      state.mainThreadReads += 1;
      return HttpResponse.json(state.mainThread);
    }),
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
      const conversationId = state.mainThread.conversation_id ?? CONVERSATION_ID;
      const message = messageFixture({
        id: `sent-${++counter}`,
        conversation_id: conversationId,
        content: body.message,
        created_at: new Date().toISOString()
      });
      state.messages = [...state.messages, message];
      const native = state.mainThread.native ?? nativeFixture();
      // Native answer: the message is recorded and the turn starts; the reply comes later.
      state.mainThread = {
        ...state.mainThread,
        conversation_id: conversationId,
        native: {
          ...native,
          turn_in_flight: true,
          deliveries: [
            ...native.deliveries,
            deliveryFixture({ message_id: message.id, state: 'recorded' })
          ]
        }
      };
      if (options.autoEndTurnMs !== undefined)
        setTimeout(() => endTurn(`Echo: ${body.message}`), options.autoEndTurnMs);
      const response: Schemas['ChatResponse'] = {
        conversation_id: conversationId,
        message: null,
        spawned_session_id: null,
        pending: true,
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

  return { state, handlers, endTurn };
}
