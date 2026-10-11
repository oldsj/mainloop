/**
 * Data hooks for the projects UI. Every read and write goes through the generated client, and cache
 * keys come from `keys` or from `$api.queryOptions`, so invalidation reaches every view of a project.
 * Each mutation re-reads what it changed before it settles, so `isPending` covers the re-read.
 */
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { $api, keys, type components } from '@/api';
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
 * The in-flight request and an uncertain reply stay in the app cache through route remounts. The
 * form stays locked until the post-create re-read settles and the owner acknowledges the check.
 */
export function useCreateWorkspace(projectId?: string) {
  const queryClient = useQueryClient();
  const checkKey = ['workspace-create-check', projectId ?? null];
  type Check = {
    body: components['schemas']['CreateWorkspaceRequest'];
    status: 'pending' | 'uncertain';
    checking: boolean;
  };
  // Client-only state: disabled queries are never refetched, even on a global invalidation.
  const check = useQuery<Check | null>({
    queryKey: checkKey,
    queryFn: () => null,
    initialData: null,
    enabled: false,
    gcTime: Infinity
  });
  const create = $api.useMutation('post', '/workspaces', {
    onMutate: ({ body }) => {
      queryClient.setQueryData<Check>(checkKey, { body, status: 'pending', checking: true });
    },
    onError: (error, { body }) => {
      if (isUncertain(error)) {
        queryClient.setQueryData<Check>(checkKey, { body, status: 'uncertain', checking: true });
      }
    },
    onSettled: async () => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: keys.workspaces() }),
        queryClient.invalidateQueries({ queryKey: keys.projects() }),
        queryClient.invalidateQueries({ queryKey: keys.sessions() }),
        queryClient.invalidateQueries({ queryKey: keys.attention() }),
        ...(projectId
          ? [queryClient.invalidateQueries({ queryKey: keys.projectDetail(projectId) })]
          : [])
      ]);
      queryClient.setQueryData<Check | null>(checkKey, (held) =>
        held?.status === 'uncertain' ? { ...held, checking: false } : null
      );
    }
  });
  return {
    ...create,
    pending: create.isPending || check.data?.checking === true,
    uncertain: check.data?.status === 'uncertain' ? check.data.body : null,
    acknowledge: () => {
      queryClient.setQueryData(checkKey, null);
      create.reset();
    }
  };
}
