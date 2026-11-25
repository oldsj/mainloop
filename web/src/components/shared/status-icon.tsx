import {
  Circle,
  CircleAlert,
  CircleCheck,
  CircleX,
  LoaderCircle,
  type LucideIcon
} from 'lucide-react';
import { cn } from '@/components/ui/cn';
import type { Tone } from '@/lib/taskState';

/**
 * The status language as icons: each state has its own shape as well as its colour, so it reads
 * without colour too. An alert is the owner's turn (a question, a decision, a block), a cross a
 * failure, a broken ring work in progress, a check finished, and an empty ring at rest. Empty is the
 * default, so an unknown state never reads as a problem.
 */
export const STATUS_ICON: Record<Tone, { icon: LucideIcon; className: string; word: string }> = {
  attention: { icon: CircleAlert, className: 'text-status-attention', word: 'Needs you' },
  bad: { icon: CircleX, className: 'text-status-failed', word: 'Failed' },
  active: { icon: LoaderCircle, className: 'text-status-working', word: 'Working' },
  unseen: { icon: CircleCheck, className: 'text-status-unseen', word: 'Finished' },
  ok: { icon: CircleCheck, className: 'text-term-green', word: 'Done' },
  muted: { icon: Circle, className: 'text-status-idle', word: 'Idle' }
};

/** The icon itself. It is decoration: the row's words always say the state too. */
export function StatusIcon({
  tone,
  className,
  testId
}: {
  tone: Tone;
  className?: string;
  testId?: string;
}) {
  const { icon: Icon, className: colour } = STATUS_ICON[tone];
  return (
    <Icon
      aria-hidden="true"
      data-testid={testId}
      data-tone={tone}
      className={cn('size-4 shrink-0', colour, className)}
    />
  );
}
