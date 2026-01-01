/**
 * Tasks store for managing active worker tasks
 *
 * Uses SSE for real-time updates instead of polling.
 */

import { writable, derived } from 'svelte/store';
import { api, type WorkerTask } from '$lib/api';
import { getSSEClient, type SSEEvent } from '$lib/sse';

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

  let sseUnsubscribe: (() => void) | null = null;

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

    async startImplementation(taskId: string) {
      try {
        await api.startImplementation(taskId);
        update((s) => ({
          ...s,
          tasks: s.tasks.map((task) =>
            task.id === taskId ? { ...task, status: 'implementing' } : task
          )
        }));
        this.fetchTasks();
      } catch (e) {
        console.error('Failed to start implementation:', e);
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

    /**
     * Start listening for SSE updates.
     * Replaces polling with real-time event-driven updates.
     */
    startListening() {
      this.stopListening();
      this.fetchTasks(); // Initial fetch

      const client = getSSEClient();

      // Handle task updates from SSE
      sseUnsubscribe = client.on('task:updated', (event: SSEEvent) => {
        const { task_id, status } = event.data as { task_id: string; status: string };

        // Update the task in our local state
        update((s) => ({
          ...s,
          tasks: s.tasks.map((task) =>
            task.id === task_id ? { ...task, status } : task
          )
        }));

        // Refetch to get full task details (status change might include other fields)
        this.fetchTasks();
      });
    },

    /**
     * Stop listening for SSE updates.
     */
    stopListening() {
      if (sseUnsubscribe) {
        sseUnsubscribe();
        sseUnsubscribe = null;
      }
    },

    // Legacy methods for backwards compatibility
    startPolling(intervalMs = 10000) {
      // Now just starts SSE listening
      this.startListening();
    },

    stopPolling() {
      this.stopListening();
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
