import { Link, useRouterState } from '@tanstack/react-router';
import { Activity } from 'lucide-react';
import type { ReactNode } from 'react';
import { buttonVariants } from '@/components/ui/button';
import { cn } from '@/components/ui/cn';
import { ExplorerAside } from '@/features/explorer';
import { MAIN_ID } from './RouteFocus';
import { PanelBoundary } from './PanelBoundary';
import { ThemeSelector } from './ThemeSelector';

/**
 * ≥1024 px: the main pane, with the explorer on the right. On home the main pane is the Mainloop
 * chat alone, which owns the only scroll; "Since you were here" lives in the explorer. Other pages
 * scroll in the main pane.
 */
export function DesktopShell({
  children,
  lastVisit
}: {
  children: ReactNode;
  lastVisit: number | null;
}) {
  const onHome = useRouterState({ select: (state) => state.location.pathname === '/' });
  return (
    <div data-testid="desktop-shell" className="flex min-h-0 flex-1 flex-col">
      <header className="gap-md border-border px-md flex items-center border-b">
        <Link to="/" className="py-sm text-primary text-sm font-semibold">
          $ mainloop
        </Link>
        <nav className="gap-sm ml-auto flex items-center" aria-label="Primary">
          <Link
            to="/health"
            data-testid="header-health"
            className={cn(buttonVariants({ variant: 'ghost' }))}
          >
            <Activity aria-hidden="true" />
            health
          </Link>
          <ThemeSelector />
        </nav>
      </header>
      <div className="flex min-h-0 flex-1">
        {/* Relative, so an absolutely placed descendant scrolls with it and never stretches the page. */}
        <main
          id={MAIN_ID}
          tabIndex={-1}
          className={cn(
            'relative flex min-w-0 flex-1 flex-col outline-none',
            onHome ? 'overflow-hidden' : 'overflow-y-auto'
          )}
        >
          <div className="min-h-0 flex-1">{children}</div>
        </main>
        <PanelBoundary name="Explorer">
          <ExplorerAside lastVisit={lastVisit} />
        </PanelBoundary>
      </div>
    </div>
  );
}
