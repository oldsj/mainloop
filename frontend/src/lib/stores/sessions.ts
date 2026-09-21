/**
 * Sessions store for managing session state
 */

import { writable, derived } from 'svelte/store';
import { api, type Session, type SessionCreate, type SessionStatus } from '$lib/api';

interface SessionsState {
  sessions: Session[];
  loading: boolean;
  error: string | null;
}

function createSessionsStore() {
  const { subscribe, set, update } = writable<SessionsState>({
    sessions: [],
    loading: false,
    error: null
  });

  return {
    subscribe,

    async fetchSessions(status?: SessionStatus) {
      update((state) => ({ ...state, loading: true, error: null }));
      try {
        const sessions = await api.listSessions({ status });
        update((state) => ({ ...state, sessions, loading: false }));
      } catch (e) {
        update((state) => ({
          ...state,
          loading: false,
          error: e instanceof Error ? e.message : 'Failed to fetch sessions'
        }));
      }
    },

    async createSession(request: SessionCreate): Promise<Session | null> {
      try {
        const session = await api.createSession(request);
        update((state) => ({
          ...state,
          sessions: [session, ...state.sessions]
        }));
        return session;
      } catch (e) {
        console.error('Failed to create session:', e);
        return null;
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
            s.id === sessionId ? { ...s, status: 'cancelled' as SessionStatus } : s
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
      set({ sessions: [], loading: false, error: null });
    }
  };
}

export const sessions = createSessionsStore();

// Derived stores for filtered views
export const activeSessions = derived(sessions, ($sessions) =>
  $sessions.sessions.filter(
    (s) => s.status === 'pending' || s.status === 'active' || s.status === 'waiting_on_user'
  )
);

/** Sessions that are over (done, failed, cancelled): these can be cleared from the list. */
export const finishedSessions = derived(sessions, ($sessions) =>
  $sessions.sessions.filter(
    (s) => s.status === 'completed' || s.status === 'failed' || s.status === 'cancelled'
  )
);

export const activeSessionCount = derived(activeSessions, ($active) => $active.length);
