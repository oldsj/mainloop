/**
 * Tasks store for managing active worker tasks
 */

import { writable, derived } from 'svelte/store';
import { api, type WorkerTask } from '$lib/api';

interface TasksState {
  tasks: WorkerTask[];
  isOpen: boolean;
  isLoading: boolean;
  error: string | null;
}

const initialState: TasksState = {
  tasks: [],
  isOpen: false,
  isLoading: false,
  error: null
};

function createTasksStore() {
  const { subscribe, set, update } = writable<TasksState>(initialState);

  let pollInterval: ReturnType<typeof setInterval> | null = null;

  return {
    subscribe,

    async fetchTasks() {
      update((s) => ({ ...s, isLoading: true, error: null }));
      try {
        const tasks = await api.listTasks();
        update((s) => ({ ...s, tasks, isLoading: false }));
      } catch (e) {
        update((s) => ({ ...s, error: 'Failed to load tasks', isLoading: false }));
      }
    },

    async cancelTask(taskId: string) {
      try {
        await api.cancelTask(taskId);
        update((s) => ({
          ...s,
          tasks: s.tasks.map((task) =>
            task.id === taskId ? { ...task, status: 'cancelled' } : task
          )
        }));
        // Refetch to update the list
        this.fetchTasks();
      } catch (e) {
        console.error('Failed to cancel task:', e);
        throw e;
      }
    },

    async retryTask(taskId: string) {
      try {
        await api.retryTask(taskId);
        update((s) => ({
          ...s,
          tasks: s.tasks.map((task) =>
            task.id === taskId ? { ...task, status: 'pending', error: null } : task
          )
        }));
        // Refetch to update the list
        this.fetchTasks();
      } catch (e) {
        console.error('Failed to retry task:', e);
        throw e;
      }
    },

    async answerQuestions(taskId: string, answers: Record<string, string>) {
      try {
        await api.answerTaskQuestions(taskId, answers);
        // Optimistically update task status
        update((s) => ({
          ...s,
          tasks: s.tasks.map((task) =>
            task.id === taskId
              ? { ...task, status: 'planning', pending_questions: null }
              : task
          )
        }));
        // Refetch to get updated state
        this.fetchTasks();
      } catch (e) {
        console.error('Failed to answer questions:', e);
        throw e;
      }
    },

    async cancelQuestions(taskId: string) {
      try {
        await api.answerTaskQuestions(taskId, {}, 'cancel');
        update((s) => ({
          ...s,
          tasks: s.tasks.map((task) =>
            task.id === taskId ? { ...task, status: 'cancelled' } : task
          )
        }));
        this.fetchTasks();
      } catch (e) {
        console.error('Failed to cancel task:', e);
        throw e;
      }
    },

    async approvePlan(taskId: string) {
      try {
        await api.approveTaskPlan(taskId, 'approve');
        update((s) => ({
          ...s,
          tasks: s.tasks.map((task) =>
            task.id === taskId ? { ...task, status: 'implementing', plan_text: null } : task
          )
        }));
        this.fetchTasks();
      } catch (e) {
        console.error('Failed to approve plan:', e);
        throw e;
      }
    },

    async revisePlan(taskId: string, feedback: string) {
      try {
        await api.approveTaskPlan(taskId, 'revise', feedback);
        update((s) => ({
          ...s,
          tasks: s.tasks.map((task) =>
            task.id === taskId ? { ...task, status: 'planning' } : task
          )
        }));
        this.fetchTasks();
      } catch (e) {
        console.error('Failed to revise plan:', e);
        throw e;
      }
    },

    open() {
      update((s) => ({ ...s, isOpen: true }));
      this.fetchTasks();
    },

    close() {
      update((s) => ({ ...s, isOpen: false }));
    },

    toggle() {
      let shouldFetch = false;
      update((s) => {
        shouldFetch = !s.isOpen;
        return { ...s, isOpen: !s.isOpen };
      });
      if (shouldFetch) {
        this.fetchTasks();
      }
    },

    startPolling(intervalMs = 10000) {
      this.stopPolling();
      this.fetchTasks();
      pollInterval = setInterval(() => {
        this.fetchTasks();
      }, intervalMs);
    },

    stopPolling() {
      if (pollInterval) {
        clearInterval(pollInterval);
        pollInterval = null;
      }
    }
  };
}

export const tasks = createTasksStore();

// Derived stores for convenience
export const tasksList = derived(tasks, ($tasks) => $tasks.tasks);
export const isTasksOpen = derived(tasks, ($tasks) => $tasks.isOpen);
export const activeTasksCount = derived(tasks, ($tasks) =>
  $tasks.tasks.filter((t) => !['completed', 'failed', 'cancelled'].includes(t.status)).length
);
