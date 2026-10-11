import { Link } from '@tanstack/react-router';
import { useState } from 'react';
import { ApiError } from '@/api';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { Button, buttonVariants } from '@/components/ui/button';
import { cn } from '@/components/ui/cn';
import { SessionRow } from './SessionRow';
import { sessionCounts, visibleSessions, workspaceFor } from './model';
import { useAttention, useProviders, useWorkspaces } from './queries';

/** The sessions list. Rows come from attention, so a status change there reaches the list too. */
export function SessionsPanel() {
  const attention = useAttention();
  const providers = useProviders();
  const workspaces = useWorkspaces();
  const [showFinished, setShowFinished] = useState(false);
  const [showIdle, setShowIdle] = useState(false);

  const sessions = attention.data?.sessions ?? [];
  const counts = sessionCounts(sessions);
  const rows = visibleSessions(sessions, { showFinished, showIdle });

  return (
    <section aria-labelledby="sessions-heading" className="gap-sm p-md flex flex-col">
      <div className="gap-sm flex items-center justify-between">
        <h2 id="sessions-heading" className="text-sm font-medium">
          Sessions
        </h2>
        <span className="text-muted-foreground text-xs">{counts.current} current</span>
        <div className="gap-xs flex items-center">
          <Button
            variant="ghost"
            size="sm"
            disabled={attention.isFetching}
            onClick={() => void attention.refetch()}
          >
            Refresh
          </Button>
          <Link
            to="/agents"
            search={(prev) => ({ ...prev, tab: undefined })}
            data-testid="new-agent-link"
            className={cn(buttonVariants({ variant: 'outline', size: 'sm' }))}
          >
            + agent
          </Link>
        </div>
      </div>

      <div role="group" aria-label="Session filters" className="gap-xs flex flex-wrap">
        <Button
          variant="outline"
          size="sm"
          data-testid="show-finished"
          onClick={() => setShowFinished((value) => !value)}
        >
          {showFinished ? 'Hide' : 'Show'} finished ({counts.finished})
        </Button>
        <Button
          variant="outline"
          size="sm"
          data-testid="show-idle"
          onClick={() => setShowIdle((value) => !value)}
        >
          {showIdle ? 'Hide' : 'Show'} idle ({counts.idle})
        </Button>
      </div>

      {attention.isError && !attention.data ? (
        <div data-testid="sessions-error">
          <ErrorRecovery
            title="Could not load sessions"
            message={
              attention.error instanceof ApiError
                ? attention.error.message
                : "Can't reach the Mainloop backend."
            }
            onRetry={() => void attention.refetch()}
            pending={attention.isFetching}
          />
        </div>
      ) : attention.isPending ? (
        <p role="status" className="text-muted-foreground text-sm">
          Loading sessions…
        </p>
      ) : rows.length === 0 ? (
        <div className="text-sm">
          <p className="font-medium">No active sessions</p>
          <p className="text-muted-foreground">
            Start one with “+ agent”, or delegate work to create one.
          </p>
        </div>
      ) : (
        <ul aria-label="Session list" className="gap-xs flex flex-col">
          {rows.map((session) => (
            <li key={session.id}>
              <SessionRow
                session={session}
                workspace={workspaceFor(workspaces.data ?? [], session.id)}
                providers={providers.data ?? []}
              />
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
