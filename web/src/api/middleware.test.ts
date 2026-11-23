import createFetchClient from 'openapi-fetch';
import { describe, expect, it, vi } from 'vitest';
import { ApiError, SendError, TaskApiError } from './errors';
import { createMiddleware, createReadLimiter, type LimiterClock } from './middleware';
import type { paths } from './schema.gen';

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });

function setup(
  respond: (request: Request) => Response | Promise<Response>,
  readTimeoutMs?: number,
  clientHeaderOnReads?: boolean
) {
  const reportFailure = vi.fn();
  const unauthorized = vi.fn();
  const fetch = vi.fn((input: Request) => Promise.resolve(respond(input)));
  const client = createFetchClient<paths>({ baseUrl: 'http://api.test', fetch });
  client.use(
    createMiddleware({
      reportFailure,
      unauthorized,
      clientHeaderOnReads,
      acquireRead: () => Promise.resolve(),
      readTimeoutMs
    })
  );
  return { client, reportFailure, unauthorized, fetch };
}

describe('sign-in', () => {
  it('sends the client header on writes, and on reads only for a same-origin API', async () => {
    const seen: Array<[string, string | null]> = [];
    const respond = (request: Request) => {
      seen.push([request.method, request.headers.get('X-Mainloop-Client')]);
      return json(200, {});
    };
    const crossOrigin = setup(respond);
    await crossOrigin.client.GET('/projects');
    await crossOrigin.client.POST('/chat', { body: { message: 'hi' } });
    const sameOrigin = setup(respond, undefined, true);
    await sameOrigin.client.GET('/projects');
    expect(seen).toEqual([
      ['GET', null],
      ['POST', 'web'],
      ['GET', 'web']
    ]);
  });

  it('reports a 401 as a lapsed sign-in and still fails the request', async () => {
    const { client, unauthorized } = setup(() => json(401, { detail: 'Authentication required' }));
    const error = await client.GET('/projects').catch((e: unknown) => e);
    expect(unauthorized).toHaveBeenCalledTimes(1);
    expect(error).toMatchObject({ status: 401 });
  });

  it('does not treat a 403 as signed out', async () => {
    const { client, unauthorized } = setup(() => json(403, { detail: 'Not permitted' }));
    await client.GET('/projects').catch(() => undefined);
    expect(unauthorized).not.toHaveBeenCalled();
  });
});

describe('error mapping', () => {
  it('turns an HTTP failure into ApiError with the detail text', async () => {
    const { client, reportFailure } = setup(() => json(404, { detail: 'No such project' }));
    const error = await client.GET('/projects').catch((e: unknown) => e);
    expect(error).toBeInstanceOf(ApiError);
    expect(error).toMatchObject({ status: 404, message: 'No such project' });
    // The backend answered, so it is reachable.
    expect(reportFailure).not.toHaveBeenCalled();
  });

  it('uses TaskApiError with the typed reason on task routes', async () => {
    const { client } = setup(() => json(409, { detail: { reason: 'stale_task_version' } }));
    const error = await client
      .GET('/tasks/{task_id}', { params: { path: { task_id: 't1' } } })
      .catch((e: unknown) => e);
    expect(error).toBeInstanceOf(TaskApiError);
    expect(error).toMatchObject({ status: 409, reason: 'stale_task_version' });
  });

  it('uses TaskApiError on loop routes', async () => {
    const { client } = setup(() => json(409, { detail: { reason: 'loop_state_conflict' } }));
    const error = await client
      .POST('/loops/{loop_id}/pause', { params: { path: { loop_id: 'l1' } } })
      .catch((e: unknown) => e);
    expect(error).toBeInstanceOf(TaskApiError);
    expect(error).toMatchObject({ status: 409, reason: 'loop_state_conflict' });
  });

  it('uses TaskApiError on a project’s supervisor and worker-limit routes, which the task service answers', async () => {
    // Given a provider change refused at the hourly restart limit, with the numbers in the detail
    const { client } = setup(() =>
      json(409, { detail: { reason: 'handoff_limit', handoffs: 3, limit: 3, window: '1h' } })
    );
    const error = await client
      .PUT('/projects/{project_id}/supervisor/settings', {
        params: { path: { project_id: 'p1' } },
        body: { provider_profile_id: 'codex-default', model: null, effort: null }
      })
      .catch((e: unknown) => e);
    expect(error).toBeInstanceOf(TaskApiError);
    expect(error).toMatchObject({ status: 409, reason: 'handoff_limit', detail: { limit: 3 } });

    const limit = await client
      .GET('/projects/{project_id}/worker-limit', { params: { path: { project_id: 'p1' } } })
      .catch((e: unknown) => e);
    expect(limit).toBeInstanceOf(TaskApiError);
    // Another project route keeps the plain error.
    const detail = await client
      .GET('/projects/{project_id}/detail', { params: { path: { project_id: 'p1' } } })
      .catch((e: unknown) => e);
    expect(detail).not.toBeInstanceOf(TaskApiError);
  });

  it('uses SendError when a message send is refused', async () => {
    const { client } = setup(() => json(503, { detail: 'Session is not running' }));
    const error = await client
      .POST('/sessions/{session_id}/message', {
        params: { path: { session_id: 's1' } },
        body: { content: 'hi' } as never
      })
      .catch((e: unknown) => e);
    expect(error).toBeInstanceOf(SendError);
    expect(error).toMatchObject({ status: 503, message: 'Session is not running' });
  });

  it('falls back to a status message when the body is not JSON', async () => {
    const { client } = setup(() => new Response('<html>bad gateway</html>', { status: 502 }));
    const error = await client.GET('/projects').catch((e: unknown) => e);
    expect(error).toMatchObject({ status: 502, message: 'Request failed (502)' });
  });

  it('passes successful responses through untouched', async () => {
    const { client } = setup(() => json(200, []));
    const { data, error } = await client.GET('/projects');
    expect(error).toBeUndefined();
    expect(data).toEqual([]);
  });
});

