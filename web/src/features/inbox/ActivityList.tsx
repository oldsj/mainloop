import { Link } from '@tanstack/react-router';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { buttonVariants } from '@/components/ui/button';
import { cn } from '@/components/ui/cn';
import { activityItems, messageOf, taskIdOf } from './model';
import { useActivityPoll } from './poll';
import { useActivity } from './queries';

/** The Activity log. It is polled only while on screen, and the unread count never includes it. */
export function ActivityList() {
  const activity = useActivity();
  useActivityPoll();
  const items = activityItems(activity.data);

  const failure = activity.isError ? (
    <div className="p-md">
      <ErrorRecovery
        title="Could not load activity."
        message={messageOf(activity.error)}
        onRetry={() => void activity.refetch()}
        pending={activity.isFetching}
      />
    </div>
  ) : null;

  if (activity.isError && !activity.data) return failure;

  if (activity.isPending) {
    return (
      <p role="status" className="text-muted-foreground p-md text-sm">
        Loading activity…
      </p>
    );
  }

  return (
    <div className="flex flex-col">
      {failure}

      {items.length === 0 && !activity.isError ? (
        <p className="text-muted-foreground p-md text-sm">No activity yet</p>
      ) : (
        <ul aria-label="Activity" className="flex flex-col">
          {items.map((item) => {
            const taskId = taskIdOf(null, item.href);
            return (
              <li key={item.id} className="border-border p-md gap-xs flex flex-col border-b">
                <h3 className="text-sm font-medium break-words">{item.title}</h3>
                <p className="text-muted-foreground text-xs break-words whitespace-pre-wrap">
                  {item.content}
                </p>
                {taskId ? (
                  <Link
                    to="/tasks/$id"
                    params={{ id: taskId }}
                    className={cn(buttonVariants({ variant: 'ghost', size: 'sm' }), 'self-start')}
                  >
                    View task
                  </Link>
                ) : null}
                <time dateTime={item.created_at} className="text-muted-foreground text-xs">
                  {new Date(item.created_at).toLocaleString()}
                </time>
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}
