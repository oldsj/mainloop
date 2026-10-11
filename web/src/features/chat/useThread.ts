import { useQueryClient } from '@tanstack/react-query';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { $api, fetchClient, keys, SendError } from '@/api';
import type { Message } from '@/lib/contract';
import { deliveryNotices } from '@/lib/delivery';
import { createSendGuard, retryMessage } from '@/lib/messageSend';
import { clearDraft, NEW_CONVERSATION, sessionDraftKey } from './drafts';
import type { NativeSessionInfo } from './types';

/** While a turn is in flight the thread is re-read this often, as a fallback to `session:message`. */
export const POLL_MS = 2000;
/**
 * How long polling lasts, counted from when the turn is first seen in flight. After that only
 * SSE and a manual refresh update the thread.
 */
export const POLL_WINDOW_MS = 6 * 60 * 1000;

const BACK_IN_BOX = ' Your message is back in the box.';
/** A send whose reply was lost, after the thread shows the message was recorded. */
export const LOST_REPLY_RECORDED =
  'Mainloop did not reply, but it recorded your message. Check the conversation before you send it again.';
const LOST_REPLY_UNCONFIRMED =
  ' Mainloop could not confirm whether it was recorded. Check the conversation before you send it again.';

/** The words for a send the backend did not accept. */
export function sendErrorText(error: unknown, restored: boolean): string {
  const suffix = restored ? BACK_IN_BOX : '';
  if (error instanceof SendError && (error.status === 409 || error.status === 0))
    return `${error.message}${suffix}`;
  return `Could not send the message.${suffix}`;
}

/** A send that got no reply at all: the backend may still have recorded it. */
function isLostReply(error: unknown): boolean {
  return error instanceof SendError && error.status === 0;
}

/** Which thread a hook reads and sends to: the main session's, or one session's conversation. */
export type ThreadSource = { kind: 'main' } | { kind: 'session'; sessionId: string };

export const MAIN_THREAD: ThreadSource = { kind: 'main' };

/**
 * The poll interval while a turn is in flight. The window is taken once per delivery, when the
 * turn is first seen, so it cannot be a countdown: the callers run it on every render.
 */
export function createTurnPoll() {
  let delivery: string | null = null;
  let deadline = 0;
  let polling = false;
  return {
    /** True while the window of the turn in flight is still open. */
    get polling() {
      return polling;
    },
    interval(native: Pick<NativeSessionInfo, 'turn_in_flight' | 'deliveries'> | null | undefined) {
      if (native?.turn_in_flight !== true) {
        delivery = null;
        polling = false;
        return false;
      }
      const id = native.deliveries.at(-1)?.message_id ?? '';
      if (delivery !== id) {
        delivery = id;
        deadline = Date.now() + POLL_WINDOW_MS;
      }
      polling = Date.now() < deadline;
      return polling ? POLL_MS : false;
    }
  };
}

interface Optimistic {
  message: Message;
  /** Set once the backend recorded it; the bubble goes away when the server list shows that id. */
  acceptedId?: string;
}

/**
 * One thread's data and actions: the thread state and its conversation (kept fresh by SSE, and
 * polled while a turn is in flight), sending with an optimistic bubble, retry and stop. The main
 * thread and a session's conversation differ only in what they read and where they send.
 */
