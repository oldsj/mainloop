/**
 * Navigation context store for managing inline session navigation.
 *
 * Tracks which context (main thread or session) is currently active,
 * the session picker state, and zoom mode.
 */

import { writable, derived, get } from 'svelte/store';
import { goto } from '$app/navigation';
import { page } from '$app/stores';
import { sessions } from './sessions';

export interface NavigationState {
  /** Current context: 'main' for main thread, or session ID */
  currentContext: 'main' | string;
  /** Whether the quick picker is open */
  pickerOpen: boolean;
  /** Session ID if in zoomed/focus mode, null otherwise */
  zoomedSession: string | null;
}

function createNavigationContext() {
  const { subscribe, set, update } = writable<NavigationState>({
    currentContext: 'main',
    pickerOpen: false,
    zoomedSession: null
  });

  return {
    subscribe,

    /**
     * Switch to main thread context
     */
    switchToMain() {
      update((s) => ({ ...s, currentContext: 'main', pickerOpen: false }));
    },

    /**
     * Switch to a specific session context
     */
    switchToSession(sessionId: string) {
      update((s) => ({ ...s, currentContext: sessionId, pickerOpen: false }));
    },

    /**
     * Toggle the session picker open/closed
     */
    togglePicker() {
      update((s) => ({ ...s, pickerOpen: !s.pickerOpen }));
    },

    /**
     * Open the session picker
     */
    openPicker() {
      update((s) => ({ ...s, pickerOpen: true }));
    },

    /**
     * Close the session picker
     */
    closePicker() {
      update((s) => ({ ...s, pickerOpen: false }));
    },

    /**
     * Enter zoom mode for a session (fullscreen focus view)
     */
    zoomSession(sessionId: string) {
      update((s) => ({
        ...s,
        zoomedSession: sessionId,
        currentContext: sessionId,
        pickerOpen: false
      }));
      goto(`/?zoom=${sessionId}`);
    },

    /**
     * Exit zoom mode and return to normal view
     */
    exitZoom() {
      update((s) => ({ ...s, zoomedSession: null }));
      goto('/');
    },

    /**
     * Initialize from URL query params (for zoom state persistence)
     */
    initFromUrl(searchParams: URLSearchParams) {
      const zoomId = searchParams.get('zoom');
      if (zoomId) {
        update((s) => ({
          ...s,
          zoomedSession: zoomId,
          currentContext: zoomId
        }));
      }
    },

    /**
     * Reset to default state
     */
    reset() {
      set({
        currentContext: 'main',
        pickerOpen: false,
        zoomedSession: null
      });
    }
  };
}

export const navigationContext = createNavigationContext();

/**
 * Derived store: current session object (if in session context)
 */
export const currentSession = derived([navigationContext, sessions], ([$nav, $sessions]) => {
  if ($nav.currentContext === 'main') {
    return null;
  }
  return $sessions.sessions.find((s) => s.id === $nav.currentContext) ?? null;
});

/**
 * Derived store: whether currently in main thread context
 */
export const isMainContext = derived(navigationContext, ($nav) => $nav.currentContext === 'main');

/**
 * Derived store: whether currently in zoom mode
 */
export const isZoomed = derived(navigationContext, ($nav) => $nav.zoomedSession !== null);

/**
 * Urgency score for session ordering in picker.
 * Higher score = more urgent = appears first.
 */
export function getUrgencyScore(status: string): number {
  switch (status) {
    case 'waiting_on_user':
      return 100;
    case 'active':
      return 50;
    case 'implementing':
      return 45;
    case 'pending':
      return 30;
    case 'completed':
      return 10;
    case 'failed':
      return 5;
    case 'cancelled':
      return 1;
    default:
      return 0;
  }
}

/**
 * Derived store: sessions sorted by urgency (for picker)
 */
export const sessionsByUrgency = derived(sessions, ($sessions) =>
  [...$sessions.sessions].sort((a, b) => getUrgencyScore(b.status) - getUrgencyScore(a.status))
);
