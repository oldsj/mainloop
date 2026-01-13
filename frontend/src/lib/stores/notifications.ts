/**
 * Notifications store for managing ephemeral session notifications
 */

import { writable, derived } from 'svelte/store';
import { api, type SessionNotification } from '$lib/api';

interface NotificationsState {
  notifications: SessionNotification[];
  loading: boolean;
}

function createNotificationsStore() {
  const { subscribe, set, update } = writable<NotificationsState>({
    notifications: [],
    loading: false
  });

  return {
    subscribe,

    async fetchNotifications(unreadOnly: boolean = true) {
      update((state) => ({ ...state, loading: true }));
      try {
        const notifications = await api.listNotifications(unreadOnly);
        update((state) => ({ ...state, notifications, loading: false }));
      } catch (e) {
        console.error('Failed to fetch notifications:', e);
        update((state) => ({ ...state, loading: false }));
      }
    },

    async dismissNotification(notificationId: string) {
      try {
        await api.dismissNotification(notificationId);
        update((state) => ({
          ...state,
          notifications: state.notifications.filter((n) => n.id !== notificationId)
        }));
      } catch (e) {
        console.error('Failed to dismiss notification:', e);
      }
    },

    addNotification(notification: SessionNotification) {
      update((state) => ({
        ...state,
        notifications: [notification, ...state.notifications]
      }));
    },

    reset() {
      set({ notifications: [], loading: false });
    }
  };
}

export const notifications = createNotificationsStore();

export const unreadNotificationCount = derived(
  notifications,
  ($notifications) => $notifications.notifications.filter((n) => !n.read).length
);
