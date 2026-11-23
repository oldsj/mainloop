import { useNavigate, useRouterState } from '@tanstack/react-router';
import { Inbox, ListTree, Search } from 'lucide-react';
import { useState, type ReactNode } from 'react';
import { cn } from '@/components/ui/cn';
import {
  AgentColumn,
  loadingTabs,
  newTaskPrefill,
  spaceTabs,
  TabStrip,
  useAgentColumn,
  useAsk,
  useExplorer,
  useOpenTarget
} from '@/features/explorer';
import { badgeText } from '@/features/inbox/model';
import { COMMAND_KEYS, useShellCommands } from './commands';
import { prefixAction } from './prefixKeys';
import { LogoMark } from './LogoMark';
import { MAIN_ID } from './RouteFocus';
import { PanelBoundary } from './PanelBoundary';
import type { Tab } from './view';

/**
 * <1024 px: one agent, full screen, between two bars.
 *
 * - On top, the current space's **tabs**, which scroll sideways, with "+" at their end.
 * - Under it the routed page. It is built the first time it shows and stays mounted while the
 *   spaces and agents cover it, so a conversation keeps its place.
 * - At the bottom, the desktop's activity bar laid flat: **Chat** (the main chat), **Inbox** with
 *   the Needs-you count, **Agents** (the spaces and agents over the page: `?tab=explorer`, so Back
 *   puts them away) and **Search** (the command palette, which reaches every other page).
 *
 * Choosing a space, an agent or a tab goes to its page and puts the list away.
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
  // As on desktop: no agent is drawn until its space is known.
  const listed = explorer.ready && explorer.placed;
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

  // The shell's actions by name, for the command palette. The phone has no prefix keys; here the
  // sidebar and the spaces are both the lists over the page.
  const tabs = listed
    ? spaceTabs(column.model, column.space, column.page, pathname)
    : loadingTabs(column.model, column.page, pathname);
  useShellCommands((command) => {
    if (command === 'sidebar') return toggle();
    if (command === 'spaces') return listing ? undefined : toggle();
    const action = prefixAction(COMMAND_KEYS[command], tabs);
    if (action?.do === 'open') open(action.target);
    else if (action?.do === 'new') {
      const prefill = newTaskPrefill(column.model, column.space);
      if (prefill) ask(prefill);
      else open({ to: 'chat' });
    }
  });

  return (
    <div data-testid="compact-shell" className="flex min-h-0 flex-1 flex-col">
      <PanelBoundary name="Tabs">
        <TabStrip
          model={column.model}
          space={column.space}
          page={column.page}
          pathname={pathname}
          onOpen={open}
          onAsk={ask}
          ready={listed}
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
              ready={listed}
              failed={explorer.failed}
              onOpen={open}
              onAsk={ask}
              onMenu={onSearch}
            />
          </PanelBoundary>
        ) : null}
      </main>
      {/* The desktop's activity bar, laid along the bottom where a thumb reaches: the same four
          destinations, with words under the icons. It takes the bottom safe area. */}
      <nav
        data-testid="mobile-tab-bar"
        aria-label="Primary"
        className="border-border-subtle bg-card flex shrink-0 border-t pb-[env(safe-area-inset-bottom)]"
      >
        <BarItem
          testId="tab-chat"
          label="Chat"
          on={onHome && !listing}
          onClick={() => open({ to: 'chat' })}
        >
          <LogoMark className="size-4" />
        </BarItem>
        <BarItem
          testId="inbox-link"
          label="Inbox"
          name={needs > 0 ? `Inbox, ${needs} need you` : 'Inbox'}
          on={pathname === '/inbox' && !listing}
          onClick={() => open({ to: 'needs' })}
        >
          <Inbox className="size-4" aria-hidden="true" />
          {needs > 0 ? (
            <span
              data-testid="needs-badge"
              aria-hidden="true"
              className="bg-destructive text-destructive-foreground text-2xs absolute -top-1.5 left-3 inline-flex min-w-4 items-center justify-center px-0.5 leading-4 font-semibold tabular-nums"
            >
              {badgeText(needs)}
            </span>
          ) : null}
        </BarItem>
        <BarItem
          testId="agents-toggle"
          label="Agents"
          name="Spaces and agents"
          on={listing}
          expanded={listing}
          onClick={toggle}
        >
          <ListTree className="size-4" aria-hidden="true" />
        </BarItem>
        <BarItem testId="tab-search" label="Search" onClick={onSearch}>
          <Search className="size-4" aria-hidden="true" />
        </BarItem>
      </nav>
    </div>
  );
}

/** One item of the phone's bottom bar: an icon over a word, a 44 px touch target, lit while it is where the owner is. */
function BarItem({
  testId,
  label,
  name,
  on = false,
  expanded,
  onClick,
  children
}: {
  testId: string;
  label: string;
  /** The accessible name, when it says more than the word under the icon. */
  name?: string;
  on?: boolean;
  /** For the item that opens the lists over the page: whether they are showing. */
  expanded?: boolean;
  onClick: () => void;
  children: ReactNode;
}) {
  return (
    <button
      type="button"
      data-testid={testId}
      aria-label={name ?? label}
      // A page's item is marked as where the owner is; the lists' item says whether they are open.
      aria-current={expanded === undefined && on ? 'page' : undefined}
      aria-expanded={expanded}
      onClick={onClick}
      className={cn(
        'gap-xs text-2xs flex min-h-11 flex-1 flex-col items-center justify-center',
        on ? 'bg-accent text-accent-foreground font-semibold' : 'text-muted-foreground'
      )}
    >
      <span className="relative inline-flex">{children}</span>
      {label}
    </button>
  );
}
