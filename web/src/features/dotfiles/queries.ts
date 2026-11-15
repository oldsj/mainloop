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

/**
 * A session's dotfiles and whether its source has moved on. The server reads the source's ref at
 * most once a minute, so this polls no faster; a pending refresh polls until it settles.
 */
export function useSessionDotfiles(sessionId: string) {
  return $api.useQuery(
    'get',
    '/sessions/{session_id}/dotfiles',
    { params: { path: { session_id: sessionId } } },
    {
      staleTime: 30_000,
      refetchInterval: (query) =>
        query.state.data?.refresh?.state === 'pending' ? 10_000 : 60_000,
      refetchIntervalInBackground: false
    }
  );
}

/** Refreshes a session's dotfiles; settled either way, it reads the session's dotfiles again. */
export function useRefreshSessionDotfiles(sessionId: string) {
  const queryClient = useQueryClient();
  const key = $api.queryOptions('get', '/sessions/{session_id}/dotfiles', {
    params: { path: { session_id: sessionId } }
  }).queryKey;
  return $api.useMutation('post', '/sessions/{session_id}/dotfiles/refresh', {
    onSettled: () => queryClient.invalidateQueries({ queryKey: key })
  });
}
