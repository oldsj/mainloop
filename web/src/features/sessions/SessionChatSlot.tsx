import { useMemo } from 'react';
import { statusWords } from '@/features/chat/MainThreadHeader';
import { ThreadBody } from '@/features/chat/ThreadBody';
import { useThread, type ThreadSource } from '@/features/chat/useThread';
import { threadStatus } from '@/lib/delivery';
import { useConnection } from '@/shell/ConnectionBanner';

/**
 * The session's conversation: the same timeline and composer as the main thread, reading and
 * sending to this session. The header shows the turn's state and the stop control.
 */
export function SessionChatSlot({ sessionId }: { sessionId: string }) {
  const source = useMemo<ThreadSource>(() => ({ kind: 'session', sessionId }), [sessionId]);
  const thread = useThread(source);
  const offline = useConnection().status === 'offline';
  const { native } = thread;
  const status = threadStatus({
    offline,
    deliveries: native?.deliveries ?? [],
    sessionState: native?.session_state
  });

  return (
    <section
      data-testid="session-chat-slot"
      data-session-id={sessionId}
      aria-label="Session chat"
      className="border-border flex flex-col border"
    >
      <div className="gap-sm px-md border-border text-muted-foreground flex min-h-11 flex-wrap items-center border-b py-1 text-xs">
        <span className="text-foreground" data-testid="session-chat-state" role="status">
          {statusWords(status)}
        </span>
        {thread.busy ? (
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
      {/* A fixed height lets the message list scroll inside the page instead of growing it. */}
      <div className="flex h-[70dvh] min-h-64 flex-col">
        <ThreadBody thread={thread} offline={offline} label="session conversation" />
      </div>
    </section>
  );
}
