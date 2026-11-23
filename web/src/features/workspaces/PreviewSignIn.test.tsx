import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it, vi } from 'vitest';
import { API_URL } from '@/api/config';
import { server } from '@/test/msw/server';
import { LOOP_LIMIT, LOOP_WINDOW_MS, openable, PreviewSignIn, tooManyOpens } from './PreviewSignIn';
import { problem } from './test/handlers';

const GRANT = `${API_URL}/workspaces/:workspace_id/previews/:port/grant`;
const SIGN_IN =
  'https://3000--ws1--preview.preview.example.test/__mainloop/preview-auth#t=abc&next=%2Fa';

function memoryStore() {
  const m = new Map<string, string>();
  return {
    getItem: (k: string) => m.get(k) ?? null,
    setItem: (k: string, v: string) => void m.set(k, v)
  };
}

function renderSignIn(props: Partial<Parameters<typeof PreviewSignIn>[0]> = {}) {
  const queryClient = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  const open = vi.fn();
  render(
    <QueryClientProvider client={queryClient}>
      <PreviewSignIn
        workspaceId="ws1"
        port={3000}
        path="/a"
        open={open}
        store={memoryStore()}
        {...props}
      />
    </QueryClientProvider>
  );
  return { open };
}

describe('PreviewSignIn', () => {
  it('asks for a grant for the host and path, then opens the URL it returns', async () => {
    let seen: { params: Record<string, unknown>; body: unknown; client: string | null } | null =
      null;
    server.use(
      http.post(GRANT, async ({ params, request }) => {
        seen = {
          params,
          body: await request.json(),
          client: request.headers.get('X-Mainloop-Client')
        };
        return HttpResponse.json({ url: SIGN_IN, expires_at: '2026-01-01T00:00:00Z' });
      })
    );
    const { open } = renderSignIn();
    await waitFor(() => expect(open).toHaveBeenCalledWith(SIGN_IN));
    expect(open).toHaveBeenCalledTimes(1);
    expect(seen).toEqual({
      params: { workspace_id: 'ws1', port: '3000' },
      body: { path: '/a' },
      client: 'web'
    });
  });

  it('says why when the workspace has no preview, and retries by hand', async () => {
    server.use(http.post(GRANT, problem(404, 'Workspace not found')));
    const { open } = renderSignIn();
    expect(await screen.findByText(/has no running preview/)).toBeInTheDocument();
    server.use(
      http.post(GRANT, () =>
        HttpResponse.json({ url: SIGN_IN, expires_at: '2026-01-01T00:00:00Z' })
      )
    );
    await userEvent.click(screen.getByRole('button', { name: 'Try again' }));
    await waitFor(() => expect(open).toHaveBeenCalledWith(SIGN_IN));
  });

  it('shows an undeclared port as the server words it', async () => {
    server.use(
      http.post(GRANT, () =>
        HttpResponse.json(
          { detail: [{ loc: [], msg: 'Preview port is not declared', type: 'value_error' }] },
          { status: 422 }
        )
      )
    );
    renderSignIn();
    expect(await screen.findByRole('alert')).toBeInTheDocument();
  });

  it('never opens a URL that is not http(s)', async () => {
    server.use(
      http.post(GRANT, () =>
        HttpResponse.json({ url: 'javascript:alert(1)', expires_at: '2026-01-01T00:00:00Z' })
      )
    );
    const { open } = renderSignIn();
    expect(await screen.findByText(/cannot open/)).toBeInTheDocument();
    expect(open).not.toHaveBeenCalled();
  });

  it('refuses an invalid port without asking the API', async () => {
    const grant = vi.fn(() => HttpResponse.json({}));
    server.use(http.post(GRANT, grant));
    renderSignIn({ port: Number('x') });
    expect(await screen.findByText(/no valid port/)).toBeInTheDocument();
    expect(grant).not.toHaveBeenCalled();
  });

  it('stops a sign-in loop instead of bouncing forever', async () => {
    const grant = vi.fn(() =>
      HttpResponse.json({ url: SIGN_IN, expires_at: '2026-01-01T00:00:00Z' })
    );
    server.use(http.post(GRANT, grant));
    const store = memoryStore();
    for (let i = 0; i < LOOP_LIMIT; i++) tooManyOpens('ws1:3000', Date.now(), store);
    const { open } = renderSignIn({ store });
    expect(await screen.findByText(/allows cookies/)).toBeInTheDocument();
    expect(grant).not.toHaveBeenCalled();
    expect(open).not.toHaveBeenCalled();
  });
});

describe('tooManyOpens', () => {
  it('counts opens of one preview within the window', () => {
    const store = memoryStore();
    const t0 = 1_000_000;
    for (let i = 0; i < LOOP_LIMIT; i++) expect(tooManyOpens('a:1', t0 + i, store)).toBe(false);
    expect(tooManyOpens('b:1', t0, store)).toBe(false);
    expect(tooManyOpens('a:1', t0 + 10, store)).toBe(true);
    expect(tooManyOpens('a:1', t0 + LOOP_WINDOW_MS + 100, store)).toBe(false);
  });

  it('is off without storage, and survives junk in it', () => {
    expect(tooManyOpens('a:1', 0, null)).toBe(false);
    const store = memoryStore();
    store.setItem('mainloop.preview-signin', 'not json');
    expect(tooManyOpens('a:1', 0, store)).toBe(false);
  });
});

describe('openable', () => {
  it('takes only http and https URLs', () => {
    expect(openable(SIGN_IN)).toBe(true);
    expect(openable('http://localhost:8001/')).toBe(true);
    for (const bad of ['javascript:alert(1)', 'data:text/html,x', '/relative', '', 42, null]) {
      expect(openable(bad)).toBe(false);
    }
  });
});
