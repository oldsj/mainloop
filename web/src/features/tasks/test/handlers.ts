import { http, HttpResponse, type HttpResponseResolver } from 'msw';
import { API_URL } from '@/api/config';
import type { ProviderProfile, TaskActionKind, TaskView } from '@/lib/contract';
import { CLAUDE_PROFILE, CODEX_PROFILE, view } from '@/test/fixtures/tasks';

const url = (path: string) => `${API_URL}${path}`;

export const taskRead = (resolve: HttpResponseResolver) =>
  http.get(url('/tasks/:task_id'), resolve);

export const tasksList = (resolve: HttpResponseResolver) => http.get(url('/tasks'), resolve);

export const taskAction = (kind: TaskActionKind, resolve: HttpResponseResolver) =>
  http.post(url(`/tasks/:task_id/${kind}`), resolve);

export const providersRead = (list: ProviderProfile[] = [CLAUDE_PROFILE, CODEX_PROFILE]) =>
  http.get(url('/providers'), () => HttpResponse.json(list));

/** A non-2xx task response with the backend's `TaskError` body: a stable `reason` code. */
export function taskProblem(status: number, reason: string): HttpResponseResolver {
  return () => HttpResponse.json({ detail: { reason } }, { status });
}

/** A non-2xx response with a plain string `detail`, as a failure before the task layer answers. */
export function problem(status: number, detail: string): HttpResponseResolver {
  return () => HttpResponse.json({ detail }, { status });
}

/** What a task page's reads see. Tests mutate it to move the server between reads. */
export interface TaskScreen {
  view: TaskView;
  list: TaskView[];
  providers: ProviderProfile[];
  /** Owner reads of this task so far, including failed ones. */
  reads: number;
}

/** A running task the server allows every action on. */
export function allowedView(): TaskView {
  const available = { available: true, reason: null };
  return view({}, { actions: { retry: available, reassign: available, cancel: available } });
}

export function taskScreen(view: TaskView, list: TaskView[] = []): TaskScreen {
  return { view, list, providers: [CLAUDE_PROFILE, CODEX_PROFILE], reads: 0 };
}

/** The three reads a task page makes, served from one screen. Put a test's overrides first. */
export function serveTaskPage(screen: TaskScreen) {
  return [
    taskRead(() => {
      screen.reads += 1;
      return HttpResponse.json(screen.view);
    }),
    tasksList(() => HttpResponse.json(screen.list)),
    providersRead(screen.providers)
  ];
}
