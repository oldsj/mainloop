import { http, HttpResponse } from 'msw';
import { API_URL } from '@/api/config';
import type { components } from '@/api/schema.gen';
import {
  CONVERSATION_ID,
  deliveryFixture,
  MAIN_SESSION_ID,
  mainThreadFixture,
  messageFixture,
  nativeFixture,
  sessionFixture
} from './fixtures';
import type { ChatMessage, MainThreadInfo, NativeSessionInfo } from './types';

type Schemas = components['schemas'];

/** One session's conversation, as its native routes read it. */
export interface SessionThreadState {
  native: NativeSessionInfo;
  messages: ChatMessage[];
}

/**
 * A send whose reply never reaches the client, so the client sees a network error. `recorded` lands
 * the message first, as the backend does before it answers. `unrecorded` fails before anything is
 * stored.
 */
export type LostSend = 'recorded' | 'unrecorded';

export interface ChatBackendState {
  mainThread: MainThreadInfo;
  messages: ChatMessage[];
  /** Pending request ids that `GET /sessions/{id}/hitl` returns for the main session. */
  hitlIds: string[];
  /** Every `POST /chat` body the backend received, in order. */
  chatPosts: Schemas['ChatRequest'][];
  /** Every `POST /sessions/{id}/message` body the backend received, by session, in order. */
  sessionPosts: Record<string, Schemas['SessionMessageRequest'][]>;
  /** Every session id that got a stop-turn POST. */
  stopped: string[];
  /** How many times `GET /main-thread` was read. */
  mainThreadReads: number;
  /** The next `POST /chat` is refused with this status and detail, then the field clears. */
  rejectNextChat: { status: number; detail: string } | null;
  /** The next send, to either route, loses its reply. The flag clears after one send. */
  lostSend: LostSend | null;
  /** Sessions other than the main one, created on first use: their native state and messages. */
  sessionThreads: Record<string, SessionThreadState>;
}

const url = (path: string) => `${API_URL}${path}`;

/** The conversation id a session's messages carry. A session has one conversation. */
const sessionConversationId = (sessionId: string) => `conv-${sessionId}`;

/** Marks the in-flight deliveries completed and the turn over. */
function completeTurn(native: NativeSessionInfo): NativeSessionInfo {
  return {
    ...native,
    turn_in_flight: false,
    deliveries: native.deliveries.map((delivery) =>
      delivery.state === 'recorded' ? { ...delivery, state: 'completed' } : delivery
    )
  };
}

/** Records a delivery for the message and starts the turn, as the native backend does. */
function startTurn(native: NativeSessionInfo, messageId: string): NativeSessionInfo {
  return {
    ...native,
    turn_in_flight: true,
    deliveries: [
      ...native.deliveries,
      deliveryFixture({ message_id: messageId, state: 'recorded' })
    ]
  };
}

/**
 * An in-memory main thread and session conversations behind the contract's chat routes. Tests read
 * and edit `state` to set up a scenario (a failed delivery, a turn in flight) and to assert what
 * was sent; the local stack (`VITE_MSW=1`) can spread `handlers` for a working demo.
 */
