import { http, HttpResponse, type HttpResponseResolver } from 'msw';
import { API_URL } from '@/api/config';
import type { ProviderProfile, ScheduleExecution, ScheduleView } from '../model';

const url = (path: string) => `${API_URL}${path}`;

/** A non-2xx response with the backend's `{ detail }` body, as the client reads it. */
export function problem(status: number, message: string): HttpResponseResolver {
  return () => HttpResponse.json({ detail: message }, { status });
}

export const providersRead = (list: ProviderProfile[]) =>
  http.get(url('/providers'), () => HttpResponse.json(list));

/** One page of schedules. A page token in the query returns the second page. */
export const schedulesRead = (
  pages: { items: ScheduleView[]; next_page_token?: string | null }[]
) =>
  http.get(url('/schedules'), ({ request }) => {
    const token = new URL(request.url).searchParams.get('page_token');
    const index = token ? Number(token) : 0;
    const page = pages[index] ?? { items: [] };
    return HttpResponse.json({
      items: page.items,
      next_page_token: index + 1 < pages.length ? String(index + 1) : null
    });
  });

export const schedulesReadWith = (resolve: HttpResponseResolver) =>
  http.get(url('/schedules'), resolve);

export const scheduleReadWith = (resolve: HttpResponseResolver) =>
  http.get(url('/schedules/:schedule_id'), resolve);

export const scheduleRead = (view: ScheduleView) =>
  http.get(url('/schedules/:schedule_id'), () => HttpResponse.json(view));

export const scheduleCreate = (resolve: HttpResponseResolver) =>
  http.post(url('/schedules'), resolve);

export const scheduleUpdate = (resolve: HttpResponseResolver) =>
  http.put(url('/schedules/:schedule_id'), resolve);

export const scheduleDelete = (resolve: HttpResponseResolver) =>
  http.delete(url('/schedules/:schedule_id'), resolve);

export const scheduleTrigger = (resolve: HttpResponseResolver) =>
  http.post(url('/schedules/:schedule_id/trigger'), resolve);

export const executionsRead = (items: ScheduleExecution[]) =>
  http.get(url('/schedules/:schedule_id/executions'), () =>
    HttpResponse.json({ items, next_page_token: null })
  );
