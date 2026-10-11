import { useQueryClient } from '@tanstack/react-query';
import { useCallback, useMemo, useRef, useState } from 'react';
import { $api, fetchClient, keys, SendError } from '@/api';
import type { Message } from '@/lib/contract';
import { deliveryNotices } from '@/lib/delivery';
import { createSendGuard, retryMessage } from '@/lib/messageSend';
import { clearDraft, NEW_CONVERSATION } from './drafts';

/** While a turn is in flight the thread is re-read this often, as a fallback to `session:message`. */
export const POLL_MS = 2000;
/** About six minutes. After that only SSE and a manual refresh update the thread. */
export const MAX_POLLS = 180;

const BACK_IN_BOX = ' Your message is back in the box.';

/** The words for a send the backend did not accept. */
export function sendErrorText(error: unknown, restored: boolean): string {
  const suffix = restored ? BACK_IN_BOX : '';
  if (error instanceof SendError && (error.status === 409 || error.status === 0))
    return `${error.message}${suffix}`;
  return `Could not send the message.${suffix}`;
}

interface Optimistic {
  message: Message;
  /** Set once the backend recorded it; the bubble goes away when the server list shows that id. */
  acceptedId?: string;
}

/**
 * The main thread's data and actions: the thread info and conversation (kept fresh by SSE, and
 * polled while a turn is in flight), sending with an optimistic bubble, retry and stop.
 */
export function useMainThread() {
  const queryClient = useQueryClient();
  const polling = useRef(false);
  const polls = useRef(0);

  const mainThread = $api.useQuery('get', '/main-thread', undefined, {
    refetchInterval: (query) => {
      const inFlight = query.state.data?.native?.turn_in_flight === true;
      if (!inFlight) {
        polls.current = 0;
        polling.current = false;
        return false;
      }
      polling.current = ++polls.current <= MAX_POLLS;
      return polling.current ? POLL_MS : false;
    }
  });
  const info = mainThread.data;
  const conversationId = info?.conversation_id ?? null;

  const conversation = $api.useQuery(
    'get',
    '/conversations/{conversation_id}',
    { params: { path: { conversation_id: conversationId ?? '' } } },
    { enabled: conversationId !== null, refetchInterval: () => (polling.current ? POLL_MS : false) }
  );

  const [optimistic, setOptimistic] = useState<Optimistic[]>([]);
  const [sendPending, setSendPending] = useState(false);
  const [sendError, setSendError] = useState<string | null>(null);
  const guard = useMemo(() => createSendGuard(setSendPending), []);
  const sequence = useRef(0);
  const chat = $api.useMutation('post', '/chat');
  const stopTurn = $api.useMutation('post', '/sessions/{session_id}/stop-turn');
  const [stopError, setStopError] = useState<string | null>(null);

  const serverMessages = useMemo(() => conversation.data?.messages ?? [], [conversation.data]);

  // A bubble goes away once the conversation shows the message it stood for.
  const visibleOptimistic = useMemo(() => {
    const seen = new Set(serverMessages.map((message) => message.id));
    return optimistic.filter((entry) => !(entry.acceptedId && seen.has(entry.acceptedId)));
  }, [optimistic, serverMessages]);

  const refresh = useCallback(
    () =>
      Promise.all([
        queryClient.invalidateQueries({ queryKey: keys.mainThread() }),
        queryClient.invalidateQueries({ queryKey: keys.conversation() })
      ]),
    [queryClient]
  );

  /**
   * Sends one message. A message the backend refused is taken back: the bubble goes away and,
   * for the composer, the text returns to the box. Once accepted, a later failure to refresh does
   * not undo it. Resolves true when the backend recorded the message.
   */
  const send = useCallback(
    async (text: string, targetConversation: string | null, restored = true): Promise<boolean> => {
      let accepted = false;
      await guard(async () => {
        const entry: Optimistic = {
          message: {
            id: `pending-${++sequence.current}`,
            conversation_id: targetConversation ?? '',
            role: 'user',
            content: text,
            created_at: new Date().toISOString()
          }
        };
        setSendError(null);
        setOptimistic([...visibleOptimistic, entry]);
        try {
          const response = await chat.mutateAsync({
            body: { message: text, conversation_id: targetConversation }
          });
          accepted = true;
          clearDraft(targetConversation ?? NEW_CONVERSATION);
          const acceptedId = response.message?.id;
          setOptimistic((list) =>
            acceptedId
              ? list.map((item) => (item === entry ? { ...item, acceptedId } : item))
              : list.filter((item) => item !== entry)
          );
          await refresh().catch(() => undefined);
        } catch (error) {
          if (!accepted) {
            setOptimistic((list) => list.filter((item) => item !== entry));
            setSendError(sendErrorText(error, restored));
          }
        }
      });
      return accepted;
    },
    [chat, guard, refresh, visibleOptimistic]
  );

  const retry = useCallback(
    async (message: Message) => {
      try {
        await retryMessage(
          message,
          async () => {
            const { data } = await fetchClient.GET('/main-thread');
            return {
              conversationId: data?.conversation_id ?? null,
              deliveries: data?.native?.deliveries ?? [],
              blocked: data?.native?.turn_in_flight ?? false
            };
          },
          async (text, targetConversation) => {
            await send(text, targetConversation, false);
          }
        );
        // A retry the fresh read refused leaves the screen stale; catch it up.
        await refresh().catch(() => undefined);
      } catch {
        setSendError('Could not check the delivery status. Try again.');
      }
    },
    [refresh, send]
  );

  const stop = useCallback(() => {
    const sessionId = info?.session_id;
    if (!sessionId) return;
    setStopError(null);
    stopTurn.mutate(
      { params: { path: { session_id: sessionId } } },
      {
        onError: () => setStopError('Could not stop the turn. Try again.'),
        onSettled: () => void refresh().catch(() => undefined)
      }
    );
  }, [info?.session_id, refresh, stopTurn]);

  const notices = useMemo(
    () => deliveryNotices(info?.native?.deliveries ?? []),
    [info?.native?.deliveries]
  );

  return {
    info,
    /** The first read of the thread finished. */
    loaded: mainThread.isSuccess,
    loadFailed: mainThread.isError && !info,
    reload: () => void mainThread.refetch(),
    conversationId,
    messages: [...serverMessages, ...visibleOptimistic.map((entry) => entry.message)],
    conversationFailed: conversation.isError,
    busy: info?.native?.turn_in_flight === true,
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
