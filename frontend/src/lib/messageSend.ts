import type { Message, NativeDelivery } from './api';
import { deliveryNotices } from './delivery.ts';

/** Local request ownership cannot be cleared by a poll of agent activity. */
export function createSendGuard(setPending: (pending: boolean) => void) {
  let pending = false;
  return async (send: () => Promise<void>) => {
    if (pending) return;
    pending = true;
    setPending(true);
    try {
      await send();
    } finally {
      pending = false;
      setPending(false);
    }
  };
}

/** Retry belongs to the displayed conversation, independently of composer navigation. */
export async function retryMessage(
  message: Message,
  load: () => Promise<{
    conversationId: string | null;
    deliveries: NativeDelivery[];
    blocked: boolean;
  }>,
  send: (text: string, conversationId: string) => Promise<void>
) {
  const fresh = await load();
  if (
    fresh.blocked ||
    fresh.conversationId !== message.conversation_id ||
    message.role !== 'user' ||
    !deliveryNotices(fresh.deliveries).get(message.id)?.retryable
  )
    return;
  await send(message.content, message.conversation_id);
}
