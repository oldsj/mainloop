import type { Middleware } from 'openapi-fetch';
import { ApiError, errorDetail, SendError, TaskApiError, taskReason } from './errors';

export const READ_TIMEOUT_MS = 10_000;
export const READS_PER_SECOND = 4;

/** Routes whose failures carry the task error shape. */
const isTaskPath = (path: string) =>
  path.startsWith('/tasks') || path.startsWith('/task-operations');
/** Routes that deliver a chat message; their failures are SendErrors. */
const isSendPath = (path: string) => path === '/chat' || path === '/sessions/{session_id}/message';

const isRead = (method: string) => method === 'GET' || method === 'HEAD';

/** Spaces read starts so no more than `perSecond` begin in any second. */
export function createReadLimiter(
  perSecond = READS_PER_SECOND,
  clock: { now: () => number; sleep: (ms: number) => Promise<void> } = {
    now: () => Date.now(),
    sleep: (ms) => new Promise((resolve) => setTimeout(resolve, ms))
  }
) {
  const spacing = 1000 / perSecond;
  let nextStart = 0;
  return async function acquire(signal?: AbortSignal): Promise<void> {
    const start = Math.max(clock.now(), nextStart);
    nextStart = start + spacing;
    const wait = start - clock.now();
    if (wait <= 0) return;
    await clock.sleep(wait);
    signal?.throwIfAborted();
  };
}

export interface MiddlewareDeps {
  reportFailure: () => void;
  acquireRead?: (signal?: AbortSignal) => Promise<void>;
  readTimeoutMs?: number;
}

/**
 * Cross-cutting API behaviour:
 * - reads wait for a limiter slot and time out after 10 s;
 * - a request that never gets an HTTP response is reported to the connection state;
 * - HTTP errors become ApiError, TaskApiError (task routes) or SendError (message sends), so
 *   TanStack Query and callers see one typed error instead of a raw body.
 */
export function createMiddleware(deps: MiddlewareDeps): Middleware {
  const acquireRead = deps.acquireRead ?? createReadLimiter();
  const timeoutMs = deps.readTimeoutMs ?? READ_TIMEOUT_MS;

  return {
    async onRequest({ request }) {
      if (!isRead(request.method)) return undefined;
      await acquireRead(request.signal);
      const signal = AbortSignal.any([request.signal, AbortSignal.timeout(timeoutMs)]);
      return new Request(request, { signal });
    },

    onError({ error, schemaPath }) {
      // Aborts are the caller's own doing; everything else means no HTTP response arrived.
      if (error instanceof DOMException && error.name === 'AbortError') return undefined;
      deps.reportFailure();
      if (isSendPath(schemaPath)) return new SendError("Can't reach the Mainloop backend.", 0);
      return undefined;
    },

    async onResponse({ response, schemaPath }) {
      if (response.ok) return undefined;
      let body: unknown;
      try {
        body = await response.clone().json();
      } catch {
        // Not JSON (a proxy's error page); the fallback message applies.
      }
      const message = errorDetail(body, `Request failed (${response.status})`);
      if (isSendPath(schemaPath)) throw new SendError(message, response.status);
      if (isTaskPath(schemaPath))
        throw new TaskApiError(message, response.status, taskReason(body));
      throw new ApiError(message, response.status);
    }
  };
}
