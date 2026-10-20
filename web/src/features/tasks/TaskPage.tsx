import { Link } from '@tanstack/react-router';
import { ArrowLeft } from 'lucide-react';
import { useState } from 'react';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { StatusBadge } from '@/components/shared/status-badge';
import { Button } from '@/components/ui/button';
import { createActionTracker } from '@/lib/taskActions';
import { childrenOf, statusLabel, statusTone } from '@/lib/taskState';
import { useConnection } from '@/shell/ConnectionBanner';
import { isTaskView, readableView } from './cache';
import { taskLoadFailure, type LoadFailure } from './model';
import {
  sendTaskAction,
  UnreadablePayloadError,
  useProviders,
  useTask,
  useTaskList
} from './queries';
import { TaskFacts, TaskHistory } from './TaskDetail';
import { TaskActions } from './TaskActions';

/**
 * One task: its status, facts, owner actions and history. The detail is the owner read for this
 * task. The parent is read by its own id and the children through the list's parent filter, so the
 * page never reads the whole list. A failed parent or child read does not put the detail into a
 * failed state. Each action re-reads the task once it settles.
 */
export function TaskPage({ id }: { id: string }) {
  const offline = useConnection().status === 'offline';
  const detail = useTask(id);
  // Known only once the detail has landed. A failed refresh keeps the last parent id, as the view does.
  const parentId = isTaskView(detail.data) ? (detail.data.task.parent_task_id ?? null) : null;
  const parentRead = useTask(parentId ?? '', parentId !== null);
  const children = useTaskList({ parent_task_id: id });
  const providers = useProviders();
  // One tracker per page: a lost reply stays resendable while this task is on screen.
  const [tracker] = useState(() => createActionTracker(sendTaskAction));
  const onRetry = () => void detail.refetch();

  // A later read that says the task is gone (an action's re-read, say) must not leave the last view
  // on screen as if it were still current.
  if (detail.isError && taskLoadFailure(detail.error) === 'not-found') {
    return (
      <TaskFailure
        failure="not-found"
        error={detail.error}
        retrying={detail.isFetching}
        onRetry={onRetry}
      />
    );
  }

  if (detail.data === undefined) {
    if (detail.isPending) {
      return (
        <p role="status" className="p-md text-muted-foreground text-sm">
          Loading task…
        </p>
      );
    }
    if (detail.error instanceof UnreadablePayloadError) {
      return <UnreadableTask retrying={detail.isFetching} onRetry={onRetry} />;
    }
    return (
      <TaskFailure
        failure={taskLoadFailure(detail.error)}
        error={detail.error}
        retrying={detail.isFetching}
        onRetry={onRetry}
      />
    );
  }

  // The contract is checked at the boundary: a payload this page cannot read is reported, not rendered.
  const raw: unknown = detail.data;
  if (!isTaskView(raw)) return <UnreadableTask retrying={detail.isFetching} onRetry={onRetry} />;

  const view = readableView(raw);
  const parent = parentRead.data;
  const childTasks = childrenOf(children.data ?? [], id);

  return (
    <article className="flex min-h-0 flex-col">
      <header className="gap-sm border-border p-md flex flex-col border-b">
        <div className="gap-sm flex items-center">
          <BackLink parentTaskId={view.task.parent_task_id} projectId={view.task.project_id} />
          <span data-testid="task-status">
            <StatusBadge
              label={`[${statusLabel(view.task.status)}]`}
              tone={statusTone(view.task.status)}
            />
          </span>
        </div>
        <h1 className="text-lg font-medium break-words">{view.task.title}</h1>
        {view.task.brief ? (
          <p className="text-muted-foreground text-sm break-words whitespace-pre-wrap">
            {view.task.brief}
          </p>
        ) : null}
        {parentId ? (
          <p className="text-sm">
            Child of{' '}
            <Link
              to="/tasks/$id"
              params={{ id: parentId }}
              className="text-foreground inline-flex min-h-11 items-center break-words underline"
            >
              {parent ? parent.task.title : 'parent task'}
            </Link>
          </p>
        ) : null}
      </header>

      {detail.isError ? (
        // A failed refresh keeps the last view on screen and says so; only a successful read clears it.
        <div
          role="alert"
          className="gap-sm p-md border-term-yellow/50 flex flex-wrap items-center border-b text-sm"
        >
          <p>Could not refresh this task. Showing the last known state.</p>
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={() => void detail.refetch()}
            disabled={detail.isFetching}
          >
            Retry loading
          </Button>
        </div>
      ) : null}

      {children.isError || parentRead.isError ? (
        <p role="status" className="px-md pt-sm text-muted-foreground text-xs">
          Parent and child tasks could not be loaded.
        </p>
      ) : null}

      {view.task.owner_decision ? (
        <>
          <OwnerAskBanner action={view.task.owner_decision.action} />
          <TaskActions
            view={view}
            tracker={tracker}
            profiles={providers.data}
            profilesError={providers.error}
            offline={offline}
          />
          <details className="border-border border-b">
            <summary className="p-md text-muted-foreground min-h-11 cursor-pointer text-sm font-medium">
              Task details
            </summary>
            <TaskFacts
              view={view}
              onRefresh={() => void detail.refetch()}
              refreshing={detail.isFetching}
            />
            <TaskHistory view={view} childTasks={childTasks} />
          </details>
        </>
      ) : (
        <>
          <TaskFacts
            view={view}
            onRefresh={() => void detail.refetch()}
            refreshing={detail.isFetching}
          />
          <TaskActions
            view={view}
            tracker={tracker}
            profiles={providers.data}
            profilesError={providers.error}
            offline={offline}
          />
          <TaskHistory view={view} childTasks={childTasks} />
        </>
      )}
    </article>
  );
}

