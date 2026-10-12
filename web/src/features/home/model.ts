/**
 * Home's work list rules: which group a task belongs to, how groups order, what the "Since you were
 * here" strip counts, and the relative time each row shows. Pure, so the rules test without a
 * QueryClient. Task state comes from the owner task read; nothing here decides what is allowed.
 */
import type { TaskView } from '@/lib/contract';
import { statusLabel, reasonLabel } from '@/lib/taskState';
import { needsOwner } from '@/features/tasks/cache';

export type WorkGroup = 'needs-you' | 'working' | 'blocked' | 'finished';

export interface WorkRow {
  view: TaskView;
  group: WorkGroup;
  /** One line: status, then the reason when the server gives one. */
  statusLine: string;
  updatedAt: number;
}

export interface WorkGroups {
  'needs-you': WorkRow[];
  working: WorkRow[];
  blocked: WorkRow[];
  finished: WorkRow[];
}

export interface SinceCounts {
  merged: TaskView[];
  failed: TaskView[];
  waiting: TaskView[];
}

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

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

function statusLine(view: TaskView): string {
  // An approval gate says what the owner must do, whatever the task's own status is.
  if (view.task.reason === 'approval' || (view.projection.pending_approval_ids ?? []).length > 0) {
    return 'Waiting for your approval';
  }
  const reason = reasonLabel(view.task.reason);
  return reason ? `${statusLabel(view.task.status)} · ${reason}` : statusLabel(view.task.status);
}

/** Newest update first. Ties keep the server's order, so the list does not shuffle on re-read. */
const byUpdated = (a: WorkRow, b: WorkRow) => b.updatedAt - a.updatedAt;

/** Groups the task list. Each task appears once; empty groups are empty arrays, not missing. */
export function groupWork(views: TaskView[], lastVisit: number | null): WorkGroups {
  const groups: WorkGroups = { 'needs-you': [], working: [], blocked: [], finished: [] };
  for (const view of views) {
    const group = workGroup(view, lastVisit);
    if (group === null) continue;
    groups[group].push({
      view,
      group,
      statusLine: statusLine(view),
      updatedAt: time(view.task.updated_at)
    });
  }
  for (const rows of Object.values(groups)) rows.sort(byUpdated);
  return groups;
}

/** Whether a task changed after the owner's last visit. No visit on record counts nothing as new. */
const changedSince = (view: TaskView, lastVisit: number | null) =>
  lastVisit !== null && time(view.task.updated_at) > lastVisit;

/** The strip's three counts. Each list is newest first, so its first task is the link target. */
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

/** "just now", "12 min ago", "3 h ago", "2 d ago", then the date. Future times read as just now. */
export function relativeTime(iso: string, now: number = Date.now()): string {
  const then = time(iso);
  if (Number.isNaN(then)) return '';
  const age = now - then;
  if (age < MINUTE) return 'just now';
  if (age < HOUR) return `${Math.floor(age / MINUTE)} min ago`;
  if (age < DAY) return `${Math.floor(age / HOUR)} h ago`;
  if (age < 7 * DAY) return `${Math.floor(age / DAY)} d ago`;
  return new Date(then).toLocaleDateString([], { month: 'short', day: 'numeric' });
}

/** Strip wording: a count and its noun, singular or plural. */
export function countText(count: number, singular: string, plural = `${singular}s`): string {
  return `${count} ${count === 1 ? singular : plural}`;
}
