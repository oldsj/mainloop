import { Link } from '@tanstack/react-router';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { buttonVariants } from '@/components/ui/button';
import { cn } from '@/components/ui/cn';
import { HITLCard } from '@/features/hitl';
import { useAnswered } from '@/features/hitl/hitl-client';
import { type AttentionItem, attentionAnchor, attentionItems, messageOf, taskIdOf } from './model';
import { QueueCard } from './QueueCard';
import { useAttention } from './queries';

/** Needs you: what the owner must answer or decide now, in the order the server sent it. */
export function AttentionList() {
  const attention = useAttention();
  const items = attentionItems(attention.data);
  const answered = useAnswered();
  // The server drops a request once it is answered. This browser keeps its receipt in place until reload.
  const listed = new Set(items.map((item) => item.queue_item?.hitl_request_id));
  const receipts = [...answered.keys()].reverse().filter((id) => !listed.has(id));

  // A failed refresh keeps the rows already shown, with the error above them.
  const failure = attention.isError ? (
    <div className="p-md" data-testid="inbox-error">
      <ErrorRecovery
        title="Could not check what needs you."
        message={messageOf(attention.error)}
        onRetry={() => void attention.refetch()}
        pending={attention.isFetching}
      />
    </div>
  ) : null;

  if (attention.isError && !attention.data) return failure;

  if (attention.isPending) {
    return (
      <p role="status" className="text-muted-foreground p-md text-sm">
        Checking what needs you…
      </p>
    );
  }

  return (
    <div className="flex flex-col">
      {failure}
      {items.length === 0 && receipts.length === 0 && attention.isError ? null : (
        <AttentionRows items={items} answered={receipts} />
      )}
    </div>
  );
}

/**
 * The Needs-you rows, or the empty state. `answered` are requests this browser just answered: they
 * stay as cards with their receipts, above the rows the server still lists. It reads no data, so it
 * can be rendered on its own.
 */
export function AttentionRows({
  items,
  answered = []
}: {
  items: AttentionItem[];
  answered?: string[];
}) {
  if (items.length === 0 && answered.length === 0) {
    return (
      <div className="p-md text-sm">
        <p className="font-medium">Nothing needs you</p>
        <p className="text-muted-foreground mt-xs">
          Approvals, questions and decisions appear here.
        </p>
      </div>
    );
  }

  return (
    <ul aria-label="Needs you" className="flex flex-col">
      {answered.map((id) => (
        <li key={`answered:${id}`} className="border-border border-b">
          <HITLCard requestId={id} />
        </li>
      ))}
      {items.map((item) => (
        <li key={item.id} id={attentionAnchor(item)} className="border-border border-b">
          <AttentionEntry item={item} />
        </li>
      ))}
    </ul>
  );
}

function AttentionEntry({ item }: { item: AttentionItem }) {
  const queue = item.queue_item;

  if (queue?.item_type === 'hitl_request' && queue.hitl_request_id) {
    return (
      <div className="gap-sm flex flex-col">
        <p className="px-md pt-md text-sm break-words">{item.needed}</p>
        <HITLCard requestId={queue.hitl_request_id} />
      </div>
    );
  }

  if (queue) return <QueueCard item={queue} />;

  const taskId = taskIdOf(item.task_id, item.href);
  return (
    <div className="gap-xs p-md flex flex-col">
      <h3 className="text-sm font-medium break-words">{item.title}</h3>
      <p className="text-muted-foreground text-sm break-words">{item.needed}</p>
      {taskId ? (
        <Link
          to="/tasks/$id"
          params={{ id: taskId }}
          className={cn(buttonVariants({ variant: 'outline', size: 'sm' }), 'self-start')}
        >
          {item.reason_code === 'reconciliation' || item.reason_code === 'runtime_stop_unresolved'
            ? 'Review task evidence'
            : 'Review task and decide'}
        </Link>
      ) : null}
    </div>
  );
}
