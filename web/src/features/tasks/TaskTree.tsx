import { Link } from '@tanstack/react-router';
import { useEffect, useState } from 'react';
import { StatusDot } from '@/components/shared/status-dot';
import { statusLine, workGroup } from '@/features/home/model';
import type { TaskView } from '@/lib/contract';
import {
  buildTree,
  isTerminal,
  openOperations,
  publicationFacts,
  relativeAge,
  statusLabel,
  type Tone
} from '@/lib/taskState';
import { unreadableTasksText } from './model';
import { useTaskList, useUnreadableTaskCount } from './queries';

/**
 * Tasks with their children beneath them, optionally limited to one project. Each row is one line
 * and shows what the list can say without opening the task. A row whose parent is not in this list is shown at
 * the top level and says so, so a filtered list never hides work. A row this page cannot read is
 * not shown, and the list says how many.
 */
export function TaskTree({ projectId }: { projectId?: string }) {
  const filter = projectId ? { project_id: projectId } : {};
  const list = useTaskList(filter);
  const unreadable = useUnreadableTaskCount(filter);
  const now = useNow();

  if (list.data === undefined) {
    if (list.isPending) {
      return (
        <p role="status" className="p-md text-muted-foreground text-sm">
          Loading tasks…
        </p>
      );
    }
    return (
      <p role="alert" className="p-md text-destructive text-sm">
        Could not load tasks: {errorText(list.error)}
      </p>
    );
  }

  const rows = buildTree(list.data);
  const unreadableCount = unreadable.data ?? 0;
  return (
    <section aria-label="Tasks" className="flex flex-col">
      {list.isError ? (
        <p role="alert" className="p-md text-destructive text-sm">
          Could not load tasks: {errorText(list.error)}.{' '}
          {rows.length > 0 ? 'Showing the last known state.' : null}
        </p>
      ) : null}
      {unreadableCount > 0 ? (
        <p role="status" className="px-md pt-sm text-term-yellow text-sm">
          {unreadableTasksText(unreadableCount)}
        </p>
      ) : null}
      {rows.length === 0 ? (
        list.isError || unreadableCount > 0 ? null : (
          <p className="p-md text-muted-foreground text-sm">No tasks yet.</p>
        )
      ) : (
        <ul data-testid="task-list" className="flex flex-col">
          {rows.map((row) => (
            <TaskRow
              key={row.view.task.id}
              view={row.view}
              depth={row.depth}
              detached={row.detached}
              now={now}
            />
          ))}
        </ul>
      )}
    </section>
  );
}

function errorText(error: unknown): string {
  return error instanceof Error ? error.message : 'unknown error';
}

/** The clock a row's age is measured from. It ticks once a minute, so ages stay current. */
function useNow(): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 60_000);
    return () => clearInterval(timer);
  }, []);
  return now;
}

/**
 * The dot's tone, in the status language the explorer uses: amber while the task works, red when it
 * needs the owner (a decision, an approval, or a failure they can still act on), hollow at rest.
 */
function rowTone(view: TaskView): Tone {
  const group = workGroup(view, null);
  if (group === 'needs-you' || group === 'blocked') return 'attention';
  if (group === 'working') return 'active';
  return 'muted';
}

/** Each level steps in by this much, and a guide line marks each ancestor. */
const INDENT_PX = 16;
const GUTTER_PX = 12;

/**
 * One task on one line: the status dot, the title, what state it is in, and how long ago it
 * changed. A child sits under its parent, stepped in, with a guide line for each level above it.
 */
function TaskRow({
  view,
  depth,
  detached,
  now
}: {
  view: TaskView;
  depth: number;
  detached: boolean;
  now: number;
}) {
  const facts = publicationFacts(view.projection);
  const operation = openOperations(view)[0];
  // CI is said only when it is known. A finished task says nothing about CI: "CI unknown" there is
  // noise, not a fact the owner can act on.
  const ci =
    facts.ci.known && !isTerminal(view.task.status) ? `CI ${facts.ci.label.toLowerCase()}` : null;
  const state = [
    view.task.mode === 'coordination' ? 'supervisor' : null,
    statusLine(view),
    operation?.summary,
    ci,
    detached ? 'child of a task not shown here' : null
  ]
    .filter(Boolean)
    .join(' · ');
  const changed = Date.parse(view.task.updated_at);
  return (
    <li
      data-depth={depth}
      // A hairline sets each top-level task, with the children under it, apart from the next.
      className={depth === 0 ? 'border-border-subtle border-t first:border-t-0' : undefined}
    >
      <Link
        to="/tasks/$id"
        params={{ id: view.task.id }}
        className="gap-sm pr-sm min-h-control hover:bg-accent hover:text-accent-foreground relative flex items-center text-sm"
        style={{ paddingLeft: `${depth * INDENT_PX + GUTTER_PX}px` }}
      >
        {Array.from({ length: depth }, (_, level) => (
          <span
            key={level}
            aria-hidden="true"
            data-testid="task-indent-guide"
            className="bg-border absolute inset-y-0 w-px"
            style={{ left: `${level * INDENT_PX + GUTTER_PX + 4}px` }}
          />
        ))}
        <StatusDot tone={rowTone(view)} testId="task-dot" />
        <span className="min-w-0 flex-1 basis-1/2 truncate">{view.task.title}</span>
        {/* A narrow screen keeps the status word alone, so the state is never colour only. */}
        <span
          data-testid="task-status"
          className="text-muted-foreground shrink-0 text-xs sm:hidden"
        >
          {statusLabel(view.task.status)}
        </span>
        <span
          data-testid="task-state"
          className="text-muted-foreground hidden min-w-0 shrink truncate text-right text-xs sm:inline"
        >
          {state}
        </span>
        {Number.isFinite(changed) ? (
          <time
            dateTime={view.task.updated_at}
            data-testid="task-age"
            className="text-muted-foreground w-8 shrink-0 text-right text-xs"
          >
            {relativeAge(Math.max(0, now - changed))}
          </time>
        ) : null}
      </Link>
    </li>
  );
}
