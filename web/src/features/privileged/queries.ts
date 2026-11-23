/** The owner's audit of privileged actions, through the generated client. Read-only. */
import { $api } from '@/api';

export function usePrivilegedActions() {
  return $api.useQuery('get', '/privileged-actions');
}

export function usePrivilegedAction(id: string) {
  return $api.useQuery('get', '/privileged-actions/{action_id}', {
    params: { path: { action_id: id } }
  });
}
