/**
 * Data hooks for the task UI. Every read and write goes through the generated client, and cache
 * keys come from `keys`, so SSE invalidation reaches these queries without a second map.
 */
import { useQuery, type QueryClient } from '@tanstack/react-query';
import { $api, fetchClient, keys } from '@/api';
import type { TaskOperation } from '@/lib/contract';
import type { ActionIntent } from '@/lib/taskActions';
import { mergeTaskCache, mergeTaskListCache } from './cache';

/**
 * One task's owner read. The version gate keeps a late, older read from replacing a newer view,
 * including the view an SSE event already pushed.
 */
export function useTask(taskId: string) {
  return $api.useQuery(
    'get',
    '/tasks/{task_id}',
    { params: { path: { task_id: taskId } } },
    { structuralSharing: mergeTaskCache }
  );
}

/**
 * The task list, for the project tree, parent and child context, and the inbox count. Its key
 * sits under `keys.tasks()`, so invalidation reaches it, but it is not the sessions page's
 * `['get', '/tasks']` query: the per-row version gate here must not change what sessions reads.
 */
export function useTaskList(filter: { project_id?: string; parent_task_id?: string } = {}) {
  return useQuery({
    queryKey: [...keys.tasks(), filter],
    queryFn: async () => {
      const { data } = await fetchClient.GET('/tasks', { params: { query: filter } });
      if (data === undefined) throw new Error('Mainloop returned no task list.');
      return data;
    },
    structuralSharing: mergeTaskListCache
  });
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
