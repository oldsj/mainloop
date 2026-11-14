import { cn } from '@/components/ui/cn';

/**
 * Mainloop's mark: a prompt and its cursor, the same drawing as the favicon. The prompt takes the
 * accent and the cursor the terminal's green, so it follows the theme. Decorative: the link or
 * heading it sits in carries the name.
 */
export function LogoMark({ className }: { className?: string }) {
  return (
    <svg
      viewBox="0 0 24 24"
      fill="none"
      strokeWidth="2.5"
      aria-hidden="true"
      data-testid="logo-mark"
      className={cn('size-5', className)}
    >
      <path d="M5 6l7 6-7 6" className="stroke-primary" />
      <path d="M13 18h7" className="stroke-term-green" />
    </svg>
  );
}
