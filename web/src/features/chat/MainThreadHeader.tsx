import { useState, type ReactNode } from 'react';
import { cn } from '@/components/ui/cn';
import { agentLabel } from '@/lib/agentLabel';
import { threadStatus, type ThreadStatus } from '@/lib/delivery';
import type { MainThreadInfo } from './types';

const DOT: Record<ThreadStatus, string> = {
  ready: 'bg-term-green',
  idle: 'bg-term-fg-muted',
  unreachable: 'bg-term-red',
  failed: 'bg-term-red',
  unconfirmed: 'bg-term-yellow',
  working: 'bg-term-yellow animate-pulse'
};

/** The words for a status. A failed or unconfirmed last delivery is never called ready. */
export function statusWords(status: ThreadStatus): string {
  if (status === 'failed') return 'last message failed';
  if (status === 'unconfirmed') return 'last message unconfirmed';
  return status;
}

export interface MainThreadHeaderProps {
  info: MainThreadInfo;
  /** The backend is unreachable, so the last state fetched is stale. */
  offline: boolean;
  onStop?: () => void;
  stopping?: boolean;
  /** The native identity strip for the main session; the sessions slice supplies it. */
  renderIdentity?: (sessionId: string) => ReactNode;
}

/** State, model and pending count of the main thread, with the stop button and topic index. */
export function MainThreadHeader({
  info,
  offline,
  onStop,
  stopping = false,
  renderIdentity
}: MainThreadHeaderProps) {
  const [open, setOpen] = useState(false);
  const { native } = info;
  const model = (
    native?.model ?? agentLabel(native?.kind as 'claude' | 'codex' | undefined)
  ).replace(/^claude-/, '');
  const pending = info.topics.reduce((total, topic) => total + topic.pending, 0);
  const status = threadStatus({
    offline,
    deliveries: native?.deliveries ?? [],
    sessionState: native?.session_state
  });

  return (
    <div
      className="border-border bg-card text-muted-foreground shrink-0 border-b text-xs"
      data-testid="main-thread-header"
    >
      <div className="gap-sm px-md flex min-h-11 flex-wrap items-center py-1">
        <span className={cn('size-2 shrink-0', DOT[status])} aria-hidden="true" />
        <span className="text-foreground" data-testid="mt-state" role="status">
          {statusWords(status)}
        </span>
        <span aria-hidden="true">·</span>
        <span data-testid="mt-model">{model}</span>
        {pending > 0 ? (
          <>
            <span aria-hidden="true">·</span>
            <span data-testid="mt-pending">{pending} pending</span>
          </>
        ) : null}
        {onStop && native?.turn_in_flight ? (
          <button
            type="button"
            className="border-border text-foreground hover:text-term-red px-md min-h-11 border disabled:opacity-50"
            disabled={stopping || offline}
            onClick={onStop}
            data-testid="mt-stop-turn"
          >
            {stopping ? 'stopping…' : 'stop'}
          </button>
        ) : null}
        <button
          type="button"
          className="hover:text-term-accent px-sm ml-auto min-h-11"
          aria-expanded={open}
          aria-controls="main-thread-details"
          onClick={() => setOpen((value) => !value)}
          data-testid="mt-details-toggle"
        >
          details {open ? '▴' : '▾'}
        </button>
      </div>

      {open ? (
        <div id="main-thread-details" className="border-border border-t">
          {info.session_id && renderIdentity ? renderIdentity(info.session_id) : null}
          <div className="px-md py-xs font-mono" data-testid="topic-index">
            topics:{' '}
            {info.topics.length ? (
              info.topics.map((topic) => (
                <span key={topic.name} className="mr-md" data-testid="topic-line">
                  {topic.name}
                  {topic.status_line ? ` (${topic.status_line})` : ''} [{topic.pending} pending]
                </span>
              ))
            ) : (
              <span>none yet</span>
            )}
          </div>
        </div>
      ) : null}
    </div>
  );
}
