import { Link, useNavigate, useRouterState } from '@tanstack/react-router';
import { Activity, Compass, Inbox, MessageSquare, Settings, type LucideIcon } from 'lucide-react';
import { useState, type ReactNode } from 'react';
import { buttonVariants } from '@/components/ui/button';
import { cn } from '@/components/ui/cn';
import { ExplorerPane, useNeedsCount } from '@/features/explorer';
import { badgeText } from '@/features/inbox/model';
import { MAIN_ID } from './RouteFocus';
import { PanelBoundary } from './PanelBoundary';
import { ThemeSelector } from './ThemeSelector';
import { activeNavTab, type NavTab, type Tab } from './view';

/**
 * The tab bar's three items. Chat is the main chat, Needs you is the answer view with its count,
 * and Explorer is the tree. The third item's label is the one word here that is still open: see
 * `EXPLORER_TAB_LABEL`.
 */
export const EXPLORER_TAB_LABEL = 'Explorer';

const TAB_ITEMS: readonly { id: NavTab; label: string; Icon: LucideIcon }[] = [
  { id: 'chat', label: 'Chat', Icon: MessageSquare },
  { id: 'needs', label: 'Needs you', Icon: Inbox },
  { id: 'explorer', label: EXPLORER_TAB_LABEL, Icon: Compass }
];

/** The tab's name. Needs you carries its count, so a screen reader announces it. */
function tabName(id: NavTab, label: string, needs: number): string {
  return id === 'needs' && needs > 0 ? `${label}, ${needs}` : label;
}

/** Hides a pane's content without unmounting it, so its state survives switching away. */
function TabPane({ active, children }: { active: boolean; children: ReactNode }) {
  return (
    <div hidden={!active} className="h-full">
      {children}
    </div>
  );
}

/**
 * <1024 px. There is no header: the page starts at the top of the screen, so on the main chat the
 * one row above the thread is the session row. A bottom tab bar has three items, and the lit one
 * follows where the owner is:
 *
 * - **Chat** opens the main chat.
 * - **Needs you** opens the answer view and carries the count, so attention keeps polling.
 * - **Explorer** shows the tree. It is also lit on a page opened from the tree (a task, a session,
 *   a project, health), where tapping it brings the tree back.
 *
 * Two panes sit under the bar: the routed page and the tree (`?tab=explorer`). Each is built the
 * first time it opens and stays mounted, so an open tree survives a switch. Health and the theme
 * are in the tree's header.
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
  const pathname = useRouterState({ select: (state) => state.location.pathname });
  const onHome = pathname === '/';
  const active = activeNavTab(tab, pathname);
  const needs = useNeedsCount();
  const [opened, setOpened] = useState<ReadonlySet<Tab>>(() => new Set([tab]));
  // Adjusts state during render, the React way to derive it from a prop: the first visit adds the tab.
  if (!opened.has(tab)) setOpened(new Set([...opened, tab]));

  const go = (id: NavTab) => {
    // Each item leads to one place. Chat and Needs you are pages, so they start the search afresh
    // (the root keeps `?view=`). The tree is a pane over the current page, so it keeps the route.
    if (id === 'chat') void navigate({ to: '/', search: {} });
    else if (id === 'needs') void navigate({ to: '/inbox', search: {} });
    else
      void navigate({
        to: '.',
        search: (prev: Record<string, unknown>) => ({ ...prev, tab: 'explorer' as const })
      });
  };

  return (
    <div data-testid="compact-shell" className="flex min-h-0 flex-1 flex-col">
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
              <ExplorerPane
                lastVisit={lastVisit}
                title={EXPLORER_TAB_LABEL}
                actions={
                  <>
                    <Link
                      to="/health"
                      data-testid="header-health"
                      aria-label="Orchestration health"
                      className={cn(buttonVariants({ variant: 'ghost', size: 'icon' }))}
                    >
                      <Activity aria-hidden="true" />
                    </Link>
                    <Link
                      to="/settings"
                      data-testid="header-settings"
                      aria-label="Settings"
                      className={cn(buttonVariants({ variant: 'ghost', size: 'icon' }))}
                    >
                      <Settings aria-hidden="true" />
                    </Link>
                    <ThemeSelector />
                  </>
                }
              />
            </PanelBoundary>
          </TabPane>
        )}
      </main>
      <nav
        data-testid="mobile-tab-bar"
        aria-label="Sections"
        className="border-border-subtle bg-card flex border-t pb-[env(safe-area-inset-bottom)]"
      >
        {TAB_ITEMS.map(({ id, label, Icon }) => (
          <button
            key={id}
            type="button"
            data-testid={`tab-${id}`}
            aria-label={tabName(id, label, needs)}
            aria-pressed={active === id}
            onClick={() => go(id)}
            className={cn(
              'gap-xs text-2xs flex min-h-11 flex-1 flex-col items-center justify-center',
              active === id
                ? 'bg-accent text-accent-foreground font-semibold'
                : 'text-muted-foreground'
            )}
          >
            <span className="relative inline-flex">
              <Icon className="size-4" aria-hidden="true" />
              {id === 'needs' && needs > 0 ? (
                <span
                  data-testid="needs-badge"
                  aria-hidden="true"
                  className="bg-destructive text-destructive-foreground text-2xs absolute -top-1.5 left-3 inline-flex min-w-4 items-center justify-center px-0.5 leading-4 font-semibold tabular-nums"
                >
                  {badgeText(needs)}
                </span>
              ) : null}
            </span>
            {label}
          </button>
        ))}
      </nav>
    </div>
  );
}
