import { Link } from '@tanstack/react-router';
import { useEffect, useMemo, useState } from 'react';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { Button } from '@/components/ui/button';
import { loadFailure } from '@/features/sessions/model';
import { choiceScope } from '@/lib/healthFilters';
import { HealthFilters } from './HealthFilters';
import { HealthRowCard } from './HealthRowCard';
import {
  attemptChoices,
  COVERAGE_NOTE,
  droppedRowsText,
  EMPTY_NOTICE,
  filtersFrom,
  healthFailureText,
  MALFORMED_VIEW,
  PARTIAL_NOTICE,
  projectChoices,
  readView,
  retentionText,
  sessionChoices,
  sortByFailures,
  taskChoices,
  WIDE_MIN_PX,
  windowSummary,
  withSelected
} from './model';
import { useHealth, useProjects } from './queries';
import type { HealthSearch } from './search';

/**
 * Tool failures, reported tokens and approval state, grouped by task. The filters are the URL's:
 * this page reads them from `search` and writes them back through Apply, so a link or a reload
 * shows the same view.
 */
export function HealthPage({ search }: { search: HealthSearch }) {
  const filters = filtersFrom(search);
  const scope = choiceScope(filters);
  const health = useHealth(filters);
  // Task, session and attempt options come from the wider scope, so a narrowed view still offers
  // the other choices. When nothing is narrowed, the view itself is that scope.
  const scoped = useHealth(scope.filters, scope.narrowed);
  const projects = useProjects();
  const wide = useViewportWide();
  // The owner's toggle holds for the width it was made at. A width crossing returns to the default.
  const [toggle, setToggle] = useState<{ wide: boolean; open: boolean } | null>(null);
  const open = toggle && toggle.wide === wide ? toggle.open : wide;

  const view = useMemo(
    () => (health.data === undefined ? null : readView(health.data)),
    [health.data]
  );
  const choiceRows = useMemo(() => {
    if (!scope.narrowed) return view?.rows ?? [];
    const wider = scoped.data === undefined ? null : readView(scoped.data);
    return wider?.rows ?? [];
  }, [scope.narrowed, scoped.data, view]);

  const tasks = withSelected(taskChoices(choiceRows), search.task_id ?? '', 'Selected task');
  const sessions = withSelected(
    sessionChoices(choiceRows),
    search.session_id ?? '',
    'Selected session'
  );
  const attempts = withSelected(
    attemptChoices(choiceRows),
    search.attempt_id ?? '',
    'Selected attempt'
  );
  const projectOptions = withSelected(
    projectChoices(projects.data ?? []),
    search.project_id ?? '',
    'Selected project'
  );

  const busy = health.isFetching;
  const hours = filters.hours ?? 24;
  const refresh = () => void health.refetch();

  return (
    <article className="p-md gap-md mx-auto flex w-full max-w-6xl flex-col">
      <Link
        to="/"
        className="text-muted-foreground inline-flex min-h-11 items-center text-sm underline underline-offset-4"
      >
        Home
      </Link>

      <header className="gap-md flex flex-wrap items-start justify-between">
        <div>
          <h1 className="text-lg font-medium">Orchestration health</h1>
          <p className="text-muted-foreground mt-sm max-w-prose text-sm">
            Tool failures and reported tokens, alongside blocked claims and approval state.
          </p>
        </div>
        <Button variant="outline" onClick={refresh} disabled={busy}>
          {busy ? 'Refreshing…' : 'Refresh'}
        </Button>
      </header>

      <div className="border-border mt-sm py-sm border-y">
        <button
          type="button"
          aria-expanded={open}
          aria-controls="health-filters"
          onClick={() => setToggle({ wide, open: !open })}
          className="min-h-11 text-sm"
        >
          Filters · {windowSummary(hours)}
        </button>
        {open ? (
          <HealthFilters
            key={JSON.stringify(search)}
            id="health-filters"
            search={search}
            projects={projectOptions}
            tasks={tasks}
            sessions={sessions}
            attempts={attempts}
          />
        ) : null}
      </div>

      <div aria-live="polite" className="gap-md grid">
        {health.isPending ? (
          <p role="status" aria-busy="true" className="text-muted-foreground">
            Loading health…
          </p>
        ) : null}
        {health.isError && health.data === undefined ? (
          <ErrorRecovery
            title="Health could not be loaded."
            message={healthFailureText(loadFailure(health.error))}
            onRetry={refresh}
            pending={busy}
          />
        ) : null}
        {health.isError && health.data !== undefined ? (
          <ErrorRecovery
            title="Couldn't refresh health."
            message={`${healthFailureText(loadFailure(health.error))} The rows below are from the last successful read.`}
            onRetry={refresh}
            pending={busy}
          />
        ) : null}
        {health.data !== undefined && view === null ? (
          <ErrorRecovery
            title="Health could not be read."
            message={MALFORMED_VIEW}
            onRetry={refresh}
            pending={busy}
          />
        ) : null}
      </div>

      {view ? (
        <>
          <p className="text-muted-foreground max-w-prose text-sm">{COVERAGE_NOTE}</p>
          {view.partial ? (
            <p role="status" className="text-term-yellow">
              {PARTIAL_NOTICE}
            </p>
          ) : null}
          {view.dropped > 0 ? (
            <p role="status" className="text-term-yellow">
              {droppedRowsText(view.dropped)}
            </p>
          ) : null}
          {view.rows.length === 0 && view.dropped === 0 ? (
            <p className="text-muted-foreground py-xl">{EMPTY_NOTICE}</p>
          ) : null}

          <ul className="border-border divide-border divide-y border-y">
            {sortByFailures(view.rows).map((row) => (
              <li key={row.id} className="py-md">
                <HealthRowCard row={row} hours={hours} generation={health.dataUpdatedAt} />
              </li>
            ))}
          </ul>

          <p className="text-muted-foreground text-xs">{retentionText(view.retention)}</p>
        </>
      ) : null}
    </article>
  );
}

/** Whether the viewport is wide enough for the filters to start open. Follows resizes. */
function useViewportWide(): boolean {
  const [wide, setWide] = useState(() => window.innerWidth >= WIDE_MIN_PX);
  useEffect(() => {
    const update = () => setWide(window.innerWidth >= WIDE_MIN_PX);
    window.addEventListener('resize', update);
    return () => window.removeEventListener('resize', update);
  }, []);
  return wide;
}
