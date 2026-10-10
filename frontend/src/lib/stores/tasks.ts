/**
 * Durable tasks, as read from the owner task API.
 *
 * The backend is the source of truth. `task:updated` events only prompt a GET; the GET result
 * replaces the held view unless it is older (see taskEvents). Actions are sent through api.ts
 * and never change a task locally: the owner sees the new state when the server reports it.
 */
import { derived, get, writable } from 'svelte/store';
import { browser } from '$app/environment';
import { api, type ProviderProfile, type TaskActionKind, type TaskView } from '$lib/api';
import { getSSEClient } from '$lib/sse';
import { createActionTracker, type ActionIntent, type ActionOutcome } from '$lib/taskActions';
import { createTaskSync, mergeView } from '$lib/taskEvents';
import { normalizeView } from '$lib/taskState';

export interface TaskNotice {
  tone: 'info' | 'warning' | 'error';
  text: string;
}

export interface TaskActionState {
  kind: TaskActionKind | null;
  busy: boolean;
  notice: TaskNotice | null;
  /** A request whose outcome is unknown; the next submit resends it unchanged. */
  resend: TaskActionKind | null;
  pending: ActionIntent | null;
}

interface TasksState {
  byId: Record<string, TaskView>;
  /** A list read is in flight. */
  loading: boolean;
  /** Whether any list read has completed. */
  loaded: boolean;
  error: string | null;
  /** Per-task read failures (not found, unreachable), so a detail page can say why it is empty. */
  taskErrors: Record<string, { status: number | null; message: string }>;
  actions: Record<string, TaskActionState>;
  profiles: ProviderProfile[];
  profilesError: string | null;
}

export interface TaskListFilter {
  project_id?: string;
  parent_task_id?: string;
}

const initialState: TasksState = {
  byId: {},
  loading: false,
  loaded: false,
  error: null,
  taskErrors: {},
  actions: {},
  profiles: [],
  profilesError: null
};

const idleAction: TaskActionState = {
  kind: null,
  busy: false,
  notice: null,
  resend: null,
  pending: null
};

