import { useRouterState } from '@tanstack/react-router';
import { PanelLeftOpen } from 'lucide-react';
import { useEffect, type ReactNode } from 'react';
import { cn } from '@/components/ui/cn';
import { isApplePlatform, isShortcut, sidebarShortcutLabel } from '@/lib/platform';
import {
  ExplorerSidebar,
  loadingTabs,
  newTaskPrefill,
  spaceTabs,
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
import { PREFIX_HINT, prefixAction, usePrefixKeys } from './prefixKeys';
import { StatusBar } from './StatusBar';

/**
 * ≥1024 px, laid out as a terminal workspace manager is: the sidebar (spaces and their agents) on
 * the left, the page beside it under a strip of tabs (the current space's agents), and a status bar
 * along the bottom. There is no header and no icon bar: **menu** in the sidebar opens the command
 * palette, which reaches every page. On home the centre is the Mainloop chat alone, which owns the
 * only scroll; other pages scroll in the centre.
 *
 * The shell owns the explorer's counts and prefs, and the sidebar's spaces and agents, because three
 * things read them: the sidebar, the tab strip and the status bar (the counts).
 */
export function DesktopShell({
  children,
  onSearch
}: {
  children: ReactNode;
  /** Opens the command palette. */
  onSearch: () => void;
}) {
  const pathname = useRouterState({ select: (state) => state.location.pathname });
  const onHome = pathname === '/';
  const open = useOpenTarget();
  const ask = useAsk();
  const { prefs, update } = useExplorerPrefs();
  const explorer = useExplorer();
  const column = useAgentColumn();
  // The lists and the tabs wait for the projects as well as the tasks: an agent drawn before its
  // project is known would sit under the main thread's space and then move.
  const listed = explorer.ready && explorer.placed;

  // ⌘B on a Mac hides and shows the sidebar, as in an editor, and works while typing. Off a Mac
  // the editor's key for it would be Ctrl+B, which is the prefix (below): there the sidebar is the
  // prefix and then B, as it also is on a Mac.
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (!isApplePlatform() || !isShortcut(event, 'b')) return;
      event.preventDefault();
      update((prev) => ({ ...prev, collapsed: !prev.collapsed }));
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [update]);

  const show = () => update((prev) => ({ ...prev, collapsed: false }));

  // Ctrl+B, then one key, as a terminal workspace manager has it: n and p move between the tabs,
  // a number goes to that tab, c starts new work, b hides the sidebar, w goes to the spaces, and g
  // opens the menu. The status bar lists them while the prefix waits.
  const tabs = listed
    ? spaceTabs(column.model, column.space, column.page, pathname)
    : loadingTabs(column.model, column.page, pathname);
  const armed = usePrefixKeys((key) => {
    const action = prefixAction(key, tabs);
    if (!action) return false;
    if (action.do === 'open') open(action.target);
    else if (action.do === 'sidebar') update((prev) => ({ ...prev, collapsed: !prev.collapsed }));
    else if (action.do === 'menu') onSearch();
    else if (action.do === 'new') {
      const prefill = newTaskPrefill(column.model, column.space);
      if (prefill) ask(prefill);
      else open({ to: 'chat' });
    } else if (action.do === 'spaces') {
      show();
      // After the sidebar has drawn: the current space's row takes focus, and the arrows move on.
      setTimeout(() => {
        const rows = document.querySelectorAll<HTMLElement>('[data-testid="space-row"]');
        const current = [...rows].find((row) => row.getAttribute('aria-current') === 'true');
        (current ?? rows[0])?.focus();
      }, 0);
    }
    return true;
  });

  return (
    <div data-testid="desktop-shell" className="flex min-h-0 flex-1 flex-col">
      <div className="flex min-h-0 flex-1">
        {prefs.collapsed ? null : (
          <PanelBoundary name="Explorer">
            <ExplorerSidebar
              prefs={prefs}
              update={update}
              column={column}
              ready={listed}
              failed={explorer.failed}
              needs={explorer.needs}
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
              ready={listed}
              leading={
                // With the sidebar hidden, the strip is the only chrome left: it brings it back.
                prefs.collapsed ? (
                  <button
                    type="button"
                    data-testid="show-sidebar"
                    aria-label="Show sidebar"
                    title={`Show sidebar (${sidebarShortcutLabel()})`}
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
        needs={explorer.needs}
        // What has an amber dot in the column, so the count and the column agree.
        working={workingCount(column.model.agents)}
        problems={explorer.problems}
        counts={explorer.ready ? 'ready' : explorer.failed ? 'failed' : 'loading'}
        onShowWorking={show}
        hint={armed ? PREFIX_HINT : undefined}
      />
    </div>
  );
}
