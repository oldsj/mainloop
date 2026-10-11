import { Link, useNavigate } from '@tanstack/react-router';
import { ArrowLeft } from 'lucide-react';
import { useState } from 'react';
import { ConfirmDialog } from '@/components/shared/confirm-dialog';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { StatusBadge } from '@/components/shared/status-badge';
import { Button } from '@/components/ui/button';
import type { SessionStatus } from '@/lib/contract';
import { isFinished, statusLabel } from '@/lib/sessionStatus';
import { taskForSession } from '@/lib/taskState';
import { zoomSession } from '@/lib/zoomSession';
import { useConnection } from '@/shell/ConnectionBanner';
import { IdentityStrip } from './IdentityStrip';
import {
  actionFailure,
  cancelNotice,
  loadFailure,
  sessionTone,
  stopNotice,
  workspaceFor,
  type LoadFailure,
  type Notice
} from './model';
import {
  useAttention,
  useSessionActions,
  useSessionDetail,
  useTasks,
  useWorkspaces
} from './queries';
import { SessionChatSlot } from './SessionChatSlot';
import { WorkspaceLifecycleBadge, WorkspacePublicationBadge } from './WorkspaceBadges';

/** A session can be cancelled while it is live. Finished sessions cannot be cancelled again. */
const CANCELLABLE: SessionStatus[] = ['active', 'pending', 'idle', 'waiting_on_user'];

/**
 * One session: its status, identity, actions, workspace and the chat slot. The session comes from
 * attention when it is listed there, and from a direct read otherwise, so a session that is not
 * in the list still opens. Each action re-reads the session once it settles (see `useSessionActions`).
 */
