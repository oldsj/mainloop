import { Plus } from 'lucide-react';
import { useEffect, useRef, type ReactNode } from 'react';
import { StatusDot } from '@/components/shared/status-dot';
import { cn } from '@/components/ui/cn';
import { loadingTabs, newLabel, newTaskPrefill, spaceTabs, type AgentColumnModel } from './agents';
import type { Target } from './model';

/** A tab: one line at the control height, its label cut at about 24 characters. */
const TAB =
  'gap-xs px-sm min-h-control inline-flex shrink-0 cursor-default items-center whitespace-nowrap outline-none focus-visible:ring-1 focus-visible:ring-inset';

export interface TabStripProps {
  model: AgentColumnModel;
  /** The space the page being shown belongs to. Its agents are the tabs. */
  space: string;
  page: Target | null;
  pathname: string;
  onOpen: (target: Target) => void;
  onAsk: (text: string) => void;
  /**
   * A control before the first tab, which stays put while the tabs scroll: the button that brings a
   * hidden sidebar back, or the phone's button for the spaces and agents.
   */
  leading?: ReactNode;
  /** A control after the tabs, which stays put too: the phone's Inbox, with its count. */
  trailing?: ReactNode;
  /**
   * Whether the agents and their spaces are known. Until they are, the strip shows only the tabs
   * that are certain (see `loadingTabs`), so a tab is never drawn in one space and then moved.
   */
  ready?: boolean;
}

/**
 * The strip over the page, as a terminal workspace manager draws it: one tab per agent in the
 * current space, the lit one filled, and a "+" at the end. A tab is a status dot and a name. The
 * tabs scroll sideways when they do not fit, with the lit tab kept in view; a control before or
 * after them stays where it is.
 *
 * It is navigation, not a tab list: each tab opens a page, the lit one is marked
 * `aria-current="page"`, and the page below is the routed one.
 */
export function TabStrip({
  model,
  space,
  page,
  pathname,
  onOpen,
  onAsk,
  leading,
  trailing,
  ready = true
}: TabStripProps) {
  const tabs = ready ? spaceTabs(model, space, page, pathname) : loadingTabs(model, page, pathname);
  const spaceRow = model.spaces.find((row) => row.id === space);
  const name = spaceRow?.label ?? 'main';
  const prefill = newTaskPrefill(model, space);
  const lit = useRef<HTMLElement | null>(null);
  const litId = tabs.find((tab) => tab.current)?.id;
  // The lit tab is brought into view when it changes, and when the tabs around it do.
  useEffect(() => {
    lit.current?.scrollIntoView?.({ block: 'nearest', inline: 'nearest' });
  }, [litId, tabs.length]);

  return (
    <nav
      aria-label={`Agents in ${name}`}
      data-testid="tab-strip"
      className="border-border-subtle bg-card text-body flex shrink-0 items-stretch border-b"
    >
      {leading}
      <div
        data-testid="tab-scroller"
        // No scrollbar: one would be as tall as the strip. The lit tab is kept in view instead.
        style={{ scrollbarWidth: 'none' }}
        // The line is as tall with no tabs as with them, so nothing moves when they arrive.
        className="min-h-control flex min-w-0 flex-1 items-stretch overflow-x-auto"
      >
        {tabs.map((tab) => {
          const target = tab.target;
          return (
            <button
              key={tab.id}
              type="button"
              ref={(el) => {
                if (tab.current) lit.current = el;
              }}
              data-testid="tab"
              aria-current={tab.current ? 'page' : undefined}
              title={tab.title}
              // A tab for a page that is no agent's leads nowhere: the owner is already on it.
              onClick={target ? () => onOpen(target) : undefined}
              className={cn(
                TAB,
                tab.current
                  ? // A hollow dot takes the tab's own text colour, so it shows on the fill.
                    'bg-primary text-primary-foreground focus-visible:ring-foreground [&_[data-tone=muted]]:border-current'
                  : 'text-muted-foreground hover:bg-accent hover:text-accent-foreground focus-visible:ring-ring'
              )}
            >
              {tab.tone ? <StatusDot tone={tab.tone} testId="tab-dot" /> : null}
              <span className="max-w-tab truncate">{tab.label}</span>
            </button>
          );
        })}
        {ready ? (
          <button
            type="button"
            data-testid="tab-new"
            aria-label={newLabel(model, space)}
            title={newLabel(model, space)}
            onClick={() => (prefill ? onAsk(prefill) : onOpen({ to: 'chat' }))}
            className={cn(
              TAB,
              'text-muted-foreground hover:bg-accent hover:text-accent-foreground focus-visible:ring-ring'
            )}
          >
            <Plus className="size-3.5" aria-hidden="true" />
          </button>
        ) : null}
      </div>
      {trailing}
    </nav>
  );
}
