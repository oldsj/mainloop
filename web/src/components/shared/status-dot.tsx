import { cn } from '@/components/ui/cn';
import type { Tone } from '@/lib/taskState';

/**
 * The status language, one dot per row: filled amber is working, filled red needs the owner
 * (including a failure that needs a decision), filled teal is finished and not yet looked at, and
 * hollow is at rest. Hollow is the default, so an unknown state never reads as a problem. Green is
 * the connection light only.
 */
export const STATUS_DOT: Record<Tone, string> = {
  active: 'bg-status-working',
  attention: 'bg-status-attention',
  bad: 'bg-status-failed',
  unseen: 'bg-status-unseen',
  ok: 'bg-term-green',
  muted: 'border border-status-idle'
};

/** The dot itself. It is decoration: the row's words always say the state too. */
export function StatusDot({ tone, testId }: { tone: Tone; testId?: string }) {
  return (
    <span
      aria-hidden="true"
      data-testid={testId}
      data-tone={tone}
      className={cn('size-2 shrink-0', STATUS_DOT[tone])}
    />
  );
}
