/**
 * Version gates for the task caches. Live updates and background reads can land out of order, so
 * an older view must never replace a newer one that is already held. These functions run as
 * TanStack `structuralSharing`, which also applies to `setQueryData`, so one rule covers every
 * write path.
 *
 * Payloads are validated at this boundary. A malformed view is passed through unchanged, so the
 * page can say it cannot read the task instead of crashing on a missing field.
 */
import { replaceEqualDeep } from '@tanstack/react-query';
import type { TaskView } from '@/lib/contract';
import { normalizeView } from '@/lib/taskState';

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === 'object' && value !== null && !Array.isArray(value);

/** The fields the page reads without a fallback. Everything else is filled by `normalizeView`. */
export function isTaskView(value: unknown): value is TaskView {
  if (!isRecord(value) || !isRecord(value.task)) return false;
  const task = value.task;
  if (
    typeof task.id !== 'string' ||
    typeof task.title !== 'string' ||
    typeof task.status !== 'string' ||
    typeof task.version !== 'number' ||
    typeof task.created_at !== 'string'
  )
    return false;
  for (const key of ['attempts', 'operations', 'artifacts', 'reports'] as const) {
    if (value[key] !== undefined && !Array.isArray(value[key])) return false;
  }
  if (value.projection !== undefined && !isRecord(value.projection)) return false;
  if (value.actions !== undefined && !isRecord(value.actions)) return false;
  return true;
}

/**
 * One task's cached view. An older version keeps the held view; a newer or equal one replaces it,
 * with unchanged parts keeping their references so renders stay cheap.
 */
export function mergeTaskCache(prev: unknown, next: unknown): unknown {
  if (!isTaskView(next)) return next;
  const fresh = normalizeView(next);
  if (!isTaskView(prev)) return fresh;
  if (fresh.task.version < prev.task.version) return prev;
  return replaceEqualDeep(prev, fresh);
}

/**
 * The task list. Membership comes from the latest read, so a task that left the list leaves it.
 * Each row is version-gated against the row already held. Rows that fail validation are dropped
 * rather than failing the whole list.
 */
export function mergeTaskListCache(prev: unknown, next: unknown): unknown {
  if (!Array.isArray(next)) return next;
  const held = new Map<string, TaskView>();
  if (Array.isArray(prev)) {
    for (const row of prev) if (isTaskView(row)) held.set(row.task.id, row);
  }
  const rows = next
    .filter(isTaskView)
    .map((row) => mergeTaskCache(held.get(row.task.id), row) as TaskView);
  return replaceEqualDeep(prev, rows);
}

const OWNER_REASONS = new Set<string>(['owner_decision', 'approval']);

/** A task that waits on the owner: a pending approval, or a reason that needs a decision. */
export function needsOwner(view: TaskView): boolean {
  if ((view.projection.pending_approval_ids ?? []).length > 0) return true;
  return view.task.reason != null && OWNER_REASONS.has(view.task.reason);
}

export function tasksNeedingOwner(views: TaskView[]): TaskView[] {
  return views.filter(needsOwner);
}
