/**
 * Sessions store for managing session state
 * (HMR refresh)
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

    async cancelSession(sessionId: string) {
      try {
        await api.cancelSession(sessionId);
        update((state) => ({
          ...state,
          sessions: state.sessions.map((s) =>
            s.id === sessionId ? { ...s, status: 'failed' as SessionStatus, error: 'Cancelled' } : s
          )
        }));
      } catch (e) {
        console.error('Failed to cancel session:', e);
      }
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

export const activeSessionCount = derived(activeSessions, ($active) => $active.length);
