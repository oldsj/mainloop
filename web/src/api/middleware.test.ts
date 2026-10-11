import createFetchClient from 'openapi-fetch';
import { describe, expect, it, vi } from 'vitest';
import { ApiError, SendError, TaskApiError } from './errors';
import { createMiddleware, createReadLimiter } from './middleware';
import type { paths } from './schema.gen';

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });

function setup(
  respond: (request: Request) => Response | Promise<Response>,
  readTimeoutMs?: number
) {
  const reportFailure = vi.fn();
  const fetch = vi.fn((input: Request) => Promise.resolve(respond(input)));
  const client = createFetchClient<paths>({ baseUrl: 'http://api.test', fetch });
  client.use(
    createMiddleware({ reportFailure, acquireRead: () => Promise.resolve(), readTimeoutMs })
  );
  return { client, reportFailure, fetch };
}

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
  it('aborts a read that takes longer than the timeout and reports it', async () => {
    const { client, reportFailure } = setup(
      (request) =>
        new Promise<Response>((_, reject) => {
          request.signal.addEventListener('abort', () => reject(request.signal.reason));
        }),
      20
    );
    await client.GET('/projects').catch(() => undefined);
    expect(reportFailure).toHaveBeenCalledOnce();
  });
});

describe('read limiter', () => {
  it('lets four reads start per second and spaces the rest', async () => {
    let now = 1_000_000;
    const sleeps: number[] = [];
    const acquire = createReadLimiter(4, {
      now: () => now,
      sleep: (ms) => {
        sleeps.push(ms);
        now += ms;
        return Promise.resolve();
      }
    });
    for (let i = 0; i < 5; i++) await acquire();
    // The first read starts at once; each later one waits for its 250 ms slot.
    expect(sleeps).toEqual([250, 250, 250, 250]);
  });

  it('does not delay a read after a quiet period', async () => {
    let now = 0;
    const sleep = vi.fn(() => Promise.resolve());
    const acquire = createReadLimiter(4, { now: () => now, sleep });
    await acquire();
    now += 5_000;
    await acquire();
    expect(sleep).not.toHaveBeenCalled();
  });
});
