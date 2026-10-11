import type { SessionStatus } from '@/lib/contract';
import type { Tone } from '@/lib/taskState';

/**
 * Tone for each session status. It is typed against the generated status set, so a status the
 * schema adds or removes (C1c dropped `implementing` and `under_review`) fails tsc here.
 */
export const SESSION_STATUS_TONE: Record<SessionStatus, Tone> = {
  pending: 'active',
  active: 'active',
  idle: 'muted',
  superseded: 'muted',
  waiting_on_user: 'attention',
  completed: 'ok',
  failed: 'bad',
  cancelled: 'muted'
};

/** Unknown statuses are muted rather than failing the page, as statusLabel does. */
export function sessionStatusTone(status: string): Tone {
  return (SESSION_STATUS_TONE as Record<string, Tone | undefined>)[status] ?? 'muted';
}