export function SessionPage({ id }: { id: string }) {
  const navigate = useNavigate();
  const offline = useConnection().status === 'offline';
  const attention = useAttention();
  const detail = useSessionDetail(id);
  const workspaces = useWorkspaces();
  const tasks = useTasks();
  const actions = useSessionActions(id);
  const [notice, setNotice] = useState<Notice | null>(null);
  const [confirmCancel, setConfirmCancel] = useState(false);

  const session = zoomSession(
    id,
    null,
    attention.data?.sessions ?? [],
    !detail.isPending,
    detail.data ?? null
  );

  if (!session) {
    if (detail.isPending) {
      return (
        <p role="status" className="p-md text-muted-foreground text-sm">
          Loading session...
        </p>
      );
    }
    return (
      <SessionFailure
        failure={loadFailure(detail.error)}
        error={detail.error}
        retrying={detail.isFetching}
        onRetry={() => void detail.refetch()}
      />
    );
  }

  const finished = isFinished(session);
  const archived = Boolean(session.archived_at);
  const workspace = workspaceFor(workspaces.data ?? [], session.id);
  const task = taskForSession(tasks.data ?? [], session.id);
  const canCancel = CANCELLABLE.includes(session.status);
  const canArchive = finished && !archived;

  function runStop() {
    setNotice(null);
    actions.stopTurn.mutate(
      { params: { path: { session_id: id } } },
      {
        onSuccess: (result) => setNotice(stopNotice(result)),
        onError: (error) => setNotice(actionFailure(error))
      }
    );
  }

  function runCancel() {
    setNotice(null);
    actions.cancel.mutate(
      { params: { path: { session_id: id } } },
      {
        onSuccess: (result) => setNotice(cancelNotice(result)),
        onError: (error) => setNotice(actionFailure(error)),
        onSettled: () => setConfirmCancel(false)
      }
    );
  }

  function runArchive() {
    setNotice(null);
    actions.archive.mutate(
      { params: { path: { session_id: id } } },
      {
        onSuccess: () => {
          void navigate({ to: '/', search: (prev) => ({ ...prev, tab: undefined }) });
        },
        onError: (error) => setNotice(actionFailure(error))
      }
    );
  }

  return (
    <article className="flex min-h-0 flex-col">
      <header className="gap-sm border-border p-md flex flex-col border-b">
        <div className="gap-sm flex items-center">
          <Button asChild variant="ghost" size="icon">
            <Link to="/" search={(prev) => ({ ...prev, tab: undefined })} aria-label="Back">
              <ArrowLeft aria-hidden="true" />
            </Link>
          </Button>
          <span data-testid="session-status">
            <StatusBadge
              label={`[${statusLabel(session.status, session.needs_owner)}]`}
              tone={sessionTone(session)}
            />
          </span>
        </div>
        <h1 className="text-lg font-medium break-words">{session.title}</h1>
        {session.description ? (
          <p className="text-muted-foreground text-sm break-words">{session.description}</p>
        ) : null}
      </header>

      <IdentityStrip session={session} />

      {/* Stop turn is offered on every session page, independently of Cancel (see the spec). */}
      <section aria-label="Session actions" className="gap-sm p-md flex flex-wrap items-center">
        <Button
          variant="outline"
          data-testid="session-stop-turn"
          onClick={runStop}
          disabled={offline || actions.stopTurn.isPending}
        >
          {actions.stopTurn.isPending ? 'Stopping…' : 'Stop turn'}
        </Button>
        {canCancel ? (
          <Button
            variant="destructive"
            data-testid="session-cancel"
            onClick={() => setConfirmCancel(true)}
            disabled={offline || actions.cancel.isPending}
          >
            Cancel session
          </Button>
        ) : null}
        {canArchive ? (
          <Button
            variant="outline"
            data-testid="archive-session"
            onClick={runArchive}
            disabled={offline || actions.archive.isPending}
          >
            {actions.archive.isPending ? 'Clearing…' : 'Clear'}
          </Button>
        ) : null}
      </section>

      {notice ? (
        <p
          data-testid="session-action-notice"
          role="alert"
          className={`px-md text-sm ${notice.tone === 'warn' ? 'text-term-yellow' : 'text-term-green'}`}
        >
          {notice.text}
        </p>
      ) : null}

      {task ? (
        <p className="px-md pt-sm text-sm">
          Task{' '}
          <Link
            to="/tasks/$id"
            params={{ id: task.view.task.id }}
            data-testid="session-task-link"
            className="text-foreground underline"
          >
            {task.view.task.title}
          </Link>
        </p>
      ) : null}

      {workspace ? (
        <section
          aria-label="Workspace"
          className="gap-sm p-md border-border flex flex-col border-t"
        >
          <div className="gap-sm flex flex-wrap items-center">
            <WorkspaceLifecycleBadge workspace={workspace} />
            <WorkspacePublicationBadge workspace={workspace} />
          </div>
          <div>
            <Button asChild variant="outline" size="sm">
              <Link to="/workspaces/$id" params={{ id: workspace.workspace_id }}>
                Manage workspace
              </Link>
            </Button>
          </div>
        </section>
      ) : null}

      {session.status === 'completed' && session.summary ? (
        <section className="gap-xs p-md flex flex-col">
          <h3 className="text-sm font-medium">Summary</h3>
          <p className="text-sm break-words whitespace-pre-wrap">{session.summary}</p>
        </section>
      ) : null}

      {session.status === 'failed' && session.error ? (
        <section className="gap-xs p-md flex flex-col">
          <h3 className="text-sm font-medium">Error</h3>
          <p className="text-destructive text-sm break-words">{session.error}</p>
        </section>
      ) : null}

      <div className="p-md">
        <SessionChatSlot sessionId={session.id} />
      </div>

      <footer className="px-md pb-md">
        <Link
          to="/health"
          search={{ session_id: session.id }}
          className="text-muted-foreground text-sm underline"
        >
          Orchestration health
        </Link>
      </footer>

      <ConfirmDialog
        open={confirmCancel}
        onOpenChange={setConfirmCancel}
        title="Cancel this session?"
        description="Its agent is stopped."
        confirmLabel="Yes, cancel"
        cancelLabel="Keep it running"
        destructive
        pending={actions.cancel.isPending}
        onConfirm={runCancel}
      />
    </article>
  );
}

function SessionFailure({
  failure,
  error,
  retrying,
  onRetry
}: {
  failure: LoadFailure;
  error: unknown;
  retrying: boolean;
  onRetry: () => void;
}) {
  if (failure === 'not-found') {
    return (
      <section data-testid="session-not-found" className="gap-md p-md flex flex-col">
        <h1 className="text-lg font-medium">Session not found</h1>
        <p className="text-muted-foreground text-sm">
          It doesn&apos;t exist, or you don&apos;t have access to it.
        </p>
        <div>
          <Button asChild variant="outline">
            <Link to="/" search={(prev) => ({ ...prev, tab: undefined })}>
              Back to home
            </Link>
          </Button>
        </div>
      </section>
    );
  }
  if (failure === 'server') {
    return (
      <div className="p-md">
        <ErrorRecovery
          title="Server error"
          message="Mainloop returned a server error. Reload to try again."
        />
      </div>
    );
  }
  if (failure === 'unreachable') {
    return (
      <div className="p-md">
        <ErrorRecovery
          title="Can't reach the Mainloop backend."
          message="Check the connection, then retry."
          onRetry={onRetry}
          pending={retrying}
        />
      </div>
    );
  }
  return (
    <div className="p-md">
      <ErrorRecovery
        title="Could not load session"
        message={error instanceof Error ? error.message : 'Unknown error.'}
        onRetry={onRetry}
        pending={retrying}
      />
    </div>
  );
}
