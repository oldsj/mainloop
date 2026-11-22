import { Plus } from 'lucide-react';
import { useEffect, useRef } from 'react';
import { StatusDot } from '@/components/shared/status-dot';
import { cn } from '@/components/ui/cn';
import { MAINLOOP_SPACE, newTaskPrefill, spaceTabs, type AgentColumnModel } from './agents';
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
}

/**
 * The strip over the page, as a terminal workspace manager draws it: one tab per agent in the
 * current space, the lit one filled, and a "+" at the end. A tab is a status dot and a name. The
 * strip scrolls sideways when the tabs do not fit, and keeps the lit tab in view.
 *
 * It is navigation, not a tab list: each tab opens a page, the lit one is marked
 * `aria-current="page"`, and the page below is the routed one.
 */
export function TabStrip({ model, space, page, pathname, onOpen, onAsk }: TabStripProps) {
  const tabs = spaceTabs(model, space, page, pathname);
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
      className="border-border-subtle bg-card text-ui flex shrink-0 [scrollbar-width:none] items-stretch overflow-x-auto border-b font-mono"
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
            <span className="max-w-[24ch] truncate">{tab.label}</span>
          </button>
        );
      })}
      <button
        type="button"
        data-testid="tab-new"
        aria-label={space === MAINLOOP_SPACE ? 'New: ask Mainloop' : `New task in ${name}`}
        title={space === MAINLOOP_SPACE ? 'Ask Mainloop' : `Start a task in ${name}`}
        onClick={() => (prefill ? onAsk(prefill) : onOpen({ to: 'chat' }))}
        className={cn(
          TAB,
          'text-muted-foreground hover:bg-accent hover:text-accent-foreground focus-visible:ring-ring'
        )}
      >
        <Plus className="size-3.5" aria-hidden="true" />
      </button>
    </nav>
  );
}
