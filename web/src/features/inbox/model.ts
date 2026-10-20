/**
 * Inbox data rules: boundary checks on what `/attention` and `/activity` send, and the small pure
 * helpers the inbox renders with. Components and hooks live in their own files.
 */
import { ApiError } from '@/api';
import type { components } from '@/api/schema.gen';
import { httpsUrl } from '@/lib/url';

type Schemas = components['schemas'];
export type AttentionItem = Schemas['AttentionItem'];
export type AttentionSnapshot = Schemas['AttentionSnapshot'];
export type ActivityItem = Schemas['ActivityItem'];
export type QueueItem = Schemas['QueueItem'];

const ATTENTION_KINDS: readonly string[] = ['approval', 'question', 'owner_decision', 'problem'];
const TASK_PATH = /^\/tasks\/([^/?#]+)$/;

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === 'object' && value !== null;

const isStringOrNull = (value: unknown): value is string | null =>
  value === null || typeof value === 'string';

const isStringList = (value: unknown): value is string[] =>
  Array.isArray(value) && value.every((option) => typeof option === 'string');

/** The fields the card reads as text or lists. A row with one of them wrong is dropped, not shown. */
function isQueueItem(value: unknown): value is QueueItem {
  return (
    isRecord(value) &&
    typeof value.id === 'string' &&
    typeof value.item_type === 'string' &&
    typeof value.title === 'string' &&
    typeof value.content === 'string' &&
    typeof value.status === 'string' &&
    typeof value.created_at === 'string' &&
    isStringOrNull(value.hitl_request_id) &&
    isStringOrNull(value.response) &&
    (value.options === null || isStringList(value.options))
  );
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
    typeof value.created_at === 'string' &&
    isStringOrNull(value.href) &&
    isStringOrNull(value.queue_item_id) &&
    typeof value.dismissible === 'boolean'
  );
}

/** The items this client understands. A malformed item is dropped, never shown half-formed. */
export function attentionItems(snapshot: AttentionSnapshot | undefined): AttentionItem[] {
  const items: unknown = snapshot?.items;
  return Array.isArray(items) ? items.filter(isAttentionItem) : [];
}

/**
 * Needs you: items with a real owner decision. A problem (a stall, an unconfirmed stop) is
 * Mainloop's own recovery work, not a decision, so it is left out here; see `problemItems`.
 */
export function needsYouItems(items: AttentionItem[]): AttentionItem[] {
  return items.filter((item) => item.kind !== 'problem');
}

/** Problems: what Mainloop is handling on its own, with no decision yet for the owner to make. */
export function problemItems(items: AttentionItem[]): AttentionItem[] {
  return items.filter((item) => item.kind === 'problem');
}

/** The server's count of unresolved owner actions. It is the badge number; Activity is not in it. */
export function unresolvedCount(snapshot: AttentionSnapshot | undefined): number {
  const count = snapshot?.count;
  return typeof count === 'number' && Number.isFinite(count) && count > 0 ? Math.floor(count) : 0;
}

/** Badge text: nothing at zero, `99+` above that. */
export function badgeText(count: number): string | null {
  if (count <= 0) return null;
  return count > 99 ? '99+' : String(count);
}

export function activityItems(list: unknown): ActivityItem[] {
  return Array.isArray(list) ? list.filter(isActivityItem) : [];
}

/**
 * The Activity rows without the ones behind one queue item. Malformed rows are dropped, as on
 * screen.
 */
export function withoutActivityItem(list: unknown, queueItemId: string): ActivityItem[] {
  return activityItems(list).filter((item) => item.queue_item_id !== queueItemId);
}

/**
 * The key a Needs-you entry is known by: its approval request id when it has one, else its queue
 * item id. A task's pending approval ids are these keys, so a task link can name its row.
 */
export function attentionKey(item: AttentionItem): string {
  return item.queue_item?.hitl_request_id ?? item.queue_item?.id ?? item.id;
}

/** The element id a Needs-you entry is linked by. It matches the id the frozen inbox used. */
export function attentionAnchor(item: AttentionItem): string {
  return `attention-${attentionKey(item)}`;
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
  return httpsUrl(item.context?.pr_url);
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
