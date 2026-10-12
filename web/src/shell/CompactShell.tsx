import { Link, useNavigate, useRouterState } from '@tanstack/react-router';
import { Activity, Compass, MessageSquare } from 'lucide-react';
import { useState, type ReactNode } from 'react';
import { buttonVariants } from '@/components/ui/button';
import { cn } from '@/components/ui/cn';
import { ExplorerPane, useNeedsCount } from '@/features/explorer';
import { MAIN_ID } from './RouteFocus';
import { PanelBoundary } from './PanelBoundary';
import { ThemeSelector } from './ThemeSelector';
import type { Tab } from './view';

const TAB_ITEMS = [
  { tab: 'chat', label: 'Chat', Icon: MessageSquare },
  { tab: 'explorer', label: 'Explorer', Icon: Compass }
] as const satisfies readonly { tab: Tab; label: string; Icon: unknown }[];

/** The tab's name. The Explorer tab carries the Needs-you count, so a screen reader announces it. */
function tabName(id: Tab, label: string, needs: number): string {
  if (id !== 'explorer' || needs === 0) return label;
  return `${label}, ${needs} need you`;
}

/** Hides a tab's content without unmounting it, so its state survives switching away. */
function TabPane({ active, children }: { active: boolean; children: ReactNode }) {
  return (
    <div hidden={!active} className="h-full">
      {children}
    </div>
  );
}

/**
 * <1024 px: two panes, Chat and Explorer, switched by a bottom tab bar. The tab lives in the URL.
 * A pane is built the first time it opens and stays mounted after that, so a draft survives a
 * switch. The Explorer tab always shows the Needs-you count, so attention keeps polling while the
 * chat is open. On home the Chat tab is the Mainloop chat alone; "Since you were here" lives in the explorer.
 */
export function CompactShell({
  tab,
  lastVisit,
  children
}: {
  tab: Tab;
  lastVisit: number | null;
  children: ReactNode;
}) {
  const navigate = useNavigate();
  const onHome = useRouterState({ select: (state) => state.location.pathname === '/' });
  const needs = useNeedsCount();
  const [opened, setOpened] = useState<ReadonlySet<Tab>>(() => new Set([tab]));
  // Adjusts state during render, the React way to derive it from a prop: the first visit adds the tab.
  if (!opened.has(tab)) setOpened(new Set([...opened, tab]));
  return (
    <div data-testid="compact-shell" className="flex min-h-0 flex-1 flex-col">
      <header className="gap-sm border-border px-md flex items-center border-b">
        <Link
          to="/"
          search={(prev) => ({ ...prev, tab: undefined })}
          className="py-sm text-primary text-sm font-semibold"
        >
          $ mainloop
        </Link>
        <nav className="gap-xs ml-auto flex items-center" aria-label="Primary">
          <Link
            to="/health"
            data-testid="header-health"
            aria-label="Orchestration health"
            className={cn(buttonVariants({ variant: 'ghost', size: 'icon' }))}
          >
            <Activity aria-hidden="true" />
          </Link>
          <ThemeSelector />
        </nav>
      </header>
      {/* The home chat owns its scroll, so the pane around it does not scroll as well. It is the
          containing block too: an absolutely placed descendant (an sr-only label) would otherwise
          stretch the document, and scrolling a row into view would scroll the whole page. */}
      <main
        id={MAIN_ID}
        tabIndex={-1}
        className={cn(
          'relative min-h-0 flex-1 outline-none',
          onHome && tab === 'chat' ? 'overflow-hidden' : 'overflow-y-auto'
        )}
      >
        {opened.has('chat') && (
          <TabPane active={tab === 'chat'}>
            <div className="h-full min-h-0">{children}</div>
          </TabPane>
        )}
        {opened.has('explorer') && (
          <TabPane active={tab === 'explorer'}>
            <PanelBoundary name="Explorer">
              <ExplorerPane lastVisit={lastVisit} />
            </PanelBoundary>
          </TabPane>
        )}
      </main>
      <nav
        data-testid="mobile-tab-bar"
        aria-label="Sections"
        className="border-border flex border-t pb-[env(safe-area-inset-bottom)]"
      >
        {TAB_ITEMS.map(({ tab: id, label, Icon }) => (
          <button
            key={id}
            type="button"
            data-testid={`tab-${id}`}
            aria-label={tabName(id, label, needs)}
            aria-pressed={tab === id}
            onClick={() =>
              void navigate({
                to: '.',
                search: (prev: Record<string, unknown>) => ({
                  ...prev,
                  tab: id === 'chat' ? undefined : id
                })
              })
            }
            className={cn(
              'gap-xs flex min-h-11 flex-1 flex-col items-center justify-center text-xs',
              tab === id ? 'bg-accent text-accent-foreground font-bold' : 'text-muted-foreground'
            )}
          >
            {/* The icon takes the middle column, so the badge beside it does not pull it off centre. */}
            <span className="gap-xs grid w-full grid-cols-[minmax(0,1fr)_auto_minmax(0,1fr)] items-center">
              <Icon className="col-start-2 size-4" aria-hidden="true" />
              {id === 'explorer' && needs > 0 && (
                <span
                  data-testid="explorer-needs-badge"
                  aria-hidden="true"
                  className="border-term-yellow text-term-yellow col-start-3 justify-self-start border px-1 text-xs tabular-nums"
                >
                  {needs}
                </span>
              )}
            </span>
            [{label.toUpperCase()}]
          </button>
        ))}
      </nav>
    </div>
  );
}
