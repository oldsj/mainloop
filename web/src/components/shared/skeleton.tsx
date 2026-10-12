import { cn } from '@/components/ui/cn';

/**
 * A placeholder bar for content that is still loading. It is hidden from assistive technology, so
 * the loading status text beside it is what gets announced. The pulse stops for reduced motion.
 */
export function Skeleton({ className }: { className?: string }) {
  return <div aria-hidden="true" className={cn('bg-muted motion-safe:animate-pulse', className)} />;
}
