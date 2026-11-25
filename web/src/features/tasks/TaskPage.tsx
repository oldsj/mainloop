import { Link, useNavigate } from '@tanstack/react-router';
import { ArrowLeft } from 'lucide-react';
import { useId, useState } from 'react';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { SkeletonHeader } from '@/components/shared/skeleton';
import { StatusBadge } from '@/components/shared/status-badge';
import { Button } from '@/components/ui/button';
import { cn } from '@/components/ui/cn';
import type { TaskView } from '@/lib/contract';
import { createActionTracker } from '@/lib/taskActions';
import {
  childrenOf,
  currentAttempt,
  modelEffortLabel,
  providerLabel,
  reasonLabel,
  statusLabel,
  statusTone
} from '@/lib/taskState';
import { useConnection } from '@/shell/ConnectionBanner';
import { isTaskView, readableView } from './cache';
import { publicationAsk, taskLoadFailure, type LoadFailure } from './model';
import {
  sendTaskAction,
  UnreadablePayloadError,
  useProviders,
  useTask,
  useTaskList
} from './queries';
import { PendingApprovals, TaskFacts, TaskHistory } from './TaskDetail';
import { FollowUp } from './FollowUp';
import { TaskActions } from './TaskActions';
import { TaskLoops } from './TaskLoops';

/**
 * One task. The page leads with what the owner needs to know and do: the status and title, what
 * is needed from them, and the actions. The facts and history follow, and only the parts that have
 * something to say; on a task with an open decision they sit behind **Task details**. The detail is
 * the owner read for this task. The parent is read by its own id and the children through the list's parent filter, so the
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
  const navigate = useNavigate();
  // Opens the main chat with the composer filled, not sent, as "Ask Mainloop" does in the explorer.
  const ask = (text: string) => void navigate({ to: '/', search: { prefill: text } });

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
        <div className="p-md">
          <p role="status" className="sr-only">
            Loading task…
          </p>
          <SkeletonHeader testId="task-loading" />
        </div>
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
    // The version shown is on the page, so a stale read that must not replace it can be told apart.
    <article className="flex min-h-0 flex-col" data-task-version={view.task.version}>
      <header className="gap-sm border-border-subtle p-md flex flex-col border-b">
        <div className="gap-sm flex items-center">
          <BackLink parentTaskId={view.task.parent_task_id} projectId={view.task.project_id} />
          <span data-testid="task-status">
            <StatusBadge
              label={statusLabel(view.task.status)}
              tone={statusTone(view.task.status)}
            />
          </span>
        </div>
        <h1 className="text-lg font-medium break-words">{view.task.title}</h1>
        <TaskMeta view={view} />
        {parentId ? (
          <p className="text-sm">
            Child of{' '}
            <Link
              to="/tasks/$id"
              params={{ id: parentId }}
              className="text-foreground min-h-control inline-flex items-center break-words underline"
            >
              {parent ? parent.task.title : 'parent task'}
            </Link>
          </p>
        ) : null}
        {view.task.brief ? <Brief text={view.task.brief} /> : null}
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

      <PendingApprovals
        view={view}
        onRefresh={() => void detail.refetch()}
        refreshing={detail.isFetching}
      />
      {view.task.owner_decision ? (
        <OwnerAskBanner action={view.task.owner_decision.action} />
      ) : null}
      <PublicationBanner view={view} />
      <TaskActions
        view={view}
        tracker={tracker}
        profiles={providers.data}
        profilesError={providers.error}
        offline={offline}
        onAsk={ask}
      />
      <FollowUp view={view} offline={offline} />
      <TaskLoops taskId={view.task.id} offline={offline} />
      {view.task.owner_decision ? (
        <details className="border-border-subtle border-t">
          <summary className="p-md text-muted-foreground min-h-control cursor-pointer text-sm font-medium">
            Task details
          </summary>
          <TaskFacts view={view} />
          <TaskHistory view={view} childTasks={childTasks} />
        </details>
      ) : (
        <>
          <TaskFacts view={view} />
          <TaskHistory view={view} childTasks={childTasks} />
        </>
      )}
    </article>
  );
}

/**
 * One muted line under the title: why the task is where it is, who is running it and on which
 * attempt. It replaces a section of its own, so the facts sit with the title they describe.
 */
