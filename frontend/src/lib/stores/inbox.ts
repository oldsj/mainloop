/**
 * Inbox store for managing queue items and unread state
 */

import { writable, derived } from 'svelte/store';
import { api, type QueueItem } from '$lib/api';

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

  let pollInterval: ReturnType<typeof setInterval> | null = null;

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
      update((s) => {
        if (!s.isOpen) {
          this.fetchItems();
        }
        return { ...s, isOpen: !s.isOpen };
      });
    },

    startPolling(intervalMs = 30000) {
      this.stopPolling();
      this.fetchUnreadCount();
      pollInterval = setInterval(() => {
        this.fetchUnreadCount();
      }, intervalMs);
    },

    stopPolling() {
      if (pollInterval) {
        clearInterval(pollInterval);
        pollInterval = null;
      }
    }
  };
}

export const inbox = createInboxStore();

// Derived stores for convenience
export const inboxItems = derived(inbox, ($inbox) => $inbox.items);
export const unreadCount = derived(inbox, ($inbox) => $inbox.unreadCount);
export const isInboxOpen = derived(inbox, ($inbox) => $inbox.isOpen);
