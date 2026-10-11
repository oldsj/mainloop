import type { ActivityItem, AttentionItem, AttentionSnapshot, QueueItem } from '../model';

const NOW = '2026-10-11T09:00:00.000Z';

export function queueItem(overrides: Partial<QueueItem> = {}): QueueItem {
  return {
    id: 'q-1',
    main_thread_id: 'main',
    task_id: 'task-1',
    user_id: 'owner',
    hitl_request_id: null,
    item_type: 'question',
    priority: 'normal',
    title: 'Which branch?',
    content: 'Pick a branch to continue from.',
    context: {},
    options: null,
    status: 'pending',
    response: null,
    responded_at: null,
    read_at: null,
    created_at: NOW,
    expires_at: null,
    ...overrides
  };
}

/** A Needs-you row for a queue item, with the fields the server derives from it. */
export function attentionItem(
  queue: QueueItem,
  overrides: Partial<AttentionItem> = {}
): AttentionItem {
  return {
    id: `queue:${queue.id}`,
    kind: queue.item_type === 'question' ? 'question' : 'approval',
    title: queue.title,
    needed: queue.content,
    href: `/?inbox=${queue.id}#attention-${queue.id}`,
    session_ids: [],
    task_id: queue.task_id,
    queue_item: queue,
    ...overrides
  };
}

/** A Needs-you row for a live HITL request. */
export function hitlAttentionItem(
  requestId: string,
  needed = 'Review the pending tool calls.'
): AttentionItem {
  const queue = queueItem({
    id: `queue-${requestId}`,
    item_type: 'hitl_request',
    hitl_request_id: requestId,
    title: 'Approve tool calls',
    content: needed
  });
  return {
    id: `hitl:${requestId}`,
    kind: 'approval',
    title: queue.title,
    needed,
    href: `/?inbox=${requestId}#attention-${requestId}`,
    session_ids: [],
    task_id: queue.task_id,
    queue_item: queue
  };
}

export function attentionSnapshot(items: AttentionItem[], count = items.length): AttentionSnapshot {
  return { items, count, sessions: [] };
}

export function activityRow(overrides: Partial<ActivityItem> = {}): ActivityItem {
  return {
    id: 'report:r-1',
    title: 'Tests passed',
    content: 'All 42 checks passed on the branch.',
    href: '/tasks/task-1',
    created_at: NOW,
    ...overrides
  };
}