function createTasksStore() {
  const store = writable<TasksState>(initialState, () => {
    startListening();
    return stopListening;
  });
  const { subscribe, update } = store;
  const scopes = new Map<string, TaskListFilter>();
  let unsubscribeEvents: (() => void) | null = null;

  const tracker = createActionTracker((intent: ActionIntent) => {
    if (intent.kind === 'retry') return api.retryTask(intent.taskId, intent.body);
    if (intent.kind === 'reassign') return api.reassignTask(intent.taskId, intent.body);
    return api.cancelTask(intent.taskId, intent.body);
  });

  function apply(view: TaskView) {
    update((s) => ({
      ...s,
      byId: { ...s.byId, [view.task.id]: mergeView(s.byId[view.task.id], normalizeView(view)) },
      taskErrors: Object.fromEntries(
        Object.entries(s.taskErrors).filter(([id]) => id !== view.task.id)
      )
    }));
  }

  const sync = createTaskSync({
    held: (id) => get(store).byId[id],
    heldIds: () => Object.keys(get(store).byId),
    fetchTask: (id) => api.getTask(id),
    apply,
    reconcileLists: async () => {
      await Promise.all([...scopes.values()].map((filter) => fetchList(filter)));
    },
    onError: (id, error) => {
      if (id === '*') {
        update((s) => ({
          ...s,
          error: error instanceof Error ? error.message : 'Could not refresh task lists'
        }));
      } else recordTaskError(id, error);
    }
  });

  function startListening() {
    if (!browser) return;
    stopListening();
    sync.reset();
    const client = getSSEClient();
    if (client.isConnected()) sync.assumeConnected();
    const offUpdated = client.on('task:updated', (event) => void sync.handleEvent(event.data));
    const offConnected = client.on('connected', () => void sync.handleConnected());
    unsubscribeEvents = () => {
      offUpdated();
      offConnected();
    };
  }

  function stopListening() {
    unsubscribeEvents?.();
    unsubscribeEvents = null;
  }

  const listReads = new Map<string, Promise<void>>();

  // The panel, a project page and a reconnect can ask for the same list at once: share one read.
  function fetchList(filter: TaskListFilter = {}, signal?: AbortSignal): Promise<void> {
    const key = JSON.stringify(filter);
    const running = listReads.get(key);
    if (running) return running;
    const read = readList(filter, signal).finally(() => listReads.delete(key));
    listReads.set(key, read);
    return read;
  }

  async function readList(filter: TaskListFilter, signal?: AbortSignal): Promise<void> {
    scopes.set(JSON.stringify(filter), filter);
    update((s) => ({ ...s, loading: true }));
    try {
      const views = await api.listTasks(filter, signal);
      for (const view of views) apply(view);
      update((s) => ({ ...s, loading: false, loaded: true, error: null }));
    } catch (error) {
      if (signal?.aborted && signal.reason?.name !== 'TimeoutError') {
        update((s) => ({ ...s, loading: false }));
        return;
      }
      update((s) => ({
        ...s,
        loading: false,
        error: error instanceof Error ? error.message : 'Could not load tasks'
      }));
    }
  }

  function recordTaskError(id: string, error: unknown) {
    const status = (error as { status?: number } | null)?.status ?? null;
    update((s) => ({
      ...s,
      taskErrors: {
        ...s.taskErrors,
        [id]: {
          status,
          message:
            status === 404
              ? 'Task not found'
              : status == null
                ? "Can't reach the Mainloop backend."
                : 'Task could not be loaded.'
        }
      }
    }));
  }

  async function fetchTask(id: string, signal?: AbortSignal): Promise<void> {
    try {
      apply(await api.getTask(id, signal));
    } catch (error) {
      if (signal?.aborted && signal.reason?.name !== 'TimeoutError') return;
      recordTaskError(id, error);
    }
  }

  async function loadProfiles(): Promise<void> {
    try {
      const profiles = await api.listProviders();
      update((s) => ({ ...s, profiles, profilesError: null }));
    } catch (error) {
      update((s) => ({
        ...s,
        profilesError: error instanceof Error ? error.message : 'Could not load providers'
      }));
    }
  }

  function setAction(taskId: string, next: Partial<TaskActionState>) {
    update((s) => ({
      ...s,
      actions: { ...s.actions, [taskId]: { ...(s.actions[taskId] ?? idleAction), ...next } }
    }));
  }

  function describe(outcome: ActionOutcome): TaskNotice {
    if (outcome.status === 'accepted')
      return {
        tone: 'info',
        text: `${outcome.operation.kind[0].toUpperCase()}${outcome.operation.kind.slice(1)} requested. Progress shows below as the server reports it.`
      };
    return {
      tone: outcome.status === 'uncertain' ? 'warning' : 'error',
      text: outcome.failure.message
    };
  }

  async function act(
    kind: TaskActionKind,
    taskId: string,
    options: { targetProfileId?: string } = {}
  ): Promise<ActionOutcome | null> {
    const view = get(store).byId[taskId];
    if (!view || get(store).actions[taskId]?.busy) return null;
    setAction(taskId, { kind, busy: true, notice: null });
    const outcome = await tracker.submit(kind, view, options);
    setAction(taskId, {
      kind,
      busy: false,
      notice: describe(outcome),
      resend: outcome.status === 'uncertain' ? kind : null,
      pending: tracker.pending(taskId, kind)
    });
    // Accepted, stale and unconfirmed all call for the server's current state, not a local guess.
    if (outcome.status === 'accepted' || outcome.failure.refresh) await sync.read(taskId);
    return outcome;
  }

  return {
    subscribe,
    fetchList,
    fetchTask,
    loadProfiles,
    refresh: (id: string) => sync.read(id),
    act,
    resend(taskId: string) {
      const pending = get(store).actions[taskId]?.pending;
      if (!pending) return Promise.resolve(null);
      return act(pending.kind, taskId);
    },
    /** Stop resending an unconfirmed request after the owner has looked at the task. */
    dismissPending(taskId: string, kind: TaskActionKind) {
      tracker.dismiss(taskId, kind);
      setAction(taskId, { resend: null, pending: null, notice: null });
    },
    clearNotice(taskId: string) {
      setAction(taskId, { notice: null });
    },
    reset() {
      scopes.clear();
      update(() => initialState);
    }
  };
}

export const tasks = createTasksStore();

export const taskViews = derived(tasks, ($tasks) => Object.values($tasks.byId));
