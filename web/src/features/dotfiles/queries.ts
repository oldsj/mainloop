/**
 * Data hooks for the dotfiles source. Reads and writes go through the generated client; a save
 * answers with the stored source, so it replaces the cached read.
 */
import { useQueryClient } from '@tanstack/react-query';
import { $api } from '@/api';

export function useOwnerDotfiles() {
  return $api.useQuery('get', '/dotfiles');
}

export function useSetOwnerDotfiles() {
  const queryClient = useQueryClient();
  const key = $api.queryOptions('get', '/dotfiles').queryKey;
  return $api.useMutation('put', '/dotfiles', {
    onSuccess: (data) => queryClient.setQueryData(key, data)
  });
}

export function useProjectDotfiles(projectId: string) {
  return $api.useQuery('get', '/projects/{project_id}/dotfiles', {
    params: { path: { project_id: projectId } }
  });
}

export function useSetProjectDotfiles(projectId: string) {
  const queryClient = useQueryClient();
  const key = $api.queryOptions('get', '/projects/{project_id}/dotfiles', {
    params: { path: { project_id: projectId } }
  }).queryKey;
  return $api.useMutation('put', '/projects/{project_id}/dotfiles', {
    onSuccess: (data) => queryClient.setQueryData(key, data)
  });
}
