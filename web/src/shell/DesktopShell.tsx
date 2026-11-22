import { useRouterState } from '@tanstack/react-router';
import { PanelLeftOpen } from 'lucide-react';
import { useEffect, type ReactNode } from 'react';
import { cn } from '@/components/ui/cn';
import { isShortcut, shortcutLabel } from '@/lib/platform';
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
  workingCount
} from '@/features/explorer';
import { MAIN_ID } from './RouteFocus';
import { PanelBoundary } from './PanelBoundary';
import { StatusBar } from './StatusBar';

/**
 * ≥1024 px, laid out as a terminal workspace manager is: the sidebar (spaces and their agents) on
 * the left, the page beside it under a strip of tabs (the current space's agents), and a status bar
 * along the bottom. There is no header and no icon bar: **menu** in the sidebar opens the command
 * palette, which reaches every page. On home the centre is the Mainloop chat alone, which owns the
 * only scroll; other pages scroll in the centre.
 *
 * The shell owns the explorer's data and prefs, and the sidebar's spaces and agents, because three
 * things read them: the sidebar, the tab strip and the status bar (the counts).
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

  const show = () => update((prev) => ({ ...prev, collapsed: false }));

  return (
    <div data-testid="desktop-shell" className="flex min-h-0 flex-1 flex-col">
      <div className="flex min-h-0 flex-1">
        {prefs.collapsed ? null : (
          <PanelBoundary name="Explorer">
            <ExplorerSidebar
              prefs={prefs}
              update={update}
              column={column}
              ready={explorer.ready}
              failed={explorer.failed}
              needs={needsCount(explorer.nodes)}
              onMenu={onSearch}
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
              leading={
                // With the sidebar hidden, the strip is the only chrome left: it brings it back.
                prefs.collapsed ? (
                  <button
                    type="button"
                    data-testid="show-sidebar"
                    aria-label="Show sidebar"
                    title={`Show sidebar (${shortcutLabel('b')})`}
                    onClick={show}
                    className="text-muted-foreground hover:bg-accent hover:text-accent-foreground px-sm min-h-control focus-visible:ring-ring inline-flex shrink-0 items-center outline-none focus-visible:ring-1 focus-visible:ring-inset"
                  >
                    <PanelLeftOpen className="size-4" aria-hidden="true" />
                  </button>
                ) : null
              }
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
        onShowWorking={show}
      />
    </div>
  );
}
