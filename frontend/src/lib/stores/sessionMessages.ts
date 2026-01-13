/**
 * Session messages store for the currently focused session.
 *
 * Provides:
 * - Messages for inline display at bottom of ConversationView
 * - Optimistic updates when sending messages
 * - Auto-refresh integration with SessionBlock
 */

import { writable, derived, get } from 'svelte/store';
import { api, type Message } from '$lib/api';
import { navigationContext, currentSession } from './navigationContext';

interface SessionMessagesState {
  sessionId: string | null;
  messages: Message[];
  loading: boolean;
}

function createSessionMessagesStore() {
  const { subscribe, set, update } = writable<SessionMessagesState>({
    sessionId: null,
    messages: [],
    loading: false
  });

  let refreshInterval: ReturnType<typeof setInterval> | null = null;

  return {
    subscribe,

    /**
     * Load messages for a session
     */
    async loadMessages(sessionId: string) {
      update((s) => ({ ...s, sessionId, loading: true }));
      try {
        const data = await api.getSessionConversation(sessionId);
        update((s) => ({
          ...s,
          sessionId,
          messages: data.messages,
          loading: false
        }));
      } catch (e) {
        console.error('Failed to load session messages:', e);
        update((s) => ({ ...s, loading: false }));
      }
    },

    /**
     * Refresh messages for the current session
     */
    async refresh() {
      const state = get({ subscribe });
      if (state.sessionId) {
        try {
          const data = await api.getSessionConversation(state.sessionId);
          update((s) => ({ ...s, messages: data.messages }));
        } catch (e) {
          console.error('Failed to refresh session messages:', e);
        }
      }
    },

    /**
     * Add a message optimistically (for immediate UI feedback)
     */
    addOptimistic(message: Message) {
      update((s) => ({
        ...s,
        messages: [...s.messages, message]
      }));
    },

    /**
     * Start auto-refresh polling
     */
    startPolling(intervalMs = 2000) {
      this.stopPolling();
      refreshInterval = setInterval(() => this.refresh(), intervalMs);
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
     * Clear messages and stop polling
     */
    clear() {
      this.stopPolling();
      set({ sessionId: null, messages: [], loading: false });
    }
  };
}

export const sessionMessages = createSessionMessagesStore();

/**
 * Derived store: messages for the current focused session
 */
export const currentSessionMessages = derived(
  [sessionMessages, currentSession],
  ([$sessionMessages, $currentSession]) => {
    // Only return messages if we're focused on this session
    if (!$currentSession || $sessionMessages.sessionId !== $currentSession.id) {
      return [];
    }
    return $sessionMessages.messages;
  }
);
