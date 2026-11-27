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

/** The owner's memory: open entries, totals and the review window (docs/specs/owner-memory.md). */
export function useOwnerMemory() {
  return $api.useQuery('get', '/owner/memory');
}

/**
 * The owner's memory and what reads it: a decision or a new review window changes the memory, the
 * Needs-you item that counts what waits, and (for a move) the project's memory.
 */
function useOwnerMemoryInvalidation() {
  const queryClient = useQueryClient();
  const key = $api.queryOptions('get', '/owner/memory').queryKey;
  return (projectId?: string) =>
    Promise.all([
      queryClient.invalidateQueries({ queryKey: key }),
      queryClient.invalidateQueries({ queryKey: keys.attention() }),
      projectId
        ? queryClient.invalidateQueries({
            queryKey: $api.queryOptions('get', '/projects/{project_id}/memory', {
              params: { path: { project_id: projectId } }
            }).queryKey
          })
        : undefined
    ]);
}

/**
 * Decides one entry. Settled either way, the memory is read again, so a refusal (an entry decided
 * elsewhere, memory that would pass its bound, a full project memory) shows what is current.
 */
export function useDecideOwnerMemory() {
  const invalidate = useOwnerMemoryInvalidation();
  return $api.useMutation('post', '/owner/memory/{entry_id}/decision', {
    onSettled: (_data, _error, variables) => invalidate(variables.body.project_id)
  });
}

/** Saves the review window. Every entry's due date moves with it, so the memory is read again. */
export function useSetOwnerMemorySettings() {
  const invalidate = useOwnerMemoryInvalidation();
  return $api.useMutation('put', '/owner/memory/settings', {
    onSettled: () => invalidate()
  });
}