function TaskMeta({ view }: { view: TaskView }) {
  const attempt = currentAttempt(view);
  const modelEffort = modelEffortLabel(view);
  const reason = view.task.reason ? reasonLabel(view.task.reason) : '';
  // Each part after the first is led by a middle dot, so the line reads as one sentence of facts.
  const dot = (
    <span aria-hidden="true" className="px-xs">
      ·
    </span>
  );
  return (
    <p className="text-muted-foreground text-xs">
      {reason ? (
        <>
          <span data-testid="task-status-reason">{reason}</span>
          {dot}
        </>
      ) : null}
      <span data-testid="task-provider">
        {providerLabel(view)}
        {attempt ? ` · attempt ${attempt.number}` : ''}
      </span>
      {modelEffort ? (
        <>
          {dot}
          <span data-testid="task-model-effort">{modelEffort}</span>
        </>
      ) : null}
    </p>
  );
}

/** A brief longer than this many characters, or with more lines than `BRIEF_LINES`, is cut short. */
const BRIEF_CHARS = 200;
const BRIEF_LINES = 2;

/**
 * The task's brief, quiet and small: it is what the agent was told, not what the owner must read
 * first. A long one shows its first two lines until the owner opens it.
 */
function Brief({ text }: { text: string }) {
  const [open, setOpen] = useState(false);
  const id = useId();
  const long = text.length > BRIEF_CHARS || text.split('\n').length > BRIEF_LINES;
  return (
    <div className="gap-xs flex flex-col items-start" data-testid="task-brief">
      <p
        id={id}
        className={cn(
          'text-muted-foreground text-xs break-words whitespace-pre-wrap',
          long && !open && 'line-clamp-2'
        )}
      >
        {text}
      </p>
      {long ? (
        <button
          type="button"
          aria-expanded={open}
          aria-controls={id}
          onClick={() => setOpen((was) => !was)}
          className="text-muted-foreground hover:text-foreground min-h-control text-xs underline lg:min-h-0"
        >
          {open ? 'Show less of the brief' : 'Show the whole brief'}
        </button>
      ) : null}
    </div>
  );
}

/**
 * What is needed from the owner on a real decision: the agent's own `owner_action` text, leading
 * the page under the status language's red edge, immediately above the actions that answer it.
 */
function OwnerAskBanner({ action }: { action: string }) {
  return (
    <section
      aria-labelledby="owner-ask-heading"
      className="gap-xs p-md border-status-attention flex flex-col border-l-2"
    >
      <p className="text-muted-foreground text-2xs font-medium tracking-wide uppercase">
        Needs you
      </p>
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

/**
 * Where a task that waits on publication stands, and what moves it on. "Awaiting publication"
 * alone does not say that nothing more happens by itself, or what to do about it.
 */
function PublicationBanner({ view }: { view: TaskView }) {
  const ask = publicationAsk(view);
  if (!ask) return null;
  return (
    <section
      aria-labelledby="publication-ask-heading"
      data-testid="task-publication-ask"
      data-kind={ask.kind}
      className={cn(
        'gap-xs p-md flex flex-col border-l-2',
        ask.needsOwner ? 'border-status-attention' : 'border-border'
      )}
    >
      <h2
        id="publication-ask-heading"
        className="text-muted-foreground text-2xs font-medium tracking-wide uppercase"
      >
        {ask.needsOwner ? 'Needs you' : 'Waiting on its pull request'}
      </h2>
      <p className="text-sm break-words">{ask.text}</p>
      {ask.prUrl ? (
        <p className="text-sm">
          <a
            href={ask.prUrl}
            target="_blank"
            rel="noopener noreferrer"
            className="min-h-control inline-flex items-center underline"
          >
            {ask.prNumber != null ? `Open PR #${ask.prNumber} on GitHub` : 'Open the pull request'}
          </a>
        </p>
      ) : null}
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
