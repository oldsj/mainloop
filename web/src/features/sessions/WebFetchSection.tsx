import { useQueryClient } from '@tanstack/react-query';
import { $api, keys } from '@/api';
import { ControlsHeading } from '@/components/shared/controls-sheet';
import { cn } from '@/components/ui/cn';

const CHOICES = [
  { value: false, label: 'Off' },
  { value: true, label: 'On' }
] as const;

/**
 * The session's own web fetch flag, as a section of the session controls picker
 * (docs/specs/web-fetch.md). On, the session's agent may read public web pages with `web_fetch`;
 * it is off until the owner turns it on. A task created with web fetch gives the agent the tool
 * whatever this says, which the section notes.
 */
export function WebFetchSection({ sessionId }: { sessionId: string }) {
  const queryClient = useQueryClient();
  const native = $api.useQuery('get', '/sessions/{session_id}/native', {
    params: { path: { session_id: sessionId } }
  });
  const set = $api.useMutation('post', '/sessions/{session_id}/web-fetch', {
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: keys.sessionNative(sessionId) });
      void queryClient.invalidateQueries({ queryKey: keys.mainThread() });
    }
  });
  const state = native.data?.web_fetch;
  // While a change is on its way, show the choice made rather than the last read.
  const enabled = set.isPending ? set.variables.body.enabled : state?.enabled;

  return (
    <section className="flex flex-col gap-2" data-testid="web-fetch-section">
      <ControlsHeading>Web fetch</ControlsHeading>
      <div
        role="radiogroup"
        aria-label="Web fetch"
        className="border-border-subtle grid grid-cols-2 border"
      >
        {CHOICES.map((choice) => (
          <button
            key={choice.label}
            type="button"
            role="radio"
            aria-checked={enabled === choice.value}
            disabled={!state || set.isPending}
            onClick={() => {
              if (enabled === choice.value) return;
              set.mutate({
                params: { path: { session_id: sessionId } },
                body: { enabled: choice.value }
              });
            }}
            className={cn(
              'border-border-subtle hover:bg-accent min-h-control text-caption border-l first:border-l-0 disabled:pointer-events-none disabled:opacity-50',
              enabled === choice.value && 'bg-term-selection'
            )}
            data-testid={`web-fetch-${choice.label.toLowerCase()}`}
          >
            {choice.label}
          </button>
        ))}
      </div>
      <p className="text-muted-foreground text-caption">
        Lets this agent read public web pages. Every fetch is recorded. Turning it off applies to
        its next call; turning it on, when it next lists its tools.
      </p>
      {state?.from_task ? (
        <p className="text-muted-foreground text-caption" data-testid="web-fetch-from-task">
          Its task was created with web fetch, so the agent has it either way.
        </p>
      ) : null}
      {set.isError ? (
        <p role="alert" className="text-destructive text-caption">
          Web fetch was not changed. Try again.
        </p>
      ) : null}
    </section>
  );
}
