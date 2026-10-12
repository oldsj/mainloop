import { Link, useRouterState } from '@tanstack/react-router';
import { Activity } from 'lucide-react';
import type { ReactNode } from 'react';
import { buttonVariants } from '@/components/ui/button';
import { cn } from '@/components/ui/cn';
import { HomeStrip } from '@/features/home';
import { ChatStarters, ExplorerAside } from '@/features/explorer';
import { MAIN_ID } from './RouteFocus';
import { PanelBoundary } from './PanelBoundary';
import { ThemeSelector } from './ThemeSelector';

/**
 * ≥1024 px: the main pane, with the explorer on the right. The "Since you were here" line sits
 * above the home thread, as it does on the phone.
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
        <main
          id={MAIN_ID}
          tabIndex={-1}
          className="flex min-w-0 flex-1 flex-col overflow-y-auto outline-none"
        >
          {onHome && (
            <PanelBoundary name="Since you were here">
              <HomeStrip lastVisit={lastVisit} />
            </PanelBoundary>
          )}
          <div className="flex min-h-0 flex-1 flex-col">
            {onHome && <ChatStarters />}
            <div className="min-h-0 flex-1">{children}</div>
          </div>
        </main>
        <PanelBoundary name="Explorer">
          <ExplorerAside />
        </PanelBoundary>
      </div>
    </div>
  );
}
