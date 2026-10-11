/**
 * Inbox data rules: boundary checks on what `/attention` and `/activity` send, and the small pure
 * helpers the inbox renders with. Components and hooks live in their own files.
 */
import { ApiError } from '@/api';
import type { components } from '@/api/schema.gen';

type Schemas = components['schemas'];
export type AttentionItem = Schemas['AttentionItem'];
export type AttentionSnapshot = Schemas['AttentionSnapshot'];
export type ActivityItem = Schemas['ActivityItem'];
export type QueueItem = Schemas['QueueItem'];

const ATTENTION_KINDS: readonly string[] = ['approval', 'question', 'owner_decision'];
const TASK_PATH = /^\/tasks\/([^/?#]+)$/;

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === 'object' && value !== null;

function isQueueItem(value: unknown): value is QueueItem {
  return isRecord(value) && typeof value.id === 'string' && typeof value.item_type === 'string';
}

function isAttentionItem(value: unknown): value is AttentionItem {
  return (
    isRecord(value) &&
    typeof value.id === 'string' &&
    typeof value.kind === 'string' &&
    ATTENTION_KINDS.includes(value.kind) &&
    typeof value.title === 'string' &&
    typeof value.needed === 'string' &&
    (value.queue_item === null || isQueueItem(value.queue_item))
  );
}

function isActivityItem(value: unknown): value is ActivityItem {
  return (
    isRecord(value) &&
    typeof value.id === 'string' &&
    typeof value.title === 'string' &&
    typeof value.content === 'string' &&
    typeof value.created_at === 'string'
  );
}

/** The items this client understands. A malformed item is dropped, never shown half-formed. */
export function attentionItems(snapshot: AttentionSnapshot | undefined): AttentionItem[] {
  const items: unknown = snapshot?.items;
  return Array.isArray(items) ? items.filter(isAttentionItem) : [];
}

/** The server's count of unresolved owner actions. It is the badge number; Activity is not in it. */
export function unresolvedCount(snapshot: AttentionSnapshot | undefined): number {
  const count = snapshot?.count;
  return typeof count === 'number' && Number.isFinite(count) && count > 0 ? Math.floor(count) : 0;
}

/** The snapshot without one queue item's row, with the count reduced by the rows removed. */
export function withoutQueueItem(
  snapshot: AttentionSnapshot,
  queueItemId: string
): AttentionSnapshot {
  const kept = snapshot.items.filter((item) => item.queue_item?.id !== queueItemId);
  const removed = snapshot.items.length - kept.length;
  return {
    ...snapshot,
    items: kept,
    count: Math.max(0, unresolvedCount(snapshot) - removed)
  };
}

/** Badge text: nothing at zero, `99+` above that. */
export function badgeText(count: number): string | null {
  if (count <= 0) return null;
  return count > 99 ? '99+' : String(count);
}

export function activityItems(list: unknown): ActivityItem[] {
  return Array.isArray(list) ? list.filter(isActivityItem) : [];
}

/** The element id a Needs-you entry is linked by. It matches the id the frozen inbox used. */
export function attentionAnchor(item: AttentionItem): string {
  return `attention-${item.queue_item?.hitl_request_id ?? item.queue_item?.id ?? item.id}`;
}

/** The task an entry belongs to: its own task id, else the `/tasks/<id>` link it carries. */
export function taskIdOf(taskId: string | null | undefined, href?: string | null): string | null {
  if (taskId) return taskId;
  const match = href ? TASK_PATH.exec(href) : null;
  if (!match?.[1]) return null;
  try {
    return decodeURIComponent(match[1]);
  } catch {
    return null;
  }
}

/** Age in the compact form the inbox has always used: now, 5m, 3h, 2d. */
export function ageLabel(iso: string, now: number = Date.now()): string {
  const time = Date.parse(iso);
  if (Number.isNaN(time)) return '';
  const minutes = Math.floor((now - time) / 60_000);
  if (minutes < 1) return 'now';
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h`;
  return `${Math.floor(hours / 24)}d`;
}

const PRIORITY_BORDER: Record<string, string> = {
  urgent: 'border-l-term-error',
  high: 'border-l-term-warning',
  normal: 'border-l-term-info',
  low: 'border-l-term-fg-muted'
};

export function priorityBorder(priority: string): string {
  return PRIORITY_BORDER[priority] ?? 'border-l-term-info';
}

/** A PR link is shown only for an https URL, so a provider-supplied string cannot point anywhere else. */
export function prLink(item: QueueItem): string | null {
  const url = item.context?.pr_url;
  if (typeof url !== 'string') return null;
  try {
    return new URL(url).protocol === 'https:' ? url : null;
  } catch {
    return null;
  }
}

/** A free-text reply is offered for questions and plan reviews that are still open. */
export function takesTextReply(item: QueueItem): boolean {
  return (
    (item.item_type === 'question' || item.item_type === 'plan_review') && item.status === 'pending'
  );
}

/** The sentence an error shows: the server's detail when it sent one, else a reachability message. */
export function messageOf(error: unknown): string {
  return error instanceof ApiError ? error.message : "Can't reach the Mainloop backend.";
}
