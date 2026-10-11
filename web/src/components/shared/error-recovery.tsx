import { Button } from '@/components/ui/button';

export interface ErrorRecoveryProps {
  /** Short heading, for example "Could not load the task". */
  title: string;
  /** What happened, in words the owner can act on. Server detail goes here as text. */
  message: string;
  /** Re-runs the failed step. Omit it when there is nothing safe to retry. */
  onRetry?: () => void;
  retryLabel?: string;
  /** The retry is in flight: the button is disabled so it cannot be pressed twice. */
  pending?: boolean;
}

/** An error with its next step. role="alert" announces it once, when it appears. */
export function ErrorRecovery({
  title,
  message,
  onRetry,
  retryLabel = 'Try again',
  pending = false
}: ErrorRecoveryProps) {
  return (
    <div
      role="alert"
      className="gap-sm border-destructive p-md text-foreground grid rounded-none border text-sm"
    >
      <p className="text-destructive font-medium">{title}</p>
      <p className="text-muted-foreground break-words">{message}</p>
      {onRetry ? (
        <div>
          <Button variant="outline" onClick={onRetry} disabled={pending}>
            {retryLabel}
          </Button>
        </div>
      ) : null}
    </div>
  );
}