export function useThread(source: ThreadSource) {
  const queryClient = useQueryClient();
  const isMain = source.kind === 'main';
  const sessionId = source.kind === 'session' ? source.sessionId : '';
  const [turn] = useState(createTurnPoll);

  // Only the source's own read is enabled. TanStack also calls the interval of a disabled query, so
  // the idle one must not touch the shared turn window: it returns before reaching `turn`.
  const mainThread = $api.useQuery('get', '/main-thread', undefined, {
    enabled: isMain,
    refetchInterval: (query) => (isMain ? turn.interval(query.state.data?.native) : false)
  });
  const sessionNative = $api.useQuery(
    'get',
    '/sessions/{session_id}/native',
    { params: { path: { session_id: sessionId } } },
    {
      enabled: !isMain,
      refetchInterval: (query) => (isMain ? false : turn.interval(query.state.data))
    }
  );
  const thread = isMain ? mainThread : sessionNative;
  const info = isMain ? mainThread.data : undefined;
  const native = isMain ? info?.native : sessionNative.data;
  const conversationId = info?.conversation_id ?? null;
  const threadSessionId = isMain ? (info?.session_id ?? null) : sessionId;

  const mainConversation = $api.useQuery(
    'get',
    '/conversations/{conversation_id}',
    { params: { path: { conversation_id: conversationId ?? '' } } },
    {
      enabled: isMain && conversationId !== null,
      refetchInterval: () => (turn.polling ? POLL_MS : false)
    }
  );
  const sessionConversation = $api.useQuery(
    'get',
    '/sessions/{session_id}/conversation',
    { params: { path: { session_id: sessionId } } },
    { enabled: !isMain, refetchInterval: () => (turn.polling ? POLL_MS : false) }
  );
  const conversation = isMain ? mainConversation : sessionConversation;
  const conversationEnabled = isMain ? conversationId !== null : true;

  const draftKey = isMain ? (conversationId ?? NEW_CONVERSATION) : sessionDraftKey(sessionId);

  const [optimistic, setOptimistic] = useState<Optimistic[]>([]);
  const [sendPending, setSendPending] = useState(false);
  const [sendError, setSendError] = useState<string | null>(null);
  const guard = useMemo(() => createSendGuard(setSendPending), []);
  const sequence = useRef(0);
  const mainChat = $api.useMutation('post', '/chat');
  const sessionChat = $api.useMutation('post', '/sessions/{session_id}/message');
  const stopTurn = $api.useMutation('post', '/sessions/{session_id}/stop-turn');
  const [stopError, setStopError] = useState<string | null>(null);

  const serverMessages = useMemo(() => conversation.data?.messages ?? [], [conversation.data]);

  // A bubble goes away once the conversation shows the message it stood for.
  const visibleOptimistic = useMemo(() => {
    const seen = new Set(serverMessages.map((message) => message.id));
    return optimistic.filter((entry) => !(entry.acceptedId && seen.has(entry.acceptedId)));
  }, [optimistic, serverMessages]);

  // Forget a bubble once the conversation lists its message, so a changed or paged conversation
  // cannot bring it back. Adjusted while rendering, as an effect may not set state.
  if (visibleOptimistic.length !== optimistic.length) setOptimistic(visibleOptimistic);

  const refresh = useCallback(() => {
    const queryKeys = isMain
      ? [keys.mainThread(), keys.conversation()]
      : [keys.sessionNative(sessionId), keys.sessionConversation(sessionId)];
    return Promise.all(queryKeys.map((queryKey) => queryClient.invalidateQueries({ queryKey })));
  }, [isMain, queryClient, sessionId]);

  /**
   * Posts one message and returns the id the backend recorded it under. The two routes answer
   * differently: the main thread gives a delivery id, a session gives its message id.
   */
  const post = useCallback(
    async (text: string, targetConversation: string | null): Promise<string | null | undefined> => {
      if (isMain) {
        const response = await mainChat.mutateAsync({
          body: { message: text, conversation_id: targetConversation }
        });
        // The native backend answers with the delivery id and no message body.
        return response.delivery_message_id ?? response.message?.id;
      }
      const response = await sessionChat.mutateAsync({
        params: { path: { session_id: sessionId } },
        body: { message: text }
      });
      return response.message_id;
    },
    [isMain, mainChat, sessionChat, sessionId]
  );

  /** The delivery ids the thread holds, read fresh. Throws when the thread cannot be read. */
  const readDeliveryIds = useCallback(async (): Promise<string[]> => {
    if (isMain) {
      const { data } = await fetchClient.GET('/main-thread');
      if (!data) throw new Error('The main thread could not be read.');
      return (data.native?.deliveries ?? []).map((delivery) => delivery.message_id);
    }
    const { data } = await fetchClient.GET('/sessions/{session_id}/native', {
      params: { path: { session_id: sessionId } }
    });
    if (!data) throw new Error('The session could not be read.');
    return data.deliveries.map((delivery) => delivery.message_id);
  }, [isMain, sessionId]);

  /**
   * Whether a send that lost its reply was recorded: the thread now holds a delivery it did not
   * hold before the send. Null when the thread cannot be re-read, so nothing is known.
   */
  const recordedSince = useCallback(
    async (known: ReadonlySet<string>): Promise<boolean | null> => {
      try {
        const ids = await readDeliveryIds();
        return ids.some((id) => !known.has(id));
      } catch {
        return null;
      }
    },
    [readDeliveryIds]
  );

  /** Posts one message and shows it at once; the caller holds the send guard. */
  const submit = useCallback(
    async (
      text: string,
      targetConversation: string | null,
      composer: boolean
    ): Promise<boolean> => {
      let accepted = false;
      const entry: Optimistic = {
        message: {
          id: `pending-${++sequence.current}`,
          conversation_id: targetConversation ?? '',
          role: 'user',
          content: text,
          created_at: new Date().toISOString()
        }
      };
      // The deliveries before this send. A reply lost in transit is checked against them.
      const known = new Set((native?.deliveries ?? []).map((delivery) => delivery.message_id));
      setSendError(null);
      setOptimistic((list) => [...list, entry]);
      try {
        const acceptedId = await post(text, targetConversation);
        accepted = true;
        // Only the composer's own text is a draft; a retry must not wipe what the owner is typing.
        if (composer) clearDraft(draftKey);
        setOptimistic((list) =>
          acceptedId
            ? list.map((item) => (item === entry ? { ...item, acceptedId } : item))
            : list.filter((item) => item !== entry)
        );
        await refresh().catch(() => undefined);
      } catch (error) {
        if (!accepted) {
          setOptimistic((list) => list.filter((item) => item !== entry));
          // No reply does not mean no record. Re-read before the text goes back to the box, so it
          // is not sent a second time.
          const lost = isLostReply(error);
          const recorded = lost ? await recordedSince(known) : false;
          if (recorded) {
            accepted = true;
            if (composer) clearDraft(draftKey);
            setSendError(LOST_REPLY_RECORDED);
            await refresh().catch(() => undefined);
          } else {
            const unconfirmed = lost && recorded === null ? LOST_REPLY_UNCONFIRMED : '';
            setSendError(sendErrorText(error, composer) + unconfirmed);
          }
        }
      }
      return accepted;
    },
    [draftKey, native?.deliveries, post, recordedSince, refresh]
  );

  /**
   * Sends the composer's text. A message the backend refused is taken back: the bubble goes away
   * and the text returns to the box. Once accepted, a later failure to refresh does not undo it.
   * Resolves true when the backend recorded the message.
   */
  const send = useCallback(
    async (text: string, targetConversation: string | null): Promise<boolean> => {
      let accepted = false;
      await guard(async () => {
        accepted = await submit(text, targetConversation, true);
      });
      return accepted;
    },
    [guard, submit]
  );

  const retry = useCallback(
    (message: Message) =>
      guard(async () => {
        try {
          await retryMessage(
            message,
            async () => {
              if (isMain) {
                const { data } = await fetchClient.GET('/main-thread');
                return {
                  conversationId: data?.conversation_id ?? null,
                  deliveries: data?.native?.deliveries ?? [],
                  blocked: data?.native?.turn_in_flight ?? false
                };
              }
              const { data } = await fetchClient.GET('/sessions/{session_id}/native', {
                params: { path: { session_id: sessionId } }
              });
              // A session has one conversation, so the message's own id is the expected one.
              return {
                conversationId: message.conversation_id,
                deliveries: data?.deliveries ?? [],
                blocked: data?.turn_in_flight ?? false
              };
            },
            async (text, targetConversation) => {
              await submit(text, targetConversation, false);
            }
          );
          // A retry the fresh read refused leaves the screen stale; catch it up.
          await refresh().catch(() => undefined);
        } catch {
          setSendError('Could not check the delivery status. Try again.');
        }
      }),
    [guard, isMain, refresh, sessionId, submit]
  );

  const stop = useCallback(() => {
    if (!threadSessionId) return;
    setStopError(null);
    stopTurn.mutate(
      { params: { path: { session_id: threadSessionId } } },
      {
        onError: () => setStopError('Could not stop the turn. Try again.'),
        onSettled: () => void refresh().catch(() => undefined)
      }
    );
  }, [threadSessionId, refresh, stopTurn]);

  // The last poll of the conversation can predate the agent's reply; read it once more when the
  // turn ends.
  const busy = native?.turn_in_flight === true;
  const wasBusy = useRef(false);
  useEffect(() => {
    if (wasBusy.current && !busy) {
      void queryClient.invalidateQueries({
        queryKey: isMain ? keys.conversation() : keys.sessionConversation(sessionId)
      });
    }
    wasBusy.current = busy;
  }, [busy, isMain, queryClient, sessionId]);

  const notices = useMemo(() => deliveryNotices(native?.deliveries ?? []), [native?.deliveries]);

  return {
    /** The main thread's header data. Undefined for a session's conversation. */
    info,
    /** The thread's native state: the turn, the deliveries and the agent kind. */
    native,
    /** The session the thread belongs to, for stop and the requests list. */
    sessionId: threadSessionId,
    /** Where the unsent text is kept. */
    draftKey,
    /** The thread itself is read, so the draft key is known. */
    threadKnown: thread.data !== undefined,
    /** The thread is read, and its conversation too when the thread has one. */
    loaded: thread.data !== undefined && (!conversationEnabled || conversation.data !== undefined),
    loadFailed: thread.isError && thread.data === undefined,
    reload: () => void thread.refetch(),
    conversationId,
    messages: [...serverMessages, ...visibleOptimistic.map((entry) => entry.message)],
    conversationFailed: conversation.isError,
    reloadConversation: () => void conversation.refetch(),
    busy,
    sendPending,
    sendError,
    notices,
    send,
    retry,
    stop,
    stopping: stopTurn.isPending,
    stopError
  };
}

export type ThreadView = ReturnType<typeof useThread>;
