/**
 * Data hooks for the task UI. Every read and write goes through the generated client, and cache
 * keys come from `keys`, so SSE invalidation reaches these queries without a second map.
 */
import { useEffect } from 'react';
import { useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query';
import { $api, fetchClient, keys } from '@/api';
import { useEventStreamOpen } from '@/api/eventStream';
import type { TaskFollowUp, TaskFollowUpResult, TaskOperation } from '@/lib/contract';
import type { ActionIntent } from '@/lib/taskActions';
import { visiblePolling } from '@/lib/visiblePolling';
import {
  isTaskView,
  mergeTaskCache,
  mergeTaskListCache,
  readableTaskRows,
  unreadableTaskCount
} from './cache';

/**
 * A 200 whose body is not the payload this page reads. The held data stays on screen and the query
 * reports an error.
 */
export class UnreadablePayloadError extends Error {}

/**
 * One task's owner read. The version gate keeps a late, older read from replacing a newer view,
 * including the view an SSE event already pushed. A malformed body fails the read instead of
 * replacing the held view, so the page can say the refresh failed and keep showing the last one.
 * `enabled` lets a page read a task only once it knows the id, such as a parent.
 */
export function useTask(taskId: string, enabled = true) {
  return useQuery({
    queryKey: keys.task(taskId),
    queryFn: async ({ signal }) => {
      const { data } = await fetchClient.GET('/tasks/{task_id}', {
        params: { path: { task_id: taskId } },
        signal
      });
      if (!isTaskView(data)) {
        throw new UnreadablePayloadError('Mainloop sent a task this page cannot read.');
      }
      return data;
    },
    structuralSharing: mergeTaskCache,
    enabled
  });
}

export type TaskListFilter = { project_id?: string; parent_task_id?: string };

/**
 * The task list's cache entry. Both readers below share it, so the list is fetched once. Its key
 * sits under `keys.tasks()`, so invalidation reaches it, but it is not the sessions page's
 * `['get', '/tasks']` query: the per-row version gate here must not change what sessions reads.
 */
function taskListQuery(filter: TaskListFilter) {
  return {
    queryKey: [...keys.tasks(), filter],
    queryFn: async ({ signal }: { signal: AbortSignal }): Promise<unknown[]> => {
      const { data } = await fetchClient.GET('/tasks', { params: { query: filter }, signal });
      if (!Array.isArray(data)) {
        throw new UnreadablePayloadError('Mainloop sent a task list this page cannot read.');
      }
      return data;
    },
    structuralSharing: mergeTaskListCache
  };
}

/** The task list, for the project tree, the owner count and the badge. Only readable rows. */
export function useTaskList(filter: TaskListFilter = {}) {
  return useQuery({ ...taskListQuery(filter), select: readableTaskRows });
}

/**
 * How often the owner task list is re-read while the event stream is open. Every task change
 * arrives as an event that re-reads the list, so the poll is only a safety net then, and the list
 * is the largest thing the app downloads.
 */
export const TASK_POLL_STREAMING_MS = 30_000;

/**
 * Visible-only fallback for the compact count. All mounts share one poll and the list cache. It
 * reads every few seconds while the event stream is down, and every `TASK_POLL_STREAMING_MS`
 * while it is open. When the stream drops, the faster poll starts with a read at once.
 */
export function useOwnerTaskPoll(enabled: boolean): void {
  const queryClient = useQueryClient();
  const streaming = useEventStreamOpen();
  useEffect(() => {
    if (!enabled) return;
    return visiblePolling().watch(
      'tasks:owner',
      async (signal) => {
        if (signal.aborted) return;
        const query = taskListQuery({});
        const cancel = () =>
          void queryClient.cancelQueries(
            { queryKey: query.queryKey, exact: true },
            { revert: true }
          );
        signal.addEventListener('abort', cancel, { once: true });
        try {
          await queryClient.fetchQuery({ ...query, staleTime: 0, retry: false });
        } catch {
          // The query holds the error; a later visible poll can recover.
        } finally {
          signal.removeEventListener('abort', cancel);
        }
      },
      // The stream opening changes only the pace: the list was read moments ago.
      streaming ? { everyMs: TASK_POLL_STREAMING_MS, afterFirstInterval: true } : undefined
    );
  }, [enabled, queryClient, streaming]);
}

/** How many rows of the same list this page cannot read, so the list can say so. */
export function useUnreadableTaskCount(filter: TaskListFilter = {}) {
  return useQuery({ ...taskListQuery(filter), select: unreadableTaskCount });
}

export function useProviders() {
  return $api.useQuery('get', '/providers');
}

function operationOf(data: TaskOperation | undefined): TaskOperation {
  if (data === undefined) throw new Error('Mainloop returned no operation.');
  return data;
}

/**
 * Sends one owner action exactly as built. The tracker decides when to call this, so this only
 * maps an intent onto the matching route. A missing body throws, which the tracker treats as
 * uncertain: the server may have recorded the request.
 */
export async function sendTaskAction(intent: ActionIntent): Promise<TaskOperation> {
  const path = { params: { path: { task_id: intent.taskId } } };
  switch (intent.kind) {
    case 'retry': {
      const { data } = await fetchClient.POST('/tasks/{task_id}/retry', {
        ...path,
        body: intent.body
      });
      return operationOf(data);
    }
    case 'cancel': {
      const { data } = await fetchClient.POST('/tasks/{task_id}/cancel', {
        ...path,
        body: intent.body
      });
      return operationOf(data);
    }
    case 'reassign': {
      const { data } = await fetchClient.POST('/tasks/{task_id}/reassign', {
        ...path,
        body: intent.body
      });
      return operationOf(data);
    }
  }
}

/**
 * Sends a follow-up to the task's own session. The caller keeps the request ID until the server
 * answers, so a resend after a lost reply finds the first send instead of sending twice.
 */
export async function sendFollowUp(
  taskId: string,
  body: TaskFollowUp
): Promise<TaskFollowUpResult> {
  const { data } = await fetchClient.POST('/tasks/{task_id}/follow-up', {
    params: { path: { task_id: taskId } },
    body
  });
  if (data === undefined) throw new Error('Mainloop returned no result.');
  return data;
}

/**
 * Re-reads the task after an action settles, or after a refusal that means the owner's view is
 * out of date. The list and the inbox count are refreshed too, since an action moves a task
 * between them. Each refetch settles on its own; a failed one leaves the cached view in place.
 */
export async function rereadTask(queryClient: QueryClient, taskId: string): Promise<void> {
  await Promise.all([
    queryClient.invalidateQueries({ queryKey: keys.task(taskId) }),
    queryClient.invalidateQueries({ queryKey: keys.tasks() }),
    queryClient.invalidateQueries({ queryKey: keys.attention() })
  ]);
}
