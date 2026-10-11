import { contractEnum } from '../lib/contractEnum';
import type { components } from './schema.gen';

type TaskErrorDetail = components['schemas']['TaskErrorDetail'];

/** The stable `code` a refused main-thread switch or message carries (`CodedHTTPError`). */
export type RefusalCode = NonNullable<components['schemas']['CodedHTTPError']['code']>;

/**
 * Every `RefusalCode`. The list is checked against the generated union, so a code the contract adds
 * or drops fails tsc here instead of reaching the page as an unknown value.
 */
export const REFUSAL_CODES = contractEnum<RefusalCode>()([
  'busy',
  'pending',
  'unconfirmed',
  'unresolved',
  'agent_unavailable'
]);

/**
 * A request failed with an HTTP response, so the backend was reachable. `code` is the contract's
 * refusal code when the body carried one, otherwise null.
 */
export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly code: RefusalCode | null = null
  ) {
    super(message);
    this.name = 'ApiError';
  }
}

/**
 * A task read or action the backend refused. Task routes answer `TaskError` (`detail` is an
 * object with a stable `reason` code) or, for failures before the task layer such as a 404 from
 * the Host guard, `HTTPError` (`detail` is a string). `reason` and `detail` are null for the
 * latter, so a free-text message is never mistaken for a code.
 */
export class TaskApiError extends ApiError {
  constructor(
    message: string,
    status: number,
    readonly reason: string | null,
    /** The whole error detail: `parent_capacity` adds `limit`, `held_tasks` and `recovery`. */
    readonly detail: TaskErrorDetail | null = null
  ) {
    super(message, status);
    this.name = 'TaskApiError';
  }
}

/**
 * A message send the backend did not accept. `status` is 0 when no HTTP response arrived. `code` is
 * the contract's refusal code when the body carried one, otherwise null.
 */
export class SendError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly code: RefusalCode | null = null
  ) {
    super(message);
    this.name = 'SendError';
  }
}

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === 'object' && value !== null;

/** The explanation in an error body, or the fallback when the body has none. */
export function errorDetail(body: unknown, fallback: string): string {
  if (typeof body === 'string' && body.trim()) return body.trim().slice(0, 300);
  if (isRecord(body) && 'detail' in body) {
    const detail = body.detail;
    if (typeof detail === 'string') return detail;
    if (Array.isArray(detail)) {
      // Request validation answers with a list of {msg}.
      const messages = detail
        .map((item) => (isRecord(item) ? item.msg : undefined))
        .filter((msg): msg is string => typeof msg === 'string' && msg !== '');
      if (messages.length) return messages.join('; ');
    }
    if (isRecord(detail) && typeof detail.reason === 'string') return detail.reason;
  }
  return fallback;
}

/** The `TaskError` detail of a task route failure, or null for any other body. */
export function taskErrorDetail(body: unknown): TaskErrorDetail | null {
  if (isRecord(body) && isRecord(body.detail) && typeof body.detail.reason === 'string') {
    return body.detail as TaskErrorDetail;
  }
  return null;
}
