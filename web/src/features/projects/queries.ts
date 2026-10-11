/**
 * Data hooks for the projects UI. Every read and write goes through the generated client, and cache
 * keys come from `keys` or from `$api.queryOptions`, so invalidation reaches every view of a project.
 * Each mutation re-reads what it changed before it settles, so `isPending` covers the re-read.
 */
import { useQueryClient } from '@tanstack/react-query';
import { $api, keys } from '@/api';
import { isUncertain } from '@/features/environments/model';

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

/**
 * Creates a workspace from a repository or a project. Both lists it can appear in are re-read, and
 * the project's detail too when the create came from its page, since a new session shows there.
 * An uncertain reply (none, or a 5xx) may still have created the workspace, so `onUncertain` lets
 * the caller lock the form until the owner has checked the list. The re-read runs before settle.
 */
export function useCreateWorkspace(projectId?: string, onUncertain?: () => void) {
  const queryClient = useQueryClient();
  return $api.useMutation('post', '/workspaces', {
    onError: (error) => {
      if (isUncertain(error)) onUncertain?.();
    },
    onSettled: () =>
      Promise.all([
        queryClient.invalidateQueries({ queryKey: keys.workspaces() }),
        queryClient.invalidateQueries({ queryKey: keys.projects() }),
        queryClient.invalidateQueries({ queryKey: keys.sessions() }),
        queryClient.invalidateQueries({ queryKey: keys.attention() }),
        ...(projectId
          ? [queryClient.invalidateQueries({ queryKey: keys.projectDetail(projectId) })]
          : [])
      ])
  });
}
