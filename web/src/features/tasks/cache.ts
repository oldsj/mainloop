/**
 * Version gates for the task caches. Live updates and background reads can land out of order, so
 * an older view must never replace a newer one that is already held. These functions run as
 * TanStack `structuralSharing`, which also applies to `setQueryData`, so one rule covers every
 * write path.
 *
 * Payloads are validated at this boundary. A malformed view never replaces a valid one, and a
 * malformed attempt is dropped, so the page keeps running instead of crashing on a missing field.
 */
import { replaceEqualDeep } from '@tanstack/react-query';
import type { TaskAttempt, TaskView } from '@/lib/contract';
import { normalizeView } from '@/lib/taskState';

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === 'object' && value !== null && !Array.isArray(value);

/** The attempt fields the page reads without a fallback. */
const isAttempt = (value: unknown): value is TaskAttempt =>
  isRecord(value) && typeof value.id === 'string' && typeof value.number === 'number';

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
 * The view the page reads: normalised, with any malformed attempt dropped. One bad attempt would
 * otherwise throw inside `currentAttempt` and `attemptHistory` and take the whole task down.
 */
export function readableView(value: TaskView): TaskView {
  const view = normalizeView(value);
  return { ...view, attempts: view.attempts.filter(isAttempt) };
}

/**
 * One task's cached view. An older version keeps the held view; a newer or equal one replaces it,
 * with unchanged parts keeping their references so renders stay cheap. A malformed payload never
 * replaces a valid held view: the held one stays, and the read is reported as failed.
 */
export function mergeTaskCache(prev: unknown, next: unknown): unknown {
  if (!isTaskView(next)) return isTaskView(prev) ? prev : next;
  const fresh = readableView(next);
  if (!isTaskView(prev)) return fresh;
  if (fresh.task.version < prev.task.version) return prev;
  return replaceEqualDeep(prev, fresh);
}

/**
 * The task list. Membership comes from the latest read, so a task that left the list leaves it.
 * Each readable row is version-gated against the row already held. A row that fails validation is
 * kept as it arrived, so the list can count it; `readableTaskRows` leaves it out of what is shown.
 */
export function mergeTaskListCache(prev: unknown, next: unknown): unknown {
  if (!Array.isArray(next)) return next;
  const held = new Map<string, TaskView>();
  if (Array.isArray(prev)) {
    for (const row of prev) if (isTaskView(row)) held.set(row.task.id, row);
  }
  const rows = next.map((row) =>
    isTaskView(row) ? mergeTaskCache(held.get(row.task.id), row) : row
  );
  return replaceEqualDeep(prev, rows);
}

/** The rows the list shows: those that pass validation. */
export function readableTaskRows(rows: unknown[]): TaskView[] {
  return rows.filter(isTaskView);
}

/** How many rows of a list read this page cannot show. */
export function unreadableTaskCount(rows: unknown[]): number {
  return rows.length - readableTaskRows(rows).length;
}

const OWNER_REASONS = new Set<string>(['owner_decision', 'approval']);

/**
 * A task waiting on publication whose pull request is open with green CI: nothing automated
 * will move it further, and only the owner can merge or otherwise follow up. Kept apart from
 * `OWNER_REASONS` since `publication` is not a policy approval gate; this checks the observed
 * PR/CI facts instead of the reason alone.
 */
function publicationStalled(view: TaskView): boolean {
  return (
    view.task.status === 'waiting' &&
    view.task.reason === 'publication' &&
    view.projection.pr_state === 'open' &&
    view.projection.ci_state === 'success'
  );
}

/**
 * A task that waits on the owner: a pending approval, a reason that needs a decision, or an
 * open, CI-green pull request that has not been merged.
 */
export function needsOwner(view: TaskView): boolean {
  if ((view.projection.pending_approval_ids ?? []).length > 0) return true;
  if (view.task.reason != null && OWNER_REASONS.has(view.task.reason)) return true;
  return publicationStalled(view);
}

export function tasksNeedingOwner(views: TaskView[]): TaskView[] {
  return views.filter(needsOwner);
}
