import { Check, Loader2, X } from 'lucide-react';
import { useEffect, useState } from 'react';
import type { ProgressFrame } from '@/api';
import { Markdown } from '@/components/markdown/markdown';
import { cn } from '@/components/ui/cn';
import { agentLabel } from '@/lib/agentLabel';
import { elapsedSince } from '@/lib/time';

/** Tool lines shown at once; earlier ones collapse into a count. */
export const VISIBLE_TOOLS = 6;

export interface LiveTurnProps {
  frame: ProgressFrame;
  context?: string;
  agentKind?: 'claude' | 'codex' | null;
}

/** Re-renders each second while `running`, so the elapsed time counts up without a server frame. */
function useNow(running: boolean): number {
  const [now, setNow] = useState(Date.now);
  useEffect(() => {
    if (!running) return;
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [running]);
  return now;
}

/**
 * The turn that is still running: the reply as it grows, the tool calls as compact lines and a
 * status line with the elapsed time. It is dimmed, because it is not the stored reply.
 *
 * Screen readers hear only the status label, through the polite region. The growing text and the
 * ticking timer are hidden from them, and the finished reply is announced as a new message.
 */
export function LiveTurn({ frame, context = 'main', agentKind }: LiveTurnProps) {
  const running = frame.active;
  const now = useNow(running);
  const hidden = frame.tools.length - VISIBLE_TOOLS;
  const tools = frame.tools.slice(-VISIBLE_TOOLS);

  return (
    <article
      aria-label={`${agentLabel(agentKind)}, working`}
      aria-busy={running}
      data-testid="live-turn"
      className="px-md py-sm bg-card w-full border-l-2 border-dashed opacity-80"
    >
      <div className="gap-xs flex flex-col">
        <span className="text-term-accent shrink-0 text-xs md:text-sm" aria-hidden="true">
          {agentLabel(agentKind)}@{context}$
        </span>
        {frame.text ? (
          <div className="min-w-0 flex-1" aria-hidden="true" data-testid="live-turn-text">
            <Markdown className="text-foreground text-sm md:text-base">
              {frame.text_truncated ? `…${frame.text}` : frame.text}
            </Markdown>
          </div>
        ) : null}
        {tools.length > 0 ? (
          <ul
            className="text-muted-foreground gap-0 text-xs"
            aria-hidden="true"
            data-testid="live-turn-tools"
          >
            {hidden > 0 ? <li className="truncate">+{hidden} earlier</li> : null}
            {tools.map((tool) => (
              <li key={tool.id || tool.label} className="gap-xs flex min-w-0 items-center">
                {tool.state === 'running' ? (
                  <Loader2 className="size-3 shrink-0 motion-safe:animate-spin" />
                ) : tool.state === 'error' ? (
                  <X className="text-destructive size-3 shrink-0" />
                ) : (
                  <Check className="size-3 shrink-0" />
                )}
                <span
                  className={cn('min-w-0 truncate', tool.state === 'error' && 'text-destructive')}
                >
                  {tool.label}
                </span>
              </li>
            ))}
          </ul>
        ) : null}
        <p className="text-muted-foreground gap-xs flex min-w-0 items-baseline text-xs">
          <span
            role="status"
            aria-live="polite"
            className="min-w-0 truncate"
            data-testid="live-turn-status"
          >
            {frame.status.label}
          </span>
          <span
            aria-hidden="true"
            className="shrink-0 tabular-nums"
            data-testid="live-turn-elapsed"
          >
            {elapsedSince(frame.started_at, now)}
          </span>
        </p>
      </div>
    </article>
  );
}
