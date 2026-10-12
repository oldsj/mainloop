/**
 * Data hooks for the projects UI. Every read and write goes through the generated client, and cache
 * keys come from `keys` or from `$api.queryOptions`, so invalidation reaches every view of a project.
 * Each mutation re-reads what it changed before it settles, so `isPending` covers the re-read.
 */
import { useQueryClient } from '@tanstack/react-query';
import { $api, keys } from '@/api';

export function useProjects() {
  return $api.useQuery('get', '/projects');
}

export function useProjectDetail(projectId: string) {
  return $api.useQuery('get', '/projects/{project_id}/detail', {
    params: { path: { project_id: projectId } }
  });
}

export function useWorkspaces() {
  return $api.useQuery('get', '/workspaces');
}

export function useProviders() {
  return $api.useQuery('get', '/providers');
}

export function useMergePolicy(projectId: string) {
  return $api.useQuery('get', '/projects/{project_id}/merge-policy', {
    params: { path: { project_id: projectId } }
  });
}

export function useUpdateMergePolicy(projectId: string) {
  const queryClient = useQueryClient();
  const policyKey = $api.queryOptions('get', '/projects/{project_id}/merge-policy', {
    params: { path: { project_id: projectId } }
  }).queryKey;
  return $api.useMutation('put', '/projects/{project_id}/merge-policy', {
    onSettled: () =>
      Promise.all([
        queryClient.invalidateQueries({ queryKey: policyKey }),
        queryClient.invalidateQueries({ queryKey: keys.projectDetail(projectId) }),
        queryClient.invalidateQueries({ queryKey: keys.projects() })
      ])
  });
}
