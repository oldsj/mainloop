/**
 * Data hooks for the sessions UI. Every read and write goes through the generated client, and
 * every cache key comes from `keys`, so SSE invalidation reaches these queries without a second
 * map. The only interval is the identity strip's native read, and only while a turn is in flight.
 */
import { useQueryClient } from '@tanstack/react-query';
import { $api, keys } from '@/api';
import { nativePollInterval } from './model';

export function useAttention() {
  return $api.useQuery('get', '/attention');
}

export function useProviders() {
  return $api.useQuery('get', '/providers');
}

export function useWorkspaces() {
  return $api.useQuery('get', '/workspaces');
}

export function useTasks() {
  return $api.useQuery('get', '/tasks');
}

/** The direct read of one session. It decides "not found" and carries the session when attention does not. */
export function useSessionDetail(sessionId: string) {
  return $api.useQuery('get', '/sessions/{session_id}', {
    params: { path: { session_id: sessionId } }
  });
}

export function useNativeSession(sessionId: string) {
  return $api.useQuery(
    'get',
    '/sessions/{session_id}/native',
    { params: { path: { session_id: sessionId } } },
    {
      refetchInterval: (query) => nativePollInterval(query.state.data),
      refetchIntervalInBackground: false
    }
  );
}

/**
 * Actions on one session. Every action re-reads the session, its native state and attention once
 * it settles, success or failure. A 409 therefore reaches the owner only after a fresh read.
 */
export function useSessionActions(sessionId: string) {
  const queryClient = useQueryClient();

  const reread = () =>
    Promise.all([
      queryClient.invalidateQueries({ queryKey: keys.session(sessionId) }),
      queryClient.invalidateQueries({ queryKey: keys.sessionNative(sessionId) }),
      queryClient.invalidateQueries({ queryKey: keys.attention() }),
      queryClient.invalidateQueries({ queryKey: keys.sessions() })
    ]);

  const options = { onSettled: reread };

  return {
    cancel: $api.useMutation('post', '/sessions/{session_id}/cancel', options),
    stopTurn: $api.useMutation('post', '/sessions/{session_id}/stop-turn', options),
    archive: $api.useMutation('post', '/sessions/{session_id}/archive', options)
  };
}

export function useCreateSession() {
  const queryClient = useQueryClient();
  return $api.useMutation('post', '/sessions', {
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: keys.attention() });
      void queryClient.invalidateQueries({ queryKey: keys.sessions() });
    }
  });
}
