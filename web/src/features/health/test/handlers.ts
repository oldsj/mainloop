import { http, HttpResponse, type HttpResponseResolver } from 'msw';
import { API_URL } from '@/api/config';

export const healthRead = (resolve: HttpResponseResolver) =>
  http.get(`${API_URL}/orchestration-health`, resolve);

export const eventsRead = (resolve: HttpResponseResolver) =>
  http.get(`${API_URL}/orchestration-health/events`, resolve);

export const projectsRead = (list: unknown[]) =>
  http.get(`${API_URL}/projects`, () => HttpResponse.json(list));

/** A failing read. The server's detail is shown as the error text. */
export function problem(status: number, detail: string): HttpResponseResolver {
  return () => HttpResponse.json({ detail }, { status });
}
