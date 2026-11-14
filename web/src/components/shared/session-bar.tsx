import type { ReactNode } from 'react';
import { cn } from '@/components/ui/cn';
import { barStatusDot, type BarStatus } from '@/lib/barStatus';

export interface SessionBarProps {
  /** The one status of the session. The word always shows, so colour is never the only signal. */
  status: BarStatus;
  /** `data-testid` of the status word. */
  statusTestId?: string;
  /** Facts that follow the status, such as the pending count. */
  lead?: ReactNode;
  /** Controls at the end of the row: wake, stop, the provider and model pickers, session info. */
  trail?: ReactNode;
  /** Rows under the bar: a provider problem, and the session info when it is open. */
  children?: ReactNode;
  className?: string;
  'data-testid'?: string;
}

/**
 * The single row above a session's conversation: its status, then its facts, then its controls.
 * The main thread and the live view both draw it, so there is one row whichever is showing.
 */
export function SessionBar({
  status,
  statusTestId,
  lead,
  trail,
  children,
  className,
  'data-testid': testId
}: SessionBarProps) {
  return (
    <div
      className={cn(
        // Chrome, even inside the monospace live view.
        'border-border-subtle bg-card text-muted-foreground shrink-0 border-b font-sans text-xs',
        className
      )}
      data-testid={testId}
    >
      <div className="gap-sm px-md min-h-control flex flex-wrap items-center py-1">
        <span className={cn('size-2 shrink-0', barStatusDot(status))} aria-hidden="true" />
        <span className="text-foreground" role="status" data-testid={statusTestId}>
          {status}
        </span>
        {lead}
        {trail ? <span className="gap-sm ml-auto flex flex-wrap items-center">{trail}</span> : null}
      </div>
      {children}
    </div>
  );
}
