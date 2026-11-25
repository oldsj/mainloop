import { CirclePause, CloudOff, LoaderCircle, Moon, type LucideIcon } from 'lucide-react';
import type { ReactNode } from 'react';
import { STATUS_ICON } from '@/components/shared/status-icon';
import { cn } from '@/components/ui/cn';
import { barStatusTone, type BarStatus } from '@/lib/barStatus';

/**
 * Where a status has a shape of its own, it takes it over its tone's: asleep is a moon, paused a
 * pause, unreachable a cloud struck through, and still connecting the working ring in the idle
 * colour.
 */
const OWN_ICON: Partial<Record<BarStatus, LucideIcon>> = {
  asleep: Moon,
  paused: CirclePause,
  unreachable: CloudOff,
  'connecting…': LoaderCircle,
  'reconnecting…': LoaderCircle
};

/** A status that is moving: its ring turns, slowly, unless the owner asked for less motion. */
const TURNING = new Set<BarStatus>([
  'working',
  'starting',
  'waking…',
  'connecting…',
  'reconnecting…'
]);

/**
 * The status as a chip: its icon and its word, the word capitalised and in the status colour when
 * it is the owner's turn or a failure. The word always shows, so colour is never the only signal.
 */
export function SessionState({ status, testId }: { status: BarStatus; testId?: string }) {
  const tone = barStatusTone(status);
  const { icon, className: colour } = STATUS_ICON[tone];
  const Icon = OWN_ICON[status] ?? icon;
  const loud = tone === 'attention' || tone === 'bad';
  return (
    <span
      data-tone={tone}
      className="bg-background gap-xs inline-flex shrink-0 items-center px-1.5 py-0.5 font-medium"
    >
      <Icon
        aria-hidden="true"
        className={cn(
          'size-3.5 shrink-0',
          colour,
          TURNING.has(status) && 'motion-safe:animate-spin motion-safe:[animation-duration:2.5s]'
        )}
      />
      {/* Capitalised by style, so the word read and tested is the status itself. */}
      <span
        role="status"
        data-testid={testId}
        className={cn('inline-block first-letter:uppercase', loud ? colour : 'text-foreground')}
      >
        {status}
      </span>
    </span>
  );
}

export interface SessionBarProps {
  /** Who the session is, ahead of its status: the main thread's mark and name. */
  title?: ReactNode;
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
  title,
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
        'border-border-subtle bg-card text-muted-foreground text-caption shrink-0 border-b',
        className
      )}
      data-testid={testId}
    >
      <div className="gap-sm px-md min-h-control flex flex-wrap items-center py-1">
        {/* The facts wrap inside their own box, so a long status keeps the controls on its row. */}
        <span className="gap-sm flex min-w-0 flex-1 flex-wrap items-center">
          {title ? (
            <span className="gap-sm text-foreground flex shrink-0 items-center">{title}</span>
          ) : null}
          <SessionState status={status} testId={statusTestId} />
          {lead}
        </span>
        {trail ? <span className="gap-sm ml-auto flex flex-wrap items-center">{trail}</span> : null}
      </div>
      {children}
    </div>
  );
}
