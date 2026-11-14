/**
 * Data hooks for Settings. Reads and writes go through the generated client. A save answers with
 * every default, so it replaces the cached list and no second read is needed.
 */
import { useQueryClient } from '@tanstack/react-query';
import { $api } from '@/api';

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