/**
 * The one-line ask for a real owner decision: the agent's own `owner_action` text, leading the
 * page, immediately above the actions that answer it. Everything else about the task sits behind
 * the collapsed "Task details" disclosure below.
 */
function OwnerAskBanner({ action }: { action: string }) {
  return (
    <section
      aria-labelledby="owner-ask-heading"
      className="gap-xs p-md border-term-yellow/50 bg-term-yellow/5 flex flex-col border-b"
    >
      <h2
        id="owner-ask-heading"
        data-testid="task-owner-ask"
        className="text-sm font-medium break-words"
      >
        {action}
      </h2>
    </section>
  );
}

/** The backend answered, but with a body this page cannot read. */
function UnreadableTask({ retrying, onRetry }: { retrying: boolean; onRetry: () => void }) {
  return (
    <div className="p-md">
      <ErrorRecovery
        title="Task could not be read"
        message="Mainloop sent a task this page cannot read."
        onRetry={onRetry}
        retryLabel="Retry loading"
        pending={retrying}
      />
    </div>
  );
}

function TaskFailure({
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
      <section data-testid="task-not-found" className="gap-md p-md flex flex-col">
        <h1 className="text-lg font-medium">Task not found</h1>
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
  if (failure === 'unreachable') {
    return (
      <div className="p-md">
        <ErrorRecovery
          title="Can't reach the Mainloop backend."
          message="Check the connection, then retry."
          onRetry={onRetry}
          retryLabel="Retry loading"
          pending={retrying}
        />
      </div>
    );
  }
  if (failure === 'server') {
    return (
      <div className="p-md">
        <ErrorRecovery
          title="Server error"
          message="Mainloop returned a server error. Retry loading to try again."
          onRetry={onRetry}
          retryLabel="Retry loading"
          pending={retrying}
        />
      </div>
    );
  }
  return (
    <div className="p-md">
      <ErrorRecovery
        title="Task could not be loaded."
        message={error instanceof Error ? error.message : 'Unknown error.'}
        onRetry={onRetry}
        retryLabel="Retry loading"
        pending={retrying}
      />
    </div>
  );
}

/** Back goes up one level: the parent task, then the project, then the sessions home. */
function BackLink({
  parentTaskId,
  projectId
}: {
  parentTaskId: string | null;
  projectId: string | null;
}) {
  if (parentTaskId) {
    return (
      <Button asChild variant="ghost" size="icon">
        <Link to="/tasks/$id" params={{ id: parentTaskId }} aria-label="Back to parent task">
          <ArrowLeft aria-hidden="true" />
        </Link>
      </Button>
    );
  }
  if (projectId) {
    return (
      <Button asChild variant="ghost" size="icon">
        <Link to="/projects/$id" params={{ id: projectId }} aria-label="Back to project">
          <ArrowLeft aria-hidden="true" />
        </Link>
      </Button>
    );
  }
  return (
    <Button asChild variant="ghost" size="icon">
      <Link to="/" search={(prev) => ({ ...prev, tab: undefined })} aria-label="Back">
        <ArrowLeft aria-hidden="true" />
      </Link>
    </Button>
  );
}
