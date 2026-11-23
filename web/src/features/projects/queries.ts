/**
 * Data hooks for the projects UI. Every read and write goes through the generated client, and cache
 * keys come from `keys` or from `$api.queryOptions`, so invalidation reaches every view of a project.
 * Each mutation re-reads what it changed before it settles, so `isPending` covers the re-read.
 */
import { useQueryClient } from '@tanstack/react-query';
import { $api, keys } from '@/api';
import { WORKER_LIMIT_POLL_MS } from './model';

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
 * How many workers the project may run at once, and how many it runs now. Task events re-read it
 * (see `api/invalidation`). One slot can be freed with no event, when a child that had already ended
 * is cleaned up later, so while any worker runs it is also read again every minute and whenever the
 * owner comes back to the page.
 */
export function useWorkerLimit(projectId: string) {
  return $api.useQuery(
    'get',
    '/projects/{project_id}/worker-limit',
    { params: { path: { project_id: projectId } } },
    {
      refetchInterval: (query) =>
        (query.state.data?.workers_running ?? 0) > 0 ? WORKER_LIMIT_POLL_MS : false,
      refetchOnWindowFocus: true
    }
  );
}

/** Sets the project's own limit, or returns it to the default. The answer is the new view. */
export function useUpdateWorkerLimit(projectId: string) {
  const queryClient = useQueryClient();
  const key = $api.queryOptions('get', '/projects/{project_id}/worker-limit', {
    params: { path: { project_id: projectId } }
  }).queryKey;
  return $api.useMutation('put', '/projects/{project_id}/worker-limit', {
    onSuccess: (data) => {
      queryClient.setQueryData(key, data);
      // The project carries its own limit too.
      return queryClient.invalidateQueries({ queryKey: keys.projects() });
    }
  });
}

/** The provider, model and effort of the project's standing supervisor. */
export function useSupervisorSettings(projectId: string) {
  return $api.useQuery('get', '/projects/{project_id}/supervisor/settings', {
    params: { path: { project_id: projectId } }
  });
}

/**
 * Replaces all three. The answer is the new view. A provider change restarts the supervisor, so
 * the tasks and the projects (which carry the supervisor's session) are read again.
 */
export function useUpdateSupervisorSettings(projectId: string) {
  const queryClient = useQueryClient();
  const key = $api.queryOptions('get', '/projects/{project_id}/supervisor/settings', {
    params: { path: { project_id: projectId } }
  }).queryKey;
  return $api.useMutation('put', '/projects/{project_id}/supervisor/settings', {
    onSuccess: (data) => {
      queryClient.setQueryData(key, data);
      return Promise.all([
        queryClient.invalidateQueries({ queryKey: keys.projects() }),
        queryClient.invalidateQueries({ queryKey: keys.tasks() })
      ]);
    }
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

export function useWorkspaceService(projectId: string) {
  return $api.useQuery('get', '/projects/{project_id}/workspace-service', {
    params: { path: { project_id: projectId } }
  });
}

/** Saves the service command. A save replaces the cached one; a failure re-reads what is stored. */
export function useUpdateWorkspaceService(projectId: string) {
  const queryClient = useQueryClient();
  const key = $api.queryOptions('get', '/projects/{project_id}/workspace-service', {
    params: { path: { project_id: projectId } }
  }).queryKey;
  return $api.useMutation('put', '/projects/{project_id}/workspace-service', {
    onSuccess: (data) => queryClient.setQueryData(key, data),
    onError: () => queryClient.invalidateQueries({ queryKey: key })
  });
}

/** Imports a repository as a project. Settling re-reads the project list, so new rows appear everywhere. */
export function useImportProject() {
  const queryClient = useQueryClient();
  return $api.useMutation('post', '/projects', {
    onSettled: () => queryClient.invalidateQueries({ queryKey: keys.projects() })
  });
}
