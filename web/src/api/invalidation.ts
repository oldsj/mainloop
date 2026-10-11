import type { QueryClient, QueryKey } from '@tanstack/react-query';
import { z } from 'zod';
import { allKeys, keys } from './keys';
import type { SseMessage } from './sse';

/**
 * What each server event does to the cache (plan §4.4). The map is pure so it can be tested
 * without a QueryClient; `applySseEvent` runs it.
 *
 * - `connected`: refetch everything (the first connect of a page load needs nothing: queries are
 *   already loading).
 * - `inbox:updated`: attention and activity.
 * - `task:updated`: attention, task lists, and the task itself when the event is newer than the
 *   held version (duplicates are dropped by event id).
 * - `workspace:updated`: upsert the workspace.
 * - `session:message`: that session's conversation and native info, plus the main thread.
 * - `heartbeat` and anything unknown: nothing.
 */
const taskEvent = z.object({
  event_id: z.string(),
  task_id: z.string(),
  version: z.number().finite()
});
const sessionMessageEvent = z.object({ session_id: z.string() });
const workspaceEvent = z.object({
  workspace: z.object({ workspace_id: z.string() }).loose()
});

export type CacheAction =
  | { kind: 'invalidate'; keys: readonly QueryKey[] }
  | { kind: 'task'; taskId: string; version: number; eventId: string }
  | { kind: 'upsertWorkspace'; workspaceId: string; workspace: unknown }
  | { kind: 'none' };

export function planSseEvent(
  message: Pick<SseMessage, 'event' | 'data'>,
  context: { firstConnect: boolean }
): CacheAction[] {
  let payload: unknown;
  try {
    payload = message.data ? JSON.parse(message.data) : {};
  } catch {
    return [{ kind: 'none' }];
  }

  switch (message.event) {
    case 'connected':
      return context.firstConnect ? [{ kind: 'none' }] : [{ kind: 'invalidate', keys: [allKeys] }];
    case 'inbox:updated':
      return [{ kind: 'invalidate', keys: [keys.attention(), keys.activity()] }];
    case 'task:updated': {
      const event = taskEvent.safeParse(payload);
      if (!event.success) return [{ kind: 'none' }];
      return [
        { kind: 'invalidate', keys: [keys.attention(), keys.tasks()] },
        {
          kind: 'task',
          taskId: event.data.task_id,
          version: event.data.version,
          eventId: event.data.event_id
        }
      ];
    }
    case 'workspace:updated': {
      const event = workspaceEvent.safeParse(payload);
      if (!event.success) return [{ kind: 'none' }];
      return [
        {
          kind: 'upsertWorkspace',
          workspaceId: event.data.workspace.workspace_id,
          workspace: event.data.workspace
        }
      ];
    }
    case 'session:message': {
      const event = sessionMessageEvent.safeParse(payload);
      if (!event.success) return [{ kind: 'none' }];
      const id = event.data.session_id;
      return [
        {
          kind: 'invalidate',
          keys: [
            keys.sessionConversation(id),
            keys.sessionNative(id),
            keys.mainThread(),
            keys.conversation()
          ]
        }
      ];
    }
    default:
      return [{ kind: 'none' }];
  }
}

/** Version of a cached task view, whatever the contract names the wrapper. */
function heldTaskVersion(queryClient: QueryClient, taskId: string): number | undefined {
  const held = queryClient.getQueryData<{ task?: { version?: unknown }; version?: unknown }>(
    keys.task(taskId)
  );
  const version = held?.task?.version ?? held?.version;
  return typeof version === 'number' ? version : undefined;
}

/** Remembers recent event ids so a redelivered event does not cause a second read. */
export function createEventGate(limit = 500) {
  const seen = new Set<string>();
  return (eventId: string): boolean => {
    if (seen.has(eventId)) return false;
    seen.add(eventId);
    if (seen.size > limit) seen.delete(seen.values().next().value as string);
    return true;
  };
}

export function createSseApplier(queryClient: QueryClient) {
  const accept = createEventGate();
  let connects = 0;

  return function apply(message: Pick<SseMessage, 'event' | 'data'>): void {
    const firstConnect = message.event === 'connected' && connects++ === 0;
    for (const action of planSseEvent(message, { firstConnect })) {
      switch (action.kind) {
        case 'invalidate':
          for (const key of action.keys) void queryClient.invalidateQueries({ queryKey: key });
          break;
        case 'task': {
          if (!accept(action.eventId)) break;
          const held = heldTaskVersion(queryClient, action.taskId);
          if (held === undefined || action.version > held) {
            void queryClient.invalidateQueries({ queryKey: keys.task(action.taskId) });
          }
          break;
        }
        case 'upsertWorkspace':
          queryClient.setQueryData(keys.workspace(action.workspaceId), action.workspace);
          queryClient.setQueryData(keys.workspaces(), (list: unknown) =>
            Array.isArray(list)
              ? upsertById(list, 'workspace_id', action.workspaceId, action.workspace)
              : list
          );
          break;
        case 'none':
          break;
      }
    }
  };
}

function upsertById(list: unknown[], field: string, id: string, value: unknown): unknown[] {
  const index = list.findIndex((item) => (item as Record<string, unknown>)[field] === id);
  if (index === -1) return [value, ...list];
  return list.map((item, i) => (i === index ? value : item));
}
