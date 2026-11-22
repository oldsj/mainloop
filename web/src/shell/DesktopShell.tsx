import { useRouterState } from '@tanstack/react-router';
import { useEffect, type ReactNode } from 'react';
import { cn } from '@/components/ui/cn';
import { isShortcut } from '@/lib/platform';
import {
  ExplorerSidebar,
  needsCount,
  shellCounts,
  TabStrip,
  useAgentColumn,
  useAsk,
  useExplorer,
  useExplorerPrefs,
  useOpenTarget,
  workingCount,
  type SidebarView
} from '@/features/explorer';
import { ActivityBar } from './ActivityBar';
import { MAIN_ID } from './RouteFocus';
import { PanelBoundary } from './PanelBoundary';
import { StatusBar } from './StatusBar';

/**
 * ≥1024 px, laid out as an IDE is: an activity bar down the left edge, the sidebar beside it, the
 * page in the centre under a strip of tabs (the current space's agents), and a status bar along the
 * bottom. There is no top header. On home the centre is the Mainloop chat alone, which owns the
 * only scroll; other pages scroll in the centre.
 *
 * The shell owns the explorer's data and prefs, and the sidebar's spaces and agents, because three
 * things read them: the activity bar (the Needs-you count and which view shows), the sidebar, and
 * the status bar (the counts).
 */
export function DesktopShell({
  children,
  lastVisit,
  onSearch
}: {
  children: ReactNode;
  lastVisit: number | null;
  /** Opens the command palette. */
  onSearch: () => void;
}) {
  const pathname = useRouterState({ select: (state) => state.location.pathname });
  const onHome = pathname === '/';
  const open = useOpenTarget();
  const ask = useAsk();
  const { prefs, update } = useExplorerPrefs();
  const explorer = useExplorer(lastVisit);
  const counts = shellCounts(explorer.nodes);
  const column = useAgentColumn();

  // Choosing the view that is showing hides the sidebar; any other choice shows that view.
  const chooseView = (view: SidebarView) =>
    update((prev) =>
      !prev.collapsed && prev.view === view
        ? { ...prev, collapsed: true }
        : { ...prev, view, collapsed: false }
    );

  // ⌘B on a Mac, Ctrl+B elsewhere, hides and shows the sidebar, as in an editor. Never both on one
  // platform: on a Mac, Ctrl+B in a text field is the system's "back one character" and must be left
  // alone. ⌘B works while typing.
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (!isShortcut(event, 'b')) return;
      event.preventDefault();
      update((prev) => ({ ...prev, collapsed: !prev.collapsed }));
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [update]);

  return (
    <div data-testid="desktop-shell" className="flex min-h-0 flex-1 flex-col">
      <div className="flex min-h-0 flex-1">
        <ActivityBar
          view={prefs.view}
          collapsed={prefs.collapsed}
          needs={needsCount(explorer.nodes)}
          onView={chooseView}
          onSearch={onSearch}
        />
        {prefs.collapsed ? null : (
          <PanelBoundary name="Explorer">
            <ExplorerSidebar
              prefs={prefs}
              update={update}
              nodes={explorer.nodes}
              column={column}
              ready={explorer.ready}
              failed={explorer.failed}
            />
          </PanelBoundary>
        )}
        <div className="flex min-w-0 flex-1 flex-col">
          <PanelBoundary name="Tabs">
            <TabStrip
              model={column.model}
              space={column.space}
              page={column.page}
              pathname={pathname}
              onOpen={open}
              onAsk={ask}
            />
          </PanelBoundary>
          {/* Relative, so an absolutely placed descendant scrolls with it and never stretches the page. */}
          <main
            id={MAIN_ID}
            tabIndex={-1}
            className={cn(
              'relative flex min-h-0 min-w-0 flex-1 flex-col outline-none',
              onHome ? 'overflow-hidden' : 'overflow-y-auto'
            )}
          >
            <div className="min-h-0 flex-1">{children}</div>
          </main>
        </div>
      </div>
      <StatusBar
        needs={counts.needs}
        // What has an amber dot in the column, so the count and the column agree.
        working={workingCount(column.model.agents)}
        problems={counts.problems}
        counts={explorer.ready ? 'ready' : explorer.failed ? 'failed' : 'loading'}
        onShowWorking={() => update((prev) => ({ ...prev, view: 'sessions', collapsed: false }))}
      />
    </div>
  );
}
