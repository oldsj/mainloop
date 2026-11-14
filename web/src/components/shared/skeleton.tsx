import { cn } from '@/components/ui/cn';

/**
 * A placeholder bar for content that is still loading. It is hidden from assistive technology, so
 * the loading status text beside it is what gets announced. The pulse stops for reduced motion.
 */
export function Skeleton({ className }: { className?: string }) {
  return <div aria-hidden="true" className={cn('bg-muted motion-safe:animate-pulse', className)} />;
}

/** Line widths for placeholder rows, cycled so a list does not look like a solid block. */
const WIDTHS = ['w-1/2', 'w-2/3', 'w-2/5', 'w-3/5'] as const;

/**
 * Placeholder list rows in the shape of the real ones: a dot and a line, at the control height, so
 * the list does not jump when its rows arrive. Hidden from assistive technology; the caller keeps a
 * `role="status"` line (it may be visually hidden) that says what is loading.
 */
export function SkeletonRows({ rows = 5, testId }: { rows?: number; testId?: string }) {
  return (
    <div aria-hidden="true" data-testid={testId} className="flex flex-col">
      {Array.from({ length: rows }, (_, index) => (
        <div key={index} className="gap-sm px-sm min-h-control flex items-center">
          <span className="bg-muted size-2 shrink-0" />
          <span
            className={cn('bg-muted h-3 motion-safe:animate-pulse', WIDTHS[index % WIDTHS.length])}
          />
        </div>
      ))}
    </div>
  );
}

/**
 * Placeholder for the top of a detail page, in the shape of its header: a status, a title, a line
 * of facts and a block of text. Hidden from assistive technology, like `SkeletonRows`.
 */
export function SkeletonHeader({ testId }: { testId?: string }) {
  return (
    <div aria-hidden="true" data-testid={testId} className="gap-sm flex flex-col">
      <Skeleton className="h-6 w-24" />
      <Skeleton className="h-5 w-2/5" />
      <Skeleton className="h-3 w-1/4" />
      <Skeleton className="h-10 w-full" />
    </div>
  );
}
