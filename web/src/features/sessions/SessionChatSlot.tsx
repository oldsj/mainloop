import { useEffect, useMemo, useState } from 'react';
import { SessionRequests } from '@/features/chat/SessionRequests';
import { statusWords } from '@/features/chat/MainThreadHeader';
import { ThreadBody } from '@/features/chat/ThreadBody';
import { useThread, type ThreadSource } from '@/features/chat/useThread';
import { useSessionRequests } from '@/features/hitl/session-requests';
import { threadStatus } from '@/lib/delivery';
import { useConnection } from '@/shell/ConnectionBanner';

/** The panel's share of the visible viewport. */
const PANEL_SHARE = 0.7;

/**
 * The visible viewport's height at scale 1. Pinch-zoom shrinks the visual viewport, so it is scaled
 * back up: zooming in does not shrink the panel.
 */
function visibleHeight(viewport: VisualViewport | null): number | null {
  return viewport ? viewport.height * viewport.scale : null;
}

/**
 * The height of the visible viewport, which the soft keyboard shrinks on iOS (`dvh` does not). Null
 * where the browser has no visual viewport, and the panel then uses `70dvh`.
 */
function useVisibleHeight(): number | null {
  const [height, setHeight] = useState<number | null>(() => visibleHeight(window.visualViewport));
  useEffect(() => {
    const viewport = window.visualViewport;
    if (!viewport) return;
    const update = () => setHeight(visibleHeight(viewport));
    update();
    viewport.addEventListener('resize', update);
    return () => viewport.removeEventListener('resize', update);
  }, []);
  return height;
}

/**
 * The session's conversation: the same timeline and composer as the main thread, reading and
 * sending to this session. The header shows the turn's state and the stop control. Requests the
 * session is waiting on show as cards above the conversation, and the state reads "needs you" while
 * one is pending. A closed session (`closedReason` set) locks the composer, says why, and hides stop.
 */
export function SessionChatSlot({
  sessionId,
  closedReason = null
}: {
  sessionId: string;
  closedReason?: string | null;
}) {
  const source = useMemo<ThreadSource>(() => ({ kind: 'session', sessionId }), [sessionId]);
  const thread = useThread(source);
  const offline = useConnection().status === 'offline';
  const visibleHeight = useVisibleHeight();
  const { native } = thread;
  const status = threadStatus({
    offline,
    deliveries: native?.deliveries ?? [],
    sessionState: native?.session_state
  });
  const { pending } = useSessionRequests(sessionId);
  // A pending request holds the turn until the owner answers, so "working" would mislead.
  const needsYou =
    pending.length > 0 && (status === 'working' || status === 'ready' || status === 'idle');

  return (
    <section
      data-testid="session-chat-slot"
      data-session-id={sessionId}
      aria-label="Session chat"
      className="border-border flex flex-col border"
    >
      <div className="gap-sm px-md border-border text-muted-foreground flex min-h-11 flex-wrap items-center border-b py-1 text-xs">
        <span className="text-foreground" data-testid="session-chat-state" role="status">
          {needsYou ? 'needs you' : statusWords(status)}
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
      {/*
        A fixed height lets the message list scroll inside the page instead of growing it. It follows
        the visible viewport, so the composer stays above a soft keyboard that `dvh` ignores.
      */}
      <div
        className={`flex min-h-64 flex-col ${visibleHeight === null ? 'h-[70dvh]' : ''}`}
        style={
          visibleHeight === null
            ? undefined
            : { height: `${Math.round(visibleHeight * PANEL_SHARE)}px` }
        }
        data-testid="session-chat-panel"
      >
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
