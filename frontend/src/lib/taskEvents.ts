/**
 * `task:updated` handling. The event only names a task and the version it reached; delivery is
 * at least once and can be lost across a reconnect. So the event is a prompt to GET the task,
 * and the GET is the source of truth: duplicates are dropped, stale reads never overwrite newer
 * ones, and a reconnect re-reads everything on screen.
 */
import type { TaskUpdatedEvent, TaskView } from './api';

/** Validate an SSE payload; anything malformed is ignored rather than trusted. */
export function parseTaskEvent(data: unknown): TaskUpdatedEvent | null {
  if (!data || typeof data !== 'object') return null;
  const value = data as Record<string, unknown>;
  if (
    typeof value.event_id !== 'string' ||
    typeof value.task_id !== 'string' ||
    typeof value.version !== 'number' ||
    !Number.isFinite(value.version)
  )
    return null;
  return {
    type: 'task:updated',
    event_id: value.event_id,
    task_id: value.task_id,
    version: value.version,
    attempt_id: typeof value.attempt_id === 'string' ? value.attempt_id : null,
    root_task_id: typeof value.root_task_id === 'string' ? value.root_task_id : value.task_id,
    parent_task_id: typeof value.parent_task_id === 'string' ? value.parent_task_id : null,
    occurred_at: typeof value.occurred_at === 'string' ? value.occurred_at : ''
  };
}

/** Remembers recent event IDs so a redelivered event does not trigger a second read. */
export function createEventGate(limit = 500) {
  const seen = new Set<string>();
  return {
    /** True the first time an ID is offered, false for a redelivery. */
    accept(eventId: string): boolean {
      if (seen.has(eventId)) return false;
      seen.add(eventId);
      if (seen.size > limit) seen.delete(seen.values().next().value as string);
      return true;
    }
  };
}

/** Whether an event requires a read, given the version of the task already held (if any). */
export function needsRead(held: number | undefined, event: TaskUpdatedEvent): boolean {
  return held === undefined || event.version > held;
}

/**
 * Keep the newer of the held and fetched views. A slow read that lands after a newer one must
 * not roll the screen back.
 */
export function mergeView(held: TaskView | undefined, fetched: TaskView): TaskView {
  if (held && fetched.task.version < held.task.version) return held;
  return fetched;
}

/**
 * One read per task at a time. An event or a refresh that arrives while a read is in flight
 * schedules exactly one follow-up read, however many arrive.
 */
export function createReadCoalescer(read: (taskId: string) => Promise<void>) {
  const inflight = new Set<string>();
  const dirty = new Set<string>();

  async function request(taskId: string): Promise<void> {
    if (inflight.has(taskId)) {
      dirty.add(taskId);
      return;
    }
    inflight.add(taskId);
    try {
      do {
        dirty.delete(taskId);
        await read(taskId);
      } while (dirty.has(taskId));
    } finally {
      inflight.delete(taskId);
      dirty.delete(taskId);
    }
  }
  return { request };
}

/**
 * EventSource reconnects by itself and announces each connection with `connected`. Anything
 * published while it was down is gone, so every connection after the first is a reconcile point.
 */
export function createReconnectWatcher() {
  let connections = 0;
  return {
    /** True when this connection follows an earlier one. */
    connected(): boolean {
      connections += 1;
      return connections > 1;
    },
    /** Forget history, e.g. when listening restarts: the next connection is a fresh start. */
    reset(): void {
      connections = 0;
    }
  };
}

export interface TaskSyncDeps {
  /** The view currently held for a task, if any. */
  held(taskId: string): TaskView | undefined;
  heldIds(): string[];
  fetchTask(taskId: string): Promise<TaskView>;
  /** Store a fetched view (already merged against the held one). */
  apply(view: TaskView): void;
  /** Re-run every list read the screen has made, to find tasks created while disconnected. */
  reconcileLists(): Promise<void>;
  onError?(taskId: string, error: unknown): void;
}

/**
 * Ties events to reads: an event for a task newer than the held view triggers a GET, a
 * redelivered event does nothing, and a reconnect re-reads every held task and every list.
 */
export function createTaskSync(deps: TaskSyncDeps) {
  const gate = createEventGate();
  const watcher = createReconnectWatcher();
  const reads = createReadCoalescer(async (taskId) => {
    try {
      const fetched = await deps.fetchTask(taskId);
      deps.apply(mergeView(deps.held(taskId), fetched));
    } catch (error) {
      deps.onError?.(taskId, error);
    }
  });

  return {
    /** Returns the read it started, so callers and tests can wait for it. */
    handleEvent(data: unknown): Promise<void> {
      const event = parseTaskEvent(data);
      if (!event || !gate.accept(event.event_id)) return Promise.resolve();
      if (!needsRead(deps.held(event.task_id)?.task.version, event)) return Promise.resolve();
      return reads.request(event.task_id);
    },
    /** Call for every SSE `connected` event. The first needs nothing: the caller just loaded. */
    handleConnected(): Promise<void> {
      if (!watcher.connected()) return Promise.resolve();
      return Promise.all([
        deps.reconcileLists().catch((error) => deps.onError?.('*', error)),
        ...deps.heldIds().map((id) => reads.request(id))
      ]).then(() => undefined);
    },
    /** Account for a connection that was already open when listening began. */
    assumeConnected(): void {
      watcher.connected();
    },
    reset(): void {
      watcher.reset();
    },
    read: (taskId: string) => reads.request(taskId)
  };
}
