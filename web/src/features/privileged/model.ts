import type { components } from '@/api/schema.gen';

export type PrivilegedActionSummary = components['schemas']['PrivilegedActionSummary'];
export type PrivilegedActionDetail = components['schemas']['PrivilegedActionDetail'];
export type PrivilegedApprovalChannel = components['schemas']['PrivilegedApprovalChannel'];
export type PrivilegedActionState = components['schemas']['PrivilegedActionState'];

/** Each state in plain words, and whether it is still open, done, or ended without running. */
export const STATE: Record<
  PrivilegedActionState,
  { label: string; tone: 'open' | 'ok' | 'bad' | 'off' }
> = {
  requested: { label: 'Checking', tone: 'open' },
  pending_approval: { label: 'Waiting for you', tone: 'open' },
  approved: { label: 'Approved', tone: 'open' },
  executing: { label: 'Running', tone: 'open' },
  succeeded: { label: 'Done', tone: 'ok' },
  failed: { label: 'Failed', tone: 'bad' },
  uncertain: { label: 'Outcome unknown', tone: 'bad' },
  stale: { label: 'Stale', tone: 'off' },
  preflight_failed: { label: 'Could not check', tone: 'off' },
  denied: { label: 'Denied', tone: 'off' },
  expired: { label: 'Expired', tone: 'off' },
  cancelled: { label: 'Withdrawn', tone: 'off' }
};

/** The requester as the card names it: the role, and the task when there is one. */
export function requester(a: PrivilegedActionSummary): string {
  return a.requester_task_id
    ? `${a.requester_role} · task ${a.requester_task_id}`
    : a.requester_role;
}

/** How the owner approved, in words: the web app carries no Touch ID signature, a phone's does. */
export const CHANNEL: Record<PrivilegedApprovalChannel, string> = {
  web: 'From the web app (no Touch ID)',
  'phone-signed': 'On a phone, signed with Touch ID'
};
