/**
 * Data hooks for the scheduled runs UI. Every read and write goes through the generated client.
 * Cache keys match what openapi-react-query builds, so the invalidations here reach the queries
 * below. `api/keys.ts` has no schedule keys yet (see the schedule:updated note in result.md), so
 * they are declared here.
 */
import { useQueryClient } from '@tanstack/react-query';
import { $api } from '@/api';
import { ifMatch, type ScheduleConfigInput } from './model';

export const PAGE_SIZE = 50;

export const scheduleKeys = {
  list: () => ['get', '/schedules'] as const,
  detail: (scheduleId: string) =>
    ['get', '/schedules/{schedule_id}', { params: { path: { schedule_id: scheduleId } } }] as const,
  executions: (scheduleId: string) =>
    [
      'get',
      '/schedules/{schedule_id}/executions',
      { params: { path: { schedule_id: scheduleId } } }
    ] as const
};

export function useProviders() {
  return $api.useQuery('get', '/providers');
}

/** Every schedule this deployment made, one page at a time. */
export function useSchedules() {
  return $api.useInfiniteQuery(
    'get',
    '/schedules',
    { params: { query: { limit: PAGE_SIZE } } },
    {
      initialPageParam: undefined as string | undefined,
      getNextPageParam: (page) => page.next_page_token ?? undefined,
      pageParamName: 'page_token'
    }
  );
}

export function useSchedule(scheduleId: string) {
  return $api.useQuery('get', '/schedules/{schedule_id}', {
    params: { path: { schedule_id: scheduleId } }
  });
}

export function useScheduleExecutions(scheduleId: string) {
  return $api.useInfiniteQuery(
    'get',
    '/schedules/{schedule_id}/executions',
    { params: { path: { schedule_id: scheduleId }, query: { limit: PAGE_SIZE } } },
    {
      initialPageParam: undefined as string | undefined,
      getNextPageParam: (page) => page.next_page_token ?? undefined,
      pageParamName: 'page_token'
    }
  );
}

/** Invalidates the list and any open detail once a write settles, so each re-reads the server. */
function useRefreshSchedules() {
  const queryClient = useQueryClient();
  return (scheduleId?: string) =>
    Promise.all([
      queryClient.invalidateQueries({ queryKey: scheduleKeys.list() }),
      ...(scheduleId
        ? [queryClient.invalidateQueries({ queryKey: scheduleKeys.detail(scheduleId) })]
        : [])
    ]);
}

export function useCreateSchedule() {
  const refresh = useRefreshSchedules();
  return $api.useMutation('post', '/schedules', {
    onSettled: () => refresh()
  });
}

/**
 * Replaces the configuration. The caller passes the etag it last read; a stale one returns 412 and
 * the detail is re-read, so the next save carries the newer etag.
 */
export function useUpdateSchedule(scheduleId: string) {
  const refresh = useRefreshSchedules();
  return $api.useMutation('put', '/schedules/{schedule_id}', {
    onSettled: () => refresh(scheduleId)
  });
}

export function useDeleteSchedule(scheduleId: string) {
  const refresh = useRefreshSchedules();
  return $api.useMutation('delete', '/schedules/{schedule_id}', {
    onSettled: () => refresh(scheduleId)
  });
}

export function useTriggerSchedule(scheduleId: string) {
  const queryClient = useQueryClient();
  return $api.useMutation('post', '/schedules/{schedule_id}/trigger', {
    onSettled: () =>
      queryClient.invalidateQueries({ queryKey: scheduleKeys.executions(scheduleId) })
  });
}

/** The arguments a write needs: the path, the etag it was read at, and the config to send. */
export function updateArgs(scheduleId: string, etag: string, body: ScheduleConfigInput) {
  return {
    params: { path: { schedule_id: scheduleId }, header: { 'If-Match': ifMatch(etag) } },
    body
  };
}
