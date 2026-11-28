import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  RouterProvider
} from '@tanstack/react-router';
import { render } from '@testing-library/react';
import { http, HttpResponse, type HttpResponseResolver } from 'msw';
import { API_URL } from '@/api/config';
import type { components } from '@/api/schema.gen';
import { OwnerMemorySection } from '../OwnerMemorySection';

type Schemas = components['schemas'];
export type OwnerEntryFixture = Schemas['OwnerMemoryEntry'];

const url = (path: string) => `${API_URL}${path}`;
const DAY_MS = 24 * 60 * 60 * 1000;

/** A timestamp `days` before now. */
export const daysAgo = (days: number) => new Date(Date.now() - days * DAY_MS).toISOString();

export function ownerEntry(overrides: Partial<OwnerEntryFixture> = {}): OwnerEntryFixture {
  const confirmed = daysAgo(3);
  return {
    id: 'om-1',
    text: 'The owner reviews UI on a phone.',
    state: 'active',
    author: 'main',
    source: {
      session_id: 'main-1',
      task_id: null,
      attempt_id: null,
      delivery_id: 'd-1',
      owner_entry_id: null
    },
    replaces_id: null,
    created_at: confirmed,
    last_confirmed_at: confirmed,
    decided_at: confirmed,
    due_at: new Date(Date.parse(confirmed) + 14 * DAY_MS).toISOString(),
    lapsed_at: null,
    in_dotfiles_commit: null,
    moved_to_project_id: null,
    moved_to_entry_id: null,
    ...overrides
  };
}

export interface OwnerMemoryBackend {
  /** Every decision body, keyed `<entry id>: <decision>`. */
  decisions: { entryId: string; body: Schemas['OwnerMemoryDecision'] }[];
  settings: number[];
  handlers: ReturnType<typeof http.get>[];
}

/**
 * `GET /owner/memory`, its decisions and `PUT /owner/memory/settings`, kept in memory as the server
 * keeps them: due dates follow the review window, a decision moves the entry's state, and only open
 * entries are listed. `refuse` answers a decision instead when it returns a resolver.
 */
export function ownerMemoryBackend(
  initial: { entries?: OwnerEntryFixture[]; review_after_days?: number } = {},
  refuse?: (body: Schemas['OwnerMemoryDecision']) => HttpResponseResolver | undefined
): OwnerMemoryBackend {
  let entries = [...(initial.entries ?? [])];
  let days = initial.review_after_days ?? 14;
  const decisions: OwnerMemoryBackend['decisions'] = [];
  const settings: number[] = [];
  const due = (entry: OwnerEntryFixture) =>
    entry.state === 'active' && entry.last_confirmed_at
      ? new Date(Date.parse(entry.last_confirmed_at) + days * DAY_MS).toISOString()
      : null;
  const view = (): Schemas['OwnerMemory'] => {
    const open = entries
      .filter((entry) => ['active', 'lapsed', 'proposed'].includes(entry.state))
      .map((entry) => ({ ...entry, due_at: due(entry) }));
    return {
      entries: open,
      used_bytes: open
        .filter((entry) => entry.state === 'active')
        .reduce((sum, entry) => sum + new TextEncoder().encode(entry.text).length, 0),
      max_bytes: 3072,
      max_entry_bytes: 500,
      review_after_days: days,
      grace_days: 7
    };
  };
  const NEXT: Record<Schemas['OwnerMemoryDecision']['decision'], Schemas['OwnerMemoryState']> = {
    accept: 'active',
    reject: 'rejected',
    keep: 'active',
    forget: 'forgotten',
    moved: 'moved',
    move_to_project: 'moved'
  };
  const handlers = [
    http.get(url('/owner/memory'), () => HttpResponse.json(view())),
    http.put(url('/owner/memory/settings'), async ({ request }) => {
      const body = (await request.json()) as Schemas['OwnerMemorySettings'];
      settings.push(body.review_after_days);
      if (body.review_after_days < 1 || body.review_after_days > 365)
        return HttpResponse.json({ detail: 'review_after_days must be 1 to 365' }, { status: 422 });
      days = body.review_after_days;
      return HttpResponse.json({ review_after_days: days });
    }),
    http.post(url('/owner/memory/:entry_id/decision'), async (info) => {
      const body = (await info.request.json()) as Schemas['OwnerMemoryDecision'];
      const entryId = String(info.params.entry_id);
      decisions.push({ entryId, body });
      const refusal = refuse?.(body);
      if (refusal) return refusal(info);
      const now = new Date().toISOString();
      let decided: OwnerEntryFixture | undefined;
      entries = entries.map((entry) => {
        if (entry.id !== entryId) return entry;
        const state = NEXT[body.decision];
        decided = {
          ...entry,
          state,
          decided_at: now,
          last_confirmed_at: state === 'active' ? now : entry.last_confirmed_at,
          lapsed_at: state === 'active' ? null : entry.lapsed_at,
          moved_to_project_id: body.project_id ?? null,
          moved_to_entry_id: body.project_id ? 'mem-moved' : null
        };
        return decided;
      });
      if (!decided) return HttpResponse.json({ detail: 'Not found' }, { status: 404 });
      return HttpResponse.json({ ...decided, due_at: due(decided) });
    })
  ];
  return { decisions, settings, handlers };
}

/** Refuses with the server's sentence. */
export const refusal =
  (status: number, detail: string): HttpResponseResolver =>
  () =>
    HttpResponse.json({ detail }, { status });

/** The section at `/settings`, in a small router with the paths its links go to. */
export function renderOwnerMemory(initialPath = '/settings') {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } }
  });
  const root = createRootRoute({ component: Outlet });
  const stub = (label: string) => () => <p>{label}</p>;
  const router = createRouter({
    routeTree: root.addChildren([
      createRoute({ getParentRoute: () => root, path: '/', component: stub('Chat') }),
      createRoute({ getParentRoute: () => root, path: '/settings', component: OwnerMemorySection }),
      createRoute({ getParentRoute: () => root, path: '/tasks/$id', component: stub('Task') }),
      createRoute({ getParentRoute: () => root, path: '/sessions/$id', component: stub('Session') })
    ]),
    history: createMemoryHistory({ initialEntries: [initialPath] })
  });
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  );
  return { router, queryClient };
}
