import type { SessionStatus } from './contract';

/** Short labels for session statuses, shared by the list and the session page. */
export const statusLabels: Record<SessionStatus, string> = {
  pending: 'PENDING',
  active: 'ACTIVE',
  idle: 'IDLE',
  superseded: 'SUPERSEDED',
  waiting_on_user: 'NEEDS INPUT',
  completed: 'DONE',
  failed: 'FAILED',
  cancelled: 'CANCELLED'
};

export function isFinished(session: { status: string }): boolean {
  return ['completed', 'failed', 'cancelled', 'superseded'].includes(session.status);
}

export function isCurrent(session: { status: string; needs_owner?: boolean }): boolean {
  return (
    !isFinished(session) &&
    (session.needs_owner === true || ['pending', 'active'].includes(session.status))
  );
}

/** Unknown statuses degrade to their upper-case wire name rather than failing the page. */
export function statusLabel(status: string, needsOwner = false): string {
  if (needsOwner && !isFinished({ status }))
    return status === 'waiting_on_user' ? 'NEEDS INPUT' : 'NEEDS YOU';
  return (
    (statusLabels as Record<string, string | undefined>)[status] ??
    status.toUpperCase().replaceAll('_', ' ')
  );
}
