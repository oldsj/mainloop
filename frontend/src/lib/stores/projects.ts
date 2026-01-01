/**
 * Projects store for managing GitHub repository projects
 */

import { writable, derived } from 'svelte/store';
import { api, type Project, type ProjectDetail } from '$lib/api';

interface ProjectsState {
  projects: Project[];
  selectedProjectId: string | null; // For filtering
  currentProjectDetail: ProjectDetail | null;
  isLoading: boolean;
  error: string | null;
}

const initialState: ProjectsState = {
  projects: [],
  selectedProjectId: null,
  currentProjectDetail: null,
  isLoading: false,
  error: null
};

function createProjectsStore() {
  const { subscribe, set, update } = writable<ProjectsState>(initialState);

  return {
    subscribe,

    async fetchProjects() {
      update((s) => ({ ...s, isLoading: true, error: null }));
      try {
        const projects = await api.listProjects();
        update((s) => ({ ...s, projects, isLoading: false }));
      } catch (e) {
        update((s) => ({
          ...s,
          error: e instanceof Error ? e.message : 'Failed to load projects',
          isLoading: false
        }));
      }
    },

    async fetchProjectDetail(projectId: string) {
      update((s) => ({ ...s, isLoading: true, error: null }));
      try {
        const detail = await api.getProjectDetail(projectId);
        update((s) => ({ ...s, currentProjectDetail: detail, isLoading: false }));
      } catch (e) {
        update((s) => ({
          ...s,
          error: e instanceof Error ? e.message : 'Failed to load project',
          isLoading: false
        }));
      }
    },

    selectProject(projectId: string | null) {
      update((s) => ({ ...s, selectedProjectId: projectId }));
    },

    clearSelection() {
      update((s) => ({ ...s, selectedProjectId: null }));
    },

    async refresh(projectId: string) {
      try {
        await api.refreshProject(projectId);
        // Refetch to get updated data
        if (projectId) {
          await this.fetchProjectDetail(projectId);
        }
        await this.fetchProjects();
      } catch (e) {
        update((s) => ({
          ...s,
          error: e instanceof Error ? e.message : 'Failed to refresh project'
        }));
      }
    }
  };
}

export const projects = createProjectsStore();

// Derived stores
export const projectsList = derived(projects, ($p) => $p.projects);
export const selectedProjectId = derived(projects, ($p) => $p.selectedProjectId);
export const currentProject = derived(projects, ($p) => $p.currentProjectDetail);
