import { Link, useNavigate } from '@tanstack/react-router';
import { useState, type ReactNode } from 'react';
import { ConfirmDialog } from '@/components/shared/confirm-dialog';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { Button } from '@/components/ui/button';
import { cn } from '@/components/ui/cn';
import {
  WorkspaceLifecycleBadge,
  WorkspacePublicationBadge
} from '@/features/sessions/WorkspaceBadges';
import { loadFailure, type Notice } from '@/features/sessions/model';
import type { TaskView } from '@/lib/contract';
import { statusLabel, taskForWorkspace } from '@/lib/taskState';
import { PanelBoundary } from '@/shell/PanelBoundary';
import {
  canResume,
  canSuspend,
  isWorkspace,
  loadMessage,
  stateHint,
  UNREADABLE,
  type WorkspaceLifecycle
} from './model';
import { useTasks, useWorkspace, useWorkspaceActions } from './queries';
import { ShortId } from './ShortId';
import { WorkspaceManifest } from './WorkspaceManifest';
import { WorkspacePreviews } from './WorkspacePreviews';

/** The task attempt that owns this workspace. Views with a malformed attempt list are skipped, not read. */
function owningTask(views: TaskView[] | undefined, workspaceId: string) {
  if (!views) return null;
  return taskForWorkspace(
    views.filter((view) => Array.isArray(view.attempts)),
    workspaceId
  );
}

/**
 * One workspace: its lifecycle and publication, the controls that change it, the manifest and the
 * previews. The notice and the delete dialog live here, above the sections, so an action's result
 * is still shown after the section that started it has gone.
 */
export function WorkspacePage({ id }: { id: string }) {
  const navigate = useNavigate();
  const [notice, setNotice] = useState<Notice | null>(null);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const query = useWorkspace(id);
  const tasks = useTasks();
  const actions = useWorkspaceActions(id, setNotice, () => void navigate({ to: '/' }));

  const raw = query.data;
  const readable = isWorkspace(raw) ? raw : null;
  // A malformed write (a live event or an action's result) must not blank a page that held good
  // data, so the last readable copy is kept and shown beside the error. This adjusts state during
  // render rather than in an effect, so the held copy is never one render behind.
  const [lastGood, setLastGood] = useState<WorkspaceLifecycle | null>(null);
  if (readable && readable !== lastGood) setLastGood(readable);
  const unreadable = raw !== undefined && readable === null;
  // A re-read that 404s means the workspace is gone, even though the last copy is still cached.
  const gone = query.isError && loadFailure(query.error) === 'not-found';
  const workspace = gone ? null : (readable ?? (unreadable ? lastGood : null));

  // The notice is shown in every state. An action's result must still be visible if the lifecycle
  // section that started it has unmounted, for example while the workspace is reloading.
  if (!workspace) {
    if (gone) {
      const { title, message } = loadMessage('not-found');
      return <WorkspaceUnavailable notice={notice} title={title} message={message} />;
    }
    if (raw === undefined && !query.isError) {
      return (
        <WorkspaceUnavailable notice={notice}>
          <p role="status" aria-busy="true" className="text-muted-foreground text-sm">
            Loading workspace…
          </p>
        </WorkspaceUnavailable>
      );
    }
    if (raw !== undefined) {
      return (
        <WorkspaceUnavailable
          notice={notice}
          title="Workspace could not be loaded"
          message="Mainloop sent a workspace this page cannot read."
          onRetry={() => void query.refetch()}
        />
      );
    }
    const failure = loadFailure(query.error);
    const { title, message } = loadMessage(failure);
    return (
      <WorkspaceUnavailable
        notice={notice}
        title={title}
        message={message}
        onRetry={failure === 'not-found' ? undefined : () => void query.refetch()}
      />
    );
  }

  const task = owningTask(tasks.data, id);
  // Names the publication this workspace has. A read-only workspace has no branch to name.
  const deleteCopy =
    workspace.publication_mode === 'branch'
      ? 'Delete this branch workspace? Its checkout and any uncommitted work are removed.'
      : 'Delete this workspace? Its checkout and any uncommitted work are removed.';

  return (
    <article className="p-md gap-md mx-auto flex max-w-4xl flex-col">
      <header className="gap-sm border-border pb-md flex flex-col border-b">
        <Link
          to="/sessions/$id"
          params={{ id: workspace.session_id }}
          className="text-muted-foreground min-h-11 self-start text-xs underline"
        >
          Back to session
        </Link>
        <div className="gap-sm flex flex-wrap items-start justify-between">
          <div className="min-w-0">
            <h1 className="text-lg font-medium">Workspace</h1>
            <div className="text-muted-foreground">
              <ShortId id={workspace.workspace_id} label="workspace" />
            </div>
          </div>
          <WorkspaceLifecycleBadge workspace={workspace} />
        </div>
        {task ? (
          <p data-testid="workspace-task-link" className="text-sm">
            <span className="text-muted-foreground">Task </span>
            <Link
              to="/tasks/$id"
              params={{ id: task.view.task.id }}
              className="text-foreground underline"
            >
              {task.view.task.title}
            </Link>{' '}
            <span className="text-muted-foreground text-xs">
              {statusLabel(task.view.task.status)}
              {task.current ? '' : ' · attempt superseded, history only'}
            </span>
          </p>
        ) : null}
      </header>

      {unreadable ? (
        <ErrorRecovery
          title="Couldn't refresh this workspace"
          message={UNREADABLE}
          onRetry={() => void query.refetch()}
          pending={query.isFetching}
        />
      ) : query.isError ? (
        <ErrorRecovery
          title="Couldn't refresh this workspace"
          message={loadMessage(loadFailure(query.error)).message}
          onRetry={() => void query.refetch()}
          pending={query.isFetching}
        />
      ) : null}

      <LifecycleSection
        workspace={workspace}
        notice={notice}
        busy={actions.busy}
        active={actions.active}
        onSuspend={actions.suspend}
        onResume={actions.resume}
        onRefresh={actions.refresh}
        onDelete={() => setConfirmDelete(true)}
      />

      <PanelBoundary name="Manifest">
        <WorkspaceManifest workspace={workspace} />
      </PanelBoundary>

      <PanelBoundary name="Previews">
        <WorkspacePreviews workspaceId={id} />
      </PanelBoundary>

      <ConfirmDialog
        open={confirmDelete}
        onOpenChange={setConfirmDelete}
        title="Delete workspace?"
        description={deleteCopy}
        confirmLabel="Delete workspace"
        destructive
        onConfirm={() => {
          setConfirmDelete(false);
          actions.remove();
        }}
      />
    </article>
  );
}

