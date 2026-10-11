import { delay, http, HttpResponse } from 'msw';
import { API_URL } from '@/api/config';

const url = (path: string) => `${API_URL}${path}`;

/** A stream that sends `connected` and then stays open, like the real /events. */
function eventStream() {
  const encoder = new TextEncoder();
  return new ReadableStream<Uint8Array>({
    start(controller) {
      controller.enqueue(encoder.encode('id: 1\nevent: connected\ndata: {}\n\n'));
    }
  });
}

/**
 * Default responses for the local stack (`VITE_MSW=1`) and the test suite. Feature slices add
 * their own fixtures next to this file and spread them into `handlers`.
 */
export const handlers = [
  http.get(url('/health'), () => HttpResponse.json({ status: 'ok' })),
  http.get(url('/events'), async () => {
    await delay(10);
    return new HttpResponse(eventStream(), {
      headers: { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache' }
    });
  }),
  http.get(url('/attention'), () => HttpResponse.json({ items: [], sessions: [] })),
  http.get(url('/activity'), () => HttpResponse.json([])),
  http.get(url('/projects'), () => HttpResponse.json([])),
  http.get(url('/workspaces'), () => HttpResponse.json([]))
];
