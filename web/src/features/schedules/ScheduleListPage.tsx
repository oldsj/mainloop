import { Link } from '@tanstack/react-router';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { Button } from '@/components/ui/button';
import { agentText, messageOf } from './model';
import { ScheduleCard } from './ScheduleCard';
import { useSchedules } from './queries';

/** Every scheduled run in this deployment, one page at a time, in the order the server returns them (by id). */
export function ScheduleListPage() {
  const schedules = useSchedules();
  const items = schedules.data?.pages.flatMap((page) => page.items) ?? [];

  return (
    <div className="gap-lg p-md flex min-w-0 flex-col">
      <header className="gap-sm flex flex-col">
        <h1 className="text-xl font-medium">Scheduled runs</h1>
        <p className="text-muted-foreground text-sm">
          An agent runs your prompt on a timetable. Each run starts a native session you can open.
        </p>
        <div>
          <Button asChild>
            <Link to="/schedules/new">New schedule</Link>
          </Button>
        </div>
      </header>

      {schedules.isPending ? <p className="text-sm">Loading schedules…</p> : null}

      {schedules.isError && items.length === 0 ? (
        <ErrorRecovery
          title="Could not load schedules"
          message={messageOf(schedules.error)}
          onRetry={() => schedules.refetch()}
          pending={schedules.isFetching}
        />
      ) : null}

      {schedules.isSuccess && items.length === 0 ? (
        <div className="border-border p-lg gap-sm flex flex-col border">
          <p className="text-sm font-medium">No schedules yet.</p>
          <p className="text-muted-foreground text-sm">
            Create one to run an agent on a fixed timetable.
          </p>
        </div>
      ) : null}

      {items.length > 0 ? (
        <ul className="gap-md flex min-w-0 flex-col">
          {items.map((schedule) => (
            <li key={schedule.id} className="min-w-0">
              <Link
                to="/schedules/$id"
                params={{ id: schedule.id }}
                className="border-border p-md hover:bg-muted/40 focus-visible:outline-ring block min-w-0 border"
              >
                <ScheduleCard
                  name={schedule.config.name}
                  schedule={schedule.config.schedule}
                  timeZone={schedule.config.time_zone}
                  agent={agentText(schedule.agent)}
                  paused={schedule.config.paused}
                  nextRun={schedule.next_execution_time ?? null}
                />
              </Link>
            </li>
          ))}
        </ul>
      ) : null}

      {schedules.hasNextPage ? (
        <div>
          <Button
            type="button"
            variant="outline"
            disabled={schedules.isFetchingNextPage}
            onClick={() => schedules.fetchNextPage()}
          >
            {schedules.isFetchingNextPage ? 'Loading…' : 'Load more'}
          </Button>
        </div>
      ) : null}
    </div>
  );
}
