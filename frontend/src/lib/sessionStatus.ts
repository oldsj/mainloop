/** Short labels for session statuses, shared by the list and the session page. */
export const statusLabels: Record<string, string> = {
  pending: 'PENDING',
  active: 'ACTIVE',
  waiting_on_user: 'NEEDS INPUT',
  implementing: 'IMPLEMENTING',
  under_review: 'IN REVIEW',
  completed: 'DONE',
  failed: 'FAILED',
  cancelled: 'CANCELLED'
};

export function statusLabel(status: string): string {
  return statusLabels[status] ?? status.toUpperCase().replaceAll('_', ' ');
}
