import { Link } from '@tanstack/react-router';
import { Activity, Plus } from 'lucide-react';
import type { ReactNode } from 'react';
import { buttonVariants } from '@/components/ui/button';
import { cn } from '@/components/ui/cn';
import { MAIN_ID } from './RouteFocus';
import { PanelBoundary } from './PanelBoundary';
import { InboxPanel, ProjectsPanel, SessionsPanel } from './slots';
import { ThemeSelector } from './ThemeSelector';

/** ≥1024 px: main pane plus a column of sessions, inbox and projects. */
export function DesktopShell({ children }: { children: ReactNode }) {
  return (
    <div data-testid="desktop-shell" className="flex min-h-0 flex-1 flex-col">
      <header className="gap-md border-border px-md flex items-center border-b">
        <Link to="/" className="py-sm text-primary text-sm font-semibold">
          $ mainloop
        </Link>
        <nav className="gap-sm ml-auto flex items-center" aria-label="Primary">
          <Link
            to="/agents"
            data-testid="header-new-agent"
            className={cn(buttonVariants({ variant: 'ghost' }))}
          >
            <Plus aria-hidden="true" />
            new agent session
          </Link>
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
        <main id={MAIN_ID} tabIndex={-1} className="min-w-0 flex-1 overflow-y-auto outline-none">
          {children}
        </main>
        <aside
          aria-label="Sessions, inbox and projects"
          className="divide-border border-border flex w-96 shrink-0 flex-col divide-y overflow-y-auto border-l"
        >
          <PanelBoundary name="Sessions">
            <SessionsPanel />
          </PanelBoundary>
          <PanelBoundary name="Inbox">
            <InboxPanel />
          </PanelBoundary>
          <PanelBoundary name="Projects">
            <ProjectsPanel />
          </PanelBoundary>
        </aside>
      </div>
    </div>
  );
}
