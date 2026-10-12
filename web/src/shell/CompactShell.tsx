import { Link, useNavigate, useRouterState } from '@tanstack/react-router';
import { Activity, FolderGit2, Inbox, ListTree, MessageSquare, Plus } from 'lucide-react';
import { useState, type ReactNode } from 'react';
import { buttonVariants } from '@/components/ui/button';
import { cn } from '@/components/ui/cn';
import { HomeStrip } from '@/features/home';
import { InboxBadge, unresolvedCount, useAttention } from '@/features/inbox';
import { ownerTaskText, TasksBadge, tasksNeedingOwner, useTaskList } from '@/features/tasks';
import { MAIN_ID } from './RouteFocus';
import { PanelBoundary } from './PanelBoundary';
import { InboxPanel, ProjectsPanel, SessionsPanel, WorkPanel } from './slots';
import { ThemeSelector } from './ThemeSelector';
import type { Tab } from './view';

const TAB_ITEMS = [
  { tab: 'chat', label: 'Chat', Icon: MessageSquare },
  { tab: 'sessions', label: 'Sessions', Icon: ListTree },
  { tab: 'inbox', label: 'Inbox', Icon: Inbox },
  { tab: 'projects', label: 'Projects', Icon: FolderGit2 }
] as const satisfies readonly { tab: Tab; label: string; Icon: unknown }[];

/**
 * The tab's name. The Inbox tab carries its owner counts, so a screen reader announces them with
 * the tab, as the badges show them.
 */
function tabName(id: Tab, label: string, needs: number, tasks: number): string {
  if (id !== 'inbox') return label;
  const parts = [label];
  if (needs > 0) parts.push(`${needs} need you`);
  if (tasks > 0) parts.push(ownerTaskText(tasks));
  return parts.join(', ');
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
 * <1024 px: one pane at a time, switched by a bottom tab bar. The tab lives in the URL. A tab is
 * built the first time it opens and stays mounted after that, so a draft or a loaded list survives
 * a switch. The Inbox tab always mounts its badges, so attention keeps polling while another tab is
 * open. The Chat tab opens with the "Since you were here" line above the thread; the Projects tab
 * opens with the Work list above the project list.
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
  const needs = unresolvedCount(useAttention().data);
  const tasks = tasksNeedingOwner(useTaskList().data ?? []).length;
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
          <Link
            to="/agents"
            data-testid="header-new-agent"
            aria-label="New agent session"
            className={cn(buttonVariants({ variant: 'ghost', size: 'icon' }))}
          >
            <Plus aria-hidden="true" />
          </Link>
          <ThemeSelector />
        </nav>
      </header>
      <main id={MAIN_ID} tabIndex={-1} className="min-h-0 flex-1 overflow-y-auto outline-none">
        {opened.has('chat') && (
          <TabPane active={tab === 'chat'}>
            <div className="flex h-full min-h-0 flex-col">
              {/* The strip belongs to home only; task and agent pages open in the same tab. */}
              {onHome && (
                <PanelBoundary name="Since you were here">
                  <HomeStrip lastVisit={lastVisit} compact />
                </PanelBoundary>
              )}
              <div className="min-h-0 flex-1">{children}</div>
            </div>
          </TabPane>
        )}
        {opened.has('sessions') && (
          <TabPane active={tab === 'sessions'}>
            <PanelBoundary name="Sessions">
              <SessionsPanel />
            </PanelBoundary>
          </TabPane>
        )}
        {opened.has('inbox') && (
          <TabPane active={tab === 'inbox'}>
            <PanelBoundary name="Inbox">
              <InboxPanel />
            </PanelBoundary>
          </TabPane>
        )}
        {opened.has('projects') && (
          <TabPane active={tab === 'projects'}>
            <div className="border-border border-b">
              <PanelBoundary name="Work">
                <WorkPanel lastVisit={lastVisit} />
              </PanelBoundary>
            </div>
            <PanelBoundary name="Projects">
              <ProjectsPanel />
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
            aria-label={tabName(id, label, needs, tasks)}
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
            {/* The icon takes the middle column, so the badges beside it do not pull it off centre. */}
            <span className="gap-xs grid w-full grid-cols-[minmax(0,1fr)_auto_minmax(0,1fr)] items-center">
              <Icon className="col-start-2 size-4" aria-hidden="true" />
              {id === 'inbox' && (
                <span className="col-start-3 flex items-center gap-0.5 justify-self-start">
                  <InboxBadge />
                  <TasksBadge compact />
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
