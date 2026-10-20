import { $api } from '@/api';
import type { ProviderProfile } from './types';

/** The provider profiles /model and /effort list and validate against: the one source of truth
 * for a provider's models and reasoning-effort levels, extended server-side (see
 * server/internal/providers) rather than hardcoded here. */
export function useProviderProfiles() {
  return $api.useQuery('get', '/providers');
}

/** The profile named by a session's native kind (its provider profile id), or undefined before
 * the profiles have loaded or for a kind the registry does not have. */
export function profileForKind(
  profiles: ProviderProfile[] | undefined,
  kind: string | undefined
): ProviderProfile | undefined {
  return profiles?.find((profile) => profile.id === kind);
}
