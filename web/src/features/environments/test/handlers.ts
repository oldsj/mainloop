import { http, HttpResponse, type HttpResponseResolver } from 'msw';
import { API_URL } from '@/api/config';
import {
  environment,
  version,
  type EnvironmentFixture,
  type SelectionFixture,
  type VersionFixture
} from './fixtures';

const url = (path: string) => `${API_URL}${path}`;

export const environmentsRead = (list: EnvironmentFixture[]) =>
  http.get(url('/environments'), () => HttpResponse.json(list));

/** A list read with a resolver of its own: a failure, a held reply, or a count of reads. */
export const environmentsReadWith = (resolve: HttpResponseResolver) =>
  http.get(url('/environments'), resolve);

/** Versions per environment id. An environment with none gets an empty list. */
export const versionsRead = (byEnvironment: Record<string, VersionFixture[]>) =>
  http.get(url('/environments/:environment_id/versions'), ({ params }) =>
    HttpResponse.json(byEnvironment[String(params.environment_id)] ?? [])
  );

/** The saved selection. A resolver returns 200 with the body, or 204 for "none saved", which has no body. */
export const selectionRead = (resolve: HttpResponseResolver) =>
  http.get(url('/projects/:project_id/environment'), resolve);

export const selectionWrite = (resolve: HttpResponseResolver) =>
  http.put(url('/projects/:project_id/environment'), resolve);

export const acceptDefaultWrite = (resolve: HttpResponseResolver) =>
  http.put(url('/environments/:environment_id/default'), resolve);

export const registerWrite = (resolve: HttpResponseResolver) =>
  http.post(url('/environments'), resolve);

/** A non-2xx response with the backend's `{ detail }` body, as the client reads it. */
export function problem(status: number, detail: string): HttpResponseResolver {
  return () => HttpResponse.json({ detail }, { status });
}

/** A fixed selection read: the saved value, or 204 when none is saved. */
export function savedSelection(value: SelectionFixture | null) {
  return selectionRead(() =>
    value === null ? new HttpResponse(null, { status: 204 }) : HttpResponse.json(value)
  );
}

/** Handlers for the environment section: one environment with an eligible default, and no saved selection. */
export function serveEnvironments(
  options: {
    environments?: EnvironmentFixture[];
    versions?: Record<string, VersionFixture[]>;
    selection?: SelectionFixture | null;
  } = {}
) {
  return [
    environmentsRead(options.environments ?? [environment()]),
    versionsRead(options.versions ?? { env: [version()] }),
    savedSelection(options.selection ?? null)
  ];
}
