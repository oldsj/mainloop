import { API_URL } from './config';
import { API_SAME_ORIGIN, CLIENT_HEADER, CLIENT_NAME, signIn } from './signin';

/**
 * Backend reachability, so a dead service reads as "down" instead of as empty or half-working UI.
 *
 * Two inputs: a periodic /health probe, and failures the API client reports. A request that never
 * got an HTTP response makes the probe run at once, instead of waiting for the next one, and the
 * probe's answer decides: one dropped request while /health still answers is not an outage, and
 * calling it one would disable every action and re-read everything on the "recovery". The browser
 * saying it has no network is taken at its word. Recovery is decided by the probe alone, because
 * a proxy can answer HTTP while the backend is down.
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
      cache: 'no-store',
      headers: API_SAME_ORIGIN ? { [CLIENT_HEADER]: CLIENT_NAME } : undefined
    });
    // The backend serves /health to anyone, so a 401 is the gateway's: it is up, the sign-in lapsed.
    if (response.status === 401) {
      signIn.expired();
      return true;
    }
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
  // Bumped by every start and stop, so a probe or timer left over from an earlier run (React
  // StrictMode mounts, unmounts and mounts again) cannot keep a second loop going.
  let generation = 0;
  // A probe asked for by a reported failure is in flight; further reports ride on it.
  let verifying = false;

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

  function probeLoop(run: number) {
    void check().then(() => schedule(run));
  }

  function schedule(run: number) {
    if (!running || run !== generation) return;
    timer = setTimeout(
      () => {
        if (run === generation) probeLoop(run);
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
      const run = ++generation;
      probeLoop(run);
      const recheck = () => void check();
      const goOffline = () => setStatus('offline');
      window.addEventListener('online', recheck);
      window.addEventListener('offline', goOffline);
      return () => {
        // A stop function from an earlier run must not stop the current one.
        if (run !== generation) return;
        running = false;
        generation++;
        clearTimeout(timer);
        window.removeEventListener('online', recheck);
        window.removeEventListener('offline', goOffline);
      };
    },

    /**
     * The API client saw a request fail without any HTTP response. That is a reason to look, not
     * a verdict: the probe runs now and its answer sets the state.
     */
    reportFailure() {
      if (state.status === 'offline' || verifying) return;
      verifying = true;
      void check().finally(() => {
        verifying = false;
      });
    },

    check
  };
}

export type Connection = ReturnType<typeof createConnection>;
export const connection = createConnection();
