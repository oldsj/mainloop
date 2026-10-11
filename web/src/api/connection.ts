import { API_URL } from './config';

/**
 * Backend reachability, so a dead service reads as "down" instead of as empty or half-working UI.
 *
 * Two inputs: a periodic /health probe, and failures the API client reports (a request that never
 * got an HTTP response flips the state at once instead of waiting for the next probe). Recovery is
 * decided by the probe alone, because a proxy can answer HTTP while the backend is down.
 */
export type ConnectionStatus = 'checking' | 'online' | 'offline';

export interface ConnectionState {
  status: ConnectionStatus;
  /** When the backend was first seen unreachable in the current outage (ISO string). */
  offlineSince: string | null;
  /** Incremented each time the backend comes back, so views can refetch what they missed. */
  recoveries: number;
}

export interface ConnectionOptions {
  probe?: () => Promise<boolean>;
  now?: () => Date;
  onlineProbeMs?: number;
  offlineProbeMs?: number;
}

const PROBE_TIMEOUT_MS = 4000;

async function httpProbe(): Promise<boolean> {
  try {
    const response = await fetch(`${API_URL}/health`, {
      signal: AbortSignal.timeout(PROBE_TIMEOUT_MS),
      cache: 'no-store'
    });
    // /health never fails while the backend is up; a proxy answers 5xx when nothing is behind it.
    return response.ok;
  } catch {
    return false;
  }
}

export function createConnection(options: ConnectionOptions = {}) {
  const probe = options.probe ?? httpProbe;
  const now = options.now ?? (() => new Date());
  const onlineProbeMs = options.onlineProbeMs ?? 5000;
  const offlineProbeMs = options.offlineProbeMs ?? 2000;

  let state: ConnectionState = { status: 'checking', offlineSince: null, recoveries: 0 };
  const listeners = new Set<() => void>();
  let timer: ReturnType<typeof setTimeout> | undefined;
  let running = false;

  function setStatus(next: ConnectionStatus) {
    const previous = state.status;
    if (next === previous) return;
    state = {
      status: next,
      offlineSince: next === 'offline' ? (state.offlineSince ?? now().toISOString()) : null,
      recoveries:
        next === 'online' && previous === 'offline' ? state.recoveries + 1 : state.recoveries
    };
    for (const listener of listeners) listener();
  }

  async function check() {
    setStatus((await probe()) ? 'online' : 'offline');
  }

  function schedule() {
    if (!running) return;
    timer = setTimeout(
      () => {
        void check().then(schedule);
      },
      state.status === 'offline' ? offlineProbeMs : onlineProbeMs
    );
  }

  return {
    /** For useSyncExternalStore. */
    subscribe(listener: () => void) {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
    getSnapshot: () => state,

    /** Begin probing; returns a stop function. Safe to call twice. */
    start(): () => void {
      if (running) return () => {};
      running = true;
      void check().then(schedule);
      const recheck = () => void check();
      const goOffline = () => setStatus('offline');
      window.addEventListener('online', recheck);
      window.addEventListener('offline', goOffline);
      return () => {
        running = false;
        clearTimeout(timer);
        window.removeEventListener('online', recheck);
        window.removeEventListener('offline', goOffline);
      };
    },

    /** The API client saw a request fail without any HTTP response. */
    reportFailure() {
      setStatus('offline');
    },

    check
  };
}

export type Connection = ReturnType<typeof createConnection>;
export const connection = createConnection();
