import { useRef, useState } from 'react';
import { ApiError } from '@/api/errors';
import { uuidV4 } from '@/lib/uuid';
import { useConnection } from '@/shell/ConnectionBanner';
import { dotfilesHint, type DotfilesHint } from './model';
import { useRefreshSessionDotfiles, useSessionDotfiles } from './queries';

export interface DotfilesRefreshViewProps {
  hint: DotfilesHint | null;
  /** A refresh is in flight from this page. */
  busy: boolean;
  offline: boolean;
  /** The last refresh from this page applied; shown once the hint is gone. */
  done: string | null;
  /** The last refresh from this page was not accepted. */
  error: string | null;
  onRefresh: () => void;
}

/**
 * "dotfiles changed" with a Refresh button: the session's native Session holds an older commit
 * than its source names now. The refresh keeps the conversation; the next turn reads the new files.
 */
export function DotfilesRefreshView({
  hint,
  busy,
  offline,
  done,
  error,
  onRefresh
}: DotfilesRefreshViewProps) {
  if (!hint && !done && !error) return null;
  return (
    <div
      data-testid="dotfiles-hint"
      className="gap-sm px-md py-xs text-muted-foreground flex flex-wrap items-center font-mono text-xs"
    >
      {hint ? (
        <>
          <span className="text-term-yellow [overflow-wrap:anywhere]" data-testid="dotfiles-change">
            {hint.text}
          </span>
          {hint.compareUrl ? (
            <a
              href={hint.compareUrl}
              target="_blank"
              rel="noopener noreferrer"
              className="hover:text-accent-foreground underline"
            >
              diff
            </a>
          ) : null}
          <button
            type="button"
            className="border-border text-foreground px-md min-h-control border disabled:opacity-50"
            disabled={busy || offline}
            onClick={onRefresh}
            data-testid="dotfiles-refresh"
          >
            {busy ? 'refreshing…' : hint.problem?.pending ? 'Retry refresh' : 'Refresh'}
          </button>
          {hint.problem ? (
            <p
              className={`w-full [overflow-wrap:anywhere] ${hint.problem.pending ? 'text-term-yellow' : 'text-destructive'}`}
              data-testid="dotfiles-problem"
            >
              {hint.problem.text}
            </p>
          ) : null}
        </>
      ) : done ? (
        <p role="status" data-testid="dotfiles-done">
          {done}
        </p>
      ) : null}
      {error ? (
        <p
          className="text-destructive w-full [overflow-wrap:anywhere]"
          role="alert"
          data-testid="dotfiles-error"
        >
          {error}
        </p>
      ) : null}
    </div>
  );
}

/**
 * The dotfiles hint for one session, with its Refresh action. One request id per refresh the owner
 * asked for: a retry after a lost reply or a 502 repeats it, so the server continues that refresh.
 */
export function DotfilesRefresh({ sessionId }: { sessionId: string }) {
  const offline = useConnection().status === 'offline';
  const info = useSessionDotfiles(sessionId);
  const refresh = useRefreshSessionDotfiles(sessionId);
  const request = useRef<string | null>(null);
  const [done, setDone] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const onRefresh = () => {
    setDone(null);
    setError(null);
    request.current ??= uuidV4();
    refresh.mutate(
      { params: { path: { session_id: sessionId } }, body: { request_id: request.current } },
      {
        onSuccess: (result) => {
          request.current = null;
          if (result.state === 'applied') {
            const commit = result.dotfiles?.commit;
            setDone(
              `dotfiles refreshed${commit ? ` to ${commit.slice(0, 7)}` : ''}; the next turn reads them`
            );
          }
        },
        // The API client throws an ApiError carrying the server's detail. A 409 says why a refresh
        // does not apply to the session, so repeating it would not help.
        onError: (failure: unknown) => {
          const why = failure instanceof Error ? failure.message : 'Could not refresh the dotfiles';
          const settled = failure instanceof ApiError && failure.status === 409;
          setError(`${why.replace(/\.?$/, '.')}${settled ? '' : ' Try again.'}`);
        }
      }
    );
  };

  return (
    <DotfilesRefreshView
      hint={info.data ? dotfilesHint(info.data) : null}
      busy={refresh.isPending}
      offline={offline}
      done={done}
      error={error}
      onRefresh={onRefresh}
    />
  );
}
