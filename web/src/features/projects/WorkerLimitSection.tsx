import { useId, useState } from 'react';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { Button } from '@/components/ui/button';
import { useUpdateWorkerLimit, useWorkerLimit } from './queries';
import {
  failureText,
  parseWorkerLimit,
  WORKER_LIMIT_MAX,
  WORKER_LIMIT_MIN,
  workerLimitGlobalText,
  workerLimitSchema,
  workerLimitSourceText,
  workersRunningText,
  type WorkerLimit
} from './model';

/** The view when it matches the contract, else undefined: a bad one is not shown half-read. */
function useValidWorkerLimit(projectId: string) {
  const limit = useWorkerLimit(projectId);
  const view =
    limit.data && workerLimitSchema.safeParse(limit.data).success ? limit.data : undefined;
  return { limit, view };
}

/**
 * "2 of 6 workers running", beside the project's task list. It says nothing until the limit has
 * been read, and nothing if it cannot be: the Settings section below is where a failure is shown.
 */
export function WorkersRunning({ projectId }: { projectId: string }) {
  const { view } = useValidWorkerLimit(projectId);
  if (!view) return null;
  return (
    <p className="text-muted-foreground text-sm tabular-nums" data-testid="project-workers-running">
      {workersRunningText(view)}
    </p>
  );
}

/**
 * How many workers the project may run at once: how many run now, where the limit comes from, and
 * a field to set the project's own (1 to 100) or return to the default. The owner-wide limit
 * applies as well, and the section says when it is the one that binds.
 */
export function WorkerLimitSection({ projectId }: { projectId: string }) {
  const { limit, view } = useValidWorkerLimit(projectId);
  const update = useUpdateWorkerLimit(projectId);

  const save = (max_workers: number | null) =>
    update.mutate({ params: { path: { project_id: projectId } }, body: { max_workers } });

  return (
    <section
      aria-labelledby="worker-limit-heading"
      className="gap-md p-md border-border flex flex-col border"
      data-testid="worker-limit"
    >
      <h3 id="worker-limit-heading" className="text-lg font-medium">
        Workers
      </h3>

      {limit.isPending ? (
        <p role="status" className="text-muted-foreground text-sm">
          Loading the worker limit…
        </p>
      ) : null}

      {limit.isError && !view ? (
        <ErrorRecovery
          title="Could not load the worker limit."
          message={failureText(limit.error)}
          onRetry={() => void limit.refetch()}
          retryLabel="Try again"
          pending={limit.isFetching}
        />
      ) : null}

      {limit.data && !view ? (
        <ErrorRecovery
          title="Could not load the worker limit."
          message="Mainloop sent a worker limit that does not match the contract."
        />
      ) : null}

      {view ? (
        <LimitForm
          // The field starts again from the saved value whenever that changes.
          key={String(view.max_workers)}
          view={view}
          busy={update.isPending}
          saveError={update.error ? failureText(update.error) : null}
          onSave={save}
        />
      ) : null}
    </section>
  );
}

function LimitForm({
  view,
  busy,
  saveError,
  onSave
}: {
  view: WorkerLimit;
  busy: boolean;
  saveError: string | null;
  onSave: (value: number | null) => void;
}) {
  const fieldId = useId();
  const hintId = useId();
  const [draft, setDraft] = useState(String(view.effective_max_workers));
  const value = parseWorkerLimit(draft);
  const changed = value !== null && value !== view.max_workers;
  // Typing the default while on the default changes nothing either.
  const sameAsDefault = view.max_workers === null && value === view.default_max_workers;
  const canSave = changed && !sameAsDefault && !busy;

  return (
    <div className="gap-md flex flex-col">
      <p className="text-sm tabular-nums" data-testid="worker-limit-running">
        {workersRunningText(view)}.
      </p>
      <p className="text-muted-foreground text-sm" data-testid="worker-limit-source">
        {workerLimitSourceText(view)} {workerLimitGlobalText(view)}
      </p>

      <form
        className="gap-sm flex flex-col"
        onSubmit={(event) => {
          event.preventDefault();
          if (canSave && value !== null) onSave(value);
        }}
      >
        <label htmlFor={fieldId} className="text-sm">
          Workers at once
        </label>
        <div className="gap-sm flex flex-wrap items-center">
          <input
            id={fieldId}
            type="number"
            inputMode="numeric"
            min={WORKER_LIMIT_MIN}
            max={WORKER_LIMIT_MAX}
            step={1}
            value={draft}
            disabled={busy}
            aria-describedby={hintId}
            aria-invalid={value === null}
            onChange={(event) => setDraft(event.target.value)}
            className="border-border bg-background text-foreground px-md min-h-control w-24 border text-sm tabular-nums"
          />
          <Button type="submit" disabled={!canSave}>
            {busy ? 'Saving…' : 'Save limit'}
          </Button>
          {view.max_workers !== null ? (
            <Button type="button" variant="outline" disabled={busy} onClick={() => onSave(null)}>
              Use the default ({view.default_max_workers})
            </Button>
          ) : null}
        </div>
        <p
          id={hintId}
          className={value === null ? 'text-destructive text-sm' : 'text-muted-foreground text-sm'}
        >
          {value === null
            ? `Enter a whole number from ${WORKER_LIMIT_MIN} to ${WORKER_LIMIT_MAX}.`
            : 'A lower limit stops nothing that is running. It only holds new workers back until enough have ended.'}
        </p>

        {saveError ? (
          <p role="alert" className="text-destructive text-sm">
            {saveError}. The limit was not changed.
          </p>
        ) : null}
      </form>
    </div>
  );
}
