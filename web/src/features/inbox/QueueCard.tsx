import { FileText, Bell, CircleQuestionMark, TriangleAlert, type LucideIcon } from 'lucide-react';
import { useState } from 'react';
import { Button } from '@/components/ui/button';
import { approveInNativeShell, inNativeShell } from '@/lib/nativeShell';
import {
  ageLabel,
  messageOf,
  prLink,
  priorityBorder,
  privilegedActionId,
  privilegedDigest,
  responseMetadata,
  takesTextReply,
  type QueueItem
} from './model';
import { useApprovedInNativeShell, useRespondToQueueItem } from './queries';

const TYPE_ICONS: Partial<Record<QueueItem['item_type'], LucideIcon>> = {
  plan_review: FileText,
  question: CircleQuestionMark,
  error: TriangleAlert
};

/**
 * A queue item that is not a live HITL request: a question, a plan to review or an approval. Answers
 * go to `POST /queue/{id}/respond`; Approve on a privileged-action card carries the card's digest.
 * In the iPhone app, Approve on a privileged-action card goes to the app instead, which shows its own
 * sheet, asks for Touch ID and sends the signed answer (a phone's unsigned Approve is refused).
 * Input notices are dismissed from Activity, not here.
 */
export function QueueCard({ item }: { item: QueueItem }) {
  const [planOpen, setPlanOpen] = useState(false);
  const [draft, setDraft] = useState('');
  const [sent, setSent] = useState<string | null>(null);
  const respond = useRespondToQueueItem();
  const approved = useApprovedInNativeShell();
  const [native, setNative] = useState<{ pending: boolean; error: string | null }>({
    pending: false,
    error: null
  });

  const Icon = TYPE_ICONS[item.item_type] ?? Bell;
  const pr = prLink(item);
  const answer = item.response ?? sent;
  const awaitingAnswer = item.status === 'pending' && !sent;
  const options = awaitingAnswer ? (item.options ?? []) : [];

  const digest = privilegedDigest(item);
  const actionId = privilegedActionId(item);
  const nativeApprove = digest !== null && actionId !== null && inNativeShell();
  const busy = respond.isPending || native.pending;

  const approveNatively = async () => {
    if (!digest || !actionId) return;
    setNative({ pending: true, error: null });
    const result = await approveInNativeShell({ itemId: item.id, actionId, digest });
    setNative({ pending: false, error: result.status === 'failed' ? result.message : null });
    if (result.status === 'approved') setSent('Approve');
    approved();
  };

  const send = (response: string, onSent?: () => void) =>
    respond.mutate(
      {
        params: { path: { item_id: item.id } },
        body: { response, metadata: responseMetadata(item, response) }
      },
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
          <h2 className="text-sm font-medium break-words">{item.title}</h2>
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
            <pre className="border-border bg-card text-foreground p-md border text-xs break-words whitespace-pre-wrap">
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
          className="text-primary min-h-control inline-flex items-center text-sm underline underline-offset-4"
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
              disabled={busy}
              onClick={() =>
                nativeApprove && option === 'Approve' ? void approveNatively() : send(option)
              }
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
              privilegedDigest(item)
                ? 'Deny with a note...'
                : item.item_type === 'plan_review'
                  ? 'Request changes...'
                  : 'Type your response...'
            }
            disabled={busy}
            className="border-input bg-background text-foreground placeholder:text-muted-foreground focus-visible:border-ring min-h-control min-w-0 flex-1 border px-3 text-sm focus-visible:outline-none disabled:opacity-50"
          />
          <Button type="submit" variant="outline" size="sm" disabled={!draft.trim() || busy}>
            Send
          </Button>
        </form>
      ) : null}

      {answer ? <p className="text-muted-foreground text-sm break-words">&gt; {answer}</p> : null}

      {native.error ? (
        <p role="alert" className="text-destructive text-sm">
          {native.error}
        </p>
      ) : null}

      {respond.isError ? (
        <p role="alert" className="text-destructive text-sm">
          {messageOf(respond.error)}
        </p>
      ) : null}
    </article>
  );
}
