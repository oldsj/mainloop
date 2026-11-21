import { useSyncExternalStore } from 'react';
import type { SseStatus } from './sse';

/**
 * Whether the `/events` stream is open right now. While it is, changes arrive as events and a poll
 * is only a safety net, so a poll of something large can run far less often. The stream's watchdog
 * drops a connection that has gone silent (`SSE_IDLE_TIMEOUT_MS`), so "open" cannot outlive a dead
 * connection by more than that.
 */
function createEventStream() {
  let open = false;
  const listeners = new Set<() => void>();
  return {
    /** The SSE client's status callback. */
    set(status: SseStatus) {
      const next = status === 'open';
      if (next === open) return;
      open = next;
      for (const listener of listeners) listener();
    },
    isOpen: () => open,
    subscribe(listener: () => void) {
      listeners.add(listener);
      return () => {
        listeners.delete(listener);
      };
    }
  };
}

export const eventStream = createEventStream();

export function useEventStreamOpen(): boolean {
  return useSyncExternalStore(eventStream.subscribe, eventStream.isOpen, () => false);
}
