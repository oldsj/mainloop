import { Link } from '@tanstack/react-router';
import { StatusBadge } from '@/components/shared/status-badge';
import type { TaskView } from '@/lib/contract';
import {
  buildTree,
  openOperations,
  providerLabel,
  publicationFacts,
  reasonLabel,
  statusLabel,
  statusTone
} from '@/lib/taskState';
import { indentClass } from './model';
import { useTaskList } from './queries';

/**
 * Tasks with their children beneath them, optionally limited to one project. Each row shows what
 * the list can say without opening the task. A row whose parent is not in this list is shown at
 * the top level and says so, so a filtered list never hides work.
 */
export function TaskTree({ projectId }: { projectId?: string }) {
  const list = useTaskList(projectId ? { project_id: projectId } : {});

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
  return (
    <section aria-label="Tasks" className="flex flex-col">
      {list.isError ? (
        <p role="alert" className="p-md text-destructive text-sm">
          Could not load tasks: {errorText(list.error)}.{' '}
          {rows.length > 0 ? 'Showing the last known state.' : null}
        </p>
      ) : null}
      {rows.length === 0 ? (
        list.isError ? null : (
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

function TaskRow({ view, depth, detached }: { view: TaskView; depth: number; detached: boolean }) {
  const facts = publicationFacts(view.projection);
  const operation = openOperations(view)[0];
  const reason = view.task.reason ? reasonLabel(view.task.reason) : null;
  return (
    <li className={indentClass(depth)}>
      <Link
        to="/tasks/$id"
        params={{ id: view.task.id }}
        className="gap-xs border-border p-md flex min-h-11 flex-col border-b"
      >
        <span className="gap-sm flex flex-wrap items-center">
          <span data-testid="task-status">
            <StatusBadge
              label={`[${statusLabel(view.task.status)}]`}
              tone={statusTone(view.task.status)}
            />
          </span>
          {view.task.mode === 'coordination' ? (
            <StatusBadge label="[SUPERVISOR]" tone="active" />
          ) : null}
        </span>
        <span className="text-sm font-medium break-words">{view.task.title}</span>
        <span className="text-muted-foreground text-xs break-words">
          {providerLabel(view)}
          {reason ? ` · ${reason}` : ''}
        </span>
        {operation ? <span className="text-xs break-words">{operation.summary}</span> : null}
        <span className="text-muted-foreground text-xs">
          CI {facts.ci.known ? facts.ci.label.toLowerCase() : 'unknown'}
        </span>
        {detached ? (
          <span className="text-muted-foreground text-xs">child of a task not shown here</span>
        ) : null}
      </Link>
    </li>
  );
}
