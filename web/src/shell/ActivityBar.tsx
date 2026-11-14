import { Link, useRouterState } from '@tanstack/react-router';
import { Activity, FolderGit2, Inbox, ListTree, type LucideIcon } from 'lucide-react';
import type { ReactNode } from 'react';
import { cn } from '@/components/ui/cn';
import { badgeText } from '@/features/inbox/model';
import { SIDEBAR_VIEWS, type SidebarView } from '@/features/explorer';
import { ThemeSelector } from './ThemeSelector';

const VIEW_ICONS: Record<SidebarView, LucideIcon> = { sessions: ListTree, projects: FolderGit2 };

/** One square item: 36 px, filled while it is the thing showing. */
const ITEM =
  'text-muted-foreground hover:text-foreground hover:bg-accent relative inline-flex size-9 items-center justify-center [&_svg]:size-4';
const ITEM_ON = 'bg-accent text-accent-foreground';

export interface ActivityBarProps {
  /** The view the sidebar shows. */
  view: SidebarView;
  /** The sidebar is hidden. */
  collapsed: boolean;
  /** How many items need the owner; the Inbox item carries it. */
  needs: number;
  /** Shows a view. Choosing the view that is already showing hides the sidebar. */
  onView: (view: SidebarView) => void;
}

/**
 * Desktop: the strip down the left edge. It replaces the old top header: the home mark, the two
 * sidebar views, the Inbox with its Needs-you count, and at the foot orchestration health and the
 * theme. The count is always on screen here, whichever view the sidebar shows and when it is hidden.
 */
export function ActivityBar({ view, collapsed, needs, onView }: ActivityBarProps) {
  const path = useRouterState({ select: (state) => state.location.pathname });
  return (
    <nav
      aria-label="Primary"
      data-testid="activity-bar"
      className="border-border-subtle bg-card gap-xs py-xs flex w-11 shrink-0 flex-col items-center border-r"
    >
      <Link
        to="/"
        aria-label="Mainloop home"
        title="Mainloop"
        className={cn(ITEM, 'text-primary hover:text-primary font-mono text-sm font-semibold')}
      >
        $
      </Link>
      {SIDEBAR_VIEWS.map(({ id, label }) => {
        const Icon = VIEW_ICONS[id];
        const showing = !collapsed && view === id;
        return (
          <button
            key={id}
            type="button"
            aria-label={label}
            aria-pressed={showing}
            title={showing ? `Hide ${label}` : label}
            onClick={() => onView(id)}
            data-testid={`activity-${id}`}
            className={cn(ITEM, showing && ITEM_ON)}
          >
            <Icon aria-hidden="true" />
          </button>
        );
      })}
      <RouteItem
        to="/inbox"
        on={path === '/inbox'}
        label={needs > 0 ? `Inbox, ${needs} need you` : 'Inbox'}
        title="Inbox"
        testId="activity-inbox"
      >
        <Inbox aria-hidden="true" />
        {needs > 0 ? (
          <span
            aria-hidden="true"
            data-testid="activity-needs"
            className="bg-destructive text-destructive-foreground text-2xs absolute top-0 right-0 inline-flex min-w-4 items-center justify-center px-0.5 leading-4 font-semibold tabular-nums"
          >
            {badgeText(needs)}
          </span>
        ) : null}
      </RouteItem>
      <span className="flex-1" />
      <RouteItem
        to="/health"
        on={path === '/health'}
        label="Orchestration health"
        title="Orchestration health"
        testId="header-health"
      >
        <Activity aria-hidden="true" />
      </RouteItem>
      <ThemeSelector compact />
    </nav>
  );
}

/** An item that opens a page in the centre, marked while that page is showing. */
function RouteItem({
  to,
  on,
  label,
  title,
  testId,
  children
}: {
  to: '/inbox' | '/health';
  on: boolean;
  label: string;
  title: string;
  testId: string;
  children: ReactNode;
}) {
  return (
    <Link
      to={to}
      aria-label={label}
      aria-current={on ? 'page' : undefined}
      title={title}
      data-testid={testId}
      className={cn(ITEM, on && ITEM_ON)}
    >
      {children}
    </Link>
  );
}
