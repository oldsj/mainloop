import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  RouterProvider,
  useParams
} from '@tanstack/react-router';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it, vi } from 'vitest';
import { API_URL } from '@/api/config';
import { server } from '@/test/msw/server';
import { PrivilegedActionPage, PrivilegedAuditPage } from './AuditPage';
import type { PrivilegedActionDetail, PrivilegedActionSummary } from './model';

// The page's reads would otherwise queue behind the app's read limiter. See fastReads.ts.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('@/features/sessions/test/fastReads');
  return withFastReads(await importOriginal());
});

const summary = (over: Partial<PrivilegedActionSummary>): PrivilegedActionSummary => ({
  id: 'pa_1',
  type: 'onepassword.item_to_secret',
  state: 'succeeded',
  title: 'Copy 1Password "widgets-db › password" to Secret widgets/widgets-db',
  requester_role: 'project_supervisor',
  requester_session_id: 'sess-1',
  requester_task_id: null,
  reason: 'The widgets app needs its database password.',
  created_at: '2026-11-20T10:00:00Z',
  expires_at: '2026-11-21T10:00:00Z',
  decided_at: '2026-11-20T10:05:00Z',
  finished_at: '2026-11-20T10:05:03Z',
  decision_note: null,
  approval_channel: null,
  last_error: null,
  ...over
});

const DENIED = summary({
  id: 'pa_2',
  type: 'github.pr_merge',
  state: 'denied',
  title: 'Merge example/widgets#7',
  decision_note: 'Wait for the review.'
});

function Action() {
  const { id } = useParams({ strict: false });
  return <PrivilegedActionPage id={id ?? ''} />;
}

function renderAt(path: string) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const root = createRootRoute({ component: Outlet });
  const router = createRouter({
    routeTree: root.addChildren([
      createRoute({ getParentRoute: () => root, path: '/', component: () => <p>Chat</p> }),
      createRoute({
        getParentRoute: () => root,
        path: '/privileged',
        component: PrivilegedAuditPage
      }),
      createRoute({ getParentRoute: () => root, path: '/privileged/$id', component: Action })
    ]),
    history: createMemoryHistory({ initialEntries: [path] })
  });
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  );
  return router;
}

describe('privileged actions audit', () => {
  it('lists each action with its state, and opens one with its card and steps', async () => {
    const detail: PrivilegedActionDetail = {
      action: summary({ approval_channel: 'phone-signed' }),
      body: 'Write key password of Secret widgets/widgets-db.',
      args: { namespace: 'widgets' },
      frozen_facts: {},
      digest: 'a'.repeat(64),
      canonical_record: null,
      result: {},
      attempts: 1,
      delivery_state: 'delivered',
      events: [
        { id: 1, step: 'requested', detail: 'asked', created_at: '2026-11-20T10:00:00Z' },
        { id: 2, step: 'executed', detail: 'wrote the key', created_at: '2026-11-20T10:05:03Z' }
      ],
      approval: { device_id: 'dev_0123456789abcdefghijklmnop', signed_at: '2026-11-20T10:05:00Z' }
    };
    server.use(
      http.get(`${API_URL}/privileged-actions/approval-mode`, () =>
        HttpResponse.json({ mode: 'signed' })
      ),
      http.get(`${API_URL}/privileged-actions`, () =>
        HttpResponse.json([summary({ approval_channel: 'phone-signed' }), DENIED])
      ),
      http.get(`${API_URL}/privileged-actions/pa_1`, () => HttpResponse.json(detail))
    );
    renderAt('/privileged');

    const list = await screen.findByRole('list', { name: 'Privileged actions' });
    const rows = within(list).getAllByRole('link');
    expect(rows).toHaveLength(2);
    expect(within(rows[0]!).getByText('Done')).toBeInTheDocument();
    expect(within(rows[1]!).getByText('Denied')).toBeInTheDocument();
    expect(within(rows[1]!).getByText('Wait for the review.')).toBeInTheDocument();
    expect(within(rows[0]!).getByText(/On a phone, signed with Touch ID/)).toBeInTheDocument();
    expect(screen.queryByRole('note')).not.toBeInTheDocument();

    await userEvent.click(rows[0]!);
    expect(await screen.findByText('wrote the key')).toBeInTheDocument();
    expect(screen.getByText(/Write key password of Secret/)).toBeInTheDocument();
    expect(screen.getByText('The widgets app needs its database password.')).toBeInTheDocument();
    expect(
      screen.getByText(/^with Touch ID on dev_0123456789abcdefghijklmnop, /)
    ).toBeInTheDocument();
    expect(screen.getByText('On a phone, signed with Touch ID')).toBeInTheDocument();
  });

  it('warns while approvals come from the web app, and shows how each was approved', async () => {
    server.use(
      http.get(`${API_URL}/privileged-actions/approval-mode`, () =>
        HttpResponse.json({ mode: 'web' })
      ),
      http.get(`${API_URL}/privileged-actions`, () =>
        HttpResponse.json([summary({ approval_channel: 'web' })])
      )
    );
    renderAt('/privileged');

    expect(await screen.findByRole('note')).toHaveTextContent('Approvals run unsigned.');
    const list = await screen.findByRole('list', { name: 'Privileged actions' });
    expect(within(list).getByText(/From the web app \(no Touch ID\)/)).toBeInTheDocument();
  });

  it('shows no warning with no executor', async () => {
    server.use(
      http.get(`${API_URL}/privileged-actions/approval-mode`, () =>
        HttpResponse.json({ mode: null })
      ),
      http.get(`${API_URL}/privileged-actions`, () => HttpResponse.json([DENIED]))
    );
    renderAt('/privileged');
    await screen.findByRole('list', { name: 'Privileged actions' });
    expect(screen.queryByRole('note')).not.toBeInTheDocument();
  });

  it('says so when no agent has asked', async () => {
    server.use(http.get(`${API_URL}/privileged-actions`, () => HttpResponse.json([])));
    renderAt('/privileged');
    expect(
      await screen.findByText('No agent has asked for a privileged action.')
    ).toBeInTheDocument();
  });
});
