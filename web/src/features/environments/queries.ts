/**
 * Data hooks for environment selection and registration. Calls go through the generated client and
 * cache keys come from `$api.queryOptions`, so the `$api` hooks and the prefix invalidations below
 * reach the same entries. Each write re-reads what it changed before it settles.
 */
import { useQueries, useQuery, useQueryClient } from '@tanstack/react-query';
import { $api, ApiError, fetchClient } from '@/api';
import { ContractError } from '@/features/projects/model';
import { isUncertain, selectionSchema, type RegisteredEnvironment } from './model';

const selectionKey = (projectId: string) =>
  $api.queryOptions('get', '/projects/{project_id}/environment', {
    params: { path: { project_id: projectId } }
  }).queryKey;

/** Every project's selection. Used when a change can reach any project that follows an environment. */
const SELECTION_PREFIX = ['get', '/projects/{project_id}/environment'];

const environmentsKey = () => $api.queryOptions('get', '/environments').queryKey;

const versionsKey = (environmentId: string) =>
  $api.queryOptions('get', '/environments/{environment_id}/versions', {
    params: { path: { environment_id: environmentId } }
  }).queryKey;

const VERSIONS_PREFIX = ['get', '/environments/{environment_id}/versions'];

export function useEnvironments() {
  return $api.useQuery('get', '/environments');
}

/** One versions query per environment, so each environment's versions load and fail on their own. */
export function useEnvironmentVersions(environmentIds: string[]) {
  return useQueries({
    queries: environmentIds.map((environmentId) =>
      $api.queryOptions('get', '/environments/{environment_id}/versions', {
        params: { path: { environment_id: environmentId } }
      })
    )
  });
}

/**
 * The project's saved selection. A 204 means none is saved. openapi-fetch returns no body for it,
 * and TanStack Query rejects an undefined result, so the body is wrapped as `{ selection }`.
 */
export function useProjectSelection(projectId: string) {
  return useQuery({
    queryKey: selectionKey(projectId),
    queryFn: async () => {
      const { data, error, response } = await fetchClient.GET(
        '/projects/{project_id}/environment',
        { params: { path: { project_id: projectId } } }
      );
      if (error !== undefined) {
        throw new ApiError('Could not load the environment selection.', response.status);
      }
      if (data !== undefined && !selectionSchema.safeParse(data).success) {
        throw new ContractError(
          'Mainloop sent an environment selection that does not match the contract.'
        );
      }
      return { selection: data ?? null };
    }
  });
}

export function useSelectProjectEnvironment(projectId: string) {
  const queryClient = useQueryClient();
  return $api.useMutation('put', '/projects/{project_id}/environment', {
    onSettled: () => queryClient.invalidateQueries({ queryKey: selectionKey(projectId) })
  });
}

/** Accepting a default changes what every project following this environment uses for new sessions. */
export function useAcceptEnvironmentDefault() {
  const queryClient = useQueryClient();
  return $api.useMutation('put', '/environments/{environment_id}/default', {
    onSettled: () =>
      Promise.all([
        queryClient.invalidateQueries({ queryKey: environmentsKey() }),
        queryClient.invalidateQueries({ queryKey: SELECTION_PREFIX })
      ])
  });
}

/**
 * Registers a public image. The reply tells the caller whether the write may have landed: an
 * uncertain reply reports through `onUncertain`, and the form stays locked until it is remounted.
 * A new environment's version is cached at once, so it can be chosen without another read.
 */
export function useRegisterEnvironment(handlers: {
  onRegistered: (result: RegisteredEnvironment) => void;
  onUncertain: () => void;
}) {
  const queryClient = useQueryClient();
  return $api.useMutation('post', '/environments', {
    onSuccess: (result) => {
      queryClient.setQueryData(versionsKey(result.environment.id), [result.version]);
      handlers.onRegistered(result);
    },
    onError: (error) => {
      if (isUncertain(error)) handlers.onUncertain();
    },
    onSettled: () => queryClient.invalidateQueries({ queryKey: environmentsKey() })
  });
}

/** Re-reads every environment list, version list and project selection the section shows. */
export function useRefreshEnvironments() {
  const queryClient = useQueryClient();
  return () =>
    Promise.all([
      queryClient.invalidateQueries({ queryKey: environmentsKey() }),
      queryClient.invalidateQueries({ queryKey: VERSIONS_PREFIX }),
      queryClient.invalidateQueries({ queryKey: SELECTION_PREFIX })
    ]);
}
