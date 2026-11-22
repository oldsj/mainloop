/**
 * Task rules the explorer shares: which group a task belongs to and its status line. Pure, so the
 * rules test without a QueryClient. Task state comes from the
 * owner task read; nothing here decides what is allowed.
 */
import type { TaskView } from '@/lib/contract';
import { statusLabel, reasonLabel } from '@/lib/taskState';
import { hasAvailableAction, needsOwner } from '@/features/tasks/cache';

export type WorkGroup = 'needs-you' | 'working' | 'blocked' | 'problem' | 'finished';

/**
 * Blocked reasons that are Mainloop's own recovery work, not an owner decision: the task is not
 * asking the owner anything yet, so it belongs with Problems, not Needs you.
 */
const PROBLEM_REASONS = new Set(['session_lost', 'stalled', 'runtime_limit', 'reconciliation']);

const time = (iso: string): number => Date.parse(iso);

/**
 * How long a failed task that asks the owner nothing stays in front of them: seven days, as long
 * as a finished task they have not opened stays listed. Retry stays available on a failed task for good, so without an end a
 * failure nobody retried would count as needing the owner for ever.
 */
export const FAILED_WINDOW_MS = 7 * 24 * 60 * 60 * 1000;

/**
 * A project's standing supervisor at rest: it waits for its next message. Only a standing task takes
 * this state, and it is neither work in progress nor anything for the owner.
 */
export function isIdle(view: TaskView): boolean {
  return view.task.status === 'waiting' && view.task.reason === 'idle';
}

/**
 * The group a task sits in. Precedence runs top down, so a failed task waiting on the owner counts
 * as needing them. A running, queued or waiting task is current work, never idle. Finished tasks
 * appear only when they finished after the owner's last visit. A failed task with no available
 * action (every one of retry/reassign/cancel refused) is not an owner decision: there is nothing
 * left to do about it, so it gets no group here and is found on its project's page. The same goes for a failed task that asks the
 * owner nothing and last changed more than `FAILED_WINDOW_MS` ago: it can still be retried from
 * its page, but it no longer counts as needing them. A standing supervisor at rest has no group: it
 * is waiting, but not on anything.
 */
export function workGroup(
  view: TaskView,
  lastVisit: number | null,
  now: number = Date.now()
): WorkGroup | null {
  if (needsOwner(view)) return 'needs-you';
  if (isIdle(view)) return null;
  const { status, reason } = view.task;
  if (status === 'blocked' && reason != null && PROBLEM_REASONS.has(reason)) return 'problem';
  if (status === 'failed') {
    const recent = now - time(view.task.updated_at) <= FAILED_WINDOW_MS;
    return hasAvailableAction(view) && recent ? 'blocked' : null;
  }
  if (status === 'blocked') return 'blocked';
  if (status === 'queued' || status === 'running' || status === 'waiting') return 'working';
  if (
    (status === 'completed' || status === 'cancelled') &&
    lastVisit !== null &&
    time(view.task.updated_at) > lastVisit
  ) {
    return 'finished';
  }
  return null;
}

/**
 * Whether the main thread has set the task aside for now (`task_hold`). A hold is not a status:
 * the task goes on. It only keeps the task out of the owner's way until the time passes.
 */
export function isHeld(view: TaskView, now: number): boolean {
  const until = view.task.held_until ? Date.parse(view.task.held_until) : Number.NaN;
  return Number.isFinite(until) && until > now;
}

export function statusLine(view: TaskView): string {
  // An approval gate says what the owner must do, whatever the task's own status is.
  if (view.task.reason === 'approval' || (view.projection.pending_approval_ids ?? []).length > 0) {
    return 'Waiting for your approval';
  }
  if (isIdle(view)) return 'Idle';
  const reason = reasonLabel(view.task.reason);
  return reason ? `${statusLabel(view.task.status)} · ${reason}` : statusLabel(view.task.status);
}

/** Row wording: a count and its noun, singular or plural. */
export function countText(count: number, singular: string, plural = `${singular}s`): string {
  return `${count} ${count === 1 ? singular : plural}`;
}
