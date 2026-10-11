/**
 * Data hooks for the health page. Every read goes through the generated client. Reads do not
 * refetch on window focus: a refresh is the owner's action, and source observations are reset by
 * it, so a focus change must not silently clear them.
 */
import { $api } from '@/api';
import type { HealthFilters } from '@/lib/contract';
import { healthQuery } from './model';

export function useHealth(filters: HealthFilters, enabled = true) {
  return $api.useQuery(
    'get',
    '/orchestration-health',
    { params: { query: healthQuery(filters) } },
    { enabled, refetchOnWindowFocus: false }
  );
}

/** One session's source events. Read only when the owner asks for them. */
export function useHealthEvents(sessionId: string, hours: number, enabled: boolean) {
  return $api.useQuery(
    'get',
    '/orchestration-health/events',
    { params: { query: { session_id: sessionId, hours } } },
    { enabled, refetchOnWindowFocus: false }
  );
}

export function useProjects() {
  return $api.useQuery('get', '/projects');
}
