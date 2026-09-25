import { writable } from 'svelte/store';
import { api, type WorkspaceLifecycle } from '$lib/api';

interface WorkspacesState {
  workspaces: WorkspaceLifecycle[];
  loading: boolean;
  error: string | null;
}

function createWorkspacesStore() {
  const { subscribe, set, update } = writable<WorkspacesState>({
    workspaces: [],
    loading: false,
    error: null
  });

  return {
    subscribe,

    async fetchWorkspaces() {
      update((state) => ({ ...state, loading: true, error: null }));
      try {
        const workspaces = await api.listWorkspaces();
        update((state) => ({ ...state, workspaces, loading: false }));
      } catch (error) {
        update((state) => ({
          ...state,
          loading: false,
          error: error instanceof Error ? error.message : 'Failed to fetch workspaces'
        }));
      }
    },

    upsert(workspace: WorkspaceLifecycle) {
      update((state) => {
        const index = state.workspaces.findIndex(
          (item) => item.workspace_id === workspace.workspace_id
        );
        const workspaces = [...state.workspaces];
        if (index === -1) workspaces.push(workspace);
        else workspaces[index] = workspace;
        return { ...state, workspaces };
      });
    },

    get(workspaceId: string): WorkspaceLifecycle | undefined {
      let result: WorkspaceLifecycle | undefined;
      const unsubscribe = subscribe((state) => {
        result = state.workspaces.find((item) => item.workspace_id === workspaceId);
      });
      unsubscribe();
      return result;
    },

    reset() {
      set({ workspaces: [], loading: false, error: null });
    }
  };
}

export const workspaces = createWorkspacesStore();
