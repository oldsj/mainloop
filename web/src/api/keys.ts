import type { QueryKey } from '@tanstack/react-query';
import type { paths } from './schema.gen';

/**
 * Query keys. They match the keys openapi-react-query builds (`[method, path, init]`), so
 * `$api.useQuery('get', '/tasks/{task_id}', { params: { path: { task_id } } })` is cached under
 * `keys.task(task_id)`. Invalidating a prefix such as `keys.tasks()` reaches every variant.
 *
 * Use these for invalidation, `setQueryData` and `getQueryData`; never hand-write a key.
 */
type GetPath = {
  [P in keyof paths]: paths[P] extends { get: unknown } ? P : never;
}[keyof paths];

const get = (path: GetPath, params?: Record<string, unknown>): QueryKey =>
  params ? ['get', path, { params }] : ['get', path];

export const keys = {
  health: () => get('/health'),
  attention: () => get('/attention'),
  activity: () => get('/activity'),
  mainThread: () => get('/main-thread'),
  /** One conversation, or all of them when no id is given. */
  conversation: (conversationId?: string) =>
    conversationId
      ? get('/conversations/{conversation_id}', { path: { conversation_id: conversationId } })
      : ['get', '/conversations/{conversation_id}'],

  sessions: () => get('/sessions'),
  session: (sessionId: string) =>
    get('/sessions/{session_id}', { path: { session_id: sessionId } }),
  sessionConversation: (sessionId: string) =>
    get('/sessions/{session_id}/conversation', { path: { session_id: sessionId } }),
  sessionNative: (sessionId: string) =>
    get('/sessions/{session_id}/native', { path: { session_id: sessionId } }),
  sessionHitl: (sessionId: string) =>
    get('/sessions/{session_id}/hitl', { path: { session_id: sessionId } }),

  hitl: (requestId: string) => get('/hitl/{request_id}', { path: { request_id: requestId } }),

  projects: () => get('/projects'),
  projectDetail: (projectId: string) =>
    get('/projects/{project_id}/detail', { path: { project_id: projectId } }),

  workspaces: () => get('/workspaces'),
  workspace: (workspaceId: string) =>
    get('/workspaces/{workspace_id}', { path: { workspace_id: workspaceId } }),
  workspacePorts: (workspaceId: string) =>
    get('/workspaces/{workspace_id}/ports', { path: { workspace_id: workspaceId } }),

  /** Task lists, in every scope. */
  tasks: () => get('/tasks'),
  task: (taskId: string) => get('/tasks/{task_id}', { path: { task_id: taskId } }),
  providers: () => get('/providers'),

  /** Scheduled runs: the deployment-wide list, and one schedule with its executions. */
  schedules: () => get('/schedules'),
  schedule: (scheduleId: string) =>
    get('/schedules/{schedule_id}', { path: { schedule_id: scheduleId } }),
  scheduleExecutions: (scheduleId: string) =>
    get('/schedules/{schedule_id}/executions', { path: { schedule_id: scheduleId } }),

  orchestrationHealth: () => get('/orchestration-health')
} as const;

/** Matches every query. */
export const allKeys: QueryKey = [];
