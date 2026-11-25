import { CircleAlert, LoaderCircle, MessageSquarePlus, Plus, Search } from 'lucide-react';
import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type KeyboardEvent,
  type MouseEvent,
  type ReactNode
} from 'react';
import { SkeletonRows } from '@/components/shared/skeleton';
import { STATUS_ICON, StatusIcon } from '@/components/shared/status-icon';
import { Button } from '@/components/ui/button';
import { cn } from '@/components/ui/cn';
import { LogoMark } from '@/shell/LogoMark';
import { relativeAge } from '@/lib/taskState';
import { shortcutLabel } from '@/lib/platform';
import {
  MAINLOOP_SPACE,
  currentAgent,
  newLabel,
  newTaskPrefill,
  type AgentColumnModel,
  type AgentRow,
  type SpaceRow
} from './agents';
import type { Target } from './model';
import { RowMenu, type RowMenuAction } from './RowMenu';

/** The section's name: the sidebar's one upper-case label. */
const SECTION = 'text-muted-foreground text-label font-semibold tracking-wide uppercase';

/**
 * A row's surface: the card colour when it is current (or its menu is open), half of it under the
 * pointer. Not the selection colour: muted and status text stay above 4.5:1 on the card in every
 * theme, and fall under it on the selection colour.
 */
const ROW_SURFACE = 'hover:bg-card/50';
const ROW_CURRENT = 'bg-card';

const FOCUS =
  'focus-visible:ring-ring cursor-default outline-none focus-visible:ring-1 focus-visible:ring-inset';

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

interface Group {
  space: SpaceRow;
  rows: AgentRow[];
}

/**
 * The column's groups: each project in the server's order, then the agents that belong to no project
 * (the main thread's own tasks and sessions) under **No project**, which is drawn only when it has
 * any. The main thread itself is pinned above every group, so it is in none.
 */
function groupsOf(model: AgentColumnModel): Group[] {
  const groups = model.spaces.map((space) => ({
    space,
    rows: model.agents.filter((agent) => agent.space === space.id && agent.role !== 'main')
  }));
  const own = groups.filter((group) => group.space.id === MAINLOOP_SPACE);
  const projects = groups.filter((group) => group.space.id !== MAINLOOP_SPACE);
  return [...projects, ...own.filter((group) => group.rows.length > 0)];
}

/** `3 need you`, `1 needs you`. */
const needYou = (n: number) => `${n} ${n === 1 ? 'needs you' : 'need you'}`;

/**
 * A row's second line: what it needs, or where it stands. A row that needs the owner or has failed
 * leads with that, in its state's colour, unless its own words already say so; a supervisor or a
 * session that belongs to no task says what it is.
 */
function MetaLine({ agent }: { agent: AgentRow }) {
  const detail = agent.detail ?? '';
  const urgent = agent.tone === 'attention' || agent.tone === 'bad';
  const word = STATUS_ICON[agent.tone].word;
  const lead = urgent && !detail.toLowerCase().startsWith(word.toLowerCase()) ? word : null;
  const kind = agent.role === 'session' ? 'Session' : null;
  if (!detail && !lead && !kind) return null;
  return (
    <span
      data-testid="agent-meta"
      title={detail || undefined}
      className="text-muted-foreground text-caption min-w-0 truncate"
    >
      {lead ? (
        <span className={cn('font-medium', STATUS_ICON[agent.tone].className)}>
          {lead}
          {detail ? ' · ' : ''}
        </span>
      ) : null}
      {kind ? (
        <span className="text-foreground">
          {kind}
          {detail ? ' · ' : ''}
        </span>
      ) : null}
      {urgent && !lead ? (
        <span className={cn('font-medium', STATUS_ICON[agent.tone].className)}>{detail}</span>
      ) : (
        detail
      )}
    </span>
  );
}

