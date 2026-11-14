import { LogIn, WifiOff } from 'lucide-react';
import { useSyncExternalStore } from 'react';
import { connection as defaultConnection, type Connection } from '@/api/connection';
import { signIn as defaultSignIn, type SignIn } from '@/api/signin';

function since(iso: string | null): string {
  if (!iso) return '';
  const date = new Date(iso);
  return ` since ${date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}`;
}

export function useConnection(connection: Connection = defaultConnection) {
  return useSyncExternalStore(connection.subscribe, connection.getSnapshot);
}

export function useSignIn(signIn: SignIn = defaultSignIn) {
  return useSyncExternalStore(signIn.subscribe, signIn.getSnapshot);
}

/**
 * Shown while the backend is unreachable, or once the owner's sign-in has lapsed and a reload did
 * not restore it. It is always one line, so a phone keeps the composer and tab bar on screen; long
 * text is cut off with an ellipsis. Recovery from offline is automatic: views refetch on
 * reconnect. Every failed read reports into the same state, so this never stacks.
 */
export function ConnectionBanner({
  connection = defaultConnection,
  signIn = defaultSignIn
}: {
  connection?: Connection;
  signIn?: SignIn;
}) {
  const state = useConnection(connection);
  const signedIn = useSignIn(signIn);
  if (signedIn === 'expired') {
    return (
      <div
        role="status"
        data-testid="signin-banner"
        className="gap-sm border-border bg-destructive px-md py-sm text-destructive-foreground flex shrink-0 items-center border-b text-sm"
      >
        <LogIn className="size-4 shrink-0" aria-hidden="true" />
        <span className="min-w-0 flex-1 truncate">Signed out</span>
        <button
          type="button"
          onClick={() => signIn.signInAgain()}
          className="min-h-11 shrink-0 font-semibold underline"
        >
          Sign in again
        </button>
      </div>
    );
  }
  if (state.status !== 'offline') return null;
  return (
    <div
      role="status"
      data-testid="connection-banner"
      className="gap-sm border-border bg-destructive px-md py-sm text-destructive-foreground flex shrink-0 items-center border-b text-sm"
    >
      <WifiOff className="size-4 shrink-0" aria-hidden="true" />
      <span className="min-w-0 truncate">Offline{since(state.offlineSince)} · reconnecting…</span>
    </div>
  );
}
