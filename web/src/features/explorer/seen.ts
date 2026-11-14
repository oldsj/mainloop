/**
 * Which finished tasks the owner has looked at, per browser. A finished task the owner has not
 * opened since it finished shows a filled teal dot; once opened it is hollow, like any idle row.
 * There is no API for this: it is kept in local storage, as the explorer's layout is.
 *
 * `since` is when this browser first kept the record. A task that finished before then counts as
 * seen, so the first load does not light every old task.
 */
import { useCallback, useSyncExternalStore } from 'react';

export const SEEN_KEY = 'mainloop.seen.v1';

/** Entries older than this are dropped on write, so the record cannot grow without bound. */
export const SEEN_KEEP_MS = 60 * 24 * 60 * 60 * 1000;

export interface SeenState {
  /** When this browser started keeping the record, in ms. */
  since: number;
  /** Task id to the last time the owner had it open, in ms. */
  seen: Record<string, number>;
}

function parse(raw: string | null): SeenState | null {
  if (!raw) return null;
  try {
    const value: unknown = JSON.parse(raw);
    if (typeof value !== 'object' || value === null) return null;
    const { since, seen } = value as { since?: unknown; seen?: unknown };
    if (typeof since !== 'number' || !Number.isFinite(since)) return null;
    const entries =
      typeof seen === 'object' && seen !== null
        ? Object.entries(seen).filter(
            (entry): entry is [string, number] =>
              typeof entry[1] === 'number' && Number.isFinite(entry[1])
          )
        : [];
    return { since, seen: Object.fromEntries(entries) };
  } catch {
    return null;
  }
}

/** The record, started now if there is none. Null when storage is blocked: nothing is unseen then. */
export function readSeen(now: number = Date.now()): SeenState | null {
  try {
    const stored = parse(window.localStorage.getItem(SEEN_KEY));
    if (stored) return stored;
    const fresh: SeenState = { since: now, seen: {} };
    window.localStorage.setItem(SEEN_KEY, JSON.stringify(fresh));
    return fresh;
  } catch {
    return null;
  }
}

/** Records that the owner has `taskId` open now. Returns the new record, or null when storage is blocked. */
export function markSeen(taskId: string, now: number = Date.now()): SeenState | null {
  const state = readSeen(now);
  if (!state) return null;
  const kept = Object.entries(state.seen).filter(([, at]) => now - at < SEEN_KEEP_MS);
  const next: SeenState = {
    since: state.since,
    seen: { ...Object.fromEntries(kept), [taskId]: now }
  };
  try {
    window.localStorage.setItem(SEEN_KEY, JSON.stringify(next));
  } catch {
    return null;
  }
  return next;
}

/**
 * Whether a finished task has not been looked at: it last changed after this browser started
 * keeping the record, and after the owner last had it open. With no record nothing is unseen.
 */
export function isUnseen(
  state: SeenState | null | undefined,
  taskId: string,
  updatedAt: string
): boolean {
  if (!state) return false;
  const changed = Date.parse(updatedAt);
  if (!Number.isFinite(changed)) return false;
  return changed > state.since && changed > (state.seen[taskId] ?? 0);
}

// One record for every reader in the page, so a mark made by one tree shows in the other.
let current: SeenState | null | undefined;
const listeners = new Set<() => void>();
const snapshot = () => (current === undefined ? (current = readSeen()) : current);
const subscribe = (listener: () => void) => {
  listeners.add(listener);
  return () => listeners.delete(listener);
};

/**
 * The record and a way to mark a task seen. Every caller shares one record. `changedAt` is the
 * task's own last change: the mark is never earlier than it, so a browser clock that runs behind
 * the server cannot leave an open task unseen.
 */
export function useSeen(): {
  seen: SeenState | null;
  mark: (taskId: string, changedAt?: string) => void;
} {
  const seen = useSyncExternalStore(subscribe, snapshot, () => null);
  const mark = useCallback((taskId: string, changedAt?: string) => {
    const changed = changedAt ? Date.parse(changedAt) : Number.NaN;
    const next = markSeen(taskId, Math.max(Date.now(), Number.isFinite(changed) ? changed : 0));
    if (next === null) return;
    current = next;
    for (const listener of listeners) listener();
  }, []);
  return { seen, mark };
}

/** Test helper: forget the in-memory record, so the next read comes from storage again. */
export function resetSeenForTests(): void {
  current = undefined;
}
