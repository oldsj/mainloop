import { useMemo, useState } from 'react';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import {
  droppedObservationsText,
  eventLines,
  MALFORMED_OBSERVATIONS,
  OBSERVATIONS_EMPTY,
  OBSERVATIONS_PARTIAL,
  readObservations
} from './model';
import { useHealthEvents } from './queries';

/**
 * Source events for one session. They load only when the owner asks, so a page of rows makes no
 * event reads until then. A failed reload keeps the events already shown, with an inline error.
 */
export function SessionObservations({ sessionId, hours }: { sessionId: string; hours: number }) {
  const [requested, setRequested] = useState(false);
  const events = useHealthEvents(sessionId, hours, requested);
  // A remount (the page refreshed) starts unrequested. The query cache still holds the last read,
  // so only a requested read is shown; the cached events do not come back unasked.
  const shown = requested ? events.data : undefined;
  const observations = useMemo(
    () => (shown === undefined ? null : readObservations(shown)),
    [shown]
  );
  const loading = events.isFetching;
  const failed = requested && events.isError;

  return (
    <div className="gap-sm grid">
      <button
        type="button"
        disabled={loading}
        onClick={() => (requested ? void events.refetch() : setRequested(true))}
        className="border-border hover:border-foreground px-md min-h-11 justify-self-start border text-sm disabled:opacity-50"
      >
        {loading ? 'Loading…' : 'Load source observations'}
      </button>

      {failed && shown === undefined ? (
        <ErrorRecovery
          title="Observations could not be loaded."
          message="Mainloop did not return the source events for this session."
          onRetry={() => void events.refetch()}
          pending={loading}
        />
      ) : null}
      {failed && shown !== undefined ? (
        <ErrorRecovery
          title="Couldn't refresh observations."
          message="The events already shown are from the last successful read."
          onRetry={() => void events.refetch()}
          pending={loading}
        />
      ) : null}
      {shown !== undefined && observations === null ? (
        <ErrorRecovery
          title="Observations could not be read."
          message={MALFORMED_OBSERVATIONS}
          onRetry={() => void events.refetch()}
          pending={loading}
        />
      ) : null}

      {observations ? (
        <>
          {observations.partial ? <p className="text-term-yellow">{OBSERVATIONS_PARTIAL}</p> : null}
          {observations.dropped > 0 ? (
            <p className="text-term-yellow">{droppedObservationsText(observations.dropped)}</p>
          ) : null}
          {observations.events.length === 0 && observations.dropped === 0 ? (
            <p className="text-muted-foreground">{OBSERVATIONS_EMPTY}</p>
          ) : null}
          <ol className="gap-sm grid">
            {observations.events.map((event) => (
              <li
                key={`${event.payload.sequence}-${event.event_id}`}
                className="text-muted-foreground text-xs break-words"
              >
                {eventLines(event).map((line, index) => (
                  <span key={index} className="block">
                    {line}
                  </span>
                ))}
              </li>
            ))}
          </ol>
        </>
      ) : null}
    </div>
  );
}
