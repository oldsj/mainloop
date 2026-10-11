import { http, HttpResponse, type HttpResponseResolver } from 'msw';
import { API_URL } from '@/api/config';
import { CLAUDE_PROFILE, CODEX_PROFILE } from '@/test/fixtures/tasks';
import {
  environment,
  version,
  type EnvironmentFixture,
  type VersionFixture
} from '@/features/environments/test/fixtures';
import {
  environmentsRead,
  savedSelection,
  versionsRead
} from '@/features/environments/test/handlers';
import type { SelectionFixture } from '@/features/environments/test/fixtures';
import {
  detail,
  policy,
  project,
  type DetailFixture,
  type PolicyFixture,
  type ProjectFixture,
  type WorkspaceFixture
} from './fixtures';

const url = (path: string) => `${API_URL}${path}`;

export const projectsRead = (list: ProjectFixture[]) =>
  http.get(url('/projects'), () => HttpResponse.json(list));

export const projectsReadWith = (resolve: HttpResponseResolver) =>
  http.get(url('/projects'), resolve);

export const detailRead = (resolve: HttpResponseResolver) =>
  http.get(url('/projects/:project_id/detail'), resolve);

export const policyRead = (resolve: HttpResponseResolver) =>
  http.get(url('/projects/:project_id/merge-policy'), resolve);

export const policyWrite = (resolve: HttpResponseResolver) =>
  http.put(url('/projects/:project_id/merge-policy'), resolve);

export const workspacesRead = (list: WorkspaceFixture[]) =>
  http.get(url('/workspaces'), () => HttpResponse.json(list));

export const workspacesReadWith = (resolve: HttpResponseResolver) =>
  http.get(url('/workspaces'), resolve);

export const createWorkspaceWrite = (resolve: HttpResponseResolver) =>
  http.post(url('/workspaces'), resolve);

export const providersRead = () =>
  http.get(url('/providers'), () => HttpResponse.json([CLAUDE_PROFILE, CODEX_PROFILE]));

export const providersReadWith = (resolve: HttpResponseResolver) =>
  http.get(url('/providers'), resolve);

/** A non-2xx response with the backend's `{ detail }` body, as the client reads it. */
export function problem(status: number, message: string): HttpResponseResolver {
  return () => HttpResponse.json({ detail: message }, { status });
}

/** Every read the project page and the projects list make, with a valid default for each. */
export function serveProjectPage(
  options: {
    project?: ProjectFixture;
    detail?: DetailFixture;
    policy?: PolicyFixture;
    workspaces?: WorkspaceFixture[];
    environments?: EnvironmentFixture[];
    versions?: Record<string, VersionFixture[]>;
    selection?: SelectionFixture | null;
  } = {}
) {
  const current = options.project ?? project();
  const value = options.detail ?? detail({ project: current });
  return [
    detailRead(() => HttpResponse.json(value)),
    policyRead(() => HttpResponse.json(options.policy ?? policy())),
    workspacesRead(options.workspaces ?? []),
    providersRead(),
    environmentsRead(options.environments ?? [environment()]),
    versionsRead(options.versions ?? { env: [version()] }),
    savedSelection(options.selection ?? null)
  ];
}
