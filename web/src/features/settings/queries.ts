/**
 * Data hooks for Settings. Reads and writes go through the generated client. A save answers with
 * every default, so it replaces the cached list and no second read is needed. A revoke answers with
 * nothing, so the device list is read again.
 */
import { useQueryClient } from '@tanstack/react-query';
import { $api, keys } from '@/api';

export function useSessionDefaults() {
  return $api.useQuery('get', '/session-defaults');
}

export function useProviders() {
  return $api.useQuery('get', '/providers');
}

export function useSetSessionDefault() {
  const queryClient = useQueryClient();
  const key = $api.queryOptions('get', '/session-defaults').queryKey;
  return $api.useMutation('put', '/session-defaults', {
    onSuccess: (data) => queryClient.setQueryData(key, data)
  });
}

/** The owner's registered phones: newest first, revoked ones included (docs/specs/devices.md). */
export function useDevices() {
  return $api.useQuery('get', '/devices');
}

/** Revokes a device. Settled either way, the list is read again, so it shows what the server holds. */
export function useRevokeDevice() {
  const queryClient = useQueryClient();
  return $api.useMutation('delete', '/devices/{device_id}', {
    onSettled: () => queryClient.invalidateQueries({ queryKey: keys.devices() })
  });
}
