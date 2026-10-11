import { Link } from '@tanstack/react-router';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { Button, buttonVariants } from '@/components/ui/button';
import { activityItems, messageOf, taskIdOf } from './model';
import { useActivityPoll } from './poll';
import { useActivity, useDismissQueueItem } from './queries';

/**
 * The Activity log. It is polled only while on screen, and the unread count never includes it.
 * The dismiss is held here rather than on the row: a dismissed row leaves at once, and a refusal
 * has to stay on screen after it has gone.
 */
export function ActivityList() {
  const activity = useActivity();
  const dismiss = useDismissQueueItem();
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

      {dismiss.isError ? (
        <p role="alert" className="text-destructive p-md text-sm">
          {messageOf(dismiss.error)}
        </p>
      ) : null}

      {items.length === 0 && !activity.isError ? (
        <p className="text-muted-foreground p-md text-sm">No activity yet</p>
      ) : (
        <ul aria-label="Activity" className="flex flex-col">
          {items.map((item) => {
            const taskId = taskIdOf(null, item.href);
            const noticeId = item.dismissible ? item.queue_item_id : null;
            return (
              <li key={item.id} className="border-border p-md gap-xs flex flex-col border-b">
                <h3 className="text-sm font-medium break-words">{item.title}</h3>
                <p className="text-muted-foreground text-xs break-words whitespace-pre-wrap">
                  {item.content}
                </p>
                {taskId || noticeId ? (
                  <div className="gap-xs flex flex-wrap items-center">
                    {taskId ? (
                      <Link
                        to="/tasks/$id"
                        params={{ id: taskId }}
                        className={buttonVariants({ variant: 'ghost', size: 'sm' })}
                      >
                        View task
                      </Link>
                    ) : null}
                    {noticeId ? (
                      <Button
                        variant="ghost"
                        size="sm"
                        disabled={dismiss.isPending}
                        onClick={() => dismiss.mutate({ params: { path: { item_id: noticeId } } })}
                      >
                        Dismiss notice
                      </Button>
                    ) : null}
                  </div>
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
