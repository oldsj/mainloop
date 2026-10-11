import { CatchBoundary } from '@tanstack/react-router';
import type { ReactNode } from 'react';
import { buttonVariants } from '@/components/ui/button';
import { cn } from '@/components/ui/cn';

/**
 * Keeps one panel's failure inside that panel: a broken sessions list must not take the inbox,
 * the projects list or the main pane down with it. The route's own error boundary still covers
 * the main pane.
 */
export function PanelBoundary({ name, children }: { name: string; children: ReactNode }) {
  return (
    <CatchBoundary
      getResetKey={() => 0}
      errorComponent={({ error, reset }) => (
        <section
          role="alert"
          data-testid={`panel-error-${name.toLowerCase()}`}
          className="p-md gap-sm flex flex-col"
        >
          <h2 className="text-destructive text-sm font-semibold">{name} failed to load</h2>
          <p className="text-muted-foreground text-xs">
            {error instanceof Error && error.message ? error.message : 'Something went wrong.'}
          </p>
          <button
            type="button"
            onClick={reset}
            className={cn(buttonVariants({ variant: 'outline', size: 'sm' }), 'self-start')}
          >
            Try again
          </button>
        </section>
      )}
    >
      {children}
    </CatchBoundary>
  );
}
