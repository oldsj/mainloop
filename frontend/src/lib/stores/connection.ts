/**
 * Backend reachability, so a dead service reads as "down" instead of as empty or half-working UI.
 *
 * Two inputs: a periodic /health probe, and failures reported by the API client (a request that
 * never got an HTTP response flips the state immediately rather than waiting for the next probe).
 */

import { writable } from 'svelte/store';
import { API_URL } from '$lib/config';

export type ConnectionStatus = 'checking' | 'online' | 'offline';

interface ConnectionState {
  status: ConnectionStatus;
  /** When the backend was first seen unreachable in the current outage (ISO string). */
  offlineSince: string | null;
  /** Incremented each time the backend comes back, so views can reload what they missed. */
  recoveries: number;
}

const ONLINE_PROBE_MS = 5000;
const OFFLINE_PROBE_MS = 2000;
const PROBE_TIMEOUT_MS = 4000;

function createConnectionStore() {
  const { subscribe, update } = writable<ConnectionState>({
    status: 'checking',
    offlineSince: null,
    recoveries: 0
  });

  let timer: ReturnType<typeof setTimeout> | null = null;
  let running = false;
  let current: ConnectionStatus = 'checking';

  function setStatus(next: ConnectionStatus) {
    const previous = current;
    current = next;
    update((s) => ({
      status: next,
      offlineSince: next === 'offline' ? (s.offlineSince ?? new Date().toISOString()) : null,
      recoveries: next === 'online' && previous === 'offline' ? s.recoveries + 1 : s.recoveries
    }));
  }

  async function probe() {
    try {
      const response = await fetch(`${API_URL}/health`, {
        signal: AbortSignal.timeout(PROBE_TIMEOUT_MS),
        cache: 'no-store'
      });
      // /health never fails while the backend is up; a proxy (dev server, ingress) answers 5xx
      // when nothing is behind it.
      setStatus(response.ok ? 'online' : 'offline');
    } catch {
      setStatus('offline');
    }
  }

  function schedule() {
    if (!running) return;
    timer = setTimeout(
      async () => {
        await probe();
        schedule();
      },
      current === 'offline' ? OFFLINE_PROBE_MS : ONLINE_PROBE_MS
    );
  }

  return {
    subscribe,

    /** Begin probing; returns a stop function. */
    start(): () => void {
      if (running) return () => {};
      running = true;
      void probe().then(schedule);
      const recheck = () => void probe();
      const goOffline = () => setStatus('offline');
      window.addEventListener('online', recheck);
      window.addEventListener('offline', goOffline);
      return () => {
        running = false;
        if (timer) clearTimeout(timer);
        window.removeEventListener('online', recheck);
        window.removeEventListener('offline', goOffline);
      };
    },

    /**
     * The API client saw a request fail without any HTTP response. Only failures are reported;
     * recovery is decided by the probe, since a proxy can answer HTTP while the backend is down.
     */
    reportFailure() {
      if (current !== 'offline') setStatus('offline');
    }
  };
}

export const connection = createConnectionStore();
