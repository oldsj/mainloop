/**
 * Task rules the explorer shares: which group a task belongs to, its status line, and what "Since you
 * were here" counts. Pure, so the rules test without a QueryClient. Task state comes from the
 * owner task read; nothing here decides what is allowed.
 */
import type { TaskView } from '@/lib/contract';
import { statusLabel, reasonLabel } from '@/lib/taskState';
import { needsOwner } from '@/features/tasks/cache';

export type WorkGroup = 'needs-you' | 'working' | 'blocked' | 'finished';

export interface SinceCounts {
  merged: TaskView[];
  failed: TaskView[];
  waiting: TaskView[];
}

const time = (iso: string): number => Date.parse(iso);

/**
 * The group a task sits in. Precedence runs top down, so a failed task waiting on the owner counts
 * as needing them. A running, queued or waiting task is current work, never idle. Finished tasks
 * appear only when they finished after the owner's last visit.
 */
export function workGroup(view: TaskView, lastVisit: number | null): WorkGroup | null {
  if (needsOwner(view)) return 'needs-you';
  const { status } = view.task;
  if (status === 'failed' || status === 'blocked') return 'blocked';
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

export function statusLine(view: TaskView): string {
  // An approval gate says what the owner must do, whatever the task's own status is.
  if (view.task.reason === 'approval' || (view.projection.pending_approval_ids ?? []).length > 0) {
    return 'Waiting for your approval';
  }
  const reason = reasonLabel(view.task.reason);
  return reason ? `${statusLabel(view.task.status)} · ${reason}` : statusLabel(view.task.status);
}

/** Whether a task changed after the owner's last visit. No visit on record counts nothing as new. */
const changedSince = (view: TaskView, lastVisit: number | null) =>
  lastVisit !== null && time(view.task.updated_at) > lastVisit;

/** The three "Since you were here" counts. Each list is newest first, so its first task is the link target. */
export function sinceCounts(views: TaskView[], lastVisit: number | null): SinceCounts {
  const counts: SinceCounts = { merged: [], failed: [], waiting: [] };
  for (const view of [...views].sort((a, b) => time(b.task.updated_at) - time(a.task.updated_at))) {
    if (!changedSince(view, lastVisit)) continue;
    if (view.task.status === 'completed' && view.projection.merge_state === 'merged') {
      counts.merged.push(view);
    }
    if (view.task.status === 'failed') counts.failed.push(view);
    if (needsOwner(view)) counts.waiting.push(view);
  }
  return counts;
}

/** Row wording: a count and its noun, singular or plural. */
export function countText(count: number, singular: string, plural = `${singular}s`): string {
  return `${count} ${count === 1 ? singular : plural}`;
}