interface LifecycleSectionProps {
  workspace: WorkspaceLifecycle;
  notice: Notice | null;
  busy: boolean;
  active: string | null;
  onSuspend: () => void;
  onResume: () => void;
  onRefresh: () => void;
  onDelete: () => void;
}

/**
 * The state, the publication and the four controls. Each control is enabled only when its state
 * allows it, and every control is disabled while any action or its re-read is in flight.
 */
function LifecycleSection({
  workspace,
  notice,
  busy,
  active,
  onSuspend,
  onResume,
  onRefresh,
  onDelete
}: LifecycleSectionProps) {
  const state = workspace.observed_state;
  const hint = stateHint(state);
  const failed = state === 'failed';

  return (
    <section aria-labelledby="lifecycle-heading" className="gap-md flex flex-col">
      <div className="gap-md flex flex-col sm:flex-row sm:items-start sm:justify-between">
        <div className="gap-xs flex flex-col text-sm">
          <h2 id="lifecycle-heading" className="text-base font-medium">
            Lifecycle
          </h2>
          <p className="text-muted-foreground">
            State <span className="text-foreground">{state}</span>
          </p>
          <p className="text-muted-foreground">
            Publication <WorkspacePublicationBadge workspace={workspace} />
          </p>
        </div>
        <div className="gap-sm flex flex-wrap">
          <Button
            type="button"
            variant="outline"
            disabled={busy || !canSuspend(state)}
            aria-busy={active === 'suspend'}
            onClick={onSuspend}
          >
            {active === 'suspend' ? 'Suspending…' : 'Suspend workspace'}
          </Button>
          <Button
            type="button"
            variant="default"
            disabled={busy || !canResume(state)}
            aria-busy={active === 'resume'}
            onClick={onResume}
          >
            {active === 'resume' ? 'Resuming…' : 'Resume workspace'}
          </Button>
          <Button
            type="button"
            variant="outline"
            disabled={busy}
            aria-busy={active === 'refresh'}
            onClick={onRefresh}
          >
            {active === 'refresh' ? 'Refreshing…' : 'Refresh status'}
          </Button>
          <Button type="button" variant="destructive" disabled={busy} onClick={onDelete}>
            {active === 'delete' ? 'Deleting…' : 'Delete workspace'}
          </Button>
        </div>
      </div>

      {notice ? <ActionNotice notice={notice} /> : null}
      {workspace.detail ? (
        <p
          role={failed ? 'alert' : 'status'}
          className={cn(
            'px-md py-sm border-l-2 text-sm',
            failed ? 'border-destructive text-destructive' : 'border-term-yellow text-term-yellow'
          )}
        >
          {workspace.detail}
        </p>
      ) : hint ? (
        <p
          role="status"
          className="border-term-yellow text-term-yellow px-md py-sm border-l-2 text-sm"
        >
          {hint}
        </p>
      ) : null}
    </section>
  );
}

/** The result of the last action, in the same colours as the lifecycle section shows it. */
function ActionNotice({ notice }: { notice: Notice }) {
  const warn = notice.tone === 'warn';
  return (
    <p
      role={warn ? 'alert' : 'status'}
      data-testid="workspace-action-notice"
      className={cn(
        'px-md py-sm border-l-2 text-sm',
        warn ? 'border-term-yellow text-term-yellow' : 'border-term-green text-term-green'
      )}
    >
      {notice.text}
    </p>
  );
}

/**
 * A page with no workspace to show: a loading line, or an error with a retry. The last action's
 * notice still appears above it, so a failed action is never lost to a reload.
 */
function WorkspaceUnavailable({
  notice,
  title,
  message,
  onRetry,
  children
}: {
  notice: Notice | null;
  title?: string;
  message?: string;
  onRetry?: () => void;
  children?: ReactNode;
}) {
  return (
    <article className="p-md gap-md mx-auto flex max-w-lg flex-col">
      {notice ? <ActionNotice notice={notice} /> : null}
      {children ?? (
        <>
          <h1 className="text-lg font-medium">{title}</h1>
          <p className="text-muted-foreground text-sm">{message}</p>
        </>
      )}
      {title || onRetry ? (
        <div className="gap-sm flex flex-wrap items-center">
          {onRetry ? (
            <Button type="button" variant="outline" onClick={onRetry}>
              Try again
            </Button>
          ) : null}
          {title ? (
            <Link
              to="/"
              className="text-muted-foreground inline-flex min-h-11 items-center text-sm underline"
            >
              Back to sessions
            </Link>
          ) : null}
        </div>
      ) : null}
    </article>
  );
}
