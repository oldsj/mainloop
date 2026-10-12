import { useMemo } from 'react';
import { SessionRequests } from '@/features/chat/SessionRequests';
import { statusWords } from '@/features/chat/MainThreadHeader';
import { ThreadBody } from '@/features/chat/ThreadBody';
import { useThread, type ThreadSource } from '@/features/chat/useThread';
import { useSessionRequests } from '@/features/hitl/session-requests';
import { threadStatus } from '@/lib/delivery';
import { useConnection } from '@/shell/ConnectionBanner';

/**
 * The session's conversation: the same timeline and composer as the main thread, reading and
 * sending to this session. The header shows the turn's state and the stop control. Requests the
 * session is waiting on show as cards above the conversation, and the state reads "needs you" while
 * one is pending. A closed session (`closedReason` set) locks the composer, says why, and hides stop.
 * A cancelled session reads `cancelled`, never the agent's own "ready".
 *
 * The slot takes the height its parent gives it. The message list scrolls inside it and the composer
 * stays at its bottom edge.
 */
export function SessionChatSlot({
  sessionId,
  closedReason = null,
  cancelled = false
}: {
  sessionId: string;
  closedReason?: string | null;
  cancelled?: boolean;
}) {
  const source = useMemo<ThreadSource>(() => ({ kind: 'session', sessionId }), [sessionId]);
  const thread = useThread(source);
  const offline = useConnection().status === 'offline';
  const { native } = thread;
  const status = threadStatus({
    offline,
    deliveries: native?.deliveries ?? [],
    sessionState: native?.session_state
  });
  const { pending } = useSessionRequests(sessionId);
  // A pending request holds the turn until the owner answers, so "working" would mislead.
  const needsYou =
    !cancelled &&
    pending.length > 0 &&
    (status === 'working' || status === 'ready' || status === 'idle');

  return (
    <section
      data-testid="session-chat-slot"
      data-session-id={sessionId}
      aria-label="Session chat"
      className="border-border flex min-h-0 flex-1 flex-col border"
    >
      <div className="gap-sm px-md border-border text-muted-foreground flex min-h-11 flex-wrap items-center border-b py-1 text-xs">
        <span className="text-foreground" data-testid="session-chat-state" role="status">
          {cancelled ? 'cancelled' : needsYou ? 'needs you' : statusWords(status)}
        </span>
        {thread.busy && closedReason === null ? (
          <button
            type="button"
            className="border-border text-foreground hover:text-term-red px-md min-h-11 border disabled:opacity-50"
            disabled={thread.stopping || offline}
            onClick={thread.stop}
            data-testid="session-chat-stop"
          >
            {thread.stopping ? 'stopping…' : 'stop'}
          </button>
        ) : null}
      </div>
      <SessionRequests sessionId={sessionId} />
      <div className="flex min-h-40 flex-1 flex-col" data-testid="session-chat-panel">
        <ThreadBody
          thread={thread}
          offline={offline}
          label="session conversation"
          closedReason={closedReason}
        />
      </div>
    </section>
  );
}
