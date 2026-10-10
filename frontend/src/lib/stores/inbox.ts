/**
 * Inbox store for managing queue items and unread state
 *
 * Uses SSE updates plus bounded foreground reads for observer-created requests.
 */

import { singleFlight, visiblePolling } from '../visiblePolling';
import { writable, derived } from 'svelte/store';
import { api, type AttentionItem } from '$lib/api';
import { getSSEClient } from '$lib/sse';
import { sessions } from './sessions';

interface InboxState {
  items: AttentionItem[];
  unreadCount: number;
  isOpen: boolean;
  isLoading: boolean;
  loaded: boolean;
  error: string | null;
}

const initialState: InboxState = {
  items: [],
  unreadCount: 0,
  isOpen: false,
  isLoading: false,
  loaded: false,
  error: null
};

function createInboxStore() {
  const { subscribe, set, update } = writable<InboxState>(initialState);

  let sseUnsubscribe: (() => void) | null = null;
  let stopPolling: (() => void) | null = null;

  return {
    subscribe,

    fetchItems: singleFlight(async (signal?: AbortSignal) => {
      update((s) => ({ ...s, isLoading: true, error: null }));
      try {
        const snapshot = await api.getAttention(signal);
        if (!signal?.aborted) {
          sessions.replaceSessions(snapshot.sessions);
          update((s) => ({
            ...s,
            items: snapshot.items,
            unreadCount: snapshot.count,
            isLoading: false,
            loaded: true
          }));
        }
      } catch (e) {
        if (!signal?.aborted || signal.reason?.name === 'TimeoutError')
          update((s) => ({ ...s, error: 'Failed to load inbox', isLoading: false }));
      } finally {
        update((s) => ({ ...s, isLoading: false }));
      }
    }),

    async fetchUnreadCount() {
      await this.fetchItems();
    },

    async dismissDiagnostic(itemId: string) {
      await api.dismissQueueDiagnostic(itemId);
      await this.fetchItems();
      await this.fetchUnreadCount();
    },

    async markRead(itemId: string) {
      try {
        await api.markQueueItemRead(itemId);
        await this.fetchItems();
      } catch (e) {
        console.error('Failed to mark item read:', e);
      }
    },

    async markAllRead() {
      try {
        await api.markAllQueueItemsRead();
        await this.fetchItems();
      } catch (e) {
        console.error('Failed to mark all read:', e);
      }
    },

    async respond(itemId: string, response: string) {
      try {
        await api.respondToQueueItem(itemId, response);
        // Refetch to update the list
        this.fetchItems();
        this.fetchUnreadCount();
      } catch (e) {
        console.error('Failed to respond:', e);
        throw e;
      }
    },

    open() {
      update((s) => ({ ...s, isOpen: true }));
      this.fetchItems();
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
        this.fetchItems();
      }
    },

    /**
     * Start listening for SSE updates.
     * Replaces polling with real-time event-driven updates.
     */
    startListening() {
      this.stopListening();
      this.fetchUnreadCount(); // Initial fetch
      stopPolling = visiblePolling().watch('attention', (signal) => this.fetchItems(signal));

      const client = getSSEClient();

      // Handle inbox updates from SSE
      const listeners = (['inbox:updated', 'task:updated', 'session:updated'] as const).map(
        (kind) => client.on(kind, () => void this.fetchItems())
      );
      sseUnsubscribe = () => listeners.forEach((stop) => stop());
    },

    /**
     * Stop listening for SSE updates.
     */
    stopListening() {
      stopPolling?.();
      stopPolling = null;
      if (sseUnsubscribe) {
        sseUnsubscribe();
        sseUnsubscribe = null;
      }
    },

    // Legacy methods for backwards compatibility
    startPolling(intervalMs = 30000) {
      // Now just starts SSE listening
      this.startListening();
    },

    stopPolling() {
      this.stopListening();
    }
  };
}

export const inbox = createInboxStore();

// Derived stores for convenience
export const inboxItems = derived(inbox, ($inbox) => $inbox.items);
export const unreadCount = derived(inbox, ($inbox) => $inbox.unreadCount);
export const isInboxOpen = derived(inbox, ($inbox) => $inbox.isOpen);
