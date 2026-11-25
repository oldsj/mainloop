import { inNativeShell, sessionExpiredInNativeShell } from '@/lib/nativeShell';
import { API_URL } from './config';

/**
 * Sign-in state for the owner API (docs/specs/authentication.md).
 *
 * The app does no login of its own: the gateway in front of it signs the owner in with Pocket ID and
 * the backend verifies the result on every request. When that sign-in lapses the API answers 401.
 * The app then reloads the page once, a top-level navigation the gateway can redirect through the
 * login and back. If a 401 comes again within a minute of that reload, the app stops reloading,
 * reports `expired`, and the owner signs in again with a button. That way a backend that refuses a
 * sign-in the gateway accepts cannot send the page into a reload loop.
 *
 * In the iPhone app there is no gateway sign-in: the app's web session is a cookie the app sets. A
 * "reload" there asks the app instead, which starts a new session and reloads the page itself.
 */

/** Sent on every owner write, and on reads when the API shares the app's origin. */
export const CLIENT_HEADER = 'X-Mainloop-Client';
export const CLIENT_NAME = 'web';

/** The gateway's sign-out path, on the app's own origin. */
export const SIGN_OUT_PATH = '/oauth2/logout';

/**
 * True when the API is served from the app's own origin (a relative base URL). Only then does the
 * gateway's sign-in cover it, so only then is there a sign-out to offer and a header to add to reads
 * (cross-origin, the header would cost a CORS preflight on every read).
 */
export const API_SAME_ORIGIN = API_URL.startsWith('/');

export const RELOAD_COOLDOWN_MS = 60_000;
const RELOAD_KEY = 'mainloop.signin-reload-at';

export type SignInStatus = 'signed-in' | 'expired';

export interface SignInOptions {
  now?: () => number;
  reload?: () => void;
  /** Where the last reload's time is kept; it must survive the reload (sessionStorage). */
  storage?: Pick<Storage, 'getItem' | 'setItem'> | null;
}

function sessionStore(): Pick<Storage, 'getItem' | 'setItem'> | null {
  try {
    return window.sessionStorage;
  } catch {
    return null;
  }
}

export function createSignIn(options: SignInOptions = {}) {
  const now = options.now ?? (() => Date.now());
  const reload =
    options.reload ??
    (() => (inNativeShell() ? sessionExpiredInNativeShell() : window.location.reload()));
  const storage = options.storage === undefined ? sessionStore() : options.storage;
  let status: SignInStatus = 'signed-in';
  const listeners = new Set<() => void>();

  const lastReload = () => {
    try {
      return Number(storage?.getItem(RELOAD_KEY)) || 0;
    } catch {
      return 0;
    }
  };
  const reloadNow = () => {
    try {
      storage?.setItem(RELOAD_KEY, String(now()));
    } catch {
      // Without storage the cooldown cannot hold across the reload; the button path still works.
    }
    reload();
  };
  const setStatus = (next: SignInStatus) => {
    if (next === status) return;
    status = next;
    for (const listener of listeners) listener();
  };

  return {
    subscribe(listener: () => void) {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
    getSnapshot: () => status,

    /** The API answered 401: the sign-in lapsed. */
    expired() {
      if (status === 'expired') return;
      // Without storage a reload could repeat forever, so go straight to the button.
      if (storage && now() - lastReload() > RELOAD_COOLDOWN_MS) {
        reloadNow();
        return;
      }
      setStatus('expired');
    },

    /** The owner asked to sign in again. */
    signInAgain() {
      reloadNow();
    }
  };
}

export type SignIn = ReturnType<typeof createSignIn>;
export const signIn = createSignIn();