export function createChatBackend(
  initial: Partial<ChatBackendState> = {},
  options: { autoEndTurnMs?: number } = {}
) {
  const freshState = (): ChatBackendState => ({
    mainThread: mainThreadFixture(),
    messages: [],
    hitlIds: [],
    chatPosts: [],
    sessionPosts: {},
    stopped: [],
    mainThreadReads: 0,
    rejectNextChat: null,
    lostSend: null,
    sessionThreads: {},
    ...initial
  });
  const state = freshState();
  let counter = state.messages.length;
  const pendingTurns = new Set<ReturnType<typeof setTimeout>>();

  /**
   * Puts the backend back to its starting state and drops any scheduled turn end. Tests that share
   * the default backend call this between cases, so one case's messages cannot leak into the next.
   */
  function reset() {
    for (const timer of pendingTurns) clearTimeout(timer);
    pendingTurns.clear();
    Object.assign(state, freshState());
    counter = state.messages.length;
  }

  /** Takes the lost-send flag, so only the send that is starting now loses its reply. */
  function takeLostSend(): LostSend | null {
    const lost = state.lostSend;
    state.lostSend = null;
    return lost;
  }

  /** Runs a turn's end later, when the backend was asked to auto-end turns. */
  function scheduleTurnEnd(end: () => void) {
    if (options.autoEndTurnMs === undefined) return;
    const timer = setTimeout(() => {
      pendingTurns.delete(timer);
      end();
    }, options.autoEndTurnMs);
    pendingTurns.add(timer);
  }

  /** A session's state, created on first use. Its messages are replaced, never edited in place. */
  function sessionThread(sessionId: string): SessionThreadState {
    return (
      state.sessionThreads[sessionId] ?? {
        native: nativeFixture({ session_id: sessionId }),
        messages: []
      }
    );
  }

  function putSessionThread(sessionId: string, thread: SessionThreadState) {
    state.sessionThreads = { ...state.sessionThreads, [sessionId]: thread };
  }

  /**
   * Ends the running turn the way the native backend does: the user message's delivery completes,
   * and the agent's reply, when given, joins the conversation.
   */
  function endTurn(reply?: string) {
    state.mainThread = {
      ...state.mainThread,
      native: completeTurn(state.mainThread.native ?? nativeFixture())
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

  /** The session's counterpart of `endTurn`. */
  function endSessionTurn(sessionId: string, reply?: string) {
    const thread = sessionThread(sessionId);
    const messages = reply
      ? [
          ...thread.messages,
          messageFixture({
            id: `reply-${++counter}`,
            conversation_id: sessionConversationId(sessionId),
            role: 'assistant',
            content: reply,
            created_at: new Date().toISOString()
          })
        ]
      : thread.messages;
    putSessionThread(sessionId, { native: completeTurn(thread.native), messages });
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
      const lost = takeLostSend();
      if (lost === 'unrecorded') return HttpResponse.error();
      const conversationId = state.mainThread.conversation_id ?? CONVERSATION_ID;
      const message = messageFixture({
        id: `sent-${++counter}`,
        conversation_id: conversationId,
        content: body.message,
        created_at: new Date().toISOString()
      });
      state.messages = [...state.messages, message];
      // Native answer: the message is recorded and the turn starts; the reply comes later.
      state.mainThread = {
        ...state.mainThread,
        conversation_id: conversationId,
        native: startTurn(state.mainThread.native ?? nativeFixture(), message.id)
      };
      scheduleTurnEnd(() => endTurn(`Echo: ${body.message}`));
      if (lost === 'recorded') return HttpResponse.error();
      const response: Schemas['ChatResponse'] = {
        conversation_id: conversationId,
        message: null,
        spawned_session_id: null,
        pending: true,
        delivery_message_id: message.id
      };
      return HttpResponse.json(response);
    }),
    http.get(url('/sessions/:sessionId/native'), ({ params }) =>
      HttpResponse.json(sessionThread(params.sessionId as string).native)
    ),
    http.get(url('/sessions/:sessionId/conversation'), ({ params }) => {
      const sessionId = params.sessionId as string;
      const body: Schemas['SessionConversationResponse'] = {
        session: sessionFixture(sessionId),
        messages: sessionThread(sessionId).messages
      };
      return HttpResponse.json(body);
    }),
    http.post(url('/sessions/:sessionId/message'), async ({ params, request }) => {
      const sessionId = params.sessionId as string;
      const body = (await request.json()) as Schemas['SessionMessageRequest'];
      state.sessionPosts = {
        ...state.sessionPosts,
        [sessionId]: [...(state.sessionPosts[sessionId] ?? []), body]
      };
      const thread = sessionThread(sessionId);
      if (thread.native.turn_in_flight)
        return HttpResponse.json({ detail: 'A turn is already running.' }, { status: 409 });
      const lost = takeLostSend();
      if (lost === 'unrecorded') return HttpResponse.error();
      const message = messageFixture({
        id: `sent-${++counter}`,
        conversation_id: sessionConversationId(sessionId),
        content: body.message,
        created_at: new Date().toISOString()
      });
      putSessionThread(sessionId, {
        native: startTurn(thread.native, message.id),
        messages: [...thread.messages, message]
      });
      scheduleTurnEnd(() => endSessionTurn(sessionId, `Echo: ${body.message}`));
      if (lost === 'recorded') return HttpResponse.error();
      const response: Schemas['SessionMessageResponse'] = {
        status: 'ok',
        message_id: message.id
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

  return { state, handlers, endTurn, endSessionTurn, reset };
}
