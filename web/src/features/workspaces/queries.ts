/**
 * Data hooks for the workspace page. Every read and write goes through the generated client and
 * every cache key comes from `keys`, so SSE invalidation reaches these queries without a second map.
 */
import { useQueryClient } from '@tanstack/react-query';
import { useRef } from 'react';
import { $api, keys } from '@/api';
import { actionFailure, type Notice } from '@/features/sessions/model';
import { isWorkspace, lifecycleNotice, UNREADABLE_ACTION, type WorkspaceAction } from './model';

export function useWorkspace(workspaceId: string) {
  return $api.useQuery('get', '/workspaces/{workspace_id}', {
    params: { path: { workspace_id: workspaceId } }
  });
}

export function useWorkspacePorts(workspaceId: string) {
  return $api.useQuery('get', '/workspaces/{workspace_id}/ports', {
    params: { path: { workspace_id: workspaceId } }
  });
}

export function useTasks() {
  return $api.useQuery('get', '/tasks');
}

type Notify = (notice: Notice | null) => void;

/**
 * The page's controls. Each action clears the last notice when it starts, and reports its own
 * failure through `notify`. The callbacks are on the mutations themselves, so a result still lands
 * if the control that started it has unmounted. The mutation stays pending until the re-read
 * settles, so the controls stay disabled until the workspace is fresh again after a 409.
 */
export function useWorkspaceActions(workspaceId: string, notify: Notify, onDeleted: () => void) {
  const queryClient = useQueryClient();
  // Set before the request starts, so a second press in the same frame cannot send a second request.
  const inFlight = useRef(false);
  const path = { params: { path: { workspace_id: workspaceId } } };

  const reread = () =>
    Promise.all([
      queryClient.invalidateQueries({ queryKey: keys.workspace(workspaceId) }),
      queryClient.invalidateQueries({ queryKey: keys.workspaces() })
    ]);

  const lifecycle = {
    onMutate: () => notify(null),
    onSuccess: (result: unknown) => {
      // An unreadable result is not written. The held copy stays, and the re-read in onSettled repairs it.
      if (!isWorkspace(result)) {
        notify({ tone: 'warn', text: UNREADABLE_ACTION });
        return;
      }
      queryClient.setQueryData(keys.workspace(workspaceId), result);
      notify(lifecycleNotice(result));
    },
    onError: (error: unknown) => notify(actionFailure(error)),
    onSettled: reread
  };

  const suspend = $api.useMutation('post', '/workspaces/{workspace_id}/suspend', lifecycle);
  const resume = $api.useMutation('post', '/workspaces/{workspace_id}/resume', lifecycle);
  const refresh = $api.useMutation('post', '/workspaces/{workspace_id}/refresh', lifecycle);
  const remove = $api.useMutation('delete', '/workspaces/{workspace_id}', {
    onMutate: () => notify(null),
    onSuccess: () => {
      // The by-id copies go with the workspace. Left in the cache, a revisit shows live controls for it.
      queryClient.removeQueries({ queryKey: keys.workspace(workspaceId) });
      queryClient.removeQueries({ queryKey: keys.workspacePorts(workspaceId) });
      void queryClient.invalidateQueries({ queryKey: keys.workspaces() });
    },
    onError: (error: unknown) => {
      notify(actionFailure(error));
      return reread();
    }
  });

  const active: WorkspaceAction | null = suspend.isPending
    ? 'suspend'
    : resume.isPending
      ? 'resume'
      : refresh.isPending
        ? 'refresh'
        : remove.isPending
          ? 'delete'
          : null;

  const start = (run: () => Promise<unknown>) => {
    if (inFlight.current) return;
    inFlight.current = true;
    // Failures are already reported through the mutation callbacks above.
    void run()
      .catch(() => undefined)
      .finally(() => {
        inFlight.current = false;
      });
  };

  return {
    active,
    busy: active !== null,
    suspend: () => start(() => suspend.mutateAsync(path)),
    resume: () => start(() => resume.mutateAsync(path)),
    refresh: () => start(() => refresh.mutateAsync(path)),
    remove: () => start(() => remove.mutateAsync(path).then(onDeleted))
  };
}
