import { delay, http, HttpResponse } from 'msw';
import { API_URL } from '@/api/config';
import type { components } from '@/api/schema.gen';
import { createChatBackend } from '@/features/chat/handlers';

type Schemas = components['schemas'];

const url = (path: string) => `${API_URL}${path}`;

// Fixtures are typed against the generated schema, so a contract change that breaks one fails tsc
// instead of letting the tests pass against a shape the backend no longer sends.
const health: Schemas['HealthResponse'] = { status: 'healthy', database: 'ok' };
const connected: Schemas['EventConnected'] = {
  user_id: 'dev-user',
  timestamp: '2026-01-01T00:00:00.000000'
};
const attention: Schemas['AttentionSnapshot'] = { items: [], count: 0, sessions: [] };
const activity: Schemas['ActivityItem'][] = [];
const projects: Schemas['Project'][] = [];
const workspaces: Schemas['WorkspaceLifecycle'][] = [];

/** A stream that sends `connected` and then stays open, like the real /events. */
function eventStream() {
  const encoder = new TextEncoder();
  return new ReadableStream<Uint8Array>({
    start(controller) {
      controller.enqueue(
        encoder.encode(`id: 1\nevent: connected\ndata: ${JSON.stringify(connected)}\n\n`)
      );
    }
  });
}

/**
 * Default responses for the local stack (`VITE_MSW=1`) and the test suite. Feature slices add
 * their own fixtures next to this file and spread them into `handlers`.
 */
export const handlers = [
  http.get(url('/health'), () => HttpResponse.json(health)),
  http.get(url('/events'), async () => {
    await delay(10);
    return new HttpResponse(eventStream(), {
      headers: { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache' }
    });
  }),
  http.get(url('/attention'), () => HttpResponse.json(attention)),
  http.get(url('/activity'), () => HttpResponse.json(activity)),
  http.get(url('/projects'), () => HttpResponse.json(projects)),
  http.get(url('/workspaces'), () => HttpResponse.json(workspaces)),
  ...createChatBackend().handlers
];
