import { http, HttpResponse, type HttpResponseResolver } from 'msw';
import { API_URL } from '@/api/config';
import type { components } from '@/api/schema.gen';
import { workspaceFixture } from '@/test/fixtures';

type Schemas = components['schemas'];
export type WorkspaceFixture = Schemas['WorkspaceLifecycle'];
type Action = 'suspend' | 'resume' | 'refresh';

const url = (path: string) => `${API_URL}${path}`;

export const workspaceRead = (resolve: HttpResponseResolver) =>
  http.get(url('/workspaces/:workspace_id'), resolve);

export const workspaceAction = (action: Action, resolve: HttpResponseResolver) =>
  http.post(url(`/workspaces/:workspace_id/${action}`), resolve);

export const workspaceDelete = (resolve: HttpResponseResolver) =>
  http.delete(url('/workspaces/:workspace_id'), resolve);

export const portsRead = (resolve: HttpResponseResolver) =>
  http.get(url('/workspaces/:workspace_id/ports'), resolve);

/** `/tasks` rows are passed as given, so a test can send a row the page must skip. */
export const tasksRead = (list: unknown[]) =>
  http.get(url('/tasks'), () => HttpResponse.json(list));

/** A non-2xx response with the backend's `{ detail }` body, as the client reads it. */
export function problem(status: number, detail: string): HttpResponseResolver {
  return () => HttpResponse.json({ detail }, { status });
}

/**
 * Handlers for one workspace's page: its state, its ports and no tasks. Put a test's own overrides
 * before these in `server.use`, since the first matching handler wins.
 */
export function serveWorkspace(
  item: WorkspaceFixture = workspaceFixture(),
  ports: Schemas['WorkspacePortsResponse'] = { ports: [] }
) {
  return [
    workspaceRead(() => HttpResponse.json(item)),
    portsRead(() => HttpResponse.json(ports)),
    tasksRead([])
  ];
}
