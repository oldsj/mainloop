import { WifiOff } from 'lucide-react';
import { useSyncExternalStore } from 'react';
import { connection as defaultConnection, type Connection } from '@/api/connection';

function since(iso: string | null): string {
  if (!iso) return '';
  const date = new Date(iso);
  return ` since ${date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}`;
}

export function useConnection(connection: Connection = defaultConnection) {
  return useSyncExternalStore(connection.subscribe, connection.getSnapshot);
}

/**
 * Shown while the backend is unreachable. It is always one line, so a phone keeps the composer and
 * tab bar on screen; long text is cut off with an ellipsis. Recovery is automatic: views refetch on
 * reconnect. Every failed read reports into the same state, so this never stacks.
 */
export function ConnectionBanner({ connection = defaultConnection }: { connection?: Connection }) {
  const state = useConnection(connection);
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
