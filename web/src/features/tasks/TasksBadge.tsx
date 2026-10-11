import { tasksNeedingOwner } from './cache';
import { useTaskList } from './queries';

/** The owner-task count in words, as the badge and the tab's accessible name both read it. */
export function ownerTaskText(count: number): string {
  return `${count} ${count === 1 ? 'task needs' : 'tasks need'} you`;
}

/**
 * How many tasks wait on the owner. The full form is a labelled pill; `compact` is the bare count
 * for the compact tab bar, where the tab's name carries the words. Renders nothing at zero.
 */
export function TasksBadge({ compact = false }: { compact?: boolean }) {
  const list = useTaskList();
  const count = tasksNeedingOwner(list.data ?? []).length;
  if (count === 0) return null;
  if (compact)
    return (
      <span
        data-testid="tasks-badge"
        className="border-term-yellow text-term-yellow bg-background inline-flex min-w-5 items-center justify-center border px-1 text-xs tabular-nums"
      >
        {count}
      </span>
    );
  return (
    <span
      data-testid="tasks-badge"
      className="px-sm py-xs border-term-yellow/50 text-term-yellow inline-flex border text-xs font-medium"
    >
      {ownerTaskText(count)}
    </span>
  );
}
