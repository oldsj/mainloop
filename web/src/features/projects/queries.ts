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

export function useVerification(projectId: string) {
  return $api.useQuery('get', '/projects/{project_id}/verification', {
    params: { path: { project_id: projectId } }
  });
}

export function useUpdateVerification(projectId: string) {
  const queryClient = useQueryClient();
  const key = $api.queryOptions('get', '/projects/{project_id}/verification', {
    params: { path: { project_id: projectId } }
  }).queryKey;
  return $api.useMutation('put', '/projects/{project_id}/verification', {
    onSuccess: (data) => queryClient.setQueryData(key, data)
  });
}

export function useInstructions(projectId: string) {
  return $api.useQuery('get', '/projects/{project_id}/instructions', {
    params: { path: { project_id: projectId } }
  });
}

/** Earlier versions, read only while the owner has the list open. */
export function useInstructionVersions(projectId: string, enabled: boolean) {
  return $api.useQuery(
    'get',
    '/projects/{project_id}/instructions/versions',
    { params: { path: { project_id: projectId } } },
    { enabled }
  );
}

/**
 * Saves the instructions. A saved version replaces the cached one at once; any failure re-reads the
 * current instructions, so a conflict shows what changed while the owner's draft stays as typed.
 */
export function useUpdateInstructions(projectId: string) {
  const queryClient = useQueryClient();
  const params = { params: { path: { project_id: projectId } } };
  const key = $api.queryOptions('get', '/projects/{project_id}/instructions', params).queryKey;
  const versionsKey = $api.queryOptions(
    'get',
    '/projects/{project_id}/instructions/versions',
    params
  ).queryKey;
  return $api.useMutation('put', '/projects/{project_id}/instructions', {
    onSuccess: (data) => queryClient.setQueryData(key, data),
    onError: () => queryClient.invalidateQueries({ queryKey: key }),
    onSettled: () => queryClient.invalidateQueries({ queryKey: versionsKey })
  });
}

export function useProjectMemory(projectId: string) {
  return $api.useQuery('get', '/projects/{project_id}/memory', {
    params: { path: { project_id: projectId } }
  });
}

/**
 * Accepts, rejects or revokes one entry. Settling re-reads the memory, so a refusal (an entry
 * decided elsewhere, or memory that would pass its bound) shows the current entries, and the
 * attention snapshot, whose review item counts the waiting proposals.
 */
export function useDecideMemory(projectId: string) {
  const queryClient = useQueryClient();
  const key = $api.queryOptions('get', '/projects/{project_id}/memory', {
    params: { path: { project_id: projectId } }
  }).queryKey;
  return $api.useMutation('post', '/projects/{project_id}/memory/{entry_id}/decision', {
    onSettled: () =>
      Promise.all([
        queryClient.invalidateQueries({ queryKey: key }),
        queryClient.invalidateQueries({ queryKey: keys.attention() })
      ])
  });
}
