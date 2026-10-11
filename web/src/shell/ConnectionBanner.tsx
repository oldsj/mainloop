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

/** Shown while the backend is unreachable. Recovery is automatic: views refetch on reconnect. */
export function ConnectionBanner({ connection = defaultConnection }: { connection?: Connection }) {
  const state = useConnection(connection);
  if (state.status !== 'offline') return null;
  return (
    <div
      role="status"
      data-testid="connection-banner"
      className="gap-sm border-border bg-destructive px-md py-sm text-destructive-foreground flex items-center border-b text-sm"
    >
      <WifiOff className="size-4 shrink-0" aria-hidden="true" />
      <span>
        Can&apos;t reach the Mainloop backend{since(state.offlineSince)}. Reconnecting
        automatically…
      </span>
    </div>
  );
}
