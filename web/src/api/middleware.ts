import type { Middleware } from 'openapi-fetch';
import {
  ApiError,
  errorDetail,
  REFUSAL_CODES,
  type RefusalCode,
  SendError,
  TaskApiError,
  taskErrorDetail
} from './errors';

export const READ_TIMEOUT_MS = 10_000;
export const READS_PER_SECOND = 4;
export const READ_BURST = 4;

/** Routes whose failures carry the task error shape. */
const isTaskPath = (path: string) =>
  path.startsWith('/tasks') || path.startsWith('/task-operations');
/** Routes that deliver a chat message; their failures are SendErrors. */
const isSendPath = (path: string) => path === '/chat' || path === '/sessions/{session_id}/message';

const isRead = (method: string) => method === 'GET' || method === 'HEAD';

/** The contract's refusal code in an error body, or null. A value the contract does not name is dropped. */
const refusalCodeOf = (body: unknown): RefusalCode | null => {
  if (typeof body !== 'object' || body === null || !('code' in body)) return null;
  return REFUSAL_CODES.find((code) => code === body.code) ?? null;
};

// By name, not instanceof: the error can come from another realm (an iframe, jsdom).
const isNamed = (error: unknown, name: string) =>
  typeof error === 'object' && error !== null && 'name' in error && error.name === name;

export interface LimiterClock {
  /** Monotonic milliseconds: a wall-clock step must not stall reads. */
  now: () => number;
  sleep: (ms: number, signal?: AbortSignal) => Promise<void>;
}

const realClock: LimiterClock = {
  now: () => performance.now(),
  sleep: (ms, signal) =>
    new Promise<void>((resolve, reject) => {
      const onAbort = () => {
        clearTimeout(timer);
        reject(signal?.reason);
      };
      const timer = setTimeout(() => {
        signal?.removeEventListener('abort', onAbort);
        resolve();
      }, ms);
      signal?.addEventListener('abort', onAbort, { once: true });
    })
};

/**
 * A token bucket for read starts: `burst` reads may start at once, then `perSecond` per second.
 * A read that has to wait reserves its slot; if it is aborted while waiting the slot is freed.
 */
export function createReadLimiter(
  perSecond = READS_PER_SECOND,
  burst = READ_BURST,
  clock: LimiterClock = realClock
) {
  let tokens = burst;
  let last = clock.now();

  const refill = () => {
    const now = clock.now();
    tokens = Math.min(burst, tokens + (Math.max(0, now - last) * perSecond) / 1000);
    last = now;
  };

  return async function acquire(signal?: AbortSignal): Promise<void> {
    signal?.throwIfAborted();
    refill();
    tokens -= 1;
    if (tokens >= 0) return;
    try {
      await clock.sleep((-tokens / perSecond) * 1000, signal);
    } catch (error) {
      refill();
      tokens = Math.min(burst, tokens + 1);
      throw error;
    }
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
 * - a request that never gets an HTTP response is reported to the connection state (a timeout is
 *   not: a slow read says nothing about reachability, so the /health probe decides);
 * - HTTP errors become ApiError, TaskApiError (task routes) or SendError (message sends), so
 *   TanStack Query and callers see one typed error instead of a raw body. A contract refusal
 *   `code` rides on the error.
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
      // Aborts and timeouts are not evidence that the backend is down.
      if (isNamed(error, 'AbortError') || isNamed(error, 'TimeoutError')) return undefined;
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
      const code = refusalCodeOf(body);
      if (isSendPath(schemaPath)) throw new SendError(message, response.status, code);
      if (isTaskPath(schemaPath)) {
        const detail = taskErrorDetail(body);
        throw new TaskApiError(message, response.status, detail?.reason ?? null, detail);
      }
      throw new ApiError(message, response.status, code);
    }
  };
}
