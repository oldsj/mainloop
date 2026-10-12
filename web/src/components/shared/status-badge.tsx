import { cn } from '@/components/ui/cn';
import { toneClass, type Tone } from '@/lib/taskState';

export interface StatusBadgeProps {
  /** The words shown. Colour is never the only signal, so the label always carries the meaning. */
  label: string;
  tone: Tone;
  className?: string;
}

/** A status word in its token colour, with a border. Used by task, session and workspace rows. */
export function StatusBadge({ label, tone, className }: StatusBadgeProps) {
  return (
    <span
      className={cn(
        'px-sm py-xs inline-flex items-center border text-xs font-medium tracking-wide uppercase',
        toneClass(tone),
        className
      )}
    >
      {label}
    </span>
  );
}
