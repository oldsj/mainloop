import { MessageSquarePlus } from 'lucide-react';
import {
  Fragment,
  useCallback,
  useEffect,
  useRef,
  useState,
  type KeyboardEvent,
  type ReactNode
} from 'react';
import { SkeletonRows } from '@/components/shared/skeleton';
import { StatusDot } from '@/components/shared/status-dot';
import { cn } from '@/components/ui/cn';
import { relativeAge } from '@/lib/taskState';
import { badgeText } from '@/features/inbox/model';
import { shortcutLabel } from '@/lib/platform';
import {
  currentAgent,
  newLabel,
  newTaskPrefill,
  type AgentColumnModel,
  type AgentRow,
  type SpaceRow
} from './agents';
import type { Target } from './model';
import { RowMenu, type RowMenuAction } from './RowMenu';

/**
 * Muted text on a row that is hovered or marked: the row's background is the selection colour,
 * where the muted colour is under 4.5:1, so the text takes the row's own foreground.
 */
const ON_ROW =
  'group-hover:text-accent-foreground group-data-[current=true]:text-accent-foreground';

/** A word in the row between the lists: muted until hovered, at the control height. */
const ROW_BUTTON =
  'text-muted-foreground hover:text-foreground px-xs min-h-control focus-visible:ring-ring inline-flex cursor-default items-center outline-none focus-visible:ring-1 focus-visible:ring-inset';

/** A list's name: lowercase and muted, with no rule or box around it. */
const LABEL = 'text-muted-foreground px-sm text-xs font-normal lowercase';

export interface AgentColumnProps {
  model: AgentColumnModel;
  /** The page being shown. The agent it belongs to is marked as the current page. */
  page: Target | null;
  /** The space the page belongs to. Its row is marked too. */
  space: string;
  ready: boolean;
  failed: boolean;
  onOpen: (target: Target) => void;
  onAsk: (text: string) => void;
  /** A control at the end of the first label's row: the button that hides the column. */
  action?: ReactNode;
  /** How many things need the owner. The row between the lists carries it on **inbox**. */
  needs?: number;
  /** Opens the command palette: the row's **menu**. Without it the row is not drawn. */
  onMenu?: () => void;
  /** The clock a row's age is measured from, in ms. Defaults to now. */
  now?: number;
}

/** The clock for row ages: `fixed` when given, else the time now, read again each minute. */
function useNow(fixed?: number): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (fixed !== undefined) return;
    const timer = setInterval(() => setNow(Date.now()), 60_000);
    return () => clearInterval(timer);
  }, [fixed]);
  return fixed ?? now;
}

/** How long ago an agent last changed, for the end of its second line. */
function ageOf(agent: AgentRow, now: number): string | undefined {
  return agent.at ? relativeAge(Math.max(0, now - Date.parse(agent.at))) : undefined;
}

/** The agents in runs of one space, in the order given. */
function bySpace(agents: AgentRow[]): { id: string; label: string; rows: AgentRow[] }[] {
  const groups: { id: string; label: string; rows: AgentRow[] }[] = [];
  for (const agent of agents) {
    const last = groups.at(-1);
    if (last?.id === agent.space) last.rows.push(agent);
    else groups.push({ id: agent.space, label: agent.spaceLabel, rows: [agent] });
  }
  return groups;
}

/**
 * The left column, in the shape of a terminal workspace manager: **spaces** (main, then each
 * project) and under them the **agents** at work, grouped under their space's name. A row is a
 * status dot, a title and one muted line. The agent whose page is showing is filled and marked `aria-current="page"`;
 * the space that page belongs to is filled too.
 *
 * Between the lists is one row of words: **new** (start work in the current space, from the main
 * chat), **inbox** (with the Needs-you count) and **menu** (the command palette, which reaches
 * every other page and the themes).
 *
 * The two lists are one stop for the Tab key: up and down move through every row, Home and End go
 * to the ends, Enter opens the row, and `a` on an agent asks Mainloop about it. A row's actions are
 * also in a small menu on right-click, Shift+F10 or the menu key.
 */
