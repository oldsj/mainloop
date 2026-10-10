/** Short labels for session statuses, shared by the list and the session page. */
export const statusLabels: Record<string, string> = {
  pending: 'PENDING',
  active: 'ACTIVE',
  idle: 'IDLE',
  superseded: 'SUPERSEDED',
  waiting_on_user: 'NEEDS INPUT',
  implementing: 'IMPLEMENTING',
  under_review: 'IN REVIEW',
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
    (session.needs_owner === true ||
      ['pending', 'active', 'implementing', 'under_review'].includes(session.status))
  );
}

export function statusLabel(status: string, needsOwner = false): string {
  if (needsOwner && !isFinished({ status }))
    return status === 'waiting_on_user' ? 'NEEDS INPUT' : 'NEEDS YOU';
  return statusLabels[status] ?? status.toUpperCase().replaceAll('_', ' ');
}
