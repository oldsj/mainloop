/**
 * Inbox store for managing queue items and unread state
 *
 * Uses SSE for real-time updates instead of polling.
 */

import { writable, derived } from 'svelte/store';
import { api, type QueueItem } from '$lib/api';
import { getSSEClient, type SSEEvent } from '$lib/sse';

interface InboxState {
  items: QueueItem[];
  unreadCount: number;
  isOpen: boolean;
  isLoading: boolean;
  error: string | null;
}

const initialState: InboxState = {
  items: [],
  unreadCount: 0,
  isOpen: false,
  isLoading: false,
  error: null
};

function createInboxStore() {
  const { subscribe, set, update } = writable<InboxState>(initialState);

  let sseUnsubscribe: (() => void) | null = null;

  return {
    subscribe,

    async fetchItems() {
      update((s) => ({ ...s, isLoading: true, error: null }));
      try {
        const items = await api.listQueueItems({ status: 'pending' });
        update((s) => ({ ...s, items, isLoading: false }));
      } catch (e) {
        update((s) => ({ ...s, error: 'Failed to load inbox', isLoading: false }));
      }
    },

    async fetchUnreadCount() {
      try {
        const unreadCount = await api.getUnreadCount();
        update((s) => ({ ...s, unreadCount }));
      } catch (e) {
        console.error('Failed to fetch unread count:', e);
      }
    },

    async markRead(itemId: string) {
      try {
        await api.markQueueItemRead(itemId);
        update((s) => ({
          ...s,
          items: s.items.map((item) =>
            item.id === itemId ? { ...item, read_at: new Date().toISOString() } : item
          ),
          unreadCount: Math.max(0, s.unreadCount - 1)
        }));
      } catch (e) {
        console.error('Failed to mark item read:', e);
      }
    },

    async markAllRead() {
      try {
        await api.markAllQueueItemsRead();
        update((s) => ({
          ...s,
          items: s.items.map((item) => ({ ...item, read_at: new Date().toISOString() })),
          unreadCount: 0
        }));
      } catch (e) {
        console.error('Failed to mark all read:', e);
      }
    },

    async respond(itemId: string, response: string) {
      try {
        await api.respondToQueueItem(itemId, response);
        update((s) => ({
          ...s,
          items: s.items.map((item) =>
            item.id === itemId
              ? { ...item, status: 'responded', response, responded_at: new Date().toISOString() }
              : item
          )
        }));
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

      const client = getSSEClient();

      // Handle inbox updates from SSE
      sseUnsubscribe = client.on('inbox:updated', (event: SSEEvent) => {
        const { unread_count, item_id } = event.data as {
          unread_count?: number;
          item_id?: string;
        };

        // Update unread count if provided
        if (unread_count !== undefined) {
          update((s) => ({ ...s, unreadCount: unread_count }));
        }

        // If a specific item was updated, refetch the items list
        if (item_id) {
          this.fetchItems();
        }
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
