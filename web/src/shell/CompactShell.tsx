import { useNavigate, useRouterState } from '@tanstack/react-router';
import { Inbox, Menu } from 'lucide-react';
import { useState, type ReactNode } from 'react';
import { cn } from '@/components/ui/cn';
import {
  AgentColumn,
  TabStrip,
  useAgentColumn,
  useAsk,
  useExplorer,
  useOpenTarget
} from '@/features/explorer';
import { badgeText } from '@/features/inbox/model';
import { MAIN_ID } from './RouteFocus';
import { PanelBoundary } from './PanelBoundary';
import type { Tab } from './view';

/** A control at either end of the top row: a 44 px touch target that does not scroll with the tabs. */
const END =
  'text-muted-foreground hover:text-foreground size-control focus-visible:ring-ring relative inline-flex shrink-0 items-center justify-center outline-none focus-visible:ring-1 focus-visible:ring-inset [&_svg]:size-4';

/**
 * <1024 px: one agent, full screen, as a terminal workspace manager is on a narrow terminal. There
 * is no header and no bar at the bottom. One row at the top holds, left to right:
 *
 * - the button for the **spaces and agents**, which opens them over the page (`?tab=explorer`, so
 *   Back closes them);
 * - the current space's **tabs**, which scroll sideways, with "+" at their end;
 * - the **Inbox**, with the Needs-you count, so attention keeps polling and is one tap away.
 *
 * Under it is the routed page. It is built the first time it shows and stays mounted while the
 * spaces and agents cover it, so a conversation keeps its place. Choosing a space, an agent or a tab
 * goes to its page and puts the list away.
 */
export function CompactShell({
  tab,
  onSearch,
  children
}: {
  tab: Tab;
  /** Opens the command palette. */
  onSearch: () => void;
  children: ReactNode;
}) {
  const navigate = useNavigate();
  const pathname = useRouterState({ select: (state) => state.location.pathname });
  const onHome = pathname === '/';
  const open = useOpenTarget();
  const ask = useAsk();
  // The explorer's reads poll while the shell is up, and mark a task seen when its page opens.
  const explorer = useExplorer();
  const { needs } = explorer;
  const column = useAgentColumn();
  const listing = tab === 'explorer';
  // The page is built the first time it is the thing showing, and then kept.
  const [paged, setPaged] = useState(!listing);
  // Adjusts state during render, the React way to derive it from a prop.
  if (!listing && !paged) setPaged(true);

  const toggle = () =>
    void navigate({
      to: '.',
      search: (prev: Record<string, unknown>) => ({
        ...prev,
        tab: listing ? undefined : ('explorer' as const)
      })
    });

  return (
    // The bottom inset is the shell's: there is no bar down there to take it.
    <div
      data-testid="compact-shell"
      className="flex min-h-0 flex-1 flex-col pb-[env(safe-area-inset-bottom)]"
    >
      <PanelBoundary name="Tabs">
        <TabStrip
          model={column.model}
          space={column.space}
          page={column.page}
          pathname={pathname}
          onOpen={open}
          onAsk={ask}
          leading={
            <button
              type="button"
              data-testid="agents-toggle"
              aria-label="Spaces and agents"
              aria-expanded={listing}
              onClick={toggle}
              className={cn(END, listing && 'bg-accent text-accent-foreground')}
            >
              <Menu aria-hidden="true" />
            </button>
          }
          trailing={
            <button
              type="button"
              data-testid="inbox-link"
              aria-label={needs > 0 ? `Inbox, ${needs} need you` : 'Inbox'}
              aria-current={pathname === '/inbox' && !listing ? 'page' : undefined}
              onClick={() => open({ to: 'needs' })}
              className={cn(END, pathname === '/inbox' && !listing && 'text-foreground')}
            >
              <Inbox aria-hidden="true" />
              {needs > 0 ? (
                <span
                  data-testid="needs-badge"
                  aria-hidden="true"
                  className="bg-destructive text-destructive-foreground text-2xs absolute top-1.5 right-1 inline-flex min-w-4 items-center justify-center px-0.5 leading-4 font-semibold tabular-nums"
                >
                  {badgeText(needs)}
                </span>
              ) : null}
            </button>
          }
        />
      </PanelBoundary>
      {/* The home chat owns its scroll, so the pane around it does not scroll as well. It is the
          containing block too: an absolutely placed descendant (an sr-only label) would otherwise
          stretch the document, and scrolling a row into view would scroll the whole page. */}
      <main
        id={MAIN_ID}
        tabIndex={-1}
        className={cn(
          'relative min-h-0 flex-1 outline-none',
          onHome && !listing ? 'overflow-hidden' : 'overflow-y-auto'
        )}
      >
        {paged ? (
          // Hidden, not unmounted, while the list covers it, so its state survives.
          <div hidden={listing} className="h-full min-h-0">
            {children}
          </div>
        ) : null}
        {listing ? (
          <PanelBoundary name="Explorer">
            <AgentColumn
              model={column.model}
              page={column.page}
              space={column.space}
              ready={explorer.ready}
              failed={explorer.failed}
              onOpen={open}
              onAsk={ask}
              needs={needs}
              onMenu={onSearch}
            />
          </PanelBoundary>
        ) : null}
      </main>
    </div>
  );
}
