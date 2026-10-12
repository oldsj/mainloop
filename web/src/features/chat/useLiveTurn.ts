import { useSyncExternalStore } from 'react';
import { liveProgress, type ProgressFrame } from '@/api';
import type { Message } from '@/lib/contract';

/**
 * The live frame to show for a thread, or undefined.
 *
 * A running turn shows while the thread says a turn is in flight (a frame the server never closed
 * cannot outlive that). A closed turn that names its reply keeps showing until that message is in
 * the list, so the reply takes the bubble's place in one render, with no gap and no duplicate.
 */
export function useLiveTurn(
  sessionId: string | null,
  busy: boolean,
  messages: readonly Message[]
): ProgressFrame | undefined {
  const frame = useSyncExternalStore(
    liveProgress.subscribe,
    () => liveProgress.get(sessionId),
    () => undefined
  );
  if (!frame) return undefined;
  if (frame.active) return busy ? frame : undefined;
  return frame.reply_id && !messages.some((m) => m.id === frame.reply_id) ? frame : undefined;
}
