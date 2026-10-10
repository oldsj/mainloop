import type { HealthFilters } from './api';

/**
 * Task, session and attempt options come from the project/provider/window
 * scope, never from a result already narrowed by one of those choices.
 */
export function choiceScope(filters: HealthFilters) {
  const scope = {
    project_id: filters.project_id ?? '',
    provider: filters.provider ?? '',
    hours: filters.hours ?? 24
  };
  return {
    key: JSON.stringify(scope),
    narrowed: Boolean(filters.task_id || filters.session_id || filters.attempt_id),
    filters: scope
  };
}
