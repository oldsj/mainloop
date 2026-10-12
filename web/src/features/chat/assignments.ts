import type { Message } from '@/lib/contract';

/**
 * The messages Mainloop sent on the owner's behalf. A message's `source` says who wrote it: the
 * owner's own are `user`, and a message without one (an optimistic send, an assistant reply) is not
 * an assignment. Anything else, such as a delegated brief, is shown as an assignment rather than as
 * a message from "You". The source comes with the message, so it holds however long the thread is.
 */
export function assignmentIds(messages: Message[]): ReadonlySet<string> {
  return new Set(
    messages
      .filter((message) => message.source !== undefined && message.source !== 'user')
      .map((message) => message.id)
  );
}
