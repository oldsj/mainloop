import { http, HttpResponse, type HttpResponseResolver } from 'msw';
import { API_URL } from '@/api/config';

const url = (path: string) => `${API_URL}${path}`;

export const attentionRead = (resolve: HttpResponseResolver) =>
  http.get(url('/attention'), resolve);

export const activityRead = (resolve: HttpResponseResolver) => http.get(url('/activity'), resolve);

export const respondPost = (resolve: HttpResponseResolver) =>
  http.post(url('/queue/:item_id/respond'), resolve);

export const dismissPost = (resolve: HttpResponseResolver) =>
  http.post(url('/queue/:item_id/dismiss'), resolve);

/** A non-2xx response with the backend's `{ detail }` body, as the client reads it. */
export function problem(status: number, detail: string): HttpResponseResolver {
  return () => HttpResponse.json({ detail }, { status });
}

/** The task list the HITL card reads for its owning task. Empty: these tests do not need a task. */
export const tasksRead = http.get(url('/tasks'), () => HttpResponse.json([]));
