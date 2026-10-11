import { Link } from '@tanstack/react-router';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { StatusBadge } from '@/components/shared/status-badge';
import { Button } from '@/components/ui/button';
import { relativeTime } from '@/features/sessions/model';
import {
  executionStateLabel,
  executionStateTone,
  messageOf,
  type ScheduleExecution
} from './model';
import { useScheduleExecutions } from './queries';

/** One schedule's runs, newest first. Each run with a session links to it. */
export function ExecutionList({ scheduleId }: { scheduleId: string }) {
  const executions = useScheduleExecutions(scheduleId);
  const items = executions.data?.pages.flatMap((page) => page.items) ?? [];

  return (
    <section aria-labelledby="executions-heading" className="gap-md flex min-w-0 flex-col">
      <h2 id="executions-heading" className="text-base font-medium">
        Executions
      </h2>
      {executions.isPending ? <p className="text-sm">Loading executions…</p> : null}
      {executions.isError && items.length === 0 ? (
        <ErrorRecovery
          title="Could not load executions"
          message={messageOf(executions.error)}
          onRetry={() => executions.refetch()}
          pending={executions.isFetching}
        />
      ) : null}
      {executions.isSuccess && items.length === 0 ? (
        <p className="text-muted-foreground text-sm">No runs yet.</p>
      ) : null}
      {items.length > 0 ? (
        <ol className="gap-sm flex flex-col">
          {items.map((execution) => (
            <ExecutionRow key={execution.id} execution={execution} />
          ))}
        </ol>
      ) : null}
      {executions.hasNextPage ? (
        <Button
          type="button"
          variant="outline"
          disabled={executions.isFetchingNextPage}
          onClick={() => executions.fetchNextPage()}
        >
          {executions.isFetchingNextPage ? 'Loading…' : 'Load older executions'}
        </Button>
      ) : null}
    </section>
  );
}

function ExecutionRow({ execution }: { execution: ScheduleExecution }) {
  return (
    <li className="border-border p-md gap-xs flex min-w-0 flex-col border">
      <div className="gap-sm flex flex-wrap items-center justify-between">
        <span className="text-sm font-medium">
          {execution.scheduled_time ? 'Scheduled run' : 'Manual run'}
        </span>
        <StatusBadge
          label={executionStateLabel(execution.state)}
          tone={executionStateTone(execution.state)}
        />
      </div>
      <p className="text-muted-foreground text-xs">Created {relativeTime(execution.created_at)}</p>
      {execution.failure_reason ? (
        <p className="text-destructive text-xs break-words">{execution.failure_reason}</p>
      ) : null}
      {execution.session_id ? (
        <Link
          to="/sessions/$id"
          params={{ id: execution.session_id }}
          className="text-foreground inline-flex min-h-11 items-center text-sm underline"
        >
          Open session
        </Link>
      ) : (
        <p className="text-muted-foreground text-xs">No session yet.</p>
      )}
    </li>
  );
}
