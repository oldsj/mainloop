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
const tasks: Schemas['TaskView'][] = [];
const orchestrationHealth: Schemas['HealthView'] = {
  rows: [],
  stops: [],
  hours: 24,
  partial: false,
  unsupported: [],
  retention: { events_days: 7, summaries_days: 90 }
};
const providers: Schemas['ProviderProfile'][] = [];

/**
 * `GET` and `PUT /session-defaults`, kept in memory. A save answers with every default, and one
 * with both fields null removes its row, as the server does.
 */
export function sessionDefaultsHandlers(initial: Schemas['SessionDefault'][] = []) {
  let rows = [...initial];
  return [
    http.get(url('/session-defaults'), () => HttpResponse.json({ defaults: rows })),
    http.put(url('/session-defaults'), async ({ request }) => {
      const body = (await request.json()) as Schemas['SessionDefault'];
      rows = rows.filter((row) => !(row.role === body.role && row.provider === body.provider));
      if (body.model !== null || body.effort !== null) rows.push(body);
      return HttpResponse.json({ defaults: rows });
    })
  ];
}

/**
 * The owner's seen state, kept in memory as the server keeps it (docs/specs/seen.md): a read
 * writes nothing; a visit moves `last_visit` to `previous_visit`; a mark is stamped now, and a
 * later one wins. `begin` sets the record a case starts from, and `reset` empties it.
 */
export function createSeenBackend() {
  const iso = (ms: number) => new Date(ms).toISOString();
  let since: number | null = null;
  let lastVisit: number | null = null;
  let previousVisit: number | null = null;
  let marks: Record<string, number> = {};
  const marked: string[] = [];
  let visits = 0;

  const body = (): Schemas['Seen'] => ({
    since: iso(since ?? Date.now()),
    last_visit: lastVisit === null ? null : iso(lastVisit),
    previous_visit: previousVisit === null ? null : iso(previousVisit),
    tasks: Object.entries(marks).map(([task_id, at]) => ({ task_id, seen_at: iso(at) }))
  });

  return {
    /** The record a case starts from: when it began, the owner's last visit, and any marks (ms). */
    begin(start: { since?: number; lastVisit?: number | null; tasks?: Record<string, number> }) {
      since = start.since ?? since;
      lastVisit = start.lastVisit ?? lastVisit;
      marks = { ...(start.tasks ?? {}) };
    },
    reset() {
      since = lastVisit = previousVisit = null;
      marks = {};
      marked.length = 0;
      visits = 0;
    },
    /** The task ids marked, in order, one entry per write. */
    get marked(): readonly string[] {
      return marked;
    },
    /** How many visits were recorded. */
    get visits(): number {
      return visits;
    },
    handlers: [
      http.get(url('/seen'), () => HttpResponse.json(body())),
      http.post(url('/seen/visits'), () => {
        const now = Date.now();
        since ??= now;
        previousVisit = lastVisit;
        lastVisit = now;
        visits++;
        return HttpResponse.json(body());
      }),
      http.put(url('/seen/tasks/:taskId'), ({ params }) => {
        const taskId = String(params.taskId);
        const now = Date.now();
        since ??= now;
        marks[taskId] = Math.max(now, marks[taskId] ?? 0);
        marked.push(taskId);
        return HttpResponse.json({ task_id: taskId, seen_at: iso(marks[taskId]) });
      })
    ]
  };
}

/**
 * The seen state the test suite and the local stack share. `setup.ts` resets it after each case,
 * so one test's visit is not the next one's "previous visit".
 */
export const testSeen = createSeenBackend();

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
 * Responses every surface reads at startup, with empty data. Neither the local stack nor the tests
 * add a turn timer here: the local stack adds its own chat backend in `browser.ts`.
 */
export const baseHandlers = [
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
  http.get(url('/tasks'), () => HttpResponse.json(tasks)),
  // The explorer reads schedules on every shell, so an empty page is the default. It issues no
  // page token, so it refuses one, as kagent refuses any token it did not issue.
  http.get(url('/schedules'), ({ request }) =>
    new URL(request.url).searchParams.has('page_token')
      ? HttpResponse.json({ detail: 'Invalid page token' }, { status: 400 })
      : HttpResponse.json({ items: [], next_page_token: null })
  ),
  // A test that only passes through /health incidentally (on its way elsewhere) still needs these.
  http.get(url('/orchestration-health'), () => HttpResponse.json(orchestrationHealth)),
  http.get(url('/providers'), () => HttpResponse.json(providers)),
  ...sessionDefaultsHandlers(),
  ...testSeen.handlers
];

/**
 * The chat backend the test suite shares when a test does not set its own. `setup.ts` resets it
 * after each case, so state from one test cannot reach the next.
 */
export const testChat = createChatBackend({});

/** Default responses for the test suite. Feature slices add their own next to this file. */
export const handlers = [...baseHandlers, ...testChat.handlers];
