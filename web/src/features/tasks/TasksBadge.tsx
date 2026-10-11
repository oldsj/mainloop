import { tasksNeedingOwner } from './cache';
import { useTaskList } from './queries';

/**
 * How many tasks wait on the owner. Not mounted by this slice: the shell's inbox toggle (F5)
 * decides where it appears. Renders nothing when no task needs the owner.
 */
export function TasksBadge() {
  const list = useTaskList();
  const count = tasksNeedingOwner(list.data ?? []).length;
  if (count === 0) return null;
  return (
    <span
      data-testid="tasks-badge"
      className="px-sm py-xs border-term-yellow/50 text-term-yellow inline-flex border text-xs font-medium"
    >
      {count} {count === 1 ? 'task needs' : 'tasks need'} you
    </span>
  );
}
