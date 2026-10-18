import { Button } from '@/components/ui/button';
import { cn } from '@/components/ui/cn';
import type { DeliveryNotice as Notice } from '@/lib/delivery';

export interface DeliveryNoticeProps {
  notice: Notice;
  /** Offered only when the notice is retryable. Retry sends the text as a new message. */
  onRetry?: () => void;
  retryDisabled?: boolean;
}

const NOTICE_STYLE: Record<DeliveryNoticeProps['notice']['state'], string> = {
  queued: 'border-term-fg-muted text-term-fg-muted',
  failed: 'border-term-red text-term-red',
  uncertain: 'border-term-yellow text-term-yellow'
};

/**
 * Says what happened to a message that was not delivered, with the server's reason, or that it is
 * still queued behind the turn in flight. A failed delivery can be retried; a queued or
 * unconfirmed one never is: a queued send has not reached the agent yet, and the agent may already
 * have received an unconfirmed one.
 */
export function DeliveryNotice({ notice, onRetry, retryDisabled = false }: DeliveryNoticeProps) {
  return (
    <div
      role="status"
      data-testid="delivery-notice"
      className={cn(
        'mt-sm gap-x-md gap-y-xs px-sm py-xs bg-muted flex flex-wrap items-center border text-xs',
        NOTICE_STYLE[notice.state]
      )}
    >
      <span className="font-medium" data-testid="delivery-notice-label">
        {notice.label}
      </span>
      {notice.reason ? (
        <span className="min-w-0 [overflow-wrap:anywhere]" data-testid="delivery-notice-reason">
          {notice.reason}
        </span>
      ) : null}
      {notice.state === 'queued' ? <span>Waiting for the current turn to finish.</span> : null}
      {notice.state === 'uncertain' ? (
        <span>Not replayed. Check for a reply before sending again.</span>
      ) : null}
      {notice.retryable && onRetry ? (
        <Button
          variant="outline"
          size="sm"
          className="border-current text-current"
          disabled={retryDisabled}
          onClick={onRetry}
          title="Sends the same text as a new message"
          aria-label="Retry sending this message"
          data-testid="delivery-retry"
        >
          retry
        </Button>
      ) : null}
    </div>
  );
}
