/**
 * Session messages store for ALL active sessions.
 *
 * Provides:
 * - Messages for all sessions (for timeline notifications)
 * - Optimistic updates when sending messages
 * - Auto-refresh for active sessions
 */

import { writable, derived, get } from 'svelte/store';
import { api, type Message, type Session } from '$lib/api';
import { sessions } from './sessions';

interface AllSessionMessagesState {
  // Map of sessionId -> messages
  bySession: Map<string, Message[]>;
  loading: Set<string>;
}

function createAllSessionMessagesStore() {
  const { subscribe, set, update } = writable<AllSessionMessagesState>({
    bySession: new Map(),
    loading: new Set()
  });

  let refreshInterval: ReturnType<typeof setInterval> | null = null;

  return {
    subscribe,

    /**
     * Load messages for a specific session
     */
    async loadSession(sessionId: string) {
      update((s) => {
        const newLoading = new Set(s.loading);
        newLoading.add(sessionId);
        return { ...s, loading: newLoading };
      });

      try {
        const data = await api.getSessionConversation(sessionId);
        update((s) => {
          const newBySession = new Map(s.bySession);
          newBySession.set(sessionId, data.messages);
          const newLoading = new Set(s.loading);
          newLoading.delete(sessionId);
          return { bySession: newBySession, loading: newLoading };
        });
      } catch (e) {
        console.error('Failed to load session messages:', e);
        update((s) => {
          const newLoading = new Set(s.loading);
          newLoading.delete(sessionId);
          return { ...s, loading: newLoading };
        });
      }
    },

    /**
     * Refresh messages for all active sessions
     */
    async refreshAll() {
      const sessionsState = get(sessions);
      const activeSessionIds = sessionsState.sessions
        .filter((s) => !['completed', 'failed', 'cancelled'].includes(s.status))
        .map((s) => s.id);

      // Load each session's messages
      await Promise.all(activeSessionIds.map((id) => this.loadSession(id)));
    },

    /**
     * Add a message optimistically to a session
     */
    addOptimistic(sessionId: string, message: Message) {
      update((s) => {
        const newBySession = new Map(s.bySession);
        const existing = newBySession.get(sessionId) || [];
        newBySession.set(sessionId, [...existing, message]);
        return { ...s, bySession: newBySession };
      });
    },

    /**
     * Get messages for a specific session
     */
    getMessages(sessionId: string): Message[] {
      const state = get({ subscribe });
      return state.bySession.get(sessionId) || [];
    },

    /**
     * Start auto-refresh polling for all active sessions
     */
    startPolling(intervalMs = 3000) {
      this.stopPolling();
      this.refreshAll(); // Initial load
      refreshInterval = setInterval(() => this.refreshAll(), intervalMs);
    },

    /**
     * Stop auto-refresh polling
     */
    stopPolling() {
      if (refreshInterval) {
        clearInterval(refreshInterval);
        refreshInterval = null;
      }
    },

    /**
     * Clear all messages
     */
    clear() {
      this.stopPolling();
      set({ bySession: new Map(), loading: new Set() });
    }
  };
}

export const allSessionMessages = createAllSessionMessagesStore();

/**
 * Derived store: all session messages flattened with session info
 * Skips the first user message of each session since it duplicates the anchor
 */
export const allSessionMessagesFlat = derived(
  [allSessionMessages, sessions],
  ([$allMessages, $sessions]) => {
    const result: Array<{ message: Message; session: Session }> = [];

    for (const session of $sessions.sessions) {
      const messages = $allMessages.bySession.get(session.id) || [];
      for (let i = 0; i < messages.length; i++) {
        const message = messages[i];
        // Skip first message if it's a user message (duplicates the anchor)
        if (i === 0 && message.role === 'user' && session.anchor_message_id) {
          continue;
        }
        result.push({ message, session });
      }
    }

    return result;
  }
);
