import { FileText, Bell, CircleQuestionMark, TriangleAlert, type LucideIcon } from 'lucide-react';
import { useState } from 'react';
import { Button } from '@/components/ui/button';
import {
  ageLabel,
  messageOf,
  prLink,
  priorityBorder,
  takesTextReply,
  type QueueItem
} from './model';
import { useDismissQueueItem, useRespondToQueueItem } from './queries';

const TYPE_ICONS: Partial<Record<QueueItem['item_type'], LucideIcon>> = {
  plan_review: FileText,
  question: CircleQuestionMark,
  error: TriangleAlert
};

/**
 * A queue item that is not a live HITL request: a question, a plan to review, or an unavailable
 * input notice. Answers go to `POST /queue/{id}/respond`; a notice can be dismissed.
 */
export function QueueCard({ item }: { item: QueueItem }) {
  const [planOpen, setPlanOpen] = useState(false);
  const [draft, setDraft] = useState('');
  const [sent, setSent] = useState<string | null>(null);
  const respond = useRespondToQueueItem();
  const dismiss = useDismissQueueItem();

  const Icon = TYPE_ICONS[item.item_type] ?? Bell;
  const pr = prLink(item);
  const answer = item.response ?? sent;
  const awaitingAnswer = item.status === 'pending' && !sent;
  const options = awaitingAnswer ? (item.options ?? []) : [];
  const canDismiss = item.item_type === 'hitl_request' && !item.hitl_request_id;

  const send = (response: string, onSent?: () => void) =>
    respond.mutate(
      { params: { path: { item_id: item.id } }, body: { response } },
      {
        onSuccess: () => {
          setSent(response);
          onSent?.();
        }
      }
    );

  return (
    <article className={`p-md gap-sm flex flex-col border-l-4 ${priorityBorder(item.priority)}`}>
      <header className="gap-sm flex items-start justify-between">
        <div className="gap-sm flex min-w-0 items-center">
          <Icon aria-hidden className="text-muted-foreground size-4 shrink-0" />
          <h3 className="text-sm font-medium break-words">{item.title}</h3>
        </div>
        <time dateTime={item.created_at} className="text-muted-foreground shrink-0 text-xs">
          {ageLabel(item.created_at)}
        </time>
      </header>

      {item.item_type === 'plan_review' ? (
        <div className="gap-sm flex flex-col">
          <Button
            variant="ghost"
            size="sm"
            aria-expanded={planOpen}
            className="justify-start"
            onClick={() => setPlanOpen((open) => !open)}
          >
            {planOpen ? 'Hide plan' : 'View plan'}
          </Button>
          {planOpen ? (
            <pre className="border-border bg-card text-foreground p-md max-h-96 overflow-y-auto border text-xs whitespace-pre-wrap">
              {item.content}
            </pre>
          ) : null}
        </div>
      ) : (
        <p className="text-muted-foreground text-sm break-words whitespace-pre-wrap">
          {item.content}
        </p>
      )}

      {pr ? (
        <a
          href={pr}
          target="_blank"
          rel="noopener noreferrer"
          className="text-primary inline-flex min-h-11 items-center text-sm underline underline-offset-4"
        >
          View PR
        </a>
      ) : null}

      {options.length > 0 ? (
        <div role="group" aria-label="Options" className="gap-xs flex flex-wrap">
          {options.map((option) => (
            <Button
              key={option}
              variant="outline"
              size="sm"
              disabled={respond.isPending}
              onClick={() => send(option)}
            >
              {option}
            </Button>
          ))}
        </div>
      ) : null}

      {takesTextReply(item) && awaitingAnswer ? (
        <form
          className="gap-sm flex items-center"
          onSubmit={(event) => {
            event.preventDefault();
            const value = draft.trim();
            if (value) send(value, () => setDraft(''));
          }}
        >
          <label htmlFor={`reply-${item.id}`} className="sr-only">
            Reply to {item.title}
          </label>
          <input
            id={`reply-${item.id}`}
            value={draft}
            onChange={(event) => setDraft(event.target.value)}
            placeholder={
              item.item_type === 'plan_review' ? 'Request changes...' : 'Type your response...'
            }
            disabled={respond.isPending}
            className="border-input bg-background text-foreground placeholder:text-muted-foreground focus-visible:border-ring min-h-11 min-w-0 flex-1 border px-3 text-sm focus-visible:outline-none disabled:opacity-50"
          />
          <Button
            type="submit"
            variant="outline"
            size="sm"
            disabled={!draft.trim() || respond.isPending}
          >
            Send
          </Button>
        </form>
      ) : null}

      {answer ? <p className="text-muted-foreground text-sm break-words">&gt; {answer}</p> : null}

      {respond.isError ? (
        <p role="alert" className="text-destructive text-sm">
          {messageOf(respond.error)}
        </p>
      ) : null}

      {canDismiss ? (
        <div className="gap-xs flex flex-col items-start">
          <Button
            variant="ghost"
            size="sm"
            disabled={dismiss.isPending}
            onClick={() => dismiss.mutate({ params: { path: { item_id: item.id } } })}
          >
            Dismiss notice
          </Button>
          {dismiss.isError ? (
            <p role="alert" className="text-destructive text-sm">
              {messageOf(dismiss.error)}
            </p>
          ) : null}
        </div>
      ) : null}
    </article>
  );
}
