import { Link } from '@tanstack/react-router';
import { useState } from 'react';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { buttonVariants } from '@/components/ui/button';
import { cn } from '@/components/ui/cn';
import { HITLCard } from '@/features/hitl';
import { type AnsweredRequest, useAnswered } from '@/features/hitl/hitl-client';
import { type AttentionItem, attentionAnchor, attentionItems, messageOf, taskIdOf } from './model';
import { QueueCard } from './QueueCard';
import { useAttention } from './queries';

/** Needs you: what the owner must answer or decide now, in the order the server sent it. */
export function AttentionList() {
  const attention = useAttention();
  const items = attentionItems(attention.data);

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
      <AttentionRows items={items} refreshFailed={attention.isError} />
    </div>
  );
}

/**
 * One row as shown. A request's row has the same key and shape whether the server lists it or not,
 * so its card stays mounted (and keeps focus) when the server drops it after an answer.
 */
type Row =
  | { key: string; kind: 'request'; requestId: string; item: AttentionItem | null }
  | { key: string; kind: 'entry'; item: AttentionItem };

function hitlKey(requestId: string): string {
  return `hitl:${requestId}`;
}

function rowOf(item: AttentionItem): Row {
  const queue = item.queue_item;
  if (queue?.item_type === 'hitl_request' && queue.hitl_request_id) {
    return {
      key: hitlKey(queue.hitl_request_id),
      kind: 'request',
      requestId: queue.hitl_request_id,
      item
    };
  }
  return { key: item.id, kind: 'entry', item };
}

/**
 * The Needs-you rows, or the empty state. A request this browser answered stays a row with its
 * receipt, in place, until reload, even after the server stops listing it. It reads no server data
 * itself, so it can be rendered on its own (the preview does).
 */
export function AttentionRows({
  items,
  refreshFailed = false
}: {
  items: AttentionItem[];
  /** The last refresh failed: no rows then means unknown, so the empty state is not shown. */
  refreshFailed?: boolean;
}) {
  const answered = useAnswered();
  const [shown, setShown] = useState<Row[]>([]);
  const rows = rowsToShow(shown, items.map(rowOf), answered);
  // Remember the rows as drawn, so the next refetch can keep them in place. Set during render, which
  // React allows for state derived from props; the next pass finds nothing to change.
  if (!sameRows(rows, shown)) setShown(rows);

  if (rows.length === 0) {
    if (refreshFailed) return null;
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
      {rows.map((row) => (
        <NeedsRow key={row.key} row={row} />
      ))}
    </ul>
  );
}

/**
 * The rows to draw. A row keeps its place across refetches, so a request the server has dropped stays
 * where it was: React moves a node when its siblings reorder, and a moved card loses its focus.
 * Requests this browser answered that were never listed here come first, as receipts.
 */
function rowsToShow(
  prev: Row[],
  listed: Row[],
  answered: ReadonlyMap<string, AnsweredRequest>
): Row[] {
  const listedRequests = new Set(
    listed.flatMap((row) => (row.kind === 'request' ? [row.requestId] : []))
  );
  const rows = [...listed];
  const keys = new Set(rows.map((row) => row.key));
  prev.forEach((row, index) => {
    if (
      row.kind !== 'request' ||
      listedRequests.has(row.requestId) ||
      !answered.has(row.requestId) ||
      keys.has(row.key)
    ) {
      return;
    }
    rows.splice(placeAfter(prev, index, rows), 0, row);
    keys.add(row.key);
  });
  const receipts = [...answered.keys()]
    .reverse()
    .filter((requestId) => !listedRequests.has(requestId) && !keys.has(hitlKey(requestId)))
    .map((requestId): Row => ({ key: hitlKey(requestId), kind: 'request', requestId, item: null }));
  return [...receipts, ...rows];
}

/** Where a kept row goes: just after the nearest row before it in the last list that is still drawn. */
function placeAfter(prev: Row[], index: number, rows: Row[]): number {
  for (let before = index - 1; before >= 0; before--) {
    const key = prev[before]?.key;
    const at = rows.findIndex((row) => row.key === key);
    if (at >= 0) return at + 1;
  }
  return 0;
}

function sameRows(a: Row[], b: Row[]): boolean {
  return (
    a.length === b.length &&
    a.every((row, i) => row.key === b[i]?.key && row.item?.needed === b[i]?.item?.needed)
  );
}

function NeedsRow({ row }: { row: Row }) {
  if (row.kind === 'entry') {
    return (
      <li id={attentionAnchor(row.item)} className="border-border border-b">
        <AttentionEntry item={row.item} />
      </li>
    );
  }
  // One element shape for a listed and a kept request. The lead-in is the server's line, kept with the
  // row, and is left out only when no entry for the request was ever listed here.
  return (
    <li id={row.item ? attentionAnchor(row.item) : undefined} className="border-border border-b">
      <div className="gap-sm flex flex-col">
        {row.item?.needed ? (
          <p className="px-md pt-md text-sm break-words">{row.item.needed}</p>
        ) : null}
        <HITLCard requestId={row.requestId} />
      </div>
    </li>
  );
}

function AttentionEntry({ item }: { item: AttentionItem }) {
  const queue = item.queue_item;

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
