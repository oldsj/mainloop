import type { QueryClient, QueryKey } from '@tanstack/react-query';
import { allKeys, keys } from './keys';
import type { components } from './schema.gen';
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
type Schemas = components['schemas'];
type TaskUpdated = Schemas['EventTaskUpdated'];
type SessionMessage = Schemas['EventSessionMessage'];
type WorkspaceUpdated = Schemas['EventWorkspaceUpdated'];
type WorkspaceLifecycle = Schemas['WorkspaceLifecycle'];

// Payload types come from the contract's `x-sse-events` schemas. The guards check only the fields
// the cache logic reads, so an event this client does not understand is skipped, not applied.
const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === 'object' && value !== null;

const isTaskUpdated = (value: unknown): value is TaskUpdated =>
  isRecord(value) &&
  typeof value.event_id === 'string' &&
  typeof value.task_id === 'string' &&
  typeof value.version === 'number' &&
  Number.isFinite(value.version);

const isSessionMessage = (value: unknown): value is SessionMessage =>
  isRecord(value) && typeof value.session_id === 'string';

const isWorkspaceUpdated = (value: unknown): value is WorkspaceUpdated =>
  isRecord(value) && isRecord(value.workspace) && typeof value.workspace.workspace_id === 'string';

export type CacheAction =
  | { kind: 'invalidate'; keys: readonly QueryKey[] }
  | { kind: 'task'; taskId: string; version: number; eventId: string }
  | { kind: 'upsertWorkspace'; workspace: WorkspaceLifecycle }
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
      if (!isTaskUpdated(payload)) return [{ kind: 'none' }];
      return [
        { kind: 'invalidate', keys: [keys.attention(), keys.tasks()] },
        {
          kind: 'task',
          taskId: payload.task_id,
          version: payload.version,
          eventId: payload.event_id
        }
      ];
    }
    case 'workspace:updated': {
      if (!isWorkspaceUpdated(payload)) return [{ kind: 'none' }];
      return [{ kind: 'upsertWorkspace', workspace: payload.workspace }];
    }
    case 'session:message': {
      if (!isSessionMessage(payload)) return [{ kind: 'none' }];
      const id = payload.session_id;
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

/** Version of the cached task view, if one is held. A malformed cached view counts as none. */
function heldTaskVersion(queryClient: QueryClient, taskId: string): number | undefined {
  const version = queryClient.getQueryData<Partial<Schemas['TaskView']> | null>(keys.task(taskId))
    ?.task?.version;
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
        case 'upsertWorkspace': {
          const { workspace } = action;
          queryClient.setQueryData(keys.workspace(workspace.workspace_id), workspace);
          // setQueriesData reaches every cached variant of the list (any query params), not one key.
          queryClient.setQueriesData<WorkspaceLifecycle[]>(
            { queryKey: keys.workspaces() },
            (list) => (list ? upsertWorkspace(list, workspace) : list)
          );
          break;
        }
        case 'none':
          break;
      }
    }
  };
}

function upsertWorkspace(list: WorkspaceLifecycle[], workspace: WorkspaceLifecycle) {
  const index = list.findIndex((item) => item.workspace_id === workspace.workspace_id);
  if (index === -1) return [workspace, ...list];
  return list.map((item, i) => (i === index ? workspace : item));
}
