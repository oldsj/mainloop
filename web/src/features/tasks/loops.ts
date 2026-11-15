/**
 * Data hooks and labels for a task's loops (docs/specs/loops.md): the durable wake-ups main and
 * supervisors arm. The owner reads them here and pauses, resumes, fires or deletes one.
 */
import { useQuery } from '@tanstack/react-query';
import { fetchClient, keys } from '@/api';
import type { TaskLoop } from '@/lib/contract';

/** How often the loops refetch while the page is open: firings land without an event. */
export const LOOP_REFRESH_MS = 15_000;

export function useTaskLoops(taskId: string) {
  return useQuery({
    queryKey: keys.taskLoops(taskId),
    queryFn: async ({ signal }) => {
      const { data } = await fetchClient.GET('/tasks/{task_id}/loops', {
        params: { path: { task_id: taskId } },
        signal
      });
      if (data === undefined) throw new Error('Mainloop returned no loops.');
      return data.loops;
    },
    refetchInterval: LOOP_REFRESH_MS
  });
}

/** Main's loops, with or without a task, for the main thread's view. */
export function useMainLoops() {
  return useQuery({
    queryKey: keys.mainLoops(),
    queryFn: async ({ signal }) => {
      const { data } = await fetchClient.GET('/main-thread/loops', { signal });
      if (data === undefined) throw new Error('Mainloop returned no loops.');
      return data.loops;
    },
    refetchInterval: LOOP_REFRESH_MS
  });
}

export type LoopControl = 'pause' | 'resume' | 'fire' | 'delete';

/**
 * Sends one control. `requestId` is fire's idempotency key: the caller keeps it until the server
 * answers, so a resend after a lost reply does not fire twice.
 */
export async function sendLoopControl(
  loopId: string,
  control: LoopControl,
  requestId: string
): Promise<TaskLoop> {
  const path = { params: { path: { loop_id: loopId } } };
  const call = async () => {
    switch (control) {
      case 'pause':
        return (await fetchClient.POST('/loops/{loop_id}/pause', path)).data;
      case 'resume':
        return (await fetchClient.POST('/loops/{loop_id}/resume', path)).data;
      case 'fire':
        return (
          await fetchClient.POST('/loops/{loop_id}/fire', {
            ...path,
            body: { request_id: requestId }
          })
        ).data;
      case 'delete':
        return (await fetchClient.DELETE('/loops/{loop_id}', path)).data;
    }
  };
  const data = await call();
  if (data === undefined) throw new Error('Mainloop returned no loop.');
  return data;
}

/** True while the loop can still fire: the owner's controls apply to it. */
export function loopLive(loop: TaskLoop): boolean {
  return loop.state === 'armed' || loop.state === 'paused';
}

const PR_CONDITIONS: Record<TaskLoop['pr_conditions'][number], string> = {
  ci_success: 'CI passes',
  ci_failure: 'CI fails',
  merged: 'merged',
  closed: 'closed'
};

/** What the loop waits for, as one phrase. `pageTaskId` is the page's task; null on main's view. */
export function loopTrigger(loop: TaskLoop, pageTaskId: string | null): string {
  const subject =
    pageTaskId === null ? 'its task' : loop.task_id === pageTaskId ? 'this task' : 'a watched task';
  switch (loop.trigger) {
    case 'task':
      return loop.statuses.length > 0
        ? `When ${subject} is ${loop.statuses.join(' or ')}`
        : `When ${subject} changes status`;
    case 'pull_request':
      return loop.pr_conditions.length > 0
        ? `On the pull request of ${subject}: ${loop.pr_conditions.map((c) => PR_CONDITIONS[c]).join(' or ')}`
        : `When the pull request of ${subject} or its CI changes`;
    case 'quiet':
      return `When ${subject} is quiet for ${minutes(loop.quiet_minutes)}`;
    case 'time':
      return loop.every_minutes != null
        ? `Every ${minutes(loop.every_minutes)}`
        : 'Once, on a timer';
  }
}

function minutes(n: number | null): string {
  if (n == null) return '?';
  if (n % 60 === 0) return n === 60 ? '1 hour' : `${n / 60} hours`;
  return n === 1 ? '1 minute' : `${n} minutes`;
}

const STATES: Record<TaskLoop['state'], string> = {
  armed: 'Armed',
  paused: 'Paused',
  done: 'Done',
  exhausted: 'Exhausted',
  expired: 'Expired',
  stopped: 'Stopped',
  ended: 'Ended'
};

export function loopStateLabel(loop: TaskLoop): string {
  if (loop.stop_reason === 'no_progress') return 'Stopped: no progress';
  return STATES[loop.state];
}

/** Text for a refusal of a control, by its reason. */
export const LOOP_REFUSALS: Record<string, string> = {
  loop_state_conflict: 'This loop changed before the request landed. Its current state is shown.',
  loop_not_found: 'This loop no longer exists.'
};