/**
 * The sidebar, an editor's explorer for agents. **Mainloop**, the main thread, is pinned at the top
 * with what it speaks for (`3 need you · 2 working`). Under the **Agents** heading and its buttons
 * (new task, search and commands, and the caller's hide button) come the projects, each a heading
 * with its counts that opens the project's page, over its agents; the main thread's own tasks and
 * sessions follow under **No project**. Every row has the same anatomy: a state icon, the title in
 * medium weight with its age at the right, and one line saying what it needs or where it stands.
 * The agent whose page is showing sits on the page's background and is marked
 * `aria-current="page"`; the project that page belongs to is marked `aria-current="true"`.
 *
 * The rows are one stop for the Tab key: up and down move through every row, Home and End go to
 * the ends, Enter opens the row, and `a` on an agent asks Mainloop about it. A row's actions are
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
  onMenu,
  now: fixedNow
}: AgentColumnProps) {
  const now = useNow(fixedNow);
  const here = currentAgent(model.agents, page);
  const main = model.agents.find((agent) => agent.role === 'main');
  const groups = ready && !failed ? groupsOf(model) : [];
  // Every row in the order drawn: the main thread, then each project's heading and its agents.
  const rowIds = [
    ...(main ? [`agent:${main.id}`] : []),
    ...groups.flatMap((group) => [
      ...(group.space.id === MAINLOOP_SPACE ? [] : [`space:${group.space.id}`]),
      ...group.rows.map((row) => `agent:${row.id}`)
    ])
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

  /** What every row button shares: the roving stop, its keys, its menu and the ref for focus. */
  const rowButton = (id: string, target: Target, ask?: string) => ({
    ref: (el: HTMLButtonElement | null) => {
      if (el) buttons.current.set(id, el);
      else buttons.current.delete(id);
    },
    type: 'button' as const,
    tabIndex: id === activeId ? 0 : -1,
    onFocus: () => setFocusId(id),
    onKeyDown: (event: KeyboardEvent<HTMLButtonElement>) => onKeyDown(event, id, ask),
    onClick: () => onOpen(target),
    onContextMenu: (event: MouseEvent<HTMLButtonElement>) => {
      event.preventDefault();
      openMenu(id, event.clientX, event.clientY);
    }
  });

  const agentRow = (agent: AgentRow) => {
    const id = `agent:${agent.id}`;
    const current = agent === here;
    const age = ageOf(agent, now);
    return (
      <li
        key={id}
        data-node-id={id}
        data-current={current ? 'true' : undefined}
        className={cn(
          'group flex items-stretch',
          current || menu?.id === id ? ROW_CURRENT : ROW_SURFACE
        )}
      >
        <button
          {...rowButton(id, agent.target, agent.ask)}
          data-testid="agent-row"
          aria-current={current ? 'page' : undefined}
          title={agent.label}
          style={agent.depth ? { paddingLeft: `${agent.depth * 20 + 8}px` } : undefined}
          className={cn(
            'gap-sm px-sm min-h-control flex min-w-0 flex-1 items-start py-1 text-left',
            FOCUS
          )}
        >
          {/* The icon sits on the title's line, whether or not a second line follows. */}
          <span className="flex h-5 shrink-0 items-center">
            <StatusIcon tone={agent.tone} testId="agent-dot" />
          </span>
          <span className="flex min-w-0 flex-1 flex-col">
            <span className="gap-sm flex min-w-0 items-baseline">
              <span className="min-w-0 flex-1 truncate font-medium">{agent.label}</span>
              {age ? (
                <span
                  data-testid="agent-age"
                  className="text-muted-foreground text-caption shrink-0 tabular-nums"
                >
                  {age}
                </span>
              ) : null}
            </span>
            <MetaLine agent={agent} />
          </span>
        </button>
        {agent.ask ? (
          <button
            type="button"
            tabIndex={-1}
            aria-label={`Ask Mainloop about ${agent.label}`}
            title="Ask Mainloop about this"
            onClick={() => onAsk(agent.ask ?? '')}
            // A phone has no hover, so it is always shown there, as a touch target. With a mouse it
            // shows on hover, or while its row has keyboard focus.
            className="text-muted-foreground hover:text-foreground lg:mr-xs inline-flex size-11 shrink-0 items-center justify-center self-center lg:size-6 lg:opacity-0 lg:group-focus-within:opacity-100 lg:group-hover:opacity-100"
          >
            <MessageSquarePlus className="size-3.5" aria-hidden="true" />
          </button>
        ) : null}
      </li>
    );
  };

  /** A project's heading: its name and counts. It opens the project's page. */
  const groupHeading = (group: Group) => {
    const { space: row } = group;
    const headingId = `agents-in-${row.id}`;
    if (row.id === MAINLOOP_SPACE) {
      return (
        <h3
          id={headingId}
          data-testid="agent-space"
          className="px-sm pt-sm pb-xs text-muted-foreground font-semibold"
        >
          No project
        </h3>
      );
    }
    const id = `space:${row.id}`;
    const counts = [
      row.needs > 0 ? needYou(row.needs) : null,
      row.working > 0 ? `${row.working} working` : null
    ].filter(Boolean);
    return (
      <h3 id={headingId} data-testid="agent-space" className="m-0 font-normal">
        <button
          {...rowButton(id, row.target)}
          data-testid="space-row"
          data-space-row=""
          aria-current={row.id === space ? 'true' : undefined}
          aria-label={[row.label, ...counts].join(', ')}
          title={row.title ? `${row.title}: open the project` : 'Open the project'}
          className={cn(
            'gap-xs px-sm pt-sm pb-xs flex w-full min-w-0 items-center text-left',
            menu?.id === id ? ROW_CURRENT : ROW_SURFACE,
            FOCUS
          )}
        >
          <span className="min-w-0 flex-1 truncate font-semibold">{row.label}</span>
          {row.needs > 0 ? (
            <span
              aria-hidden="true"
              className="bg-status-attention text-background text-label min-w-5 px-1 text-center font-semibold tabular-nums"
            >
              {row.needs}
            </span>
          ) : null}
          {row.working > 0 ? (
            <span
              aria-hidden="true"
              className="text-status-working text-label inline-flex items-center gap-0.5 font-semibold tabular-nums"
            >
              <LoaderCircle className="size-3" />
              {row.working}
            </span>
          ) : null}
        </button>
      </h3>
    );
  };

  const mainCurrent = main !== undefined && main === here;
  const needs = model.main.needs;
  const working = model.main.working;

  return (
    <section
      aria-label="Spaces and agents"
      data-testid="agent-column"
      className="text-body flex min-h-0 flex-1 flex-col"
    >
      {/* The main thread, pinned: not a project's agent, but the one that runs them all. */}
      {main ? (
        <button
          {...rowButton(`agent:${main.id}`, main.target)}
          data-testid="main-entry"
          data-space-row=""
          aria-current={mainCurrent ? 'page' : space === MAINLOOP_SPACE ? 'true' : undefined}
          aria-label={[
            'Mainloop',
            needs > 0 ? needYou(needs) : null,
            working > 0 ? `${working} working` : null,
            needs === 0 && working === 0 ? 'idle' : null
          ]
            .filter(Boolean)
            .join(', ')}
          className={cn(
            'gap-sm px-sm py-sm border-border-subtle flex min-h-14 shrink-0 items-center border-b text-left',
            mainCurrent || menu?.id === `agent:${main.id}` ? ROW_CURRENT : ROW_SURFACE,
            FOCUS
          )}
        >
          <span className="bg-background border-border-subtle inline-flex size-8 shrink-0 items-center justify-center border">
            <LogoMark />
          </span>
          <span className="flex min-w-0 flex-1 flex-col" aria-hidden="true">
            <span className="text-title font-semibold">Mainloop</span>
            <span
              data-testid="main-state"
              className="text-caption gap-xs text-muted-foreground flex min-w-0 items-center"
            >
              {needs > 0 ? (
                <span className="text-status-attention gap-xs inline-flex shrink-0 items-center font-medium">
                  <CircleAlert className="size-3.5" />
                  {needYou(needs)}
                </span>
              ) : null}
              {working > 0 ? (
                <span className="truncate">
                  {needs > 0 ? '· ' : ''}
                  {working} working
                </span>
              ) : null}
              {needs === 0 && working === 0 ? <span>Idle</span> : null}
            </span>
          </span>
        </button>
      ) : null}
      {/* The heading and its buttons are there from the start, so the menu works while the list loads. */}
      <header className="pl-sm pr-xs flex h-9 shrink-0 items-center">
        <h2 id="agents-heading" className={cn(SECTION, 'min-w-0 flex-1')}>
          Agents
        </h2>
        {onMenu ? (
          <div data-testid="column-actions" className="flex items-center">
            <Button
              type="button"
              variant="ghost"
              size="sm"
              data-testid="column-new"
              aria-label={newLabel(model, space)}
              title={newLabel(model, space)}
              onClick={() => {
                const prefill = newTaskPrefill(model, space);
                if (prefill) onAsk(prefill);
                else onOpen({ to: 'chat' });
              }}
            >
              <Plus aria-hidden="true" />
            </Button>
            <Button
              type="button"
              variant="ghost"
              size="sm"
              data-testid="column-menu"
              aria-label="Search and commands"
              title={`Search and commands (${shortcutLabel('k')})`}
              onClick={onMenu}
            >
              <Search aria-hidden="true" />
            </Button>
          </div>
        ) : null}
        {action}
      </header>
      <div className="pb-sm min-h-0 flex-1 overflow-y-auto">
        {failed ? (
          <p role="alert" className="px-sm py-sm text-term-red text-body">
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
          <div role="group" aria-labelledby="agents-heading">
            {groups.map((group) => (
              <section key={group.space.id} aria-labelledby={`agents-in-${group.space.id}`}>
                {groupHeading(group)}
                {group.rows.length > 0 ? (
                  <ul aria-labelledby={`agents-in-${group.space.id}`} className="m-0 list-none p-0">
                    {group.rows.map(agentRow)}
                  </ul>
                ) : null}
              </section>
            ))}
          </div>
        )}
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
