/** A request failed with an HTTP response, so the backend was reachable. */
export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number
  ) {
    super(message);
    this.name = 'ApiError';
  }
}

/**
 * A task read or action the backend refused. The task routes put a typed code in
 * `detail.reason` (for example `stale_task_version`); `reason` is null for other bodies.
 */
export class TaskApiError extends ApiError {
  constructor(
    message: string,
    status: number,
    readonly reason: string | null
  ) {
    super(message, status);
    this.name = 'TaskApiError';
  }
}

/** A message send the backend did not accept. `status` is 0 when no HTTP response arrived. */
export class SendError extends Error {
  constructor(
    message: string,
    readonly status: number
  ) {
    super(message);
    this.name = 'SendError';
  }
}

/** The explanation in an error body, or the fallback when the body has none. */
export function errorDetail(body: unknown, fallback: string): string {
  if (typeof body === 'string' && body.trim()) return body.trim().slice(0, 300);
  if (body && typeof body === 'object' && 'detail' in body) {
    const detail = (body as { detail: unknown }).detail;
    if (typeof detail === 'string') return detail;
    if (Array.isArray(detail)) {
      // Request validation answers with a list of {msg}.
      const messages = detail
        .map((item) => (item as { msg?: unknown } | null)?.msg)
        .filter((msg): msg is string => typeof msg === 'string' && msg !== '');
      if (messages.length) return messages.join('; ');
    }
    if (detail && typeof detail === 'object' && 'reason' in detail) {
      const reason = (detail as { reason: unknown }).reason;
      if (typeof reason === 'string') return reason;
    }
  }
  return fallback;
}

/** The typed `detail.reason` code of a task route error, or null. */
export function taskReason(body: unknown): string | null {
  if (body && typeof body === 'object' && 'detail' in body) {
    const detail = (body as { detail: unknown }).detail;
    if (typeof detail === 'string') return detail;
    if (detail && typeof detail === 'object' && 'reason' in detail) {
      const reason = (detail as { reason: unknown }).reason;
      if (typeof reason === 'string') return reason;
    }
  }
  return null;
}
