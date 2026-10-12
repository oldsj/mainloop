import { Markdown } from '@/components/markdown/markdown';
import { cn } from '@/components/ui/cn';
import { agentLabel } from '@/lib/agentLabel';
import type { Message } from '@/lib/contract';
import type { DeliveryNotice as Notice } from '@/lib/delivery';
import { messageTime } from '@/lib/time';
import { parseChildReport } from './childReport';
import { DeliveryNotice } from './DeliveryNotice';

export const STOPPED_NOTE = 'This turn was stopped before it finished.';

export interface MessageBubbleProps {
  message: Message;
  /** Where the message was written, shown in the prompt: `user@main$`. */
  context?: string;
  agentKind?: 'claude' | 'codex' | null;
  /** The message was not delivered, or its delivery is unconfirmed. */
  notice?: Notice;
  /** Mainloop wrote it on the owner's behalf (a delegated brief): collapsed, not "You". */
  assignment?: boolean;
  retryDisabled?: boolean;
  onRetry?: (message: Message) => void;
}

/** One message. A child's report is labelled as the child's, not as something the owner said. */
export function MessageBubble({
  message,
  context = 'main',
  agentKind,
  notice,
  assignment = false,
  retryDisabled,
  onRetry
}: MessageBubbleProps) {
  const isUser = message.role === 'user';
  const stopped = !isUser && message.content.endsWith(STOPPED_NOTE);
  const content = stopped
    ? message.content.slice(0, -STOPPED_NOTE.length).trimEnd()
    : message.content;
  const report = isUser ? parseChildReport(message.content) : null;
  const writer = message.provider ?? agentKind;
  const author = `${isUser ? 'user' : agentLabel(writer)}@${context}$`;

  if (isUser && assignment && !report) {
    return (
      <article
        aria-label={`Assignment from Mainloop, ${messageTime(message.created_at)}`}
        className="px-md py-sm border-border w-full border-l-2 bg-transparent"
      >
        <details data-testid="assignment-block">
          <summary className="text-muted-foreground min-h-11 cursor-pointer text-xs md:text-sm">
            Assignment from Mainloop · {messageTime(message.created_at)}
          </summary>
          <div className="mt-xs min-w-0">
            <Markdown className="text-foreground text-sm md:text-base">{content}</Markdown>
          </div>
        </details>
        {notice ? (
          <DeliveryNotice
            notice={notice}
            retryDisabled={retryDisabled}
            onRetry={onRetry ? () => onRetry(message) : undefined}
          />
        ) : null}
      </article>
    );
  }

  return (
    <article
      aria-label={`${report ? `${report.title} · ${report.outcome}` : isUser ? 'You' : agentLabel(writer)}, ${messageTime(message.created_at)}`}
      data-testid={report ? 'child-report' : undefined}
      className={cn(
        'px-md py-sm w-full border-l-2',
        report ? 'border-term-fg-muted bg-card' : 'border-border',
        !report && !isUser ? 'bg-card' : 'bg-transparent'
      )}
    >
      <div className="gap-xs flex flex-col">
        <span
          className={cn(
            'shrink-0 text-xs md:text-sm',
            report ? 'text-muted-foreground' : isUser ? 'text-term-accent-alt' : 'text-term-accent'
          )}
        >
          {report ? (
            <>
              {report.title} · {report.outcome}
            </>
          ) : (
            <>
              {author}
              {stopped ? <span className="text-term-yellow"> · stopped</span> : null}
            </>
          )}
        </span>
        <div className="min-w-0 flex-1">
          <Markdown className="text-foreground text-sm md:text-base">
            {report ? report.body : content}
          </Markdown>
          {stopped ? (
            <p className="mt-sm text-muted-foreground text-sm" data-testid="turn-stopped-note">
              {STOPPED_NOTE}
            </p>
          ) : null}
          {notice ? (
            <DeliveryNotice
              notice={notice}
              retryDisabled={retryDisabled}
              onRetry={onRetry ? () => onRetry(message) : undefined}
            />
          ) : null}
          <time dateTime={message.created_at} className="text-muted-foreground mt-xs block text-xs">
            {messageTime(message.created_at)}
          </time>
        </div>
      </div>
    </article>
  );
}
