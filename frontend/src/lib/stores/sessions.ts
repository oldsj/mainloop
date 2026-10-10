/**
 * Sessions store for managing session state
 */

import { writable, derived } from 'svelte/store';
import { api, type Session, type SessionCreate, type SessionStatus } from '$lib/api';
import { isCurrent, isFinished } from '$lib/sessionStatus';
import { singleFlight } from '../visiblePolling';

interface SessionsState {
  sessions: Session[];
  loading: boolean;
  loaded: boolean;
  error: string | null;
}

function createSessionsStore() {
  const { subscribe, set, update } = writable<SessionsState>({
    sessions: [],
    loading: false,
    loaded: false,
    error: null
  });

  return {
    subscribe,

    replaceSessions(projected: Session[]) {
      update((state) => ({
        ...state,
        sessions: projected,
        loading: false,
        loaded: true,
        error: null
      }));
    },

    fetchSessions: singleFlight(async (signal?: AbortSignal) => {
      update((state) => ({ ...state, loading: true, error: null }));
      try {
        const sessions = await api.listSessions(undefined, signal);
        update((state) => ({ ...state, sessions, loading: false, loaded: true }));
      } catch (e) {
        update((state) => ({
          ...state,
          loading: false,
          error: e instanceof Error ? e.message : 'Failed to fetch sessions'
        }));
      }
    }),

    async createSession(request: SessionCreate): Promise<Session> {
      try {
        const session = await api.createSession(request);
        update((state) => ({
          ...state,
          sessions: [session, ...state.sessions]
        }));
        return session;
      } catch (e) {
        console.error('Failed to create session:', e);
        throw e;
      }
    },

    /** Never throws: `ok` false carries the reason, `unconfirmed` says the agent may still run. */
    async cancelSession(
      sessionId: string
    ): Promise<{ ok: true; unconfirmed: boolean } | { ok: false; message: string }> {
      try {
        const result = await api.cancelSession(sessionId);
        update((state) => ({
          ...state,
          sessions: state.sessions.map((s) =>
            s.id === sessionId
              ? { ...s, status: 'cancelled' as SessionStatus, needs_owner: false }
              : s
          )
        }));
        return { ok: true, unconfirmed: result.agent === 'unknown' };
      } catch (e) {
        console.error('Failed to cancel session:', e);
        return { ok: false, message: e instanceof Error ? e.message : 'Failed to cancel session' };
      }
    },

    /** Clear one finished session from the list. Throws with the backend's reason if refused. */
    async archiveSession(sessionId: string) {
      await api.archiveSession(sessionId);
      update((state) => ({ ...state, sessions: state.sessions.filter((s) => s.id !== sessionId) }));
    },

    /** Clear every finished session from the list; returns how many were cleared. */
    async archiveFinished(): Promise<number> {
      const cleared = new Set(await api.archiveFinishedSessions());
      update((state) => ({
        ...state,
        sessions: state.sessions.filter((s) => !cleared.has(s.id))
      }));
      return cleared.size;
    },

    updateSession(sessionId: string, updates: Partial<Session>) {
      update((state) => ({
        ...state,
        sessions: state.sessions.map((s) => (s.id === sessionId ? { ...s, ...updates } : s))
      }));
    },

    reset() {
      set({ sessions: [], loading: false, loaded: false, error: null });
    }
  };
}

export const sessions = createSessionsStore();

// Derived stores for filtered views
export const activeSessions = derived(sessions, ($sessions) =>
  $sessions.sessions.filter(isCurrent)
);

/** Sessions that are over (done, failed, cancelled): these can be cleared from the list. */
export const finishedSessions = derived(sessions, ($sessions) =>
  $sessions.sessions.filter(isFinished)
);

export const activeSessionCount = derived(activeSessions, ($active) => $active.length);
