/**
 * Which tasks the owner has looked at. A finished task the owner has not opened since it last
 * changed shows a filled teal dot; once opened it is hollow, like any row at rest.
 *
 * The record is the owner's, kept on the server (`GET /seen`, `PUT /seen/tasks/{task_id}`; see
 * docs/specs/seen.md), so a task opened on the phone is seen on the desktop too. Both times in the
 * comparison are the server's, so a device's clock does not matter. Until the record has been read,
 * or if it cannot be, nothing shows as unseen.
 */
import { useQueryClient, type QueryClient } from '@tanstack/react-query';
import { useCallback, useMemo } from 'react';
import { $api } from '@/api';
import type { components } from '@/api';

type Seen = components['schemas']['Seen'];

export interface SeenState {
  /** When the owner's record started, in ms. A task that last changed before it counts as seen. */
  since: number;
  /** Task id to the last time the owner had it open, in ms. */
  seen: Record<string, number>;
}

/** The record as the explorer reads it, or null when there is none to go by. */
export function seenState(seen: Seen | undefined | null): SeenState | null {
  if (!seen) return null;
  const since = Date.parse(seen.since);
  if (!Number.isFinite(since)) return null;
  const marks: Record<string, number> = {};
  for (const task of seen.tasks) {
    const at = Date.parse(task.seen_at);
    if (Number.isFinite(at)) marks[task.task_id] = at;
  }
  return { since, seen: marks };
}

/**
 * Whether a task has not been looked at: it last changed after the owner's record started, and
 * after the owner last had it open. With no record nothing is unseen.
 */
export function isUnseen(
  state: SeenState | null | undefined,
  taskId: string,
  updatedAt: string
): boolean {
  if (!state) return false;
  const changed = Date.parse(updatedAt);
  if (!Number.isFinite(changed)) return false;
  return changed > state.since && changed > (state.seen[taskId] ?? 0);
}

/** Whether a mark would change anything: the task has no mark, or changed after its mark. */
function needsMark(seen: Seen, taskId: string, changedAt: string | undefined): boolean {
  const mark = seen.tasks.find((task) => task.task_id === taskId)?.seen_at;
  if (!mark) return true;
  const changed = changedAt ? Date.parse(changedAt) : Number.NaN;
  return Number.isFinite(changed) && changed > Date.parse(mark);
}

// Marks being written, per query client, so two trees on one page send one write between them.
const writing = new WeakMap<QueryClient, Set<string>>();

/**
 * The owner's record and a way to mark a task as looked at. `changedAt` is the task's own last
 * change: a task already marked after it is left alone, so opening a page does not write each time.
 * A mark that fails (offline) is simply not recorded; the next change or visit tries again.
 */
export function useSeen(): {
  seen: SeenState | null;
  mark: (taskId: string, changedAt?: string) => void;
} {
  const queryClient = useQueryClient();
  const key = $api.queryOptions('get', '/seen').queryKey;
  // The record changes only by this owner's own marks, which update the cache themselves, so it is
  // not re-read on a timer. A reconnect re-reads it with everything else.
  // No init object: with one the key would differ from `key` above, and the lookup in `mark` and
  // the record the visit hands over would both miss this read.
  const read = $api.useQuery('get', '/seen', undefined, { retry: false, staleTime: Infinity });
  const write = $api.useMutation('put', '/seen/tasks/{task_id}');
  const { mutate } = write;
  const seen = useMemo(() => seenState(read.data), [read.data]);

  const mark = useCallback(
    (taskId: string, changedAt?: string) => {
      const current = queryClient.getQueryData<Seen>(key);
      // Before the record is read there is nothing to compare with; the caller marks again once it is.
      if (!current || !needsMark(current, taskId, changedAt)) return;
      let pending = writing.get(queryClient);
      if (!pending) writing.set(queryClient, (pending = new Set()));
      if (pending.has(taskId)) return;
      pending.add(taskId);
      const done = pending;
      mutate(
        { params: { path: { task_id: taskId } } },
        {
          onSuccess: (row) =>
            queryClient.setQueryData<Seen>(key, (prev) =>
              prev
                ? { ...prev, tasks: [...prev.tasks.filter((t) => t.task_id !== row.task_id), row] }
                : prev
            ),
          onSettled: () => done.delete(taskId)
        }
      );
    },
    // `key` is rebuilt each render with the same contents; the client and `mutate` are stable.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [queryClient, mutate]
  );
  return { seen, mark };
}
