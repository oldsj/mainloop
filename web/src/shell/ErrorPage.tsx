import { Link } from '@tanstack/react-router';
import { buttonVariants } from '@/components/ui/button';
import { cn } from '@/components/ui/cn';

export function ErrorPage({
  status,
  message,
  onRetry
}: {
  status?: number;
  message: string;
  onRetry?: () => void;
}) {
  return (
    <main role="alert" data-testid="error-page" className="p-lg mx-auto max-w-xl">
      {status !== undefined && <p className="text-destructive text-3xl">{status}</p>}
      <p className="mt-sm text-foreground text-sm">{message}</p>
      <div className="mt-md gap-sm flex">
        <Link to="/" className={cn(buttonVariants({ variant: 'outline' }))}>
          Back to home
        </Link>
        {onRetry && (
          <button type="button" onClick={onRetry} className={cn(buttonVariants())}>
            Try again
          </button>
        )}
      </div>
    </main>
  );
}

/** Route render or loader failure (the router's error boundary). */
export function RouteError({ error, reset }: { error: unknown; reset: () => void }) {
  const message = error instanceof Error && error.message ? error.message : 'Something went wrong.';
  return <ErrorPage message={message} onRetry={reset} />;
}

export function NotFound() {
  return <ErrorPage status={404} message="Page not found." />;
}