describe('connection reporting', () => {
  it('reports a request that got no HTTP response', async () => {
    const { client, reportFailure } = setup(() => {
      throw new TypeError('Failed to fetch');
    });
    await client.GET('/projects').catch(() => undefined);
    expect(reportFailure).toHaveBeenCalledOnce();
  });

  it('turns a failed send into SendError with status 0', async () => {
    const { client } = setup(() => {
      throw new TypeError('Failed to fetch');
    });
    const error = await client
      .POST('/sessions/{session_id}/message', {
        params: { path: { session_id: 's1' } },
        body: { content: 'hi' } as never
      })
      .catch((e: unknown) => e);
    expect(error).toBeInstanceOf(SendError);
    expect(error).toMatchObject({ status: 0 });
  });
});

describe('read timeout', () => {
  it('aborts a read that takes longer than the timeout without reporting the backend offline', async () => {
    const { client, reportFailure } = setup(
      (request) =>
        new Promise<Response>((_, reject) => {
          request.signal.addEventListener('abort', () => reject(request.signal.reason));
        }),
      20
    );
    const error = await client.GET('/projects').catch((e: unknown) => e);
    expect(error).toMatchObject({ name: 'TimeoutError' });
    // A slow read is not proof of an outage; the /health probe decides.
    expect(reportFailure).not.toHaveBeenCalled();
  });
});

describe('task errors', () => {
  it('keeps the whole TaskError detail', async () => {
    const detail = {
      reason: 'parent_capacity',
      limit: 2,
      held_tasks: [],
      recovery: 'Finish or cancel a held task.'
    };
    const { client } = setup(() => json(409, { detail }));
    const error = await client
      .GET('/tasks/{task_id}', { params: { path: { task_id: 't1' } } })
      .catch((e: unknown) => e);
    expect(error).toMatchObject({ reason: 'parent_capacity', detail });
  });

  it('has a null reason when the detail is a plain string', async () => {
    const { client } = setup(() => json(404, { detail: 'Not Found' }));
    const error = await client
      .GET('/tasks/{task_id}', { params: { path: { task_id: 't1' } } })
      .catch((e: unknown) => e);
    expect(error).toBeInstanceOf(TaskApiError);
    expect(error).toMatchObject({ status: 404, reason: null, detail: null, message: 'Not Found' });
  });
});

function fakeClock() {
  let now = 1_000_000;
  const sleeps: number[] = [];
  const clock: LimiterClock = {
    now: () => now,
    sleep: (ms) => {
      sleeps.push(ms);
      now += ms;
      return Promise.resolve();
    }
  };
  return { clock, sleeps, advance: (ms: number) => (now += ms) };
}

describe('read limiter', () => {
  it('lets a burst of four start at once, then spaces reads 250 ms apart', async () => {
    const { clock, sleeps } = fakeClock();
    const acquire = createReadLimiter(4, 4, clock);
    for (let i = 0; i < 4; i++) await acquire();
    expect(sleeps).toEqual([]);
    for (let i = 0; i < 3; i++) await acquire();
    expect(sleeps).toEqual([250, 250, 250]);
  });

  it('refills the bucket after a quiet period', async () => {
    const { clock, sleeps, advance } = fakeClock();
    const acquire = createReadLimiter(4, 4, clock);
    for (let i = 0; i < 4; i++) await acquire();
    advance(5_000);
    for (let i = 0; i < 4; i++) await acquire();
    expect(sleeps).toEqual([]);
  });

  it('frees the slot of a waiter that is aborted', async () => {
    const now = 0;
    const sleeps: number[] = [];
    const clock: LimiterClock = {
      now: () => now,
      sleep: (ms, signal) => {
        sleeps.push(ms);
        // Never resolves on its own: only an abort ends the wait.
        return new Promise<void>((_, reject) => {
          signal?.addEventListener('abort', () => reject(signal.reason));
        });
      }
    };
    const acquire = createReadLimiter(4, 4, clock);
    for (let i = 0; i < 4; i++) await acquire();
    const controller = new AbortController();
    const waiting = acquire(controller.signal);
    controller.abort();
    await expect(waiting).rejects.toBeDefined();
    // Without the refund this read would queue behind the aborted one (500 ms).
    void acquire();
    expect(sleeps).toEqual([250, 250]);
  });

  it('does not start a read whose signal is already aborted', async () => {
    const { clock } = fakeClock();
    const acquire = createReadLimiter(4, 4, clock);
    const controller = new AbortController();
    controller.abort();
    await expect(acquire(controller.signal)).rejects.toBeDefined();
    for (let i = 0; i < 4; i++) await acquire();
  });
});
