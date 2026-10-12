import { useState } from 'react';
import { ApiError } from '@/api';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { Skeleton } from '@/components/shared/skeleton';
import { Button } from '@/components/ui/button';
import { useTaskList } from '@/features/tasks';
import { taskForSession } from '@/lib/taskState';
import { SessionRow } from './SessionRow';
import { sessionCounts, visibleSessions, workspaceFor } from './model';
import { useAttention, useProviders, useWorkspaces } from './queries';

/** Three placeholder rows with a session row's padding, minimum height and line heights. */
function SessionRowsSkeleton() {
  return (
    <ul aria-hidden="true" data-testid="session-rows-skeleton" className="gap-xs flex flex-col">
      {[0, 1, 2].map((row) => (
        <li key={row} className="gap-xs border-border p-md flex min-h-11 flex-col border">
          <Skeleton className="h-5 w-3/5" />
          <Skeleton className="h-4 w-2/5" />
        </li>
      ))}
    </ul>
  );
}

/** The sessions list. Rows come from attention, so a status change there reaches the list too. */
export function SessionsPanel() {
  const attention = useAttention();
  const providers = useProviders();
  const workspaces = useWorkspaces();
  const tasks = useTaskList();
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
        </div>
      </div>

      <div role="group" aria-label="Session filters" className="gap-xs flex flex-wrap">
        <Button
          variant="outline"
          size="sm"
          data-testid="show-finished"
          aria-pressed={showFinished}
          onClick={() => setShowFinished((value) => !value)}
        >
          Finished ({counts.finished})
        </Button>
        <Button
          variant="outline"
          size="sm"
          data-testid="show-idle"
          aria-pressed={showIdle}
          onClick={() => setShowIdle((value) => !value)}
        >
          Idle ({counts.idle})
        </Button>
      </div>

      {attention.isError && attention.data ? (
        <div
          data-testid="sessions-refresh-error"
          className="gap-sm text-muted-foreground flex flex-wrap items-center text-xs"
        >
          <span>Could not refresh sessions. Showing the last list.</span>
          <Button
            variant="outline"
            size="sm"
            disabled={attention.isFetching}
            onClick={() => void attention.refetch()}
          >
            Retry loading sessions
          </Button>
        </div>
      ) : null}

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
        <div className="gap-xs flex flex-col">
          <p role="status" className="text-muted-foreground text-sm">
            Loading sessions…
          </p>
          <SessionRowsSkeleton />
        </div>
      ) : rows.length === 0 ? (
        <div className="text-sm">
          <p className="font-medium">No active sessions</p>
          <p className="text-muted-foreground">Mainloop starts one when you ask it in chat.</p>
        </div>
      ) : (
        <ul aria-label="Session list" className="gap-xs flex flex-col">
          {rows.map((session) => (
            <li key={session.id}>
              <SessionRow
                session={session}
                workspace={workspaceFor(workspaces.data ?? [], session.id)}
                providers={providers.data ?? []}
                taskTitle={taskForSession(tasks.data ?? [], session.id)?.view.task.title}
              />
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
