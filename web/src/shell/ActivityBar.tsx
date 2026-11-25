import { Link, useRouterState } from '@tanstack/react-router';
import { Activity, Inbox, PanelLeft, Search, Settings } from 'lucide-react';
import type { ReactNode } from 'react';
import { cn } from '@/components/ui/cn';
import { badgeText } from '@/features/inbox/model';
import { shortcutLabel, sidebarShortcutLabel } from '@/lib/platform';
import { LogoMark } from './LogoMark';
import { ThemeSelector } from './ThemeSelector';

/** One square item: 36 px, with a 20 px icon. */
const ITEM =
  'text-muted-foreground hover:text-foreground hover:bg-accent relative inline-flex size-9 items-center justify-center [&_svg]:size-5';
/** While it is the thing showing: filled, with a rule down its left edge, as an editor marks it. */
const ITEM_ON =
  'bg-accent text-accent-foreground before:bg-primary before:absolute before:inset-y-1 before:-left-1 before:w-0.5';

export interface ActivityBarProps {
  /** The sidebar (spaces and agents) is hidden. */
  collapsed: boolean;
  /** How many items need the owner; the Inbox item carries it. */
  needs: number;
  /** Hides or shows the sidebar. */
  onToggleSidebar: () => void;
  /** Opens the command palette. */
  onSearch: () => void;
}

/**
 * Desktop: the strip of icons down the left edge, as an editor has. Mainloop's mark at the top opens
 * the main chat; under it the Inbox with its Needs-you count, the button that hides and shows the
 * sidebar, and search (the command palette); at the foot orchestration health, Settings and the
 * theme. The Inbox is first because what needs the owner is the first thing to check, and its count
 * is on screen here whether or not the sidebar is.
 */
export function ActivityBar({ collapsed, needs, onToggleSidebar, onSearch }: ActivityBarProps) {
  const path = useRouterState({ select: (state) => state.location.pathname });
  return (
    <nav
      aria-label="Primary"
      data-testid="activity-bar"
      className="border-border-subtle bg-card gap-xs py-xs flex w-11 shrink-0 flex-col items-center border-r"
    >
      <Link
        to="/"
        search={{}}
        aria-label="Mainloop home"
        title="Mainloop"
        className={cn(ITEM, '[&_svg]:size-5')}
      >
        <LogoMark />
      </Link>
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
            className="bg-status-attention text-background text-label absolute top-0 right-0 inline-flex min-w-4 items-center justify-center px-0.5 leading-4 font-semibold tabular-nums"
          >
            {badgeText(needs)}
          </span>
        ) : null}
      </RouteItem>
      <button
        type="button"
        aria-label="Spaces and agents"
        aria-pressed={!collapsed}
        title={`${collapsed ? 'Show' : 'Hide'} the sidebar (${sidebarShortcutLabel()})`}
        onClick={onToggleSidebar}
        data-testid="activity-sidebar"
        className={cn(ITEM, !collapsed && ITEM_ON)}
      >
        <PanelLeft aria-hidden="true" />
      </button>
      <button
        type="button"
        aria-label="Search"
        title={`Search and commands (${shortcutLabel('k')})`}
        onClick={onSearch}
        data-testid="activity-search"
        className={ITEM}
      >
        <Search aria-hidden="true" />
      </button>
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
      <RouteItem
        to="/settings"
        on={path === '/settings'}
        label="Settings"
        title="Settings"
        testId="activity-settings"
      >
        <Settings aria-hidden="true" />
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
  to: '/inbox' | '/health' | '/settings';
  on: boolean;
  label: string;
  title: string;
  testId: string;
  children: ReactNode;
}) {
  return (
    <Link
      to={to}
      search={{}}
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