export function AgentColumn({
  model,
  page,
  space,
  ready,
  failed,
  onOpen,
  onAsk,
  action,
  needs = 0,
  onMenu,
  now: fixedNow
}: AgentColumnProps) {
  const now = useNow(fixedNow);
  const here = currentAgent(model.agents, page);
  const rowIds = [
    ...model.spaces.map((row) => `space:${row.id}`),
    ...model.agents.map((row) => `agent:${row.id}`)
  ];
  const [focusId, setFocusId] = useState<string | null>(null);
  const buttons = useRef(new Map<string, HTMLButtonElement>());
  // The roving stop is the focused row if it is still listed, otherwise the first row.
  const activeId = focusId && rowIds.includes(focusId) ? focusId : (rowIds[0] ?? null);

  const focusRow = (id: string | undefined) => {
    if (!id) return;
    setFocusId(id);
    buttons.current.get(id)?.focus();
  };

  const [menu, setMenu] = useState<{ id: string; x: number; y: number } | null>(null);
  const menuId = useRef<string | null>(null);
  const actionsFor = (id: string): { label: string; actions: RowMenuAction[] } | null => {
    const spaceRow = model.spaces.find((row) => `space:${row.id}` === id);
    if (spaceRow) {
      return {
        label: spaceRow.label,
        actions: [{ label: 'Open', run: () => onOpen(spaceRow.target) }]
      };
    }
    const agent = model.agents.find((row) => `agent:${row.id}` === id);
    if (!agent) return null;
    const ask = agent.ask;
    return {
      label: agent.label,
      actions: [
        { label: 'Open', run: () => onOpen(agent.target) },
        ...(ask ? [{ label: 'Ask Mainloop', run: () => onAsk(ask) }] : [])
      ]
    };
  };
  const openMenu = (id: string, x: number, y: number) => {
    setFocusId(id);
    menuId.current = id;
    setMenu({ id, x, y });
  };
  // Focus goes back to the row only when it was in the menu, never from wherever the owner moved it.
  const closeMenu = useCallback((restoreFocus: boolean) => {
    const id = menuId.current;
    menuId.current = null;
    setMenu(null);
    if (restoreFocus && id) buttons.current.get(id)?.focus();
  }, []);
  const menuRow = menu ? actionsFor(menu.id) : null;

  const onKeyDown = (event: KeyboardEvent<HTMLButtonElement>, id: string, ask?: string) => {
    const index = rowIds.indexOf(id);
    switch (event.key) {
      case 'ArrowDown':
        focusRow(rowIds[index + 1]);
        break;
      case 'ArrowUp':
        focusRow(rowIds[index - 1]);
        break;
      case 'Home':
        focusRow(rowIds[0]);
        break;
      case 'End':
        focusRow(rowIds[rowIds.length - 1]);
        break;
      case 'a':
        if (!ask) return;
        onAsk(ask);
        break;
      case 'ContextMenu':
      case 'F10': {
        if (event.key === 'F10' && !event.shiftKey) return;
        const box = event.currentTarget.getBoundingClientRect();
        openMenu(id, box.left + 24, box.bottom);
        break;
      }
      default:
        return;
    }
    event.preventDefault();
    event.stopPropagation();
  };

  /** One row: the button that opens it, and the controls beside it. */
  const row = (
    id: string,
    opts: {
      target: Target;
      tone: SpaceRow['tone'];
      label: string;
      title?: string;
      meta?: string;
      /** The row's age. It ends the second line and is never the part that is cut short. */
      age?: string;
      /** `page` for the agent whose page is showing, `true` for the space it belongs to. */
      current?: 'page' | 'true';
      ask?: string;
      depth?: number;
      testId: string;
    }
  ) => (
    <li
      key={id}
      data-node-id={id}
      data-current={opts.current ? 'true' : undefined}
      className={cn(
        'group flex items-stretch',
        'hover:bg-accent hover:text-accent-foreground',
        (opts.current || menu?.id === id) && 'bg-accent text-accent-foreground'
      )}
      onContextMenu={(event) => {
        event.preventDefault();
        openMenu(id, event.clientX, event.clientY);
      }}
    >
      <button
        type="button"
        ref={(el) => {
          if (el) buttons.current.set(id, el);
          else buttons.current.delete(id);
        }}
        data-testid={opts.testId}
        aria-current={opts.current}
        title={opts.title}
        tabIndex={id === activeId ? 0 : -1}
        onFocus={() => setFocusId(id)}
        onKeyDown={(event) => onKeyDown(event, id, opts.ask)}
        onClick={() => onOpen(opts.target)}
        style={opts.depth ? { paddingLeft: `${opts.depth * 12 + 8}px` } : undefined}
        className="gap-xs px-sm min-h-control focus-visible:ring-ring flex min-w-0 flex-1 cursor-default items-start py-1 text-left outline-none focus-visible:ring-1 focus-visible:ring-inset"
      >
        {/* The dot sits on the title's line, whether or not a second line follows. */}
        <span className="flex h-5 shrink-0 items-center">
          <StatusDot tone={opts.tone} testId="agent-dot" />
        </span>
        <span className="flex min-w-0 flex-1 flex-col">
          <span className="min-w-0 truncate">{opts.label}</span>
          {opts.meta || opts.age ? (
            <span
              data-testid="agent-meta"
              title={opts.meta}
              className={cn('text-muted-foreground flex min-w-0 text-xs', ON_ROW)}
            >
              {opts.meta ? <span className="min-w-0 truncate">{opts.meta}</span> : null}
              {opts.age ? (
                <span className="shrink-0 whitespace-pre tabular-nums">
                  {opts.meta ? ' · ' : ''}
                  {opts.age}
                </span>
              ) : null}
            </span>
          ) : null}
        </span>
      </button>
      {opts.ask ? (
        <button
          type="button"
          tabIndex={-1}
          aria-label={`Ask Mainloop about ${opts.label}`}
          title="Ask Mainloop about this"
          onClick={() => onAsk(opts.ask ?? '')}
          // A phone has no hover, so it is always shown there, as a touch target. With a mouse it
          // shows on hover, or while its row has keyboard focus.
          className="text-muted-foreground hover:text-foreground lg:mr-xs inline-flex size-11 shrink-0 items-center justify-center self-center lg:size-6 lg:opacity-0 lg:group-focus-within:opacity-100 lg:group-hover:opacity-100"
        >
          <MessageSquarePlus className="size-3.5" aria-hidden="true" />
        </button>
      ) : null}
    </li>
  );

  return (
    <section
      aria-label="Spaces and agents"
      data-testid="agent-column"
      className="text-ui flex min-h-0 flex-1 flex-col font-mono"
    >
      {/* As tall as the tab strip beside it, rule included, so the two first rows line up. */}
      <header className="pr-xs min-h-control box-content flex shrink-0 items-center justify-between border-b border-transparent">
        <h2 id="spaces-heading" className={LABEL}>
          spaces
        </h2>
        {action}
      </header>
      <div className="pb-sm min-h-0 flex-1 overflow-y-auto">
        {failed ? (
          <p role="alert" className="px-sm py-sm text-term-red text-sm">
            Couldn’t load the agents. They fill in when the next read succeeds.
          </p>
        ) : !ready ? (
          // Rows in the list's own shape, so nothing jumps when it arrives. The line is for a screen
          // reader; the rows are hidden from it.
          <div className="px-xs">
            <p role="status" className="sr-only">
              Loading Mainloop…
            </p>
            <SkeletonRows rows={8} testId="explorer-loading" />
          </div>
        ) : (
          <ul aria-labelledby="spaces-heading" className="m-0 list-none p-0">
            {model.spaces.map((spaceRow) =>
              row(`space:${spaceRow.id}`, {
                target: spaceRow.target,
                tone: spaceRow.tone,
                label: spaceRow.label,
                title: spaceRow.title,
                meta: spaceRow.detail,
                current: spaceRow.id === space ? 'true' : undefined,
                testId: 'space-row'
              })
            )}
          </ul>
        )}
        {/* The row of words is there from the start, so the menu works while the lists load. */}
        {onMenu ? (
          // Between the lists, as a terminal workspace manager has it: start something, and
          // everything else. The Inbox is here too, with its count, since what needs the owner
          // is the first thing to check.
          <div
            data-testid="column-actions"
            className="px-xs mt-sm flex items-center justify-between text-xs"
          >
            <button
              type="button"
              data-testid="column-new"
              aria-label={newLabel(model, space)}
              title={newLabel(model, space)}
              onClick={() => {
                const prefill = newTaskPrefill(model, space);
                if (prefill) onAsk(prefill);
                else onOpen({ to: 'chat' });
              }}
              className={ROW_BUTTON}
            >
              new
            </button>
            <span className="flex items-center">
              <button
                type="button"
                data-testid="column-inbox"
                aria-label={needs > 0 ? `Inbox, ${needs} need you` : 'Inbox'}
                aria-current={page?.to === 'needs' ? 'page' : undefined}
                onClick={() => onOpen({ to: 'needs' })}
                className={cn(ROW_BUTTON, 'gap-xs', page?.to === 'needs' && 'text-foreground')}
              >
                inbox
                {needs > 0 ? (
                  <span
                    data-testid="column-needs"
                    aria-hidden="true"
                    className="text-status-attention font-semibold tabular-nums"
                  >
                    {badgeText(needs)}
                  </span>
                ) : null}
              </button>
              <button
                type="button"
                data-testid="column-menu"
                aria-label="Menu"
                title={`Search and commands (${shortcutLabel('k')})`}
                onClick={onMenu}
                className={ROW_BUTTON}
              >
                menu
              </button>
            </span>
          </div>
        ) : null}
        {ready && !failed ? (
          <>
            <h2 id="agents-heading" className={cn(LABEL, 'mt-sm flex h-6 items-center')}>
              agents
            </h2>
            <div role="group" aria-labelledby="agents-heading">
              {bySpace(model.agents).map((group) => (
                <Fragment key={group.id}>
                  {/* The space's name once, over its agents, so a row's second line is all state. */}
                  <h3
                    id={`agents-in-${group.id}`}
                    data-testid="agent-space"
                    className="text-muted-foreground px-sm pt-xs truncate text-xs font-normal"
                  >
                    {group.label}
                  </h3>
                  <ul aria-labelledby={`agents-in-${group.id}`} className="m-0 list-none p-0">
                    {group.rows.map((agent) =>
                      row(`agent:${agent.id}`, {
                        target: agent.target,
                        tone: agent.tone,
                        label: agent.label,
                        title: agent.label,
                        meta: agent.detail,
                        age: ageOf(agent, now),
                        current: agent === here ? 'page' : undefined,
                        ask: agent.ask,
                        depth: agent.depth,
                        testId: 'agent-row'
                      })
                    )}
                  </ul>
                </Fragment>
              ))}
            </div>
          </>
        ) : null}
      </div>
      {menu && menuRow ? (
        <RowMenu
          label={menuRow.label}
          x={menu.x}
          y={menu.y}
          rowId={menu.id}
          actions={menuRow.actions}
          onClose={closeMenu}
        />
      ) : null}
    </section>
  );
}
