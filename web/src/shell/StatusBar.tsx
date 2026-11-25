import { Link } from '@tanstack/react-router';
import { cn } from '@/components/ui/cn';
import { signIn } from '@/api/signin';
import { useConnection, useSignIn } from './ConnectionBanner';

export interface StatusBarProps {
  /** How many items need the owner. */
  needs: number;
  /** How many tasks are working. */
  working: number;
  /** How many problems Mainloop is handling. */
  problems: number;
  /** Shows the sidebar, where the working agents are. */
  onShowWorking: () => void;
  /**
   * Whether the counts are known. Before the first read they are not zero, they are unknown, so the
   * bar says it is loading; when that read failed it says the counts are unavailable.
   */
  counts?: 'ready' | 'loading' | 'failed';
  /** The prefix keys, shown while the prefix waits for its key. */
  hint?: string;
}

function since(iso: string | null): string {
  if (!iso) return '';
  return ` since ${new Date(iso).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}`;
}

const plural = (count: number, one: string, many: string) => `${count} ${count === 1 ? one : many}`;

/**
 * Desktop: one 24 px line along the bottom. The connection, which turns the whole bar red while the
 * backend is unreachable (so nothing moves, as a banner at the top would make it), then what needs
 * the owner, what is working and what Mainloop is handling. Each count leads to its list.
 */
export function StatusBar({
  needs,
  working,
  problems,
  onShowWorking,
  counts = 'ready',
  hint
}: StatusBarProps) {
  const connection = useConnection();
  const signedOut = useSignIn() === 'expired';
  const offline = signedOut || connection.status === 'offline';
  const item = cn(
    'inline-flex h-6 items-center whitespace-nowrap hover:underline',
    offline ? 'text-destructive-foreground' : 'hover:text-foreground'
  );
  return (
    <footer
      aria-label="Status"
      data-testid="status-bar"
      data-offline={offline ? 'true' : undefined}
      className={cn(
        'border-border-subtle gap-md px-sm text-2xs flex h-6 shrink-0 items-center border-t',
        offline ? 'bg-destructive text-destructive-foreground' : 'bg-card text-muted-foreground'
      )}
    >
      {signedOut ? (
        <button
          type="button"
          onClick={() => signIn.signInAgain()}
          className={cn(item, 'gap-xs min-w-0')}
          data-testid="status-signin"
        >
          <span aria-hidden="true" className="bg-destructive-foreground size-2 shrink-0" />
          <span role="status" className="min-w-0 truncate">
            Signed out · Sign in again
          </span>
        </button>
      ) : (
        <Link to="/health" className={cn(item, 'gap-xs min-w-0')} data-testid="status-connection">
          <span
            aria-hidden="true"
            className={cn(
              'size-2 shrink-0',
              offline ? 'bg-destructive-foreground' : 'bg-term-green'
            )}
          />
          {/* A live region, so going offline is announced without moving focus. */}
          <span role="status" className="min-w-0 truncate">
            {offline ? `Offline${since(connection.offlineSince)} · reconnecting…` : 'Connected'}
          </span>
        </Link>
      )}
      {/* Announced when it appears, so the keys are heard as well as seen. */}
      <span
        aria-live="polite"
        data-testid="status-keys"
        className="text-foreground min-w-0 flex-1 truncate"
      >
        {hint}
      </span>
      {counts === 'ready' ? (
        <>
          <Link to="/inbox" className={item} data-testid="status-needs">
            {needs > 0 ? plural(needs, 'needs you', 'need you') : 'Nothing needs you'}
          </Link>
          <button
            type="button"
            onClick={onShowWorking}
            className={item}
            data-testid="status-working"
          >
            {working} working
          </button>
          <Link
            to="/inbox"
            search={{ show: 'problems' }}
            className={item}
            data-testid="status-problems"
          >
            {plural(problems, 'problem', 'problems')}
          </Link>
        </>
      ) : (
        // Not "Nothing needs you": until the first read answers, that is not known.
        <span data-testid="status-counts-pending" className="whitespace-nowrap">
          {counts === 'loading' ? 'Loading…' : 'Counts unavailable'}
        </span>
      )}
    </footer>
  );
}
